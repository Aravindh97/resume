package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/Aravindh97/thari/internal/auth"
	"github.com/Aravindh97/thari/internal/db"
	"github.com/Aravindh97/thari/internal/domain"
	"github.com/Aravindh97/thari/internal/live"
	"github.com/Aravindh97/thari/internal/seed"
	"github.com/Aravindh97/thari/internal/store"
)

var today = time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)

type env struct {
	t   *testing.T
	ts  *httptest.Server
	db  *pgxpool.Pool
	srv *Server
}

// newEnv resets the test database to the demo data and starts a server.
// Set THARI_TEST_DATABASE_URL to run these tests.
func newEnv(t *testing.T, demo bool) *env {
	t.Helper()
	url := os.Getenv("THARI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("THARI_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(cancel) // runs first: stops the listener so the pool can close
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	a := auth.New(pool)
	a.Cost = bcrypt.MinCost
	if err := seed.Reset(ctx, pool, today, a.Cost); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{Demo: demo, Today: func() time.Time { return today }}, pool, a, live.New(log), log)
	go s.Hub().Listen(ctx, pool)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &env{t, ts, pool, s}
}

type client struct {
	e *env
	h *http.Client
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e, &http.Client{Jar: jar}}
}

// as signs in with the demo login.
func (e *env) as(email string) *client {
	c := e.client()
	if code, body := c.do("POST", "/api/demo-login", map[string]string{"email": email}, nil); code != 200 {
		e.t.Fatalf("demo login %s: %d %s", email, code, body)
	}
	return c
}

func (c *client) do(method, path string, body any, out any) (int, string) {
	c.e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.ts.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Thari", "1")
	resp, err := c.h.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		json.Unmarshal(b, out)
	}
	return resp.StatusCode, string(b)
}

const (
	arun  = "arun@kaveriknits.demo"
	selvi = "selvi@kaveriknits.demo"
	admin = "lakshmi@kaveriknits.demo"
	jana  = "jana@nordwind.demo" // Nordwind GmbH
	kenji = "kenji@sakura.demo"  // Sakura Apparel
)

type list struct {
	Orders []store.Order `json:"orders"`
	Next   string        `json:"next_cursor"`
}

func (c *client) orders(q string) []store.Order {
	var all []store.Order
	cursor := ""
	for {
		var l list
		if code, b := c.do("GET", "/api/orders?limit=7&"+q+"&cursor="+cursor, nil, &l); code != 200 {
			c.e.t.Fatalf("list: %d %s", code, b)
		}
		all = append(all, l.Orders...)
		if l.Next == "" {
			return all
		}
		cursor = l.Next
	}
}

// findOrder returns an open order of the given buyer matching ok.
func (e *env) findOrder(buyer string, ok func(store.Order) bool) store.Order {
	for _, o := range e.as(arun).orders("status=all") {
		if o.Buyer == buyer && ok(o) {
			return o
		}
	}
	e.t.Fatalf("no %s order matching", buyer)
	return store.Order{}
}

func TestBuyersSeeOnlyTheirOwnOrders(t *testing.T) {
	e := newEnv(t, true)
	j := e.as(jana)
	mine := j.orders("status=all")
	if len(mine) == 0 {
		t.Fatal("Jana sees no orders")
	}
	for _, o := range mine {
		if o.Buyer != "Nordwind GmbH" || o.UnitPricePaise != nil {
			t.Fatalf("Jana sees %s, price %v", o.Buyer, o.UnitPricePaise)
		}
	}
	other := e.findOrder("Sakura Apparel", func(store.Order) bool { return true })
	if code, _ := j.do("GET", fmt.Sprintf("/api/orders/%d", other.ID), nil, nil); code != 404 {
		t.Fatalf("Jana opened a Sakura order: %d", code)
	}
	if code, _ := j.do("POST", fmt.Sprintf("/api/orders/%d/comments", other.ID), map[string]any{"text": "hi"}, nil); code != 404 {
		t.Fatalf("Jana commented on a Sakura order: %d", code)
	}
	// Internal notes on her own orders are hidden from her.
	nw := mine[0]
	e.as(arun).do("POST", fmt.Sprintf("/api/orders/%d/comments", nw.ID), map[string]any{"text": "Margin is thin on this one", "internal": true}, nil)
	var d store.OrderDetail
	j.do("GET", fmt.Sprintf("/api/orders/%d", nw.ID), nil, &d)
	for _, ev := range d.Events {
		if !ev.BuyerVisible || strings.Contains(string(ev.Detail), "Margin") {
			t.Fatalf("buyer sees an internal event: %s", ev.Detail)
		}
	}
	if d.Can[domain.AddProgress] || d.Can[domain.ViewPrice] {
		t.Fatalf("buyer capabilities: %v", d.Can)
	}
	if code, _ := j.do("GET", "/api/users", nil, nil); code != 403 {
		t.Fatal("buyer listed users")
	}
}

