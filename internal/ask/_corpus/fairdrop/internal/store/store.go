// Package store keeps seats, holds and bookings in PostgreSQL.
//
// The guarantees, and what provides each one:
//
//   - A seat is held by at most one buyer at a time: Hold claims seats with
//     one conditional UPDATE (only free seats, or seats whose hold expired)
//     and rolls back unless every requested seat was claimed.
//   - A seat is sold at most once: bookings has PRIMARY KEY (show_id, seat_id),
//     so even a bug in the code above cannot double-sell.
//   - Paying twice for one hold books once: payments has a UNIQUE
//     idempotency key, and Confirm returns the first result on a retry.
//   - Abandoned holds come back: a hold has an expiry; expired seats are
//     claimable again immediately, and Sweep releases them in bulk.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Schema = `
CREATE TABLE IF NOT EXISTS seats (
    show_id     text NOT NULL,
    seat_id     text NOT NULL,
    row_label   text NOT NULL,
    num         int  NOT NULL,
    status      text NOT NULL DEFAULT 'free' CHECK (status IN ('free','held','sold')),
    hold_id     text,
    hold_expires timestamptz,
    PRIMARY KEY (show_id, seat_id)
);
CREATE INDEX IF NOT EXISTS seats_row ON seats (show_id, row_label, num);
CREATE TABLE IF NOT EXISTS holds (
    hold_id    text PRIMARY KEY,
    show_id    text NOT NULL,
    buyer_id   text NOT NULL,
    seat_ids   text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    state      text NOT NULL DEFAULT 'active' CHECK (state IN ('active','confirmed','released'))
);
CREATE TABLE IF NOT EXISTS payments (
    idempotency_key text PRIMARY KEY,
    hold_id   text NOT NULL,
    buyer_id  text NOT NULL,
    outcome   text NOT NULL,
    detail    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS bookings (
    show_id  text NOT NULL,
    seat_id  text NOT NULL,
    hold_id  text NOT NULL,
    buyer_id text NOT NULL,
    booked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (show_id, seat_id)
);
-- The naive baseline writes here: no key, so its double sales are visible.
CREATE TABLE IF NOT EXISTS naive_bookings (show_id text, seat_id text, buyer_id text);
`

type Store struct {
	DB  *pgxpool.Pool
	TTL time.Duration // hold lifetime
	Now func() time.Time
}

func Open(ctx context.Context, url string, ttl time.Duration) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if cfg.MaxConns < 50 {
		cfg.MaxConns = 50
	}
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, Schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, TTL: ttl, Now: time.Now}, nil
}

// CreateShow resets a show and creates rows × perRow seats, e.g. A1..A20.
func (s *Store) CreateShow(ctx context.Context, show string, rows, perRow int) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, t := range []string{"seats", "holds", "bookings", "naive_bookings"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+t+" WHERE show_id=$1", show); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM payments WHERE hold_id LIKE $1`, show+"/%"); err != nil {
		return err
	}
	var data [][]any
	for r := 0; r < rows; r++ {
		label := rowLabel(r)
		for n := 1; n <= perRow; n++ {
			data = append(data, []any{show, fmt.Sprintf("%s%d", label, n), label, n})
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"seats"}, []string{"show_id", "seat_id", "row_label", "num"}, pgx.CopyFromRows(data)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func rowLabel(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

var (
	ErrUnavailable = errors.New("seats no longer available")
	ErrNoBlock     = errors.New("no block of adjacent seats that size is left")
	ErrHoldExpired = errors.New("hold expired")
	ErrNotYours    = errors.New("hold belongs to another buyer")
	ErrSoldOut     = errors.New("sold out")
)

type Hold struct {
	ID      string    `json:"hold_id"`
	Seats   []string  `json:"seats"`
	Expires time.Time `json:"expires_at"`
}

func newID(prefix string) string {
	b := make([]byte, 9)
	rand.Read(b)
	return prefix + "/" + hex.EncodeToString(b)
}

const claimable = `(status = 'free' OR (status = 'held' AND hold_expires < $2))`

// HoldSeats claims exactly these seats or none of them.
func (s *Store) HoldSeats(ctx context.Context, show, buyer string, seats []string) (*Hold, error) {
	now := s.Now()
	h := &Hold{ID: newID(show), Seats: seats, Expires: now.Add(s.TTL)}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// Lock the requested rows in a fixed order and skip any another buyer is
	// holding right now. Fixed order rules out deadlocks; SKIP LOCKED means a
	// buyer never waits on another, it just sees fewer seats and tries the
	// next block.
	tag, err := tx.Exec(ctx, `WITH c AS (
			SELECT seat_id FROM seats WHERE show_id=$1 AND seat_id = ANY($5) AND `+claimable+`
			ORDER BY seat_id FOR UPDATE SKIP LOCKED)
		UPDATE seats s SET status='held', hold_id=$3, hold_expires=$4
		FROM c WHERE s.show_id=$1 AND s.seat_id = c.seat_id`, show, now, h.ID, h.Expires, seats)
	if err != nil {
		return nil, err
	}
	if int(tag.RowsAffected()) != len(seats) {
		return nil, ErrUnavailable // rollback releases the ones we did claim
	}
	if _, err := tx.Exec(ctx, `INSERT INTO holds (hold_id, show_id, buyer_id, seat_ids, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		h.ID, show, buyer, seats, h.Expires); err != nil {
		return nil, err
	}
	return h, tx.Commit(ctx)
}

// HoldBest holds n seats. If together is set they must be adjacent in one
// row; the search tries the best block it can see (rows nearest the front,
// seats nearest the centre) and, if another buyer takes it first, the next
// one, up to a few attempts. If together is false and no block exists, the
// seats may be split.
func (s *Store) HoldBest(ctx context.Context, show, buyer string, n int, together bool) (*Hold, error) {
	for attempt := 0; attempt < 6; attempt++ {
		blocks, singles, err := s.available(ctx, show, n)
		if err != nil {
			return nil, err
		}
		var pick []string
		switch {
		case len(blocks) > 0:
			pick = blocks[min(attempt, len(blocks)-1)]
		case together && len(singles) > 0:
			return nil, ErrNoBlock
		case len(singles) >= n:
			pick = singles[:n]
		default:
			return nil, ErrSoldOut
		}
		h, err := s.HoldSeats(ctx, show, buyer, pick)
		if !errors.Is(err, ErrUnavailable) {
			return h, err
		}
	}
	return nil, ErrUnavailable
}

// available returns candidate blocks of n adjacent claimable seats (best
// first, at most one per row segment) and all claimable seats.
func (s *Store) available(ctx context.Context, show string, n int) ([][]string, []string, error) {
	rows, err := s.DB.Query(ctx, `SELECT seat_id, row_label, num FROM seats WHERE show_id=$1 AND `+claimable+`
		ORDER BY length(row_label), row_label, num`, show, s.Now())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type seat struct {
		id  string
		row string
		num int
	}
	var all []seat
	for rows.Next() {
		var x seat
		if err := rows.Scan(&x.id, &x.row, &x.num); err != nil {
			return nil, nil, err
		}
		all = append(all, x)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var singles []string
	type cand struct {
		ids   []string
		rowIx int
		off   float64
	}
	var cands []cand
	rowIx := -1
	prevRow := ""
	for i := 0; i < len(all); {
		if all[i].row != prevRow {
			rowIx++
			prevRow = all[i].row
		}
		// find a run of consecutive numbers in this row
		j := i
		for j+1 < len(all) && all[j+1].row == all[i].row && all[j+1].num == all[j].num+1 {
			j++
		}
		run := all[i : j+1]
		for _, x := range run {
			singles = append(singles, x.id)
		}
		if len(run) >= n {
			// the block in this run closest to the row centre (assume 1..max)
			best, bestOff := 0, 1e9
			centre := float64(run[0].num+run[len(run)-1].num) / 2
			for k := 0; k+n <= len(run); k++ {
				mid := float64(run[k].num+run[k+n-1].num) / 2
				if off := abs(mid - centre); off < bestOff {
					best, bestOff = k, off
				}
			}
			ids := make([]string, n)
			for k := 0; k < n; k++ {
				ids[k] = run[best+k].id
			}
			cands = append(cands, cand{ids, rowIx, bestOff})
		}
		i = j + 1
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].rowIx != cands[b].rowIx {
			return cands[a].rowIx < cands[b].rowIx
		}
		return cands[a].off < cands[b].off
	})
	blocks := make([][]string, len(cands))
	for i, c := range cands {
		blocks[i] = c.ids
	}
	return blocks, singles, nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// Payer charges a buyer. It may fail, time out or succeed.
type Payer interface {
	Charge(ctx context.Context, idempotencyKey, buyer string, amount int) error
	Refund(ctx context.Context, idempotencyKey string) error
}

type Result struct {
	Outcome string   `json:"outcome"` // booked | payment_failed | expired | refunded | not_yours
	Seats   []string `json:"seats,omitempty"`
	Replay  bool     `json:"replay,omitempty"` // same key seen before; first result returned
	Detail  string   `json:"detail,omitempty"`
}

// Confirm pays for a hold and books its seats. Calling it again with the same
// idempotency key (a retried request, a double click) returns the first
// result and never charges or books twice.
func (s *Store) Confirm(ctx context.Context, holdID, buyer, key string, pay Payer, price int) (*Result, error) {
	// 1. Claim the idempotency key before charging, so two concurrent retries
	//    cannot both reach the payment provider.
	tag, err := s.DB.Exec(ctx, `INSERT INTO payments (idempotency_key, hold_id, buyer_id, outcome) VALUES ($1,$2,$3,'pending')
		ON CONFLICT DO NOTHING`, key, holdID, buyer)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return s.replay(ctx, key)
	}
	// 2. Check the hold before charging.
	var owner, state string
	var seats []string
	var expires time.Time
	err = s.DB.QueryRow(ctx, `SELECT buyer_id, state, seat_ids, expires_at FROM holds WHERE hold_id=$1`, holdID).Scan(&owner, &state, &seats, &expires)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && owner != buyer {
		return s.finish(ctx, key, &Result{Outcome: "not_yours"})
	}
	if err != nil {
		return nil, err
	}
	if state != "active" || !s.Now().Before(expires) {
		return s.finish(ctx, key, &Result{Outcome: "expired"})
	}
	// 3. Charge. The provider gets the same key, so a timeout followed by a
	//    retry is charged once on its side too.
	if err := pay.Charge(ctx, key, buyer, price*len(seats)); err != nil {
		s.Release(context.WithoutCancel(ctx), holdID)
		return s.finish(ctx, key, &Result{Outcome: "payment_failed", Detail: err.Error()})
	}
	// 4. Book. The hold may have expired while the payment was in flight; the
	//    seat update is conditional on still owning it, and bookings' primary
	//    key is the final guard.
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	tag, err = tx.Exec(ctx, `UPDATE seats SET status='sold', hold_expires=NULL WHERE show_id = split_part($1,'/',1)
		AND seat_id = ANY($2) AND hold_id=$1 AND status='held'`, holdID, seats)
	if err != nil {
		return nil, err
	}
	if int(tag.RowsAffected()) != len(seats) {
		tx.Rollback(ctx)
		// Lost the seats after charging: a real system refunds here.
		if err := pay.Refund(context.WithoutCancel(ctx), key); err != nil {
			return nil, fmt.Errorf("refund %s: %w", key, err)
		}
		return s.finish(ctx, key, &Result{Outcome: "refunded", Detail: "hold lapsed during payment"})
	}
	rows := make([][]any, len(seats))
	for i, id := range seats {
		rows[i] = []any{showOf(holdID), id, holdID, buyer}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"bookings"}, []string{"show_id", "seat_id", "hold_id", "buyer_id"}, pgx.CopyFromRows(rows)); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return nil, fmt.Errorf("double sale prevented by primary key: %w", err)
		}
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE holds SET state='confirmed' WHERE hold_id=$1`, holdID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET outcome='booked', detail=$2 WHERE idempotency_key=$1`, key, joinSeats(seats)); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &Result{Outcome: "booked", Seats: seats}, nil
}

func showOf(holdID string) string {
	for i := 0; i < len(holdID); i++ {
		if holdID[i] == '/' {
			return holdID[:i]
		}
	}
	return holdID
}

func joinSeats(s []string) string {
	b, _ := jsonMarshal(s)
	return b
}

func (s *Store) finish(ctx context.Context, key string, r *Result) (*Result, error) {
	_, err := s.DB.Exec(context.WithoutCancel(ctx), `UPDATE payments SET outcome=$2, detail=$3 WHERE idempotency_key=$1`, key, r.Outcome, r.Detail)
	return r, err
}

func (s *Store) replay(ctx context.Context, key string) (*Result, error) {
	// The first request may still be running; wait briefly for its outcome.
	for i := 0; i < 50; i++ {
		var outcome, detail string
		if err := s.DB.QueryRow(ctx, `SELECT outcome, detail FROM payments WHERE idempotency_key=$1`, key).Scan(&outcome, &detail); err != nil {
			return nil, err
		}
		if outcome != "pending" {
			r := &Result{Outcome: outcome, Replay: true}
			if outcome == "booked" {
				jsonUnmarshal(detail, &r.Seats)
			} else {
				r.Detail = detail
			}
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return &Result{Outcome: "pending", Replay: true}, nil
}

// Release returns a hold's seats immediately (buyer cancelled, payment failed).
func (s *Store) Release(ctx context.Context, holdID string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE seats SET status='free', hold_id=NULL, hold_expires=NULL WHERE hold_id=$1 AND status='held'`, holdID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE holds SET state='released' WHERE hold_id=$1 AND state='active'`, holdID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Sweep frees seats of expired holds and returns how many were freed.
func (s *Store) Sweep(ctx context.Context) (int64, error) {
	tag, err := s.DB.Exec(ctx, `WITH c AS (
			SELECT show_id, seat_id FROM seats WHERE status='held' AND hold_expires < $1
			ORDER BY show_id, seat_id FOR UPDATE SKIP LOCKED)
		UPDATE seats s SET status='free', hold_id=NULL, hold_expires=NULL
		FROM c WHERE s.show_id=c.show_id AND s.seat_id=c.seat_id`, s.Now())
	if err != nil {
		return 0, err
	}
	_, err = s.DB.Exec(ctx, `UPDATE holds SET state='released' WHERE state='active' AND expires_at < $1`, s.Now())
	return tag.RowsAffected(), err
}

// NaiveBook is the common bug, kept as a baseline: read which seats look
// free, then write, with no lock and no condition. Under load two buyers
// read the same free seat and both "book" it.
func (s *Store) NaiveBook(ctx context.Context, show, buyer string, n int) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT seat_id FROM seats WHERE show_id=$1 AND status='free' ORDER BY length(row_label), row_label, num LIMIT $2`, show, n)
	if err != nil {
		return nil, err
	}
	var seats []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		seats = append(seats, id)
	}
	rows.Close()
	if len(seats) < n {
		return nil, ErrSoldOut
	}
	time.Sleep(time.Millisecond) // the gap where a real app calls the payment provider
	for _, id := range seats {
		if _, err := s.DB.Exec(ctx, `UPDATE seats SET status='sold' WHERE show_id=$1 AND seat_id=$2`, show, id); err != nil {
			return nil, err
		}
		if _, err := s.DB.Exec(ctx, `INSERT INTO naive_bookings VALUES ($1,$2,$3)`, show, id, buyer); err != nil {
			return nil, err
		}
	}
	return seats, nil
}

// Audit checks the invariants after a sale.
type Audit struct {
	Seats, Sold, Held, Free  int
	Bookings                 int
	DoubleSold               int // seats with more than one booking (must be 0)
	SoldWithoutBooking       int // status sold but no booking row (must be 0)
	BookingsOnUnsoldSeat     int // booking row but seat not sold (must be 0)
	NaiveBookings, NaiveDupe int // naive baseline, for comparison
	TogetherBroken           int // holds that asked for adjacency but got a gap (must be 0)
}

func (s *Store) Audit(ctx context.Context, show string) (*Audit, error) {
	a := &Audit{}
	q := func(dst *int, sql string) error { return s.DB.QueryRow(ctx, sql, show).Scan(dst) }
	checks := []struct {
		dst *int
		sql string
	}{
		{&a.Seats, `SELECT count(*) FROM seats WHERE show_id=$1`},
		{&a.Sold, `SELECT count(*) FROM seats WHERE show_id=$1 AND status='sold'`},
		{&a.Held, `SELECT count(*) FROM seats WHERE show_id=$1 AND status='held'`},
		{&a.Free, `SELECT count(*) FROM seats WHERE show_id=$1 AND status='free'`},
		{&a.Bookings, `SELECT count(*) FROM bookings WHERE show_id=$1`},
		{&a.DoubleSold, `SELECT count(*) FROM (SELECT seat_id FROM bookings WHERE show_id=$1 GROUP BY seat_id HAVING count(*) > 1) x`},
		{&a.SoldWithoutBooking, `SELECT count(*) FROM seats s WHERE show_id=$1 AND status='sold' AND NOT EXISTS
			(SELECT 1 FROM bookings b WHERE b.show_id=s.show_id AND b.seat_id=s.seat_id)`},
		{&a.BookingsOnUnsoldSeat, `SELECT count(*) FROM bookings b JOIN seats s USING (show_id, seat_id) WHERE b.show_id=$1 AND s.status <> 'sold'`},
		{&a.TogetherBroken, `SELECT count(*) FROM holds h WHERE h.show_id=$1 AND h.state='confirmed' AND cardinality(h.seat_ids) >= 3
			AND (SELECT count(DISTINCT row_label) <> 1 OR max(num) - min(num) <> count(*) - 1
			     FROM seats s WHERE s.show_id = h.show_id AND s.seat_id = ANY(h.seat_ids))`},
		{&a.NaiveBookings, `SELECT count(*) FROM naive_bookings WHERE show_id=$1`},
		{&a.NaiveDupe, `SELECT coalesce(sum(c-1),0) FROM (SELECT count(*) c FROM naive_bookings WHERE show_id=$1 GROUP BY seat_id HAVING count(*) > 1) x`},
	}
	for _, c := range checks {
		if err := q(c.dst, c.sql); err != nil {
			return nil, err
		}
	}
	return a, nil
}
