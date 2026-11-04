// Package store is Thari's data access. Every function takes the signed-in
// Principal and applies the permission rules in the query itself: a buyer's
// query can only ever return that buyer's orders, and an order the caller
// may not see is reported as not found rather than forbidden, so ids reveal
// nothing.
package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/thari/internal/domain"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("not allowed for your role")
)

// InvalidError is a validation failure, optionally for one field.
type InvalidError struct {
	Field string `json:"field,omitempty"`
	Msg   string `json:"error"`
}

func (e *InvalidError) Error() string { return e.Msg }

func invalid(field, msg string) error { return &InvalidError{field, msg} }

// ConflictError means the order changed since the caller loaded it.
type ConflictError struct{ Current *Order }

func (e *ConflictError) Error() string { return "this order was changed by someone else" }

type Store struct{ DB *pgxpool.Pool }

type StageProgress struct {
	Stage string `json:"stage"`
	Label string `json:"label"`
	Done  int    `json:"done"`
}

type Order struct {
	ID             int64           `json:"id"`
	BuyerID        int64           `json:"buyer_id"`
	Buyer          string          `json:"buyer"`
	PONumber       string          `json:"po_number"`
	Style          string          `json:"style"`
	Description    string          `json:"description"`
	Colour         string          `json:"colour"`
	Quantity       int             `json:"quantity"`
	UnitPricePaise *int64          `json:"unit_price_paise,omitempty"` // omitted for roles that may not see prices
	OrderedOn      string          `json:"ordered_on"`
	DueDate        string          `json:"due_date"`
	Status         string          `json:"status"`
	Version        int             `json:"version"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Progress       []StageProgress `json:"progress"`
	Completion     float64         `json:"completion"`
	Stage          string          `json:"stage"`
	Risk           domain.Risk     `json:"risk"`
	factoryID      int64
}

type Event struct {
	ID           int64           `json:"id"`
	OrderID      *int64          `json:"order_id,omitempty"`
	PONumber     string          `json:"po_number,omitempty"`
	At           time.Time       `json:"at"`
	Kind         string          `json:"kind"`
	User         string          `json:"user"`
	Role         string          `json:"role"`
	Detail       json.RawMessage `json:"detail"`
	BuyerVisible bool            `json:"buyer_visible"`
}

type Inspection struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	Inspector  string    `json:"inspector"`
	LotSize    int       `json:"lot_size"`
	SampleSize int       `json:"sample_size"`
	Critical   int       `json:"critical"`
	Major      int       `json:"major"`
	Minor      int       `json:"minor"`
	Result     string    `json:"result"`
	Notes      string    `json:"notes"`
}

type FileInfo struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	Size        int       `json:"size"`
	At          time.Time `json:"at"`
	UploadedBy  string    `json:"uploaded_by"`
}

type OrderDetail struct {
	Order       Order                  `json:"order"`
	Events      []Event                `json:"events"`
	Inspections []Inspection           `json:"inspections"`
	Files       []FileInfo             `json:"files"`
	Plan        domain.AQLPlan         `json:"plan"` // for the next final inspection
	Can         map[domain.Action]bool `json:"can"`
}

// Notice is sent on Postgres channel "thari" after a change commits, so
// every server instance can push it to the browsers allowed to see it.
type Notice struct {
	FactoryID    int64  `json:"factory_id"`
	BuyerID      int64  `json:"buyer_id"`
	OrderID      int64  `json:"order_id"`
	EventID      int64  `json:"event_id"`
	Kind         string `json:"kind"`
	BuyerVisible bool   `json:"buyer_visible"`
}

var stagesArray = domain.Stages

const orderCols = `o.id, o.factory_id, o.buyer_id, b.name, o.po_number, o.style, o.description, o.colour, o.quantity,
	o.unit_price_paise, o.ordered_on::text, o.due_date::text, o.status, o.version, o.updated_at,
	(select array_agg(pr.done order by array_position($1::text[], pr.stage)) from progress pr where pr.order_id = o.id) as done`

func scanOrder(row pgx.Row, p domain.Principal, today time.Time) (*Order, error) {
	var o Order
	var price int64
	var done []int32
	if err := row.Scan(&o.ID, &o.factoryID, &o.BuyerID, &o.Buyer, &o.PONumber, &o.Style, &o.Description, &o.Colour, &o.Quantity,
		&price, &o.OrderedOn, &o.DueDate, &o.Status, &o.Version, &o.UpdatedAt, &done); err != nil {
		return nil, err
	}
	d := make([]int, len(done))
	for i, v := range done {
		d[i] = int(v)
		o.Progress = append(o.Progress, StageProgress{domain.Stages[i], domain.StageLabel[domain.Stages[i]], int(v)})
	}
	if p.Can(domain.ViewPrice, o.factoryID, o.BuyerID) {
		o.UnitPricePaise = &price
	}
	o.Completion = domain.Completion(d, o.Quantity)
	o.Stage = domain.CurrentStage(d, o.Quantity)
	from, _ := time.Parse(time.DateOnly, o.OrderedOn)
	due, _ := time.Parse(time.DateOnly, o.DueDate)
	o.Risk = domain.RiskOf(o.Status, from, due, today, o.Completion)
	return &o, nil
}

// visibility is the where-clause every order query uses.
func visibility(p domain.Principal, first int) (string, []any) {
	if p.Role == domain.Buyer {
		return fmt.Sprintf("o.factory_id = $%d and o.buyer_id = $%d", first, first+1), []any{p.FactoryID, p.OrgID}
	}
	return fmt.Sprintf("o.factory_id = $%d", first), []any{p.FactoryID}
}

type Filter struct {
	Status  string // open (default), shipped, cancelled, all
	BuyerID int64
	Risk    string // on_track, at_risk, late
	Stage   string
	Q       string // PO number or style contains
	Cursor  string
	Limit   int
}

func encodeCursor(due string, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(due + "|" + strconv.FormatInt(id, 10)))
}

func decodeCursor(c string) (string, int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", 0, invalid("cursor", "bad cursor")
	}
	due, id, ok := strings.Cut(string(b), "|")
	n, err := strconv.ParseInt(id, 10, 64)
	if _, derr := time.Parse(time.DateOnly, due); !ok || err != nil || derr != nil {
		return "", 0, invalid("cursor", "bad cursor")
	}
	return due, n, nil
}

// ListOrders returns one page of orders by due date, with a cursor for the next.
func (s *Store) ListOrders(ctx context.Context, p domain.Principal, f Filter, today time.Time) ([]Order, string, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 25
	}
	where, args := visibility(p, 3)
	args = append([]any{stagesArray, today.Format(time.DateOnly)}, args...)
	add := func(cond string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" and "+cond, len(args))
	}
	switch f.Status {
	case "", "open":
		where += " and o.status = 'open'"
	case "shipped", "cancelled":
		add("o.status = $%d", f.Status)
	case "all":
	default:
		return nil, "", invalid("status", "unknown status")
	}
	if f.BuyerID != 0 {
		add("o.buyer_id = $%d", f.BuyerID)
	}
	if f.Q = strings.TrimSpace(f.Q); f.Q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(f.Q) + "%"
		add("(o.po_number ilike $%d or o.style ilike $"+strconv.Itoa(len(args)+1)+")", like)
		args = append(args, like)
	}
	if f.Cursor != "" {
		due, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		args = append(args, due, id)
		where += fmt.Sprintf(" and (o.due_date, o.id) > ($%d::date, $%d)", len(args)-1, len(args))
	}
	inner := `select ` + orderCols + `,
		(select coalesce(sum(pr.done), 0)::float8 from progress pr where pr.order_id = o.id) / (o.quantity * 5) as completion
		from orders o join orgs b on b.id = o.buyer_id where ` + where
	q := `select * from (` + inner + `) x where $2::date is not null`
	switch f.Risk {
	case "":
	case "on_track", "at_risk", "late":
		args = append(args, f.Risk)
		q += fmt.Sprintf(" and order_risk(x.status, x.ordered_on::date, x.due_date::date, $2::date, x.completion) = $%d", len(args))
	default:
		return nil, "", invalid("risk", "unknown risk")
	}
	if f.Stage != "" {
		i := domain.StageIndex(f.Stage)
		if i < 0 && f.Stage != "ready" {
			return nil, "", invalid("stage", "unknown stage")
		}
		// first stage with done < quantity
		args = append(args, f.Stage)
		q += fmt.Sprintf(` and coalesce((select s from unnest($1::text[], x.done) with ordinality u(s, d, n) where d < x.quantity order by n limit 1), 'ready') = $%d`, len(args))
	}
	q += fmt.Sprintf(" order by x.due_date, x.id limit %d", f.Limit+1)
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		var comp float64
		o, err := scanOrder(scanExtra{rows, &comp}, p, today)
		if err != nil {
			return nil, "", err
		}
		out = append(out, *o)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		out = out[:f.Limit]
		last := out[len(out)-1]
		next = encodeCursor(last.DueDate, last.ID)
	}
	return out, next, nil
}

// scanExtra lets scanOrder read rows that carry extra trailing columns.
type scanExtra struct {
	rows  pgx.Rows
	extra *float64
}

func (s scanExtra) Scan(dest ...any) error { return s.rows.Scan(append(dest, s.extra)...) }

func (s *Store) getOrder(ctx context.Context, q pgx.Tx, p domain.Principal, id int64, today time.Time, lock bool) (*Order, error) {
	where, args := visibility(p, 3)
	allArgs := append([]any{stagesArray, id}, args...)
	if lock {
		// Lock first, in its own statement. Reading the progress counts in the
		// same statement as "for update" would be wrong under read committed:
		// after waiting for the lock, Postgres rechecks the locked row but a
		// subquery still sees the data from before the wait, so concurrent
		// updates would each start from stale counts and overwrite each other.
		var locked int64
		err := q.QueryRow(ctx, `select o.id from orders o where o.id = $2 and `+where+` and $1::text[] is not null for update`, allArgs...).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
	}
	sql := `select ` + orderCols + ` from orders o join orgs b on b.id = o.buyer_id where o.id = $2 and ` + where
	var row pgx.Row
	if q != nil {
		row = q.QueryRow(ctx, sql, allArgs...)
	} else {
		row = s.DB.QueryRow(ctx, sql, allArgs...)
	}
	o, err := scanOrder(row, p, today)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}

func (s *Store) GetOrder(ctx context.Context, p domain.Principal, id int64, today time.Time) (*OrderDetail, error) {
	o, err := s.getOrder(ctx, nil, p, id, today, false)
	if err != nil {
		return nil, err
	}
	d := &OrderDetail{Order: *o, Events: []Event{}, Inspections: []Inspection{}, Files: []FileInfo{}, Can: map[domain.Action]bool{}}
	for _, a := range []domain.Action{domain.ViewPrice, domain.EditOrder, domain.AddProgress, domain.CorrectProgress, domain.Inspect,
		domain.Comment, domain.UploadFile, domain.Ship, domain.ViewInternal} {
		d.Can[a] = p.Can(a, o.factoryID, o.BuyerID)
	}
	internal := d.Can[domain.ViewInternal]
	rows, err := s.DB.Query(ctx, `select e.id, e.at, e.kind, coalesce(u.name, 'system'), coalesce(u.role, ''), e.detail, e.buyer_visible
		from events e left join users u on u.id = e.user_id where e.order_id = $1 and (e.buyer_visible or $2) order by e.id desc limit 200`, id, internal)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &e.User, &e.Role, &e.Detail, &e.BuyerVisible); err != nil {
			rows.Close()
			return nil, err
		}
		d.Events = append(d.Events, e)
	}
	rows.Close()
	rows, err = s.DB.Query(ctx, `select i.id, i.at, coalesce(u.name, ''), i.lot_size, i.sample_size, i.critical, i.major, i.minor, i.result, i.notes
		from inspections i left join users u on u.id = i.inspector_id where i.order_id = $1 order by i.id desc`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var i Inspection
		if err := rows.Scan(&i.ID, &i.At, &i.Inspector, &i.LotSize, &i.SampleSize, &i.Critical, &i.Major, &i.Minor, &i.Result, &i.Notes); err != nil {
			rows.Close()
			return nil, err
		}
		d.Inspections = append(d.Inspections, i)
	}
	rows.Close()
	rows, err = s.DB.Query(ctx, `select f.id, f.name, f.content_type, f.size, f.at, coalesce(u.name, '') from files f left join users u on u.id = f.uploaded_by
		where f.order_id = $1 order by f.id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f FileInfo
		if err := rows.Scan(&f.ID, &f.Name, &f.ContentType, &f.Size, &f.At, &f.UploadedBy); err != nil {
			rows.Close()
			return nil, err
		}
		d.Files = append(d.Files, f)
	}
	rows.Close()
	d.Plan = domain.PlanFor(max(o.Progress[len(o.Progress)-1].Done, 1))
	return d, nil
}