func TestRolesAndPrices(t *testing.T) {
	e := newEnv(t, true)
	o := e.findOrder("Nordwind GmbH", func(o store.Order) bool {
		return o.Status == "open" && o.Progress[1].Done < o.Quantity && o.Progress[1].Done > o.Progress[2].Done+100
	})
	s := e.as(selvi)
	var got store.OrderDetail
	s.do("GET", fmt.Sprintf("/api/orders/%d", o.ID), nil, &got)
	if got.Order.UnitPricePaise != nil {
		t.Fatal("production sees the price")
	}
	if code, _ := s.do("PATCH", fmt.Sprintf("/api/orders/%d", o.ID), map[string]any{"version": got.Order.Version, "colour": "Red"}, nil); code != 403 {
		t.Fatalf("production edited an order: %d", code)
	}
	if code, _ := s.do("POST", fmt.Sprintf("/api/orders/%d/progress", o.ID), map[string]any{"stage": "stitching", "delta": 50}, nil); code != 200 {
		t.Fatalf("production logging progress: %d", code)
	}
	if code, _ := s.do("POST", fmt.Sprintf("/api/orders/%d/progress", o.ID), map[string]any{"stage": "stitching", "delta": -50}, nil); code != 403 {
		t.Fatalf("production made a correction: %d", code)
	}
	gap := got.Order.Progress[1].Done - got.Order.Progress[2].Done
	if code, b := s.do("POST", fmt.Sprintf("/api/orders/%d/progress", o.ID), map[string]any{"stage": "stitching", "delta": gap - 50 + 1}, nil); code != 422 || !strings.Contains(b, "more than Cut") {
		t.Fatalf("stitching beyond cutting: %d %s", code, b)
	}
	if code, _ := e.as(arun).do("POST", fmt.Sprintf("/api/orders/%d/progress", o.ID), map[string]any{"stage": "stitching", "delta": -50}, nil); code != 200 {
		t.Fatal("merchandiser correction refused")
	}
}

// 40 people on the floor log 50 stitched pieces each at the same moment,
// on an order with only 1,000 pieces cut. Exactly 20 can succeed; none may
// be lost and the count may never pass cutting.
func TestConcurrentProgressIsNeverLostOrExceeded(t *testing.T) {
	e := newEnv(t, true)
	o := e.findOrder("Urban Thread Co.", func(o store.Order) bool { return o.Status == "open" })
	ctx := context.Background()
	e.db.Exec(ctx, `update progress set done = case stage when 'fabric' then $2 when 'cutting' then 1000 else 0 end where order_id = $1`, o.ID, o.Quantity)
	if o.Quantity < 1000 {
		t.Skip("order too small")
	}
	var ok, refused atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := e.as(selvi)
			code, _ := c.do("POST", fmt.Sprintf("/api/orders/%d/progress", o.ID), map[string]any{"stage": "stitching", "delta": 50}, nil)
			switch code {
			case 200:
				ok.Add(1)
			case 422:
				refused.Add(1)
			default:
				t.Errorf("status %d", code)
			}
		}()
	}
	wg.Wait()
	var stitched, events int
	e.db.QueryRow(ctx, `select done from progress where order_id = $1 and stage = 'stitching'`, o.ID).Scan(&stitched)
	e.db.QueryRow(ctx, `select count(*) from events where order_id = $1 and kind = 'progress' and detail->>'stage' = 'stitching' and at > now() - interval '1 minute'`, o.ID).Scan(&events)
	if ok.Load() != 20 || refused.Load() != 20 || stitched != 1000 || events != 20 {
		t.Fatalf("accepted %d, refused %d, stitched %d, events %d", ok.Load(), refused.Load(), stitched, events)
	}
}

