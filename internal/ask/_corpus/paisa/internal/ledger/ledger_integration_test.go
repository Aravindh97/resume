//go:build integration

package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Aravindh97/paisa/internal/ledger"
)

func open(t *testing.T) (*ledger.Ledger, string) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	l, err := ledger.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.DB.Close)
	p := fmt.Sprintf("t%d:", time.Now().UnixNano())
	l.CreateAccounts(context.Background(), ledger.Account{ID: p + "bank", Kind: "asset", AllowNegative: true},
		ledger.Account{ID: p + "alice", Kind: "liability"}, ledger.Account{ID: p + "shop", Kind: "liability"})
	return l, p
}

func TestExactlyOnceUnderConcurrentRetries(t *testing.T) {
	l, p := open(t)
	ctx := context.Background()
	l.Post(ctx, ledger.Request{Key: p + "top", Kind: "topup", Postings: []ledger.Posting{ledger.P(p+"bank", -10000), ledger.P(p+"alice", 10000)}})
	req := ledger.Request{Key: p + "pay1", Kind: "payment", Reference: p + "UPI1", Postings: []ledger.Posting{ledger.P(p+"alice", -2500), ledger.P(p+"shop", 2500)}}
	var wg sync.WaitGroup
	firsts := 0
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := l.Post(ctx, req)
			if err != nil {
				t.Error(err)
				return
			}
			if !o.Replay {
				mu.Lock()
				firsts++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	b, _ := l.Balance(ctx, p+"alice")
	if firsts != 1 || b != 7500 {
		t.Fatalf("applied %d times, alice=%d", firsts, b)
	}
}

func TestRejectionsAreStableAndKeysCannotBeReused(t *testing.T) {
	l, p := open(t)
	ctx := context.Background()
	big := ledger.Request{Key: p + "big", Kind: "payment", Postings: []ledger.Posting{ledger.P(p+"alice", -1), ledger.P(p+"shop", 1)}}
	o, err := l.Post(ctx, big)
	if err != nil || o.Rejected == "" {
		t.Fatalf("overdraft allowed: %+v %v", o, err)
	}
	// Money arrives later; a retry of the same key must still be rejected,
	// or a stale retry could pay twice in a different state.
	l.Post(ctx, ledger.Request{Key: p + "top", Kind: "topup", Postings: []ledger.Posting{ledger.P(p+"bank", -100), ledger.P(p+"alice", 100)}})
	if o, _ := l.Post(ctx, big); o.Rejected == "" || !o.Replay {
		t.Fatalf("retry changed outcome: %+v", o)
	}
	other := big
	other.Postings = []ledger.Posting{ledger.P(p+"alice", -5), ledger.P(p+"shop", 5)}
	if _, err := l.Post(ctx, other); !errors.Is(err, ledger.ErrKeyReuse) {
		t.Fatalf("key reuse: %v", err)
	}
	if _, err := l.Post(ctx, ledger.Request{Key: p + "x", Kind: "payment", Postings: []ledger.Posting{ledger.P(p+"alice", -5), ledger.P(p+"shop", 4)}}); !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("unbalanced accepted: %v", err)
	}
}

func TestCrashBeforeCommitLeavesNothing(t *testing.T) {
	l, p := open(t)
	ctx := context.Background()
	ledger.FaultHook = func(string) error { return errors.New("crash") }
	_, err := l.Post(ctx, ledger.Request{Key: p + "c", Kind: "topup", Postings: []ledger.Posting{ledger.P(p+"bank", -50), ledger.P(p+"alice", 50)}})
	ledger.FaultHook = nil
	if err == nil {
		t.Fatal("expected crash")
	}
	if b, _ := l.Balance(ctx, p+"alice"); b != 0 {
		t.Fatalf("partial write: alice=%d", b)
	}
	if o, err := l.Post(ctx, ledger.Request{Key: p + "c", Kind: "topup", Postings: []ledger.Posting{ledger.P(p+"bank", -50), ledger.P(p+"alice", 50)}}); err != nil || o.Replay {
		t.Fatalf("retry after crash: %+v %v", o, err)
	}
}

func TestAuditOK(t *testing.T) {
	l, _ := open(t)
	a, err := l.Audit(context.Background())
	if err != nil || !a.OK() {
		t.Fatalf("%+v %v", a, err)
	}
}
