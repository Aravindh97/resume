// Command loadtest drives a running Thari server (started with DEMO=1) with
// many signed-in users at once: production staff logging pieces while
// merchandisers and buyers browse lists and orders. It reports throughput
// and latency per kind of request, and checks the progress invariant after.
//
//	go run ./cmd/loadtest -url http://127.0.0.1:8095 -users 50 -for 20s
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
	"time"
)

type sample struct {
	kind string
	d    time.Duration
	code int
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8095", "server")
	users := flag.Int("users", 50, "concurrent users")
	dur := flag.Duration("for", 20*time.Second, "test length")
	flag.Parse()

	who := []string{"selvi@kaveriknits.demo", "arun@kaveriknits.demo", "jana@nordwind.demo", "maya@urbanthread.demo", "kenji@sakura.demo"}
	var mu sync.Mutex
	var samples []sample
	var wg sync.WaitGroup
	stop := time.Now().Add(*dur)
	for i := 0; i < *users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			email := who[i%len(who)]
			jar, _ := cookiejar.New(nil)
			c := &http.Client{Jar: jar, Timeout: 30 * time.Second}
			do := func(method, path string, body any) (int, []byte, time.Duration) {
				var r io.Reader
				if body != nil {
					b, _ := json.Marshal(body)
					r = bytes.NewReader(b)
				}
				req, _ := http.NewRequest(method, *base+path, r)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Thari", "1")
				t := time.Now()
				resp, err := c.Do(req)
				if err != nil {
					return 0, nil, time.Since(t)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return resp.StatusCode, b, time.Since(t)
			}
			if code, _, _ := do("POST", "/api/demo-login", map[string]string{"email": email}); code != 200 {
				fmt.Fprintln(os.Stderr, "login failed", email, code)
				return
			}
			rng := rand.New(rand.NewPCG(uint64(i), 1))
			var mine []sample
			for time.Now().Before(stop) {
				code, b, d := do("GET", "/api/orders?limit=25", nil)
				mine = append(mine, sample{"list orders", d, code})
				var page struct {
					Orders []struct {
						ID       int64 `json:"id"`
						Progress []struct {
							Stage string `json:"stage"`
							Done  int    `json:"done"`
						} `json:"progress"`
						Quantity int `json:"quantity"`
					} `json:"orders"`
				}
				json.Unmarshal(b, &page)
				if len(page.Orders) == 0 {
					continue
				}
				o := page.Orders[rng.IntN(len(page.Orders))]
				code, _, d = do("GET", fmt.Sprintf("/api/orders/%d", o.ID), nil)
				mine = append(mine, sample{"open order", d, code})
				if email == who[0] { // production logs pieces at the first unfinished stage
					for _, p := range o.Progress {
						if p.Done < o.Quantity {
							code, _, d = do("POST", fmt.Sprintf("/api/orders/%d/progress", o.ID), map[string]any{"stage": p.Stage, "delta": 1})
							mine = append(mine, sample{"log progress", d, code})
							break
						}
					}
				}
				if rng.IntN(5) == 0 {
					code, _, d = do("GET", "/api/dashboard", nil)
					mine = append(mine, sample{"dashboard", d, code})
				}
			}
			mu.Lock()
			samples = append(samples, mine...)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	by := map[string][]sample{}
	for _, s := range samples {
		by[s.kind] = append(by[s.kind], s)
	}
	fmt.Printf("%d users for %s against %s: %d requests, %.0f/s\n", *users, *dur, *base, len(samples), float64(len(samples))/dur.Seconds())
	fmt.Printf("%-14s %8s %9s %9s %9s %s\n", "request", "count", "p50", "p95", "p99", "status codes")
	for _, k := range []string{"list orders", "open order", "log progress", "dashboard"} {
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
		fmt.Printf("%-14s %8d %9s %9s %9s %v\n", k, len(ss), q(0.5), q(0.95), q(0.99), codes)
	}
}