func TestEditConflict(t *testing.T) {
	e := newEnv(t, true)
	o := e.findOrder("Nordwind GmbH", func(o store.Order) bool { return o.Status == "open" })
	a, l := e.as(arun), e.as(admin)
	if code, _ := a.do("PATCH", fmt.Sprintf("/api/orders/%d", o.ID), map[string]any{"version": o.Version, "colour": "Ink blue"}, nil); code != 200 {
		t.Fatal("first edit refused")
	}
	var conflict struct {
		Current store.Order `json:"current"`
	}
	code, _ := l.do("PATCH", fmt.Sprintf("/api/orders/%d", o.ID), map[string]any{"version": o.Version, "colour": "Olive"}, &conflict)
	if code != 409 || conflict.Current.Colour != "Ink blue" || conflict.Current.Version != o.Version+1 {
		t.Fatalf("second edit on a stale version: %d %+v", code, conflict.Current.Colour)
	}
	// The price change is recorded, but not where the buyer can see it.
	l.do("PATCH", fmt.Sprintf("/api/orders/%d", o.ID), map[string]any{"version": o.Version + 1, "unit_price_paise": 99900}, nil)
	var d store.OrderDetail
	e.as(jana).do("GET", fmt.Sprintf("/api/orders/%d", o.ID), nil, &d)
	for _, ev := range d.Events {
		if strings.Contains(string(ev.Detail), "unit_price") {
			t.Fatal("buyer sees a price change")
		}
	}
}

func TestShippingNeedsFullPackingAndAPassedInspection(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	o := e.findOrder("Sakura Apparel", func(o store.Order) bool { return o.Status == "open" && o.Progress[4].Done < o.Quantity })
	a, s := e.as(arun), e.as(selvi)
	if code, b := a.do("POST", fmt.Sprintf("/api/orders/%d/ship", o.ID), nil, nil); code != 422 || !strings.Contains(b, "packed") {
		t.Fatalf("shipped an unpacked order: %d %s", code, b)
	}
	e.db.Exec(ctx, `update progress set done = $2 where order_id = $1`, o.ID, o.Quantity)
	e.db.Exec(ctx, `delete from inspections where order_id = $1`, o.ID)
	if code, _ := a.do("POST", fmt.Sprintf("/api/orders/%d/ship", o.ID), nil, nil); code != 422 {
		t.Fatal("shipped without an inspection")
	}
	plan := domain.PlanFor(o.Quantity)
	var ins store.Inspection
	s.do("POST", fmt.Sprintf("/api/orders/%d/inspections", o.ID), map[string]any{"major": plan.AcceptMajor + 1}, &ins)
	if ins.Result != "fail" || ins.SampleSize != plan.SampleSize {
		t.Fatalf("inspection: %+v", ins)
	}
	if code, _ := a.do("POST", fmt.Sprintf("/api/orders/%d/ship", o.ID), nil, nil); code != 422 {
		t.Fatal("shipped after a failed inspection")
	}
	s.do("POST", fmt.Sprintf("/api/orders/%d/inspections", o.ID), map[string]any{"major": plan.AcceptMajor, "minor": plan.AcceptMinor}, &ins)
	if ins.Result != "pass" {
		t.Fatal("inspection at the acceptance numbers should pass")
	}
	if code, b := a.do("POST", fmt.Sprintf("/api/orders/%d/ship", o.ID), nil, nil); code != 200 {
		t.Fatalf("ship: %d %s", code, b)
	}
	if code, _ := s.do("POST", fmt.Sprintf("/api/orders/%d/progress", o.ID), map[string]any{"stage": "packing", "delta": 1}, nil); code != 422 {
		t.Fatal("progress on a shipped order")
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	e := newEnv(t, true)
	if _, err := e.db.Exec(context.Background(), `update events set kind = 'x' where id = 1`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("update allowed: %v", err)
	}
	if _, err := e.db.Exec(context.Background(), `delete from events where id = 1`); err == nil {
		t.Fatal("delete allowed")
	}
}

func TestSessionsAndCSRF(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	// demo login is off without DEMO
	if code, _ := e.client().do("POST", "/api/demo-login", map[string]string{"email": admin}, nil); code != 404 {
		t.Fatalf("demo login without DEMO: %d", code)
	}
	h, _ := auth.HashPassword("correct horse battery", bcrypt.MinCost)
	e.db.Exec(ctx, `update users set password_hash = $1, demo = false where email = $2`, h, admin)

	c := e.client()
	if code, _ := c.do("POST", "/api/login", map[string]string{"email": admin, "password": "wrong password 1"}, nil); code != 401 {
		t.Fatal("wrong password accepted")
	}
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/login", strings.NewReader(`{"email":"`+admin+`","password":"correct horse battery"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Thari", "1")
	resp, err := c.h.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login: %v %d", err, resp.StatusCode)
	}
	ck := resp.Cookies()[0]
	if ck.Name != cookieName || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || len(ck.Value) < 40 {
		t.Fatalf("cookie: %+v", ck)
	}
	var stored int
	e.db.QueryRow(ctx, `select count(*) from sessions where token_hash = $1`, []byte(ck.Value)).Scan(&stored)
	if stored != 0 {
		t.Fatal("the raw token is stored")
	}
	if code, _ := c.do("GET", "/api/me", nil, nil); code != 200 {
		t.Fatal("session not working")
	}
	// A cross-site form post has no custom header; a cross-site fetch would carry a foreign Origin.
	for _, hdr := range []map[string]string{{}, {"X-Thari": "1", "Origin": "https://evil.example"}} {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/logout", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if resp, _ := c.h.Do(req); resp.StatusCode != 403 {
			t.Fatalf("cross-site request %v: %d", hdr, resp.StatusCode)
		}
	}
	c.do("POST", "/api/logout", nil, nil)
	if code, _ := c.do("GET", "/api/me", nil, nil); code != 401 {
		t.Fatal("session survived logout")
	}
	// Throttling: after 5 failures, even the right password waits.
	for i := 0; i < 5; i++ {
		e.client().do("POST", "/api/login", map[string]string{"email": admin, "password": fmt.Sprint("nope nope ", i)}, nil)
	}
	if code, _ := e.client().do("POST", "/api/login", map[string]string{"email": admin, "password": "correct horse battery"}, nil); code != 429 {
		t.Fatalf("not throttled: %d", code)
	}
}

