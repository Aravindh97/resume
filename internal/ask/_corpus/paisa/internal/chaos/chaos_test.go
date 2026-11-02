package chaos

import (
	"context"
	"testing"

	"github.com/Aravindh97/paisa/internal/ledger"
	"github.com/Aravindh97/paisa/internal/recon"
)

func TestChaosOnMemLedger(t *testing.T) {
	ctx := context.Background()
	m := ledger.NewMem()
	c := Default()
	c.Payments, c.Customers = 5000, 300
	res, err := Run(ctx, m, c)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := m.Audit(ctx)
	if !a.OK() {
		t.Fatalf("audit: %+v", a)
	}
	rep, err := recon.Reconcile(ctx, m, res.Bank)
	if err != nil {
		t.Fatal(err)
	}
	for k, n := range res.Planted {
		if rep.ByKind[k] != n {
			t.Errorf("%s: planted %d found %d", k, n, rep.ByKind[k])
		}
	}
	first, err := recon.Resolve(ctx, m, rep)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := recon.Resolve(ctx, m, rep)
	total := 0
	for _, v := range first {
		total += v
	}
	for _, v := range second {
		if v != 0 {
			t.Fatalf("second resolve applied entries: %v", second)
		}
	}
	if total != len(rep.Breaks) || res.Replays == 0 || res.Crashes == 0 {
		t.Fatalf("fixed %d of %d, replays %d, crashes %d", total, len(rep.Breaks), res.Replays, res.Crashes)
	}
	if a, _ := m.Audit(ctx); !a.OK() {
		t.Fatalf("audit after recon: %+v", a)
	}
}

func TestMemKeyReuseAndRejection(t *testing.T) {
	ctx := context.Background()
	m := ledger.NewMem()
	m.CreateAccounts(ctx, ledger.Account{ID: "bank", Kind: "asset", AllowNegative: true}, ledger.Account{ID: "a", Kind: "liability"}, ledger.Account{ID: "b", Kind: "liability"})
	r := ledger.Request{Key: "k", Kind: "payment", Postings: []ledger.Posting{ledger.P("a", -5), ledger.P("b", 5)}}
	if o, _ := m.Post(ctx, r); o.Rejected == "" {
		t.Fatal("overdraft allowed")
	}
	m.Post(ctx, ledger.Request{Key: "top", Kind: "topup", Postings: []ledger.Posting{ledger.P("bank", -10), ledger.P("a", 10)}})
	if o, _ := m.Post(ctx, r); o.Rejected == "" || !o.Replay {
		t.Fatal("retry changed a stored rejection")
	}
	r2 := r
	r2.Postings = []ledger.Posting{ledger.P("a", -6), ledger.P("b", 6)}
	if _, err := m.Post(ctx, r2); err != ledger.ErrKeyReuse {
		t.Fatalf("key reuse: %v", err)
	}
}
