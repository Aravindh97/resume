// Command loadtest drives a running Arogya server (started with DEMO=1)
// like a busy OPD morning: receptionists registering patients and giving
// walk-in tokens for all four doctors, the two doctors with demo logins
// calling patients as fast as they can, and
// waiting-room screens reading the board. It reports latency per kind of
// request, then checks the queue's rules held.
//
//	go run ./cmd/loadtest -url http://127.0.0.1:8098 -users 50 -for 20s
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

type client struct {
	base string
	c    *http.Client
}

func login(base, email string) (*client, error) {
	jar, _ := cookiejar.New(nil)
	c := &client{base, &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	if code, b, _ := c.do("POST", "/api/demo-login", map[string]string{"email": email}); code != 200 {
		return nil, fmt.Errorf("login %s: %d %s", email, code, b)
	}
	return c, nil
}

func (c *client) do(method, path string, body any) (int, []byte, time.Duration) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Arogya", "1")
	t := time.Now()
	resp, err := c.c.Do(req)
	if err != nil {
		return 0, nil, time.Since(t)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, b, time.Since(t)
}

type visit struct {
	ID       int64  `json:"id"`
	DoctorID int64  `json:"doctor_id"`
	Doctor   string `json:"doctor"`
	Token    int    `json:"token"`
	Status   string `json:"status"`
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8098", "server")
	users := flag.Int("users", 50, "concurrent users: 2 doctors, then about a third receptionists and the rest waiting-room screens")
	dur := flag.Duration("for", 20*time.Second, "test length")
	flag.Parse()

	doctors := []string{"priya@arogya.demo", "senthil@arogya.demo"} // the doctors with demo logins
	desk, err := login(*base, "malar@arogya.demo")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var cfg struct {
		Now string `json:"now"`
	}
	_, b, _ := desk.do("GET", "/api/config", nil)
	json.Unmarshal(b, &cfg)
	if len(cfg.Now) < 10 {
		fmt.Fprintln(os.Stderr, "no server date:", string(b))
		os.Exit(1)
	}
	today := cfg.Now[:10]
	var docs []struct {
		ID int64 `json:"id"`
	}
	_, b, _ = desk.do("GET", "/api/doctors?day="+today, nil)
	json.Unmarshal(b, &docs)
	if len(docs) == 0 {
		fmt.Fprintln(os.Stderr, "no doctors:", string(b))
		os.Exit(1)
	}

	var mu sync.Mutex
	var samples []sample
	var wg sync.WaitGroup
	stop := time.Now().Add(*dur)
	run := func(i int, email string, loop func(c *client, rng *rand.Rand, add func(string, int, time.Duration))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := login(*base, email)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			rng := rand.New(rand.NewPCG(uint64(i), 7))
			var mine []sample
			add := func(k string, code int, d time.Duration) { mine = append(mine, sample{k, d, code}) }
			for time.Now().Before(stop) {
				loop(c, rng, add)
			}
			mu.Lock()
			samples = append(samples, mine...)
			mu.Unlock()
		}()
	}

	reception := max(1, (*users-len(doctors))/3)
	screens := max(0, *users-len(doctors)-reception)
	for i, email := range doctors {
		run(i, email, func(c *client, rng *rand.Rand, add func(string, int, time.Duration)) {
			code, _, d := c.do("POST", "/api/queue/next", nil)
			add("call next", code, d)
			code, _, d = c.do("GET", "/api/queue", nil)
			add("doctor queue", code, d)
		})
	}
	for i := 0; i < reception; i++ {
		email := []string{"malar@arogya.demo", "anand@arogya.demo"}[i%2]
		run(100+i, email, func(c *client, rng *rand.Rand, add func(string, int, time.Duration)) {
			code, b, d := c.do("POST", "/api/patients", map[string]string{
				"name": fmt.Sprintf("Load Patient %d", rng.IntN(1e9)), "birth_date": "1985-06-15", "sex": []string{"female", "male"}[rng.IntN(2)],
				"phone": fmt.Sprintf("9%09d", rng.IntN(1e9)), "area": "Palladam",
			})
			add("register", code, d)
			var p struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(b, &p) != nil || p.ID == 0 {
				return
			}
			code, _, d = c.do("POST", "/api/visits", map[string]any{"patient_id": p.ID, "doctor_id": docs[rng.IntN(len(docs))].ID, "day": today, "slot": ""})
			add("walk-in token", code, d)
			if rng.IntN(4) == 0 {
				code, _, d = c.do("GET", "/api/reception", nil)
				add("reception list", code, d)
			}
		})
	}
	for i := 0; i < screens; i++ {
		run(200+i, "kavitha@arogya.demo", func(c *client, rng *rand.Rand, add func(string, int, time.Duration)) {
			code, _, d := c.do("GET", "/api/board", nil)
			add("board", code, d)
			time.Sleep(time.Duration(rng.IntN(20)) * time.Millisecond)
		})
	}
	wg.Wait()

	by := map[string][]sample{}
	for _, s := range samples {
		by[s.kind] = append(by[s.kind], s)
	}
	fmt.Printf("%d users (%d doctors, %d at reception, %d screens) for %s: %d requests, %.0f/s\n", len(doctors)+reception+screens, len(doctors), reception, screens, *dur, len(samples), float64(len(samples))/dur.Seconds())
	fmt.Printf("%-15s %8s %9s %9s %9s %s\n", "request", "count", "p50", "p95", "p99", "status codes")
	for _, k := range []string{"register", "walk-in token", "call next", "doctor queue", "reception list", "board"} {
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
		fmt.Printf("%-15s %8d %9s %9s %9s %v\n", k, len(ss), q(0.5), q(0.95), q(0.99), codes)
	}

	// The rules after the rush: tokens are unique per doctor, and no doctor
	// has two patients with them at once.
	_, b, _ = desk.do("GET", "/api/reception", nil)
	var all []visit
	json.Unmarshal(b, &all)
	tokens, called := map[[2]int64]int{}, map[int64]int{}
	bad := 0
	for _, v := range all {
		k := [2]int64{v.DoctorID, int64(v.Token)}
		if tokens[k]++; tokens[k] == 2 {
			fmt.Printf("FAIL: token %d given twice for %s\n", v.Token, v.Doctor)
			bad++
		}
		if v.Status == "called" {
			if called[v.DoctorID]++; called[v.DoctorID] == 2 {
				fmt.Printf("FAIL: %s has two patients called at once\n", v.Doctor)
				bad++
			}
		}
	}
	fmt.Printf("checked %d visits today: %d problems\n", len(all), bad)
	if bad > 0 {
		os.Exit(1)
	}
}