func TestAdminManagesUsers(t *testing.T) {
	e := newEnv(t, true)
	a := e.as(admin)
	var buyers []store.Buyer
	a.do("GET", "/api/buyers", nil, &buyers)
	var created struct {
		User     auth.User `json:"user"`
		Password string    `json:"password"`
	}
	if code, b := a.do("POST", "/api/users", map[string]any{"email": "ola@nordwind.example", "name": "Ola", "role": "buyer", "buyer_id": buyers[2].ID}, &created); code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	o := e.client()
	if code, _ := o.do("POST", "/api/login", map[string]string{"email": "ola@nordwind.example", "password": created.Password}, nil); code != 200 {
		t.Fatal("new user cannot sign in")
	}
	if code, _ := e.as(arun).do("POST", "/api/users", map[string]any{"email": "x@y.z", "name": "X", "role": "admin"}, nil); code != 403 {
		t.Fatal("merchandiser created a user")
	}
	a.do("POST", fmt.Sprintf("/api/users/%d/active", created.User.UserID), map[string]bool{"active": false}, nil)
	if code, _ := o.do("GET", "/api/me", nil, nil); code != 401 {
		t.Fatal("disabled user still signed in")
	}
}

func TestUploads(t *testing.T) {
	e := newEnv(t, true)
	o := e.findOrder("Nordwind GmbH", func(o store.Order) bool { return o.Status == "open" })
	a := e.as(arun)
	up := func(name string, data []byte) (int, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(data)
		mw.Close()
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/orders/%d/files", e.ts.URL, o.ID), &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-Thari", "1")
		resp, err := a.h.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	pdf := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("x"), 1000)...)
	code, body := up("tech-pack.pdf", pdf)
	if code != 201 {
		t.Fatalf("pdf: %d %s", code, body)
	}
	if code, _ := up("notes.pdf", []byte("<html><script>alert(1)</script>")); code != 422 {
		t.Fatal("HTML renamed to .pdf accepted")
	}
	if code, _ := up("big.pdf", append([]byte("%PDF-1.4\n"), make([]byte, store.MaxFile)...)); code != 422 && code != 413 {
		t.Fatalf("oversized file: %d", code)
	}
	var f store.FileInfo
	json.Unmarshal([]byte(body), &f)
	resp, _ := e.as(jana).h.Get(fmt.Sprintf("%s/api/files/%d", e.ts.URL, f.ID))
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d %s", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
	resp, _ = e.as(kenji).h.Get(fmt.Sprintf("%s/api/files/%d", e.ts.URL, f.ID))
	if resp.StatusCode != 404 {
		t.Fatal("another buyer downloaded the file")
	}
}

