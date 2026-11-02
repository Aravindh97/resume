//go:build !(js && wasm)

package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Schema = `
CREATE TABLE IF NOT EXISTS accounts (
    id         text PRIMARY KEY,
    kind       text NOT NULL CHECK (kind IN ('asset','liability','revenue','expense')),
    -- Customer wallets are liabilities: the money belongs to the customer.
    allow_negative boolean NOT NULL DEFAULT false,
    balance    bigint NOT NULL DEFAULT 0,
    version    bigint NOT NULL DEFAULT 0,
    CHECK (allow_negative OR balance >= 0)
);
CREATE TABLE IF NOT EXISTS entries (
    id          bigserial PRIMARY KEY,
    idem_key    text NOT NULL UNIQUE,
    kind        text NOT NULL,
    reference   text NOT NULL DEFAULT '',   -- e.g. UPI transaction id, used by reconciliation
    fingerprint text NOT NULL,              -- sha256 of the request, to reject key reuse
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS entries_reference ON entries (reference) WHERE reference <> '';
CREATE TABLE IF NOT EXISTS postings (
    entry_id   bigint NOT NULL REFERENCES entries(id),
    account_id text NOT NULL REFERENCES accounts(id),
    amount     bigint NOT NULL CHECK (amount <> 0),  -- positive = debit, negative = credit
    PRIMARY KEY (entry_id, account_id)
);
CREATE INDEX IF NOT EXISTS postings_account ON postings (account_id);
-- Rejected requests are recorded too, so a retry gets the same rejection.
CREATE TABLE IF NOT EXISTS rejections (
    idem_key    text PRIMARY KEY,
    fingerprint text NOT NULL,
    reason      text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
`

type Ledger struct{ DB *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Ledger, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if cfg.MaxConns < 40 {
		cfg.MaxConns = 40
	}
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, Schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Ledger{DB: db}, nil
}

func (l *Ledger) CreateAccounts(ctx context.Context, as ...Account) error {
	for _, a := range as {
		if _, err := l.DB.Exec(ctx, `INSERT INTO accounts (id, kind, allow_negative) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
			a.ID, a.Kind, a.AllowNegative); err != nil {
			return err
		}
	}
	return nil
}

// Post applies a request exactly once.
//
// Business rejections (insufficient funds) are outcomes, not errors: they
// are stored under the key so a retry sees the same answer. Errors are for
// broken requests and infrastructure failures; on an infrastructure error
// nothing was written, and the caller should retry with the same key.
func (l *Ledger) Post(ctx context.Context, r Request) (*Outcome, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	fp := r.fingerprint()
	for attempt := 0; ; attempt++ {
		out, err := l.post(ctx, r, fp)
		// Serialization failures and deadlocks are safe to retry: the
		// transaction rolled back completely.
		var pe *pgconn.PgError
		if errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01") && attempt < 5 {
			continue
		}
		return out, err
	}
}

func (l *Ledger) post(ctx context.Context, r Request, fp string) (*Outcome, error) {
	tx, err := l.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// 1. Replay check. Serialise requests with the same key on an advisory
	//    lock so two concurrent retries cannot both pass this check.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 42))`, r.Key); err != nil {
		return nil, err
	}
	if out, err := replay(ctx, tx, r.Key, fp); out != nil || err != nil {
		return out, err
	}

	// 2. Lock the accounts in a fixed order (no deadlocks) and check funds.
	accts := make([]string, 0, len(r.Postings))
	delta := map[string]int64{}
	for _, p := range r.Postings {
		accts = append(accts, p.Account)
		delta[p.Account] = p.Amount
	}
	sort.Strings(accts)
	rows, err := tx.Query(ctx, `SELECT id, balance, allow_negative FROM accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, accts)
	if err != nil {
		return nil, err
	}
	found := 0
	reason := ""
	for rows.Next() {
		var id string
		var bal int64
		var neg bool
		if err := rows.Scan(&id, &bal, &neg); err != nil {
			rows.Close()
			return nil, err
		}
		found++
		if !neg && bal+delta[id] < 0 && reason == "" {
			reason = "insufficient funds in " + id
		}
	}
	rows.Close()
	if found != len(accts) {
		return nil, fmt.Errorf("%w: unknown account", ErrInvalid)
	}
	if reason != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO rejections (idem_key, fingerprint, reason) VALUES ($1,$2,$3)`, r.Key, fp, reason); err != nil {
			return nil, err
		}
		return &Outcome{Rejected: reason}, tx.Commit(ctx)
	}

	// 3. Write the entry, its postings and the balance cache together.
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO entries (idem_key, kind, reference, fingerprint) VALUES ($1,$2,$3,$4) RETURNING id`,
		r.Key, r.Kind, r.Reference, fp).Scan(&id); err != nil {
		return nil, err
	}
	for _, a := range accts {
		if _, err := tx.Exec(ctx, `INSERT INTO postings (entry_id, account_id, amount) VALUES ($1,$2,$3)`, id, a, delta[a]); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE accounts SET balance = balance + $2, version = version + 1 WHERE id = $1`, a, delta[a]); err != nil {
			return nil, err
		}
	}
	if FaultHook != nil {
		if err := FaultHook(r.Key); err != nil {
			return nil, err // simulate a crash before commit: everything rolls back
		}
	}
	return &Outcome{EntryID: id}, tx.Commit(ctx)
}