// event appends to the timeline and queues a notice; both commit with tx.
func event(ctx context.Context, tx pgx.Tx, p domain.Principal, o *Order, kind string, detail any, buyerVisible bool) error {
	b, _ := json.Marshal(detail)
	var id int64
	if err := tx.QueryRow(ctx, `insert into events (factory_id, order_id, user_id, kind, detail, buyer_visible) values ($1, $2, $3, $4, $5, $6) returning id`,
		o.factoryID, o.ID, p.UserID, kind, b, buyerVisible).Scan(&id); err != nil {
		return err
	}
	n, _ := json.Marshal(Notice{o.factoryID, o.BuyerID, o.ID, id, kind, buyerVisible})
	_, err := tx.Exec(ctx, `select pg_notify('thari', $1)`, string(n)) // delivered on commit
	return err
}

func (s *Store) tx(ctx context.Context, f func(pgx.Tx) error) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type OrderInput struct {
	BuyerID        int64  `json:"buyer_id"`
	PONumber       string `json:"po_number"`
	Style          string `json:"style"`
	Description    string `json:"description"`
	Colour         string `json:"colour"`
	Quantity       int    `json:"quantity"`
	UnitPricePaise int64  `json:"unit_price_paise"`
	OrderedOn      string `json:"ordered_on"`
	DueDate        string `json:"due_date"`
}

