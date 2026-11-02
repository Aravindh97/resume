// Package ledger is a double-entry ledger in PostgreSQL.
//
// Every movement of money is a journal entry with two or more postings that
// sum to zero (money is never created or destroyed, only moved). Balances
// are derived from postings and cached on the account row; the cache is
// updated in the same transaction as the postings, and an audit recomputes
// it from scratch.
//
// Exactly-once: every request carries an idempotency key. The key, the
// request fingerprint and the outcome are stored in the same transaction as
// the postings, so a request either happened completely, with its key
// recorded, or not at all. A retry with the same key returns the stored
// outcome; a different request reusing a key is rejected.
//
// Amounts are int64 paise (1 rupee = 100 paise). Floating point is never used
// for money.
package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Store is the ledger interface: Postgres in production (Ledger, in
// postgres.go), in memory for the browser demo and fast tests (Mem). Both
// apply the same rules.
type Store interface {
	CreateAccounts(ctx context.Context, as ...Account) error
	Post(ctx context.Context, r Request) (*Outcome, error)
	Balance(ctx context.Context, account string) (int64, error)
	Audit(ctx context.Context) (*Audit, error)
	// MerchantCredits returns, for every payment with an external
	// reference, the amount credited to a merchant and which merchant.
	MerchantCredits(ctx context.Context) (map[string]Credit, error)
}

type Credit struct {
	Amount   int64
	Merchant string
}

type Account struct {
	ID            string
	Kind          string
	AllowNegative bool
}

// Posting moves Amount paise into (positive) or out of (negative) an account.
type Posting struct {
	Account string `json:"account"`
	Amount  int64  `json:"amount"`
}

// P is shorthand for a posting.
func P(account string, amount int64) Posting { return Posting{Account: account, Amount: amount} }

type Request struct {
	Key       string    `json:"key"`
	Kind      string    `json:"kind"` // payment | refund | settlement | fee | topup
	Reference string    `json:"reference,omitempty"`
	Postings  []Posting `json:"postings"`
}

func (r Request) fingerprint() string {
	ps := append([]Posting(nil), r.Postings...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Account < ps[j].Account })
	b, _ := json.Marshal(struct {
		K, R string
		P    []Posting
	}{r.Kind, r.Reference, ps})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Outcome struct {
	EntryID  int64  `json:"entry_id,omitempty"`
	Rejected string `json:"rejected,omitempty"`
	Replay   bool   `json:"replay,omitempty"`
}

var (
	ErrUnbalanced = errors.New("postings do not sum to zero")
	ErrKeyReuse   = errors.New("idempotency key reused for a different request")
	ErrInvalid    = errors.New("invalid request")
)

// FaultHook, when set by tests or the chaos tool, runs just before commit.
// Returning an error aborts the transaction as a crash would.
var FaultHook func(key string) error

// Audit checks the ledger's invariants from scratch.
type Audit struct {
	Entries         int64
	Postings        int64
	UnbalancedEntry int64 // entries whose postings don't sum to zero (must be 0)
	CacheMismatch   int64 // accounts whose cached balance != sum of postings (must be 0)
	TotalSum        int64 // sum of all balances (must be 0)
	Negative        int64 // accounts below zero that may not be (must be 0)
	DuplicateRef    int64 // payments with the same external reference twice (must be 0)
	Took            time.Duration
}

func (a Audit) OK() bool {
	return a.UnbalancedEntry == 0 && a.CacheMismatch == 0 && a.TotalSum == 0 && a.Negative == 0 && a.DuplicateRef == 0
}

func (r Request) validate() error {
	if r.Key == "" || len(r.Postings) < 2 {
		return ErrInvalid
	}
	var sum int64
	seen := map[string]bool{}
	for _, p := range r.Postings {
		if p.Amount == 0 || seen[p.Account] {
			return fmt.Errorf("%w: zero amount or account repeated", ErrInvalid)
		}
		seen[p.Account] = true
		sum += p.Amount
	}
	if sum != 0 {
		return ErrUnbalanced
	}
	return nil
}
