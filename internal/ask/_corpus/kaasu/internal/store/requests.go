package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Aravindh97/kaasu/internal/auth"
	"github.com/Aravindh97/kaasu/internal/money"
)

type MoneyRequest struct {
	ID        int64      `json:"id"`
	From      string     `json:"from"` // who asked (gets paid)
	FromID    int64      `json:"from_id"`
	To        string     `json:"to"` // who is asked to pay
	ToID      int64      `json:"to_id"`
	Amount    int64      `json:"amount"`
	Note      string     `json:"note"`
	Status    string     `json:"status"`
	Split     string     `json:"split,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	DecidedAt *time.Time `json:"decided_at"`
}

type NewRequest struct {
	To     string `json:"to"`
	Amount int64  `json:"amount"`
	Note   string `json:"note"`
}

func (s *Store) RequestMoney(ctx context.Context, u *auth.User, in NewRequest, ip string) (*MoneyRequest, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	in.Note = strings.TrimSpace(in.Note)
	if in.Amount < 100 || in.Amount > DailySendLimit {
		return nil, problem("amount", "ask for between ₹1 and %s", fmtMoney(DailySendLimit))
	}
	if len(in.Note) > 80 {
		return nil, problem("note", "keep the note under 80 characters")
	}
	p, err := s.Lookup(ctx, u, in.To)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.tx(ctx, func(tx pgx.Tx) error {
		// at most 20 open requests to one person, so requests can't be used to spam
		var open int
		if err := tx.QueryRow(ctx, `select count(*) from money_requests where requester_id = $1 and payer_id = $2 and status = 'pending'`, u.ID, p.ID).Scan(&open); err != nil {
			return err
		}
		if open >= 20 {
			return problem("to", "you already have 20 open requests to %s", p.Name)
		}
		if err := tx.QueryRow(ctx, `insert into money_requests (requester_id, payer_id, amount, note, created_at) values ($1, $2, $3, $4, $5) returning id`,
			u.ID, p.ID, in.Amount, in.Note, s.Now()).Scan(&id); err != nil {
			return err
		}
		if err := audit(ctx, tx, u.ID, u.ID, "request", ip, map[string]any{"amount": in.Amount, "from": p.ID, "request": id}); err != nil {
			return err
		}
		return notify(ctx, tx, userTopic(u.ID), userTopic(p.ID))
	})
	if err != nil {
		return nil, err
	}
	return s.getRequest(ctx, u, id)
}

const requestCols = `r.id, a.name, r.requester_id, b.name, r.payer_id, r.amount, r.note, r.status, coalesce(sp.title, ''), r.created_at, r.decided_at
	from money_requests r join users a on a.id = r.requester_id join users b on b.id = r.payer_id left join splits sp on sp.id = r.split_id`

func scanRequest(row pgx.Row) (*MoneyRequest, error) {
	var m MoneyRequest
	err := row.Scan(&m.ID, &m.From, &m.FromID, &m.To, &m.ToID, &m.Amount, &m.Note, &m.Status, &m.Split, &m.CreatedAt, &m.DecidedAt)
	return &m, err
}

func (s *Store) getRequest(ctx context.Context, u *auth.User, id int64) (*MoneyRequest, error) {
	m, err := scanRequest(s.DB.QueryRow(ctx, `select `+requestCols+` where r.id = $1 and (r.requester_id = $2 or r.payer_id = $2)`, id, u.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

type Requests struct {
	ToPay    []MoneyRequest `json:"to_pay"`   // others asking me
	Asked    []MoneyRequest `json:"asked"`    // me asking others
	Finished []MoneyRequest `json:"finished"` // recent paid, declined or cancelled
}

func (s *Store) Requests(ctx context.Context, u *auth.User) (*Requests, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `select `+requestCols+` where (r.requester_id = $1 or r.payer_id = $1)
		and (r.status = 'pending' or r.decided_at > $2) order by r.id desc limit 200`, u.ID, s.Now().AddDate(0, 0, -30))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &Requests{ToPay: []MoneyRequest{}, Asked: []MoneyRequest{}, Finished: []MoneyRequest{}}
	for rows.Next() {
		m, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		switch {
		case m.Status != "pending":
			out.Finished = append(out.Finished, *m)
		case m.ToID == u.ID:
			out.ToPay = append(out.ToPay, *m)
		default:
			out.Asked = append(out.Asked, *m)
		}
	}
	return out, rows.Err()
}

type PayRequest struct {
	PIN string `json:"pin"`
}

// PayRequest pays a request exactly once. The request row is locked, so two
// devices paying the same request at once move the money once; the second
// is told it's already paid.
func (s *Store) PayRequest(ctx context.Context, u *auth.User, r Request, id int64, in PayRequest) (*Moved, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	m, err := s.getRequest(ctx, u, id)
	if err != nil {
		return nil, err
	}
	if m.ToID != u.ID {
		return nil, ErrNotFound
	}
	if err := s.CheckPIN(ctx, u, in.PIN, r.IP); err != nil {
		return nil, err
	}
	raw, replay, err := s.once(ctx, u, r, map[string]int64{"pay_request": id}, func(tx pgx.Tx) (any, error) {
		var status string
		var amount, to int64
		var note string
		if err := tx.QueryRow(ctx, `select status, amount, requester_id, note from money_requests where id = $1 and payer_id = $2 for update`, id, u.ID).Scan(&status, &amount, &to, &note); err != nil {
			return nil, err
		}
		if status != "pending" {
			return nil, &Problem{Msg: "this request is already " + status, Code: 409}
		}
		mv, err := s.send(ctx, tx, u, to, amount, note, "Transfers", &id, r.IP)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(ctx, `update money_requests set status = 'paid', txn_id = $2, decided_at = $3 where id = $1`, id, mv.TxnID, s.Now())
		return mv, err
	})
	return decodeMoved(raw, replay, err)
}

// DecideRequest declines (the payer) or cancels (the requester) a pending request.
func (s *Store) DecideRequest(ctx context.Context, u *auth.User, id int64, action, ip string) (*MoneyRequest, error) {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var status string
		var from, to int64
		err := tx.QueryRow(ctx, `select status, requester_id, payer_id from money_requests where id = $1 and (requester_id = $2 or payer_id = $2) for update`, id, u.ID).Scan(&status, &from, &to)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		next := map[string]string{"decline": "declined", "cancel": "cancelled"}[action]
		switch {
		case next == "":
			return problem("", "unknown action")
		case status != "pending":
			return &Problem{Msg: "this request is already " + status, Code: 409}
		case next == "declined" && to != u.ID, next == "cancelled" && from != u.ID:
			return ErrForbidden
		}
		if _, err := tx.Exec(ctx, `update money_requests set status = $2, decided_at = $3 where id = $1`, id, next, s.Now()); err != nil {
			return err
		}
		if err := audit(ctx, tx, u.ID, u.ID, "request_"+next, ip, map[string]any{"request": id}); err != nil {
			return err
		}
		return notify(ctx, tx, userTopic(from), userTopic(to))
	})
	if err != nil {
		return nil, err
	}
	return s.getRequest(ctx, u, id)
}

// ---- Splits -------------------------------------------------------------

type NewSplit struct {
	Title string   `json:"title"`
	Total int64    `json:"total"`
	With  []string `json:"with"` // mobile numbers or UPI IDs; the creator is included automatically
}

type Split struct {
	ID       int64          `json:"id"`
	Title    string         `json:"title"`
	Total    int64          `json:"total"`
	MyShare  int64          `json:"my_share"`
	Requests []MoneyRequest `json:"requests"`
}

// SplitBill shares a bill equally between the creator and the others, and
// sends each of the others a request for their share. The shares add up to
// the total exactly: leftover paise go one each to the first shares, and
// the creator, who paid, takes the smallest.
func (s *Store) SplitBill(ctx context.Context, u *auth.User, in NewSplit, ip string) (*Split, error) {
	if err := member(u); err != nil {
		return nil, err
	}
	in.Title = strings.TrimSpace(in.Title)
	switch {
	case in.Title == "" || len(in.Title) > 60:
		return nil, problem("title", "give the bill a name (up to 60 characters)")
	case in.Total < 200 || in.Total > 1_00_000_00:
		return nil, problem("total", "the bill must be between ₹2 and ₹1,00,000")
	case len(in.With) == 0 || len(in.With) > 10:
		return nil, problem("with", "add 1 to 10 people")
	}
	var people []*Payee
	seen := map[int64]bool{}
	for _, w := range in.With {
		p, err := s.Lookup(ctx, u, w)
		if err != nil {
			var pr *Problem
			if errors.As(err, &pr) {
				return nil, problem("with", "%s: %s", w, pr.Msg)
			}
			return nil, err
		}
		if seen[p.ID] {
			return nil, problem("with", "%s is listed twice", p.Name)
		}
		seen[p.ID] = true
		people = append(people, p)
	}
	shares := money.Split(in.Total, len(people)+1)
	mine := shares[len(shares)-1] // the smallest share
	sp := &Split{Title: in.Title, Total: in.Total, MyShare: mine, Requests: []MoneyRequest{}}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `insert into splits (created_by, title, total, created_at) values ($1, $2, $3, $4) returning id`, u.ID, in.Title, in.Total, s.Now()).Scan(&sp.ID); err != nil {
			return err
		}
		topics := []string{userTopic(u.ID)}
		for i, p := range people {
			if _, err := tx.Exec(ctx, `insert into money_requests (requester_id, payer_id, amount, note, split_id, created_at) values ($1, $2, $3, $4, $5, $6)`,
				u.ID, p.ID, shares[i], in.Title, sp.ID, s.Now()); err != nil {
				return err
			}
			topics = append(topics, userTopic(p.ID))
		}
		if err := audit(ctx, tx, u.ID, u.ID, "split", ip, map[string]any{"split": sp.ID, "total": in.Total, "people": len(people)}); err != nil {
			return err
		}
		return notify(ctx, tx, topics...)
	})
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `select `+requestCols+` where r.split_id = $1 order by r.id`, sp.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		sp.Requests = append(sp.Requests, *m)
	}
	return sp, rows.Err()
}