func (in *OrderInput) validate() error {
	in.PONumber, in.Style = strings.TrimSpace(in.PONumber), strings.TrimSpace(in.Style)
	in.Description, in.Colour = strings.TrimSpace(in.Description), strings.TrimSpace(in.Colour)
	switch {
	case in.PONumber == "" || len(in.PONumber) > 40:
		return invalid("po_number", "PO number is required (up to 40 characters)")
	case in.Style == "" || len(in.Style) > 120:
		return invalid("style", "style is required (up to 120 characters)")
	case len(in.Description) > 2000:
		return invalid("description", "description is too long")
	case in.Quantity < 1 || in.Quantity > 1000000:
		return invalid("quantity", "quantity must be between 1 and 1,000,000")
	case in.UnitPricePaise < 0:
		return invalid("unit_price_paise", "price cannot be negative")
	}
	from, err1 := time.Parse(time.DateOnly, in.OrderedOn)
	due, err2 := time.Parse(time.DateOnly, in.DueDate)
	if err1 != nil {
		return invalid("ordered_on", "order date must be YYYY-MM-DD")
	}
	if err2 != nil || due.Before(from) {
		return invalid("due_date", "due date must be on or after the order date")
	}
	return nil
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func (s *Store) CreateOrder(ctx context.Context, p domain.Principal, in OrderInput, today time.Time) (*Order, error) {
	if !p.CanDo(domain.CreateOrder) || p.Role == domain.Buyer {
		return nil, ErrForbidden
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	var id int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from orgs where id = $1 and kind = 'buyer' and factory_id = $2)`, in.BuyerID, p.FactoryID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return invalid("buyer_id", "choose one of your buyers")
		}
		err := tx.QueryRow(ctx, `insert into orders (factory_id, buyer_id, po_number, style, description, colour, quantity, unit_price_paise, ordered_on, due_date)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) returning id`,
			p.FactoryID, in.BuyerID, in.PONumber, in.Style, in.Description, in.Colour, in.Quantity, in.UnitPricePaise, in.OrderedOn, in.DueDate).Scan(&id)
		if isUnique(err) {
			return invalid("po_number", "an order with this PO number already exists")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into progress (order_id, stage) select $1, unnest($2::text[])`, id, stagesArray); err != nil {
			return err
		}
		o := &Order{ID: id, BuyerID: in.BuyerID, factoryID: p.FactoryID}
		return event(ctx, tx, p, o, "created", map[string]any{"po_number": in.PONumber, "quantity": in.Quantity, "due_date": in.DueDate}, true)
	})
	if err != nil {
		return nil, err
	}
	return s.getOrder(ctx, nil, p, id, today, false)
}

