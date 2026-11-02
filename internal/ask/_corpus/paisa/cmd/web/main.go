//go:build js && wasm

// Command web runs the paisa ledger in the browser: a payment playground
// for idempotency, a crash-before-commit demo, and the full chaos run with
// reconciliation, on the in-memory ledger (same rules as the Postgres one).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Aravindh97/paisa/internal/chaos"
	"github.com/Aravindh97/paisa/internal/ledger"
	"github.com/Aravindh97/paisa/internal/recon"
)

var (
	mu  sync.Mutex
	led *ledger.Mem
	log []string
)

var accounts = []ledger.Account{
	{ID: "bank:clearing", Kind: "asset", AllowNegative: true},
	{ID: "customer:asha", Kind: "liability"},
	{ID: "customer:ravi", Kind: "liability"},
	{ID: "merchant:chai-stall", Kind: "liability"},
	{ID: "revenue:fees", Kind: "revenue", AllowNegative: true},
}

func reset() {
	led = ledger.NewMem()
	ctx := context.Background()
	led.CreateAccounts(ctx, accounts...)
	led.Post(ctx, ledger.Request{Key: "topup:asha", Kind: "topup", Postings: []ledger.Posting{ledger.P("bank:clearing", -50000), ledger.P("customer:asha", 50000)}})
	led.Post(ctx, ledger.Request{Key: "topup:ravi", Kind: "topup", Postings: []ledger.Posting{ledger.P("bank:clearing", -2000), ledger.P("customer:ravi", 2000)}})
	log = []string{"start: Asha topped up ₹500, Ravi ₹20"}
}

func state() map[string]any {
	a, _ := led.Audit(context.Background())
	bal := led.Balances()
	var rows []map[string]any
	for _, acc := range accounts {
		rows = append(rows, map[string]any{"id": acc.ID, "paise": bal[acc.ID], "may_go_negative": acc.AllowNegative})
	}
	tail := log
	if len(tail) > 14 {
		tail = tail[len(tail)-14:]
	}
	return map[string]any{"balances": rows, "audit": a, "ok": a.OK(), "entries": led.Entries(8), "log": tail}
}

func rupees(p int64) string { return fmt.Sprintf("₹%d.%02d", p/100, p%100) }

func main() {
	reset()
	ctx := context.Background()
	serve(map[string]handler{
		"GET state": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			return ok(state())
		},
		"POST reset": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			reset()
			return ok(state())
		},
		"POST pay": func(_ url.Values, b []byte) resp {
			var in struct {
				Key, From string
				Rupees    float64
				Crash     bool // abort just before commit, as a process crash would
			}
			if json.Unmarshal(b, &in) != nil || in.Key == "" {
				return fail(400, "an idempotency key is required")
			}
			amount := int64(in.Rupees*100 + 0.5)
			if amount <= 0 || amount > 10_000_000 {
				return fail(400, "amount must be between ₹0.01 and ₹1,00,000")
			}
			fee := max(amount/100, 1)
			req := ledger.Request{Key: in.Key, Kind: "payment", Reference: "UPI-" + in.Key, Postings: []ledger.Posting{
				ledger.P(in.From, -amount), ledger.P("merchant:chai-stall", amount-fee), ledger.P("revenue:fees", fee)}}
			mu.Lock()
			defer mu.Unlock()
			if in.Crash {
				ledger.FaultHook = func(string) error { return errors.New("process crashed before commit") }
			}
			out, err := led.Post(ctx, req)
			ledger.FaultHook = nil
			switch {
			case errors.Is(err, ledger.ErrKeyReuse):
				log = append(log, fmt.Sprintf("key %q reused for a different payment: REFUSED (the client has a bug; nothing changed)", in.Key))
			case err != nil:
				log = append(log, fmt.Sprintf("%s pays %s with key %q: %v. Nothing was written; retrying with the same key is safe", in.From, rupees(amount), in.Key, err))
			case out.Replay && out.Rejected != "":
				log = append(log, fmt.Sprintf("retry of key %q: same answer as before, rejected (%s)", in.Key, out.Rejected))
			case out.Replay:
				log = append(log, fmt.Sprintf("retry of key %q: already done as entry %d, not charged again", in.Key, out.EntryID))
			case out.Rejected != "":
				log = append(log, fmt.Sprintf("%s pays %s with key %q: rejected, %s (stored under the key)", in.From, rupees(amount), in.Key, out.Rejected))
			default:
				log = append(log, fmt.Sprintf("%s pays %s with key %q: entry %d (merchant %s, fee %s)", in.From, rupees(amount), in.Key, out.EntryID, rupees(amount-fee), rupees(fee)))
			}
			return ok(state())
		},
		"POST chaos": func(q url.Values, _ []byte) resp {
			n, _ := strconv.Atoi(q.Get("payments"))
			n = min(max(n, 100), 50000)
			c := chaos.Default()
			c.Payments = n
			c.Customers = min(2000, max(100, n/10))
			m := ledger.NewMem()
			t := time.Now()
			res, err := chaos.Run(ctx, m, c)
			if err != nil {
				return fail(500, err.Error())
			}
			took := time.Since(t)
			before, _ := m.Audit(ctx)
			rep, err := recon.Reconcile(ctx, m, res.Bank)
			if err != nil {
				return fail(500, err.Error())
			}
			first, err := recon.Resolve(ctx, m, rep)
			if err != nil {
				return fail(500, err.Error())
			}
			second, _ := recon.Resolve(ctx, m, rep)
			after, _ := m.Audit(ctx)
			var kinds []map[string]any
			for _, k := range []recon.Kind{recon.MissingInLedger, recon.MissingInBank, recon.AmountMismatch, recon.DuplicateInBank} {
				kinds = append(kinds, map[string]any{"kind": k, "planted": res.Planted[k], "found": rep.ByKind[k], "fixed": first[k]})
			}
			n2 := 0
			for _, v := range second {
				n2 += v
			}
			var sample []recon.Break
			for i, b := range rep.Breaks {
				if i%max(1, len(rep.Breaks)/6) == 0 && len(sample) < 6 {
					sample = append(sample, b)
				}
			}
			sort.Slice(sample, func(i, j int) bool { return sample[i].Kind < sample[j].Kind })
			return ok(map[string]any{"payments": n, "booked": res.Posted, "rejected": res.Rejected, "crashes": res.Crashes, "lost": res.LostResponses,
				"replays": res.Replays, "refunds": res.Refunds, "double_refunds": res.DoubleRefundTries, "took_ms": took.Milliseconds(),
				"audit_before": before, "ok_before": before.OK(), "matched": rep.Matched, "breaks": kinds, "sample": sample,
				"second_resolve_entries": n2, "audit_after": after, "ok_after": after.OK()})
		},
	})
}
