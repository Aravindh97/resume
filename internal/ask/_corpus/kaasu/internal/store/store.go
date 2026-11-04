// Package store is Kaasu's data access and money rules. Every function takes
// the signed-in user; members only ever see their own money.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/kaasu/internal/auth"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("not allowed")
)

// Problem is a request the rules refuse, with the field it is about.
type Problem struct {
	Field string `json:"field,omitempty"`
	Msg   string `json:"error"`
	Code  int    `json:"-"` // HTTP status; 422 if zero
}

func (e *Problem) Error() string { return e.Msg }

func problem(field, format string, a ...any) error {
	return &Problem{Field: field, Msg: fmt.Sprintf(format, a...)}
}

// locked: the request is fine but the account or PIN is locked (423).
func locked(field, format string, a ...any) error {
	return &Problem{Field: field, Msg: fmt.Sprintf(format, a...), Code: 423}
}

type Store struct {
	DB      *pgxpool.Pool
	Now     func() time.Time // Asia/Kolkata
	PINCost int              // bcrypt cost for PINs
}

var IST = time.FixedZone("IST", 5*3600+1800)

func (s *Store) today() time.Time {
	n := s.Now().In(IST)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, IST)
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

// notify tells every server instance, and through them the browsers, that a
// topic changed. Postgres sends it only if the transaction commits.
func notify(ctx context.Context, tx pgx.Tx, topics ...string) error {
	for _, t := range topics {
		if _, err := tx.Exec(ctx, `select pg_notify('kaasu', $1)`, t); err != nil {
			return err
		}
	}
	return nil
}

func userTopic(id int64) string { return fmt.Sprintf("user:%d", id) }

func audit(ctx context.Context, tx pgx.Tx, user, actor int64, action, ip string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	null := func(id int64) any { // 0: no user (a system action such as a demo reset)
		if id == 0 {
			return nil
		}
		return id
	}
	_, err := tx.Exec(ctx, `insert into audit_log (user_id, actor_id, action, detail, ip) values ($1, $2, $3, $4, $5)`, null(user), null(actor), action, detail, ip)
	return err
}

func uniqueViolation(err error, constraint string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && (constraint == "" || pe.ConstraintName == constraint)
}

func member(u *auth.User) error {
	if u.Role != "member" {
		return ErrForbidden
	}
	return nil
}

// Request carries what every money-moving call needs besides its own fields.
type Request struct {
	Key string // Idempotency-Key header
	IP  string
}

// once runs f at most once per (user, key). The key row is inserted first,
// in the same transaction as the money movement: a second request with the
// same key waits on the unique index until the first commits, then gets the
// first response back. If the first fails, its key row rolls back with it,
// so a retry runs normally. The same key with a different request is refused.
// refusal is returned by a once() body that refuses the request but must
// keep what it wrote (pausing an account). The transaction commits, and the
// refusal is stored as the key's response, so a retry gets it too.
type refusal struct{ p *Problem }

func (r refusal) Error() string { return r.p.Msg }

type storedRefusal struct {
	Refused *Problem `json:"refused"`
	Code    int      `json:"code"`
}