// OrderPatch is an edit; nil fields are left unchanged.
type OrderPatch struct {
	Version        int     `json:"version"`
	Style          *string `json:"style"`
	Description    *string `json:"description"`
	Colour         *string `json:"colour"`
	Quantity       *int    `json:"quantity"`
	UnitPricePaise *int64  `json:"unit_price_paise"`
	DueDate        *string `json:"due_date"`
}

// UpdateOrder applies an edit if the order is still at the version the
// caller loaded; otherwise it returns a ConflictError with the current order.
func (s *Store) UpdateOrder(ctx context.Context, p domain.Principal, id int64, patch OrderPatch, today time.Time) (*Order, error) {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		o, err := s.getOrder(ctx, tx, p, id, today, true)
		if err != nil {
			return err
		}
		if !p.Can(domain.EditOrder, o.factoryID, o.BuyerID) {
			return ErrForbidden
		}
		if o.Status != "open" {
			return invalid("", "only open orders can be edited")
		}
		if patch.Version != o.Version {
			return &ConflictError{Current: o}
		}
		price := int64(0)
		if o.UnitPricePaise != nil {
			price = *o.UnitPricePaise
		}
		in := OrderInput{BuyerID: o.BuyerID, PONumber: o.PONumber, Style: o.Style, Description: o.Description, Colour: o.Colour,
			Quantity: o.Quantity, UnitPricePaise: price, OrderedOn: o.OrderedOn, DueDate: o.DueDate}
		changes := map[string]any{}
		set := func(name string, old, new any) {
			if fmt.Sprint(old) != fmt.Sprint(new) {
				changes[name] = map[string]any{"from": old, "to": new}
			}
		}
		if patch.Style != nil {
			set("style", in.Style, *patch.Style)
			in.Style = *patch.Style
		}
		if patch.Description != nil {
			set("description", in.Description, *patch.Description)
			in.Description = *patch.Description
		}
		if patch.Colour != nil {
			set("colour", in.Colour, *patch.Colour)
			in.Colour = *patch.Colour
		}
		if patch.Quantity != nil {
			set("quantity", in.Quantity, *patch.Quantity)
			in.Quantity = *patch.Quantity
		}
		if patch.UnitPricePaise != nil {
			set("unit_price_paise", in.UnitPricePaise, *patch.UnitPricePaise)
			in.UnitPricePaise = *patch.UnitPricePaise
		}
		if patch.DueDate != nil {
			set("due_date", in.DueDate, *patch.DueDate)
			in.DueDate = *patch.DueDate
		}
		if err := in.validate(); err != nil {
			return err
		}
		if len(changes) == 0 {
			return nil
		}
		if o.Progress[0].Done > in.Quantity {
			return invalid("quantity", fmt.Sprintf("%d pieces already have fabric; the quantity cannot be lower", o.Progress[0].Done))
		}
		if _, err := tx.Exec(ctx, `update orders set style = $2, description = $3, colour = $4, quantity = $5, unit_price_paise = $6, due_date = $7,
			version = version + 1, updated_at = now() where id = $1`, id, in.Style, in.Description, in.Colour, in.Quantity, in.UnitPricePaise, in.DueDate); err != nil {
			return err
		}
		_, priceChanged := changes["unit_price_paise"]
		if priceChanged {
			// the price change is internal; record it separately from the buyer-visible part
			if err := event(ctx, tx, p, o, "edited", map[string]any{"unit_price_paise": changes["unit_price_paise"]}, false); err != nil {
				return err
			}
			delete(changes, "unit_price_paise")
		}
		if len(changes) > 0 {
			return event(ctx, tx, p, o, "edited", changes, true)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.getOrder(ctx, nil, p, id, today, false)
}

// AddProgress records pieces completing a stage. The order's progress rows
// are locked first, so concurrent updates from the factory floor are applied
// one at a time against the latest counts and none is lost.
func (s *Store) AddProgress(ctx context.Context, p domain.Principal, id int64, stage string, delta int, note string, today time.Time) (*Order, error) {
	if len(note) > 500 {
		return nil, invalid("note", "note is too long")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		o, err := s.getOrder(ctx, tx, p, id, today, true)
		if err != nil {
			return err
		}
		if !p.Can(domain.AddProgress, o.factoryID, o.BuyerID) || delta < 0 && !p.Can(domain.CorrectProgress, o.factoryID, o.BuyerID) {
			return ErrForbidden
		}
		if o.Status != "open" {
			return invalid("", "this order is "+o.Status)
		}
		done := make([]int, len(o.Progress))
		for i, sp := range o.Progress {
			done[i] = sp.Done
		}
		n, err := domain.ApplyProgress(done, o.Quantity, stage, delta)
		if err != nil {
			var pe *domain.ProgressError
			if errors.As(err, &pe) || errors.Is(err, domain.ErrUnknownStage) {
				return invalid("delta", err.Error())
			}
			return err
		}
		if _, err := tx.Exec(ctx, `update progress set done = $3 where order_id = $1 and stage = $2`, id, stage, n); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update orders set version = version + 1, updated_at = now() where id = $1`, id); err != nil {
			return err
		}
		return event(ctx, tx, p, o, "progress", map[string]any{"stage": stage, "delta": delta, "done": n, "note": strings.TrimSpace(note)}, true)
	})
	if err != nil {
		return nil, err
	}
	return s.getOrder(ctx, nil, p, id, today, false)
}

func (s *Store) Comment(ctx context.Context, p domain.Principal, id int64, text string, internal bool, today time.Time) error {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 2000 {
		return invalid("text", "comment must be 1 to 2,000 characters")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		o, err := s.getOrder(ctx, tx, p, id, today, false)
		if err != nil {
			return err
		}
		if !p.Can(domain.Comment, o.factoryID, o.BuyerID) || internal && !p.Can(domain.ViewInternal, o.factoryID, o.BuyerID) {
			return ErrForbidden
		}
		return event(ctx, tx, p, o, "comment", map[string]any{"text": text, "internal": internal}, !internal)
	})
}

// Inspect records a final inspection of the packed pieces, sampled and
// judged by the AQL plan for that lot size.
func (s *Store) Inspect(ctx context.Context, p domain.Principal, id int64, critical, major, minor int, notes string, today time.Time) (*Inspection, error) {
	if critical < 0 || major < 0 || minor < 0 {
		return nil, invalid("major", "defect counts cannot be negative")
	}
	if len(notes) > 1000 {
		return nil, invalid("notes", "notes are too long")
	}
	var ins Inspection
	err := s.tx(ctx, func(tx pgx.Tx) error {
		o, err := s.getOrder(ctx, tx, p, id, today, true)
		if err != nil {
			return err
		}
		if !p.Can(domain.Inspect, o.factoryID, o.BuyerID) {
			return ErrForbidden
		}
		packed := o.Progress[len(o.Progress)-1].Done
		if packed == 0 {
			return invalid("", "nothing is packed yet; final inspection samples packed pieces")
		}
		plan := domain.PlanFor(packed)
		if critical+major+minor > plan.SampleSize {
			return invalid("major", fmt.Sprintf("more defects than the %d pieces sampled", plan.SampleSize))
		}
		ins = Inspection{LotSize: packed, SampleSize: plan.SampleSize, Critical: critical, Major: major, Minor: minor,
			Result: plan.Judge(critical, major, minor), Notes: strings.TrimSpace(notes)}
		if err := tx.QueryRow(ctx, `insert into inspections (order_id, inspector_id, lot_size, sample_size, critical, major, minor, result, notes)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9) returning id, at`, id, p.UserID, ins.LotSize, ins.SampleSize, critical, major, minor, ins.Result, ins.Notes).Scan(&ins.ID, &ins.At); err != nil {
			return err
		}
		return event(ctx, tx, p, o, "inspection", map[string]any{"result": ins.Result, "sample_size": ins.SampleSize, "lot_size": packed,
			"critical": critical, "major": major, "minor": minor, "accept_major": plan.AcceptMajor, "accept_minor": plan.AcceptMinor}, true)
	})
	if err != nil {
		return nil, err
	}
	return &ins, nil
}

// Ship marks an order shipped. It must be fully packed and its most recent
// final inspection, made after the last packing change, must have passed.
func (s *Store) Ship(ctx context.Context, p domain.Principal, id int64, today time.Time) (*Order, error) {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		o, err := s.getOrder(ctx, tx, p, id, today, true)
		if err != nil {
			return err
		}
		if !p.Can(domain.Ship, o.factoryID, o.BuyerID) {
			return ErrForbidden
		}
		if o.Status != "open" {
			return invalid("", "this order is "+o.Status)
		}
		if packed := o.Progress[len(o.Progress)-1].Done; packed < o.Quantity {
			return invalid("", fmt.Sprintf("only %d of %d pieces are packed", packed, o.Quantity))
		}
		var result string
		var lot int
		err = tx.QueryRow(ctx, `select result, lot_size from inspections where order_id = $1 order by id desc limit 1`, id).Scan(&result, &lot)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (result != "pass" || lot < o.Quantity) {
			return invalid("", "a passed final inspection of the full packed quantity is required before shipping")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update orders set status = 'shipped', version = version + 1, updated_at = now() where id = $1`, id); err != nil {
			return err
		}
		return event(ctx, tx, p, o, "shipped", map[string]any{"quantity": o.Quantity}, true)
	})
	if err != nil {
		return nil, err
	}
	return s.getOrder(ctx, nil, p, id, today, false)
}