// Live updates reach exactly the users allowed to see them.
func TestLiveUpdatesRespectPermissions(t *testing.T) {
	e := newEnv(t, true)
	o := e.findOrder("Nordwind GmbH", func(o store.Order) bool { return o.Status == "open" && o.Progress[0].Done < o.Quantity })
	listen := func(email string) <-chan string {
		c := e.as(email)
		req, _ := http.NewRequest("GET", e.ts.URL+"/api/stream", nil)
		resp, err := c.h.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("stream: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		ch := make(chan string, 10)
		go func() {
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "data: ") {
					ch <- sc.Text()
				}
			}
		}()
		return ch
	}
	jane, ken, arunCh := listen(jana), listen(kenji), listen(arun)
	time.Sleep(200 * time.Millisecond) // let the subscriptions register
	e.as(selvi).do("POST", fmt.Sprintf("/api/orders/%d/progress", o.ID), map[string]any{"stage": "fabric", "delta": 50}, nil)
	for name, ch := range map[string]<-chan string{"Jana": jane, "Arun": arunCh} {
		select {
		case m := <-ch:
			if !strings.Contains(m, fmt.Sprintf(`"order_id":%d`, o.ID)) {
				t.Fatalf("%s got %s", name, m)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s got no update", name)
		}
	}
	e.as(arun).do("POST", fmt.Sprintf("/api/orders/%d/comments", o.ID), map[string]any{"text": "internal", "internal": true}, nil)
	select {
	case <-arunCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Arun missed the internal note")
	}
	select {
	case m := <-jane:
		t.Fatalf("Jana got an internal note: %s", m)
	case m := <-ken:
		t.Fatalf("Kenji got a Nordwind update: %s", m)
	case <-time.After(700 * time.Millisecond):
	}
}

func TestPaginationVisitsEveryOrderOnce(t *testing.T) {
	e := newEnv(t, true)
	all := e.as(arun).orders("status=all")
	var total int
	e.db.QueryRow(context.Background(), `select count(*) from orders`).Scan(&total)
	seen := map[int64]bool{}
	for i, o := range all {
		if seen[o.ID] {
			t.Fatalf("order %d twice", o.ID)
		}
		seen[o.ID] = true
		if i > 0 && (o.DueDate < all[i-1].DueDate || o.DueDate == all[i-1].DueDate && o.ID < all[i-1].ID) {
			t.Fatal("pages out of order")
		}
	}
	if len(all) != total {
		t.Fatalf("visited %d of %d", len(all), total)
	}
	if code, _ := e.as(arun).do("GET", "/api/orders?cursor=garbage", nil, nil); code != 422 {
		t.Fatal("bad cursor accepted")
	}
}

// The SQL risk rule used for filtering agrees with the Go rule shown on screen.
func TestRiskSQLMatchesGo(t *testing.T) {
	e := newEnv(t, true)
	r := rand.New(rand.NewPCG(4, 4))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3000; i++ {
		from := base.AddDate(0, 0, r.IntN(200))
		due := from.AddDate(0, 0, r.IntN(90))
		now := from.AddDate(0, 0, r.IntN(120)-10)
		comp := float64(r.IntN(101)) / 100
		status := []string{"open", "open", "open", "shipped"}[r.IntN(4)]
		var got string
		if err := e.db.QueryRow(context.Background(), `select order_risk($1, $2, $3, $4, $5)`, status, from, due, now, comp).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want := domain.RiskOf(status, from, due, now, comp); got != string(want) {
			t.Fatalf("%s %s..%s on %s at %.2f: SQL %s, Go %s", status, from.Format(time.DateOnly), due.Format(time.DateOnly), now.Format(time.DateOnly), comp, got, want)
		}
	}
}

func TestDeepLinksServeTheApp(t *testing.T) {
	e := newEnv(t, true)
	dir := t.TempDir()
	os.WriteFile(dir+"/index.html", []byte("<div id=root></div>"), 0o644)
	os.MkdirAll(dir+"/assets", 0o755)
	os.WriteFile(dir+"/assets/app.js", []byte("console.log(1)"), 0o644)
	e.srv.cfg.Static = os.DirFS(dir)
	ts := httptest.NewServer(e.srv.Handler())
	defer ts.Close()
	for path, want := range map[string]string{"/orders/12": "id=root", "/": "id=root", "/assets/app.js": "console"} {
		resp, _ := http.Get(ts.URL + path)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
		}
	}
	if resp, _ := http.Get(ts.URL + "/api/nope"); resp.StatusCode != 404 {
		t.Fatal("unknown API path served the app")
	}
}
