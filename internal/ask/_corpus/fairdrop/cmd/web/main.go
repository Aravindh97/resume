//go:build js && wasm

// Command web runs the fairdrop rush and the verifiable queue in the
// browser. Seats live in the in-memory store, which follows the same rules
// as the Postgres store; the lottery fetches real randomness from drand.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/Aravindh97/fairdrop/internal/beacon"
	"github.com/Aravindh97/fairdrop/internal/lottery"
	"github.com/Aravindh97/fairdrop/internal/sim"
	"github.com/Aravindh97/fairdrop/internal/store"
)

var (
	mu      sync.Mutex
	mem     *store.Mem
	show    string
	showN   int
	entries []lottery.Entry
	commit  lottery.Commitment
	rnd     []byte
	order   []string
)

func newShow(rows, per int) {
	mem = store.NewMem(1500 * time.Millisecond)
	showN++
	show = fmt.Sprintf("show%d", showN)
	mem.CreateShow(context.Background(), show, rows, per)
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	ctx := context.Background()
	newShow(12, 24)
	drand := beacon.NewDrand()
	serve(map[string]handler{
		"POST rush": func(q url.Values, _ []byte) resp {
			buyers, _ := strconv.Atoi(q.Get("buyers"))
			buyers = min(max(buyers, 10), 20000)
			rows, _ := strconv.Atoi(q.Get("rows"))
			per, _ := strconv.Atoi(q.Get("per"))
			rows, per = min(max(rows, 2), 30), min(max(per, 4), 40)
			naive := q.Get("mode") == "naive"
			mu.Lock()
			defer mu.Unlock()
			newShow(rows, per)
			seed := uint64(time.Now().UnixNano())
			r, err := sim.Run(ctx, mem, sim.Config{Show: show, Buyers: buyers, Concurrency: 64, Seed: seed, Naive: naive})
			if err != nil {
				return fail(500, err.Error())
			}
			seats, _ := mem.Seats(show)
			return ok(map[string]any{"report": r, "seats": seats, "rows": rows, "per": per})
		},
		"POST lottery/register": func(q url.Values, _ []byte) resp {
			n, _ := strconv.Atoi(q.Get("n"))
			n = min(max(n, 10), 50000)
			mu.Lock()
			defer mu.Unlock()
			entries = make([]lottery.Entry, n)
			for i := range entries {
				entries[i] = lottery.Entry{ID: randomID(), AccountID: fmt.Sprintf("acct-%06d", i), Seats: 1 + i%6, Together: i%2 == 0}
			}
			// Commit to a drand round about 9 seconds away: its value does not exist yet.
			round := drand.RoundAt(time.Now().Add(9 * time.Second))
			commit = lottery.Commit("browser-demo", entries, round, time.Now().UTC())
			rnd, order = nil, nil
			return ok(map[string]any{"commitment": commit, "first_entries": entries[:min(5, n)]})
		},
		"POST lottery/draw": func(url.Values, []byte) resp {
			mu.Lock()
			c := commit
			mu.Unlock()
			if c.Round == 0 {
				return fail(400, "register entries first")
			}
			cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
			defer cancel()
			r, err := drand.Randomness(cctx, c.Round)
			if err != nil {
				return fail(502, "drand: "+err.Error())
			}
			mu.Lock()
			defer mu.Unlock()
			rnd, order = r, lottery.Order(entries, r)
			return ok(map[string]any{"round": c.Round, "randomness": hex.EncodeToString(r), "first_in_queue": order[:min(10, len(order))]})
		},
		"POST lottery/verify": func(_ url.Values, b []byte) resp {
			var in struct{ Tamper string }
			json.Unmarshal(b, &in)
			mu.Lock()
			es, ord, r, c := append([]lottery.Entry(nil), entries...), append([]string(nil), order...), rnd, commit
			mu.Unlock()
			if r == nil {
				return fail(400, "draw first")
			}
			what := "the published list and order, unchanged"
			switch in.Tamper {
			case "swap":
				ord[0], ord[len(ord)/2] = ord[len(ord)/2], ord[0]
				what = "the operator moves someone from the middle of the queue to the front"
			case "late":
				es = append(es, lottery.Entry{ID: "00friend", AccountID: "friend", Seats: 6})
				ord = lottery.Order(es, r)
				what = "the operator adds a friend after registration closed, and recomputes the order"
			case "randomness":
				r = append([]byte("other"), r...)
				what = "the operator uses randomness other than the committed drand round"
			}
			err := lottery.Verify(c, es, r, ord)
			res := map[string]any{"scenario": what, "ok": err == nil}
			if err != nil {
				res["error"] = err.Error()
			}
			return ok(res)
		},
	})
}