const MaxFile = 2 << 20

func (s *Store) AddFile(ctx context.Context, p domain.Principal, id int64, name, contentType string, data []byte, today time.Time) (*FileInfo, error) {
	if len(data) > MaxFile {
		return nil, invalid("file", "files can be up to 2 MB")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return nil, invalid("file", "file name is required")
	}
	f := FileInfo{Name: name, ContentType: contentType, Size: len(data)}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		o, err := s.getOrder(ctx, tx, p, id, today, false)
		if err != nil {
			return err
		}
		if !p.Can(domain.UploadFile, o.factoryID, o.BuyerID) {
			return ErrForbidden
		}
		if err := tx.QueryRow(ctx, `insert into files (order_id, name, content_type, size, data, uploaded_by) values ($1, $2, $3, $4, $5, $6) returning id, at`,
			id, name, contentType, len(data), data, p.UserID).Scan(&f.ID, &f.At); err != nil {
			return err
		}
		return event(ctx, tx, p, o, "file", map[string]any{"name": name, "size": len(data)}, true)
	})
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// File returns a file if the caller may see its order.
func (s *Store) File(ctx context.Context, p domain.Principal, fileID int64) (name, contentType string, data []byte, err error) {
	where, args := visibility(p, 2)
	err = s.DB.QueryRow(ctx, `select f.name, f.content_type, f.data from files f join orders o on o.id = f.order_id where f.id = $1 and `+where,
		append([]any{fileID}, args...)...).Scan(&name, &contentType, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

type Buyer struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	City   string `json:"city"`
	Orders int    `json:"open_orders"`
}

func (s *Store) Buyers(ctx context.Context, p domain.Principal) ([]Buyer, error) {
	q := `select b.id, b.name, b.city, (select count(*) from orders o where o.buyer_id = b.id and o.status = 'open')::int
		from orgs b where b.kind = 'buyer' and b.factory_id = $1`
	args := []any{p.FactoryID}
	if p.Role == domain.Buyer {
		q += " and b.id = $2"
		args = append(args, p.OrgID)
	}
	rows, err := s.DB.Query(ctx, q+" order by b.name", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Buyer{}
	for rows.Next() {
		var b Buyer
		if err := rows.Scan(&b.ID, &b.Name, &b.City, &b.Orders); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type Dashboard struct {
	Open        int            `json:"open"`
	AtRisk      int            `json:"at_risk"`
	Late        int            `json:"late"`
	DueThisWeek int            `json:"due_this_week"`
	Pieces      int            `json:"pieces"`          // pieces on open orders
	ByStage     map[string]int `json:"orders_by_stage"` // open orders by current stage
	PiecesDone  map[string]int `json:"pieces_by_stage"` // pieces through each stage, open orders
	Attention   []Order        `json:"attention"`       // late and at-risk orders, soonest due first
}

func (s *Store) Dashboard(ctx context.Context, p domain.Principal, today time.Time) (*Dashboard, error) {
	d := &Dashboard{ByStage: map[string]int{}, PiecesDone: map[string]int{}, Attention: []Order{}}
	cursor := ""
	week := today.AddDate(0, 0, 7).Format(time.DateOnly)
	for { // open orders are bounded per factory; page through them all
		page, next, err := s.ListOrders(ctx, p, Filter{Status: "open", Limit: 100, Cursor: cursor}, today)
		if err != nil {
			return nil, err
		}
		for _, o := range page {
			d.Open++
			d.Pieces += o.Quantity
			d.ByStage[o.Stage]++
			for _, sp := range o.Progress {
				d.PiecesDone[sp.Stage] += sp.Done
			}
			if o.DueDate <= week {
				d.DueThisWeek++
			}
			switch o.Risk {
			case domain.AtRisk:
				d.AtRisk++
				d.Attention = append(d.Attention, o)
			case domain.Late:
				d.Late++
				d.Attention = append(d.Attention, o)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(d.Attention) > 10 {
		d.Attention = d.Attention[:10]
	}
	return d, nil
}

// Audit lists every event in the factory, newest first (admins only).
func (s *Store) Audit(ctx context.Context, p domain.Principal, before int64, limit int) ([]Event, error) {
	if !p.CanDo(domain.ViewAudit) || p.Role == domain.Buyer {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := s.DB.Query(ctx, `select e.id, e.order_id, coalesce(o.po_number, ''), e.at, e.kind, coalesce(u.name, 'system'), coalesce(u.role, ''), e.detail, e.buyer_visible
		from events e left join users u on u.id = e.user_id left join orders o on o.id = e.order_id
		where e.factory_id = $1 and e.id < $2 order by e.id desc limit $3`, p.FactoryID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.OrderID, &e.PONumber, &e.At, &e.Kind, &e.User, &e.Role, &e.Detail, &e.BuyerVisible); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