func replay(ctx context.Context, tx pgx.Tx, key, fp string) (*Outcome, error) {
	var id int64
	var efp string
	err := tx.QueryRow(ctx, `SELECT id, fingerprint FROM entries WHERE idem_key=$1`, key).Scan(&id, &efp)
	if err == nil {
		if efp != fp {
			return nil, ErrKeyReuse
		}
		return &Outcome{EntryID: id, Replay: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var rfp, reason string
	err = tx.QueryRow(ctx, `SELECT fingerprint, reason FROM rejections WHERE idem_key=$1`, key).Scan(&rfp, &reason)
	if err == nil {
		if rfp != fp {
			return nil, ErrKeyReuse
		}
		return &Outcome{Rejected: reason, Replay: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return nil, nil
}

func (l *Ledger) Balance(ctx context.Context, account string) (int64, error) {
	var b int64
	err := l.DB.QueryRow(ctx, `SELECT balance FROM accounts WHERE id=$1`, account).Scan(&b)
	return b, err
}

func (l *Ledger) Audit(ctx context.Context) (*Audit, error) {
	t := time.Now()
	a := &Audit{}
	err := l.DB.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM entries),
		(SELECT count(*) FROM postings),
		(SELECT count(*) FROM (SELECT entry_id FROM postings GROUP BY entry_id HAVING sum(amount) <> 0) x),
		(SELECT count(*) FROM accounts a WHERE balance <> coalesce((SELECT sum(amount) FROM postings p WHERE p.account_id=a.id),0)),
		(SELECT coalesce(sum(balance),0) FROM accounts),
		(SELECT count(*) FROM accounts WHERE NOT allow_negative AND balance < 0),
		(SELECT count(*) FROM (SELECT reference FROM entries WHERE kind='payment' AND reference <> '' GROUP BY reference HAVING count(*) > 1) y)`).
		Scan(&a.Entries, &a.Postings, &a.UnbalancedEntry, &a.CacheMismatch, &a.TotalSum, &a.Negative, &a.DuplicateRef)
	a.Took = time.Since(t)
	return a, err
}

func (l *Ledger) MerchantCredits(ctx context.Context) (map[string]Credit, error) {
	rows, err := l.DB.Query(ctx, `SELECT e.reference, p.amount, p.account_id FROM entries e
		JOIN postings p ON p.entry_id = e.id
		WHERE e.kind='payment' AND e.reference <> '' AND p.account_id LIKE 'merchant:%' AND p.amount > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Credit{}
	for rows.Next() {
		var ref, acct string
		var amt int64
		if err := rows.Scan(&ref, &amt, &acct); err != nil {
			return nil, err
		}
		out[ref] = Credit{amt, acct}
	}
	return out, rows.Err()
}