func (s *Store) once(ctx context.Context, u *auth.User, r Request, body any, f func(tx pgx.Tx) (any, error)) (json.RawMessage, bool, error) {
	if len(r.Key) < 8 || len(r.Key) > 100 {
		return nil, false, problem("", "an Idempotency-Key header of 8 to 100 characters is required")
	}
	b, _ := json.Marshal(body)
	h := sha256.Sum256(b)
	var out json.RawMessage
	replay := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// One person's money requests run one at a time. NO KEY UPDATE (not
		// FOR UPDATE) because rows that reference this user take a KEY SHARE
		// lock on it, and FOR UPDATE would deadlock against those.
		if _, err := tx.Exec(ctx, `select 1 from users where id = $1 for no key update`, u.ID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `insert into idempotency (user_id, key, request_hash, response) values ($1, $2, $3, 'null') on conflict do nothing`, u.ID, r.Key, h[:])
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var prev []byte
			if err := tx.QueryRow(ctx, `select request_hash, response from idempotency where user_id = $1 and key = $2`, u.ID, r.Key).Scan(&prev, &out); err != nil {
				return err
			}
			if string(prev) != string(h[:]) {
				return problem("", "this Idempotency-Key was already used for a different request")
			}
			replay = true
			return nil
		}
		v, err := f(tx)
		var ref refusal
		if errors.As(err, &ref) {
			v, err = storedRefusal{ref.p, ref.p.Code}, nil
		}
		if err != nil {
			return err
		}
		if out, err = json.Marshal(v); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update idempotency set response = $3 where user_id = $1 and key = $2`, u.ID, r.Key, []byte(out))
		return err
	})
	if err != nil {
		return nil, false, err
	}
	var sr storedRefusal
	if json.Unmarshal(out, &sr) == nil && sr.Refused != nil {
		sr.Refused.Code = sr.Code
		return nil, replay, sr.Refused
	}
	return out, replay, nil
}

type moveSpec struct {
	kind         string // topup, transfer, goal_in, goal_out
	user         int64  // who made it
	counterparty *int64
	from, to     int64 // account ids
	amount       int64
	note         string
	category     string
	requestID    *int64
	at           time.Time
}

// move is the only code that changes balances. It locks both accounts in id
// order (so two transfers in opposite directions can't deadlock), refuses to
// overdraw, writes a txn with two entries that sum to zero, and updates the
// balances. A deferred trigger re-checks the sum at commit.
func move(ctx context.Context, tx pgx.Tx, m moveSpec) (int64, error) {
	if m.amount <= 0 {
		return 0, problem("amount", "enter an amount above zero")
	}
	rows, err := tx.Query(ctx, `select id, kind, balance, closed_at is not null from accounts where id = any($1) order by id for no key update`, []int64{m.from, m.to})
	if err != nil {
		return 0, err
	}
	bal := map[int64]int64{}
	kind := map[int64]string{}
	for rows.Next() {
		var id, b int64
		var k string
		var closed bool
		if err := rows.Scan(&id, &k, &b, &closed); err != nil {
			rows.Close()
			return 0, err
		}
		if closed {
			rows.Close()
			return 0, problem("", "that account is closed")
		}
		bal[id], kind[id] = b, k
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(bal) != 2 {
		return 0, ErrNotFound
	}
	if kind[m.from] != "bank" && bal[m.from] < m.amount {
		return 0, problem("amount", "not enough balance: you have %s", fmtMoney(bal[m.from]))
	}
	if m.at.IsZero() {
		m.at = time.Now()
	}
	if m.category == "" {
		m.category = "Transfers"
	}
	var txn int64
	if err := tx.QueryRow(ctx, `insert into txns (kind, user_id, counterparty_id, amount, note, category, request_id, created_at) values ($1, $2, $3, $4, $5, $6, $7, $8) returning id`,
		m.kind, m.user, m.counterparty, m.amount, m.note, m.category, m.requestID, m.at).Scan(&txn); err != nil {
		return 0, err
	}
	for _, e := range []struct{ acct, amt int64 }{{m.from, -m.amount}, {m.to, m.amount}} {
		nb := bal[e.acct] + e.amt
		if _, err := tx.Exec(ctx, `update accounts set balance = $2 where id = $1`, e.acct, nb); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `insert into entries (txn_id, account_id, amount, balance_after) values ($1, $2, $3, $4)`, txn, e.acct, e.amt, nb); err != nil {
			return 0, err
		}
	}
	return txn, nil
}

func (s *Store) wallet(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, user int64) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `select id from accounts where user_id = $1 and kind = 'wallet'`, user).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

func bankAccount(ctx context.Context, tx pgx.Tx) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `select id from accounts where kind = 'bank'`).Scan(&id)
	return id, err
}
