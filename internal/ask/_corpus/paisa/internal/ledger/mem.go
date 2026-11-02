package ledger

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Mem is an in-memory Store with the same rules as the Postgres ledger: a
// key lock serialises requests that share an idempotency key, accounts are
// locked in sorted order, the entry, its postings and the balances change
// together, and rejections are stored under their key.
type Mem struct {
	mu       sync.Mutex // guards the maps below
	accounts map[string]*memAccount
	entries  []memEntry
	byKey    map[string]int // key -> index in entries
	rejected map[string]memRejection
	keyLocks map[string]*sync.Mutex
}

type memAccount struct {
	mu sync.Mutex
	Account
	balance int64
}

type memEntry struct {
	key, kind, ref, fp string
	postings           []Posting
}

type memRejection struct{ fp, reason string }

func NewMem() *Mem {
	return &Mem{accounts: map[string]*memAccount{}, byKey: map[string]int{}, rejected: map[string]memRejection{}, keyLocks: map[string]*sync.Mutex{}}
}

func (m *Mem) CreateAccounts(_ context.Context, as ...Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range as {
		if _, ok := m.accounts[a.ID]; !ok {
			m.accounts[a.ID] = &memAccount{Account: a}
		}
	}
	return nil
}

func (m *Mem) keyLock(k string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.keyLocks[k]
	if !ok {
		l = &sync.Mutex{}
		m.keyLocks[k] = l
	}
	return l
}

func (m *Mem) Post(ctx context.Context, r Request) (*Outcome, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	fp := r.fingerprint()
	kl := m.keyLock(r.Key) // the advisory lock on the key
	kl.Lock()
	defer kl.Unlock()

	m.mu.Lock()
	if i, ok := m.byKey[r.Key]; ok {
		e := m.entries[i]
		m.mu.Unlock()
		if e.fp != fp {
			return nil, ErrKeyReuse
		}
		return &Outcome{EntryID: int64(i + 1), Replay: true}, nil
	}
	if rj, ok := m.rejected[r.Key]; ok {
		m.mu.Unlock()
		if rj.fp != fp {
			return nil, ErrKeyReuse
		}
		return &Outcome{Rejected: rj.reason, Replay: true}, nil
	}
	ids := make([]string, 0, len(r.Postings))
	delta := map[string]int64{}
	var accts []*memAccount
	for _, p := range r.Postings {
		ids = append(ids, p.Account)
		delta[p.Account] = p.Amount
	}
	sort.Strings(ids)
	for _, id := range ids {
		a, ok := m.accounts[id]
		if !ok {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: unknown account", ErrInvalid)
		}
		accts = append(accts, a)
	}
	m.mu.Unlock()

	// Lock accounts in sorted order, like SELECT ... ORDER BY id FOR UPDATE.
	for _, a := range accts {
		a.mu.Lock()
		runtime.Gosched() // let other goroutines contend, as concurrent transactions would
	}
	unlock := func() {
		for i := len(accts) - 1; i >= 0; i-- {
			accts[i].mu.Unlock()
		}
	}
	reason := ""
	for _, a := range accts {
		if !a.AllowNegative && a.balance+delta[a.ID] < 0 {
			reason = "insufficient funds in " + a.ID
			break
		}
	}
	if reason != "" {
		m.mu.Lock()
		m.rejected[r.Key] = memRejection{fp, reason}
		m.mu.Unlock()
		unlock()
		return &Outcome{Rejected: reason}, nil
	}
	if FaultHook != nil {
		if err := FaultHook(r.Key); err != nil {
			unlock()
			return nil, err // a crash before commit: nothing was applied
		}
	}
	// Commit: entry, postings and balances together.
	for _, a := range accts {
		a.balance += delta[a.ID]
	}
	m.mu.Lock()
	m.entries = append(m.entries, memEntry{key: r.Key, kind: r.Kind, ref: r.Reference, fp: fp, postings: append([]Posting(nil), r.Postings...)})
	id := len(m.entries)
	m.byKey[r.Key] = id - 1
	m.mu.Unlock()
	unlock()
	return &Outcome{EntryID: int64(id)}, nil
}

var errNoAccount = errors.New("no such account")

func (m *Mem) Balance(_ context.Context, id string) (int64, error) {
	m.mu.Lock()
	a, ok := m.accounts[id]
	m.mu.Unlock()
	if !ok {
		return 0, errNoAccount
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.balance, nil
}

// Audit recomputes every invariant from the raw postings.
func (m *Mem) Audit(_ context.Context) (*Audit, error) {
	t := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	a := &Audit{Entries: int64(len(m.entries))}
	sums := map[string]int64{}
	refs := map[string]int{}
	for _, e := range m.entries {
		var s int64
		for _, p := range e.postings {
			s += p.Amount
			sums[p.Account] += p.Amount
			a.Postings++
		}
		if s != 0 {
			a.UnbalancedEntry++
		}
		if e.kind == "payment" && e.ref != "" {
			refs[e.ref]++
		}
	}
	for id, acc := range m.accounts {
		acc.mu.Lock()
		b := acc.balance
		acc.mu.Unlock()
		if b != sums[id] {
			a.CacheMismatch++
		}
		a.TotalSum += b
		if !acc.AllowNegative && b < 0 {
			a.Negative++
		}
	}
	for _, n := range refs {
		if n > 1 {
			a.DuplicateRef++
		}
	}
	a.Took = time.Since(t)
	return a, nil
}

func (m *Mem) MerchantCredits(_ context.Context) (map[string]Credit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Credit{}
	for _, e := range m.entries {
		if e.kind != "payment" || e.ref == "" {
			continue
		}
		for _, p := range e.postings {
			if len(p.Account) > 9 && p.Account[:9] == "merchant:" && p.Amount > 0 {
				out[e.ref] = Credit{p.Amount, p.Account}
			}
		}
	}
	return out, nil
}

// Balances returns every account balance, for display.
func (m *Mem) Balances() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int64, len(m.accounts))
	for id, a := range m.accounts {
		a.mu.Lock()
		out[id] = a.balance
		a.mu.Unlock()
	}
	return out
}

// Entries returns the most recent n entries, newest first, for display.
func (m *Mem) Entries(n int) []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []map[string]any
	for i := len(m.entries) - 1; i >= 0 && len(out) < n; i-- {
		e := m.entries[i]
		out = append(out, map[string]any{"id": i + 1, "key": e.key, "kind": e.kind, "reference": e.ref, "postings": e.postings})
	}
	return out
}
