// Command loadtest drives a running Kaasu server (started with DEMO=1):
// the three friends paying each other from several devices at once, a
// fifth of the payments sent twice with the same key (a phone retrying on
// a bad network), and others reading their wallets. Afterwards it checks
// that every retried payment moved money once and that the ledger balances.
//
//	go run ./cmd/loadtest -url http://127.0.0.1:8100 -devices 8 -readers 30 -for 20s
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type sample struct {
	kind string
	d    time.Duration
	code int
}

type client struct {
	base string
	c    *http.Client
}

func login(base, email string) *client {
	jar, _ := cookiejar.New(nil)
	c := &client{base, &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	if code, b, _ := c.do("POST", "/api/demo-login", map[string]string{"email": email}, ""); code != 200 {
		fmt.Fprintln(os.Stderr, "login", email, code, string(b))
		os.Exit(1)
	}
	return c
}

func (c *client) do(method, path string, body any, key string) (int, []byte, time.Duration) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kaasu", "1")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	t := time.Now()
	resp, err := c.c.Do(req)
	if err != nil {
		return 0, nil, time.Since(t)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, b, time.Since(t)
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8100", "server")
	devices := flag.Int("devices", 8, "devices per sender (three senders)")
	readers := flag.Int("readers", 30, "sessions reading wallets and activity")
	dur := flag.Duration("for", 20*time.Second, "test length")
	flag.Parse()

	people := []struct{ email, upi string }{{"anbu@kaasu.demo", "anbu@kaasu"}, {"kavya@kaasu.demo", "kavya@kaasu"}, {"ravi@kaasu.demo", "ravi@kaasu"}}
	var mu sync.Mutex
	var samples []sample
	// key -> txn id from every 200 response; a key must always map to one txn
	txnOf := map[string]int64{}
	var conflicts, retried atomic.Int64
	var wg sync.WaitGroup
	stop := time.Now().Add(*dur)
	for p := range people {
		for d := range *devices {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := login(*base, people[p].email)
				r := rand.New(rand.NewPCG(uint64(p), uint64(d)))
				var mine []sample
				n := 0
				for time.Now().Before(stop) {
					n++
					key := fmt.Sprintf("load-%d-%d-%d-%d", p, d, n, time.Now().UnixNano())
					to := people[(p+1+r.IntN(2))%3].upi
					body := map[string]any{"to": to, "amount": 100 + r.IntN(400), "note": "load test", "pin": "2580"}
					times := 1
					if r.IntN(5) == 0 {
						times = 2 // the retry
						retried.Add(1)
					}
					for range times {
						code, b, dt := c.do("POST", "/api/send", body, key)
						mine = append(mine, sample{"send", dt, code})
						if code == 200 {
							var m struct {
								TxnID int64 `json:"txn_id"`
							}
							json.Unmarshal(b, &m)
							mu.Lock()
							if prev, ok := txnOf[key]; ok && prev != m.TxnID {
								conflicts.Add(1)
							}
							txnOf[key] = m.TxnID
							mu.Unlock()
						}
					}
				}
				mu.Lock()
				samples = append(samples, mine...)
				mu.Unlock()
			}()
		}
	}
	for i := range *readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := login(*base, people[i%3].email)
			var mine []sample
			for time.Now().Before(stop) {
				code, _, d := c.do("GET", "/api/wallet", nil, "")
				mine = append(mine, sample{"wallet", d, code})
				code, _, d = c.do("GET", "/api/activity?limit=20", nil, "")
				mine = append(mine, sample{"activity", d, code})
			}
			mu.Lock()
			samples = append(samples, mine...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	by := map[string][]sample{}
	for _, s := range samples {
		by[s.kind] = append(by[s.kind], s)
	}
	fmt.Printf("%d sending devices (3 people) and %d readers for %s: %d requests, %.0f/s\n", 3**devices, *readers, *dur, len(samples), float64(len(samples))/dur.Seconds())
	fmt.Printf("%-10s %8s %9s %9s %9s %s\n", "request", "count", "p50", "p95", "p99", "status codes")
	for _, k := range []string{"send", "wallet", "activity"} {
		ss := by[k]
		if len(ss) == 0 {
			continue
		}
		ds := make([]time.Duration, len(ss))
		codes := map[int]int{}
		for i, s := range ss {
			ds[i] = s.d
			codes[s.code]++
		}
		slices.Sort(ds)
		q := func(p float64) time.Duration {
			return ds[min(len(ds)-1, int(p*float64(len(ds))))].Round(100 * time.Microsecond)
		}
		fmt.Printf("%-10s %8d %9s %9s %9s %v\n", k, len(ss), q(0.5), q(0.95), q(0.99), codes)
	}

	ops := login(*base, "selva@kaasu.demo")
	_, b, _ := ops.do("GET", "/api/ops/ledger", nil, "")
	var lc struct {
		OK   bool  `json:"ok"`
		Txns int   `json:"txns"`
		Tot  int64 `json:"total"`
	}
	json.Unmarshal(b, &lc)
	fmt.Printf("payments that went through: %d (%d sent twice with the same key); a key answered with two different payments: %d\n", len(txnOf), retried.Load(), conflicts.Load())
	fmt.Printf("ledger check: %s\n", b)
	if !lc.OK || conflicts.Load() > 0 {
		os.Exit(1)
	}
}
