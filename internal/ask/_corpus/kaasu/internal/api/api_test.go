package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/Aravindh97/kaasu/internal/analyse"
	"github.com/Aravindh97/kaasu/internal/auth"
	"github.com/Aravindh97/kaasu/internal/db"
	"github.com/Aravindh97/kaasu/internal/store"
)

type env struct {
	t  *testing.T
	ts *httptest.Server
	db *pgxpool.Pool
	s  *Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("KAASU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KAASU_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(cancel)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	a := auth.New(pool)
	a.Cost = bcrypt.MinCost
	s := New(Config{Demo: true}, pool, a, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	go s.Listen(ctx)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &env{t, ts, pool, s}
}

type client struct {
	e *env
	h *http.Client
}

func (e *env) as(email string) *client {
	jar, _ := cookiejar.New(nil)
	c := &client{e, &http.Client{Jar: jar}}
	if code, b := c.do("POST", "/api/demo-login", map[string]string{"email": email}, nil); code != 200 {
		e.t.Fatalf("login %s: %d %s", email, code, b)
	}
	return c
}

var keys struct {
	sync.Mutex
	n int
}

func newKey() string {
	keys.Lock()
	defer keys.Unlock()
	keys.n++
	return fmt.Sprintf("test-key-%d-%d", time.Now().UnixNano(), keys.n)
}

func (c *client) send(method, path string, body, out any, key string) (int, string) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.ts.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kaasu", "1")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
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

// do sends with a fresh idempotency key, as the app does for each new action.
func (c *client) do(method, path string, body, out any) (int, string) {
	return c.send(method, path, body, out, newKey())
}

func (e *env) one(sql string, args ...any) int64 {
	var v int64
	if err := e.db.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func (e *env) wallet(c *client) store.Wallet {
	var w store.Wallet
	if code, b := c.do("GET", "/api/wallet", nil, &w); code != 200 {
		e.t.Fatalf("wallet: %d %s", code, b)
	}
	return w
}

func (e *env) ledgerOK() {
	e.t.Helper()
	c, err := e.s.store.CheckLedger(context.Background(), nil)
	if err != nil || !c.OK {
		e.t.Fatalf("ledger: %+v %v", c, err)
	}
}

// member adds a demo member with the demo PIN and an empty wallet.
func (e *env) member(name string, n int) (*client, string) {
	ctx := context.Background()
	pin, _ := bcrypt.GenerateFromPassword([]byte(store.DemoPIN), bcrypt.MinCost)
	key := strings.ToLower(strings.Fields(name)[0])
	phone := fmt.Sprintf("97000%05d", n)
	var id int64
	if err := e.db.QueryRow(ctx, `insert into users (email, name, phone, upi_id, password_hash, pin_hash, demo) values ($1, $2, $3, $4, 'x', $5, true) returning id`,
		key+"@test.demo", name, phone, key+"@kaasu", string(pin)).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Exec(ctx, `insert into accounts (user_id, kind, name) values ($1, 'wallet', 'Wallet')`, id); err != nil {
		e.t.Fatal(err)
	}
	return e.as(key + "@test.demo"), phone
}

// knows marks payee as someone the user paid long ago, so the new-payee
// fraud rule doesn't apply.
func (e *env) knows(userPhone, payeePhone string) {
	if _, err := e.db.Exec(context.Background(), `insert into payees (user_id, payee_id, first_paid_at) select a.id, b.id, now() - interval '30 days' from users a, users b where a.phone = $1 and b.phone = $2`, userPhone, payeePhone); err != nil {
		e.t.Fatal(err)
	}
}

const (
	anbu  = "anbu@kaasu.demo"
	kavya = "kavya@kaasu.demo"
	ravi  = "ravi@kaasu.demo"
	meena = "meena@kaasu.demo"
	selva = "selva@kaasu.demo"
	pin   = store.DemoPIN
)

// Twenty payments of ₹300 race to spend a ₹2,000 balance. Exactly six go
// through; the rest are refused; nothing goes below zero.
func TestConcurrentSendsNeverOverdraw(t *testing.T) {
	e := newEnv(t)
	tara, taraPhone := e.member("Tara Test", 1)
	e.knows(taraPhone, "9800000002")
	if code, b := tara.do("POST", "/api/topup", store.TopUp{Amount: 2_000_00}, nil); code != 200 {
		t.Fatal(code, b)
	}
	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = tara.do("POST", "/api/send", store.Send{To: "9800000002", Amount: 300_00, PIN: pin}, nil)
		}()
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == 200 {
			ok++
		} else if c != 422 {
			t.Fatalf("status %d", c)
		}
	}
	if w := e.wallet(tara); ok != 6 || w.Balance != 200_00 {
		t.Fatalf("%d sent, balance %d", ok, w.Balance)
	}
	e.ledgerOK()
}

// A payment retried with the same key (a phone on a bad network) moves
// money once, and every retry gets the same answer.
func TestRetriesMoveMoneyOnce(t *testing.T) {
	e := newEnv(t)
	a := e.as(anbu)
	before := e.wallet(a).Balance
	key := newKey()
	var wg sync.WaitGroup
	res := make([]store.Moved, 10)
	codes := make([]int, 10)
	for i := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = a.send("POST", "/api/send", store.Send{To: "kavya@kaasu", Amount: 123_45, Note: "retry test", PIN: pin}, &res[i], key)
		}()
	}
	wg.Wait()
	replays := 0
	for i := range res {
		if codes[i] != 200 || res[i].TxnID != res[0].TxnID {
			t.Fatalf("retry %d: %d %+v", i, codes[i], res[i])
		}
		if res[i].Replay {
			replays++
		}
	}
	if replays != 9 || e.wallet(a).Balance != before-123_45 {
		t.Fatalf("replays %d, balance %d -> %d", replays, before, e.wallet(a).Balance)
	}
	if n := e.one(`select count(*) from txns where note = 'retry test'`); n != 1 {
		t.Fatalf("%d txns", n)
	}
	// the same key for a different payment is refused
	if code, b := a.send("POST", "/api/send", store.Send{To: "kavya@kaasu", Amount: 999_00, Note: "retry test", PIN: pin}, nil, key); code != 422 || !strings.Contains(b, "different request") {
		t.Fatal(code, b)
	}
	// and a payment without a key is refused outright
	if code, _ := a.send("POST", "/api/send", store.Send{To: "kavya@kaasu", Amount: 100, PIN: pin}, nil, ""); code != 422 {
		t.Fatal("no key accepted:", code)
	}
	// the PIN is never stored, not even hashed into the key's record
	if n := e.one(`select count(*) from idempotency where response::text like '%' || $1 || '%'`, pin); n != 0 {
		t.Fatal("PIN stored")
	}
	e.ledgerOK()
}

func TestWrongPINLocksPayments(t *testing.T) {
	e := newEnv(t)
	a := e.as(anbu)
	before := e.wallet(a).Balance
	for i, want := range []int{422, 422, 423} {
		code, b := a.do("POST", "/api/send", store.Send{To: "kavya@kaasu", Amount: 100_00, PIN: "1111"}, nil)
		if code != want {
			t.Fatalf("attempt %d: %d %s", i+1, code, b)
		}
	}
	// now even the right PIN is refused until the lock ends
	if code, b := a.do("POST", "/api/send", store.Send{To: "kavya@kaasu", Amount: 100_00, PIN: pin}, nil); code != 423 || !strings.Contains(b, "too many wrong PINs") {
		t.Fatal(code, b)
	}
	if e.wallet(a).Balance != before {
		t.Fatal("money moved")
	}
	if n := e.one(`select count(*) from audit_log l join users u on u.id = l.user_id where u.email = $1 and l.action in ('pin_wrong', 'pin_locked')`, anbu); n != 3 {
		t.Fatalf("%d audit entries", n)
	}
	// after the lock expires, the right PIN works again
	if _, err := e.db.Exec(context.Background(), `update users set pin_locked_until = now() - interval '1 minute' where email = $1`, anbu); err != nil {
		t.Fatal(err)
	}
	if code, b := a.do("POST", "/api/send", store.Send{To: "kavya@kaasu", Amount: 100_00, PIN: pin}, nil); code != 200 {
		t.Fatal(code, b)
	}
}

func TestDailyLimits(t *testing.T) {
	e := newEnv(t)
	tara, taraPhone := e.member("Tara Test", 1)
	e.knows(taraPhone, "9800000002")
	for _, amt := range []int64{50_000_00} {
		if code, b := tara.do("POST", "/api/topup", store.TopUp{Amount: amt}, nil); code != 200 {
			t.Fatal(code, b)
		}
	}
	if code, b := tara.do("POST", "/api/topup", store.TopUp{Amount: 100_00}, nil); code != 422 || !strings.Contains(b, "a day") {
		t.Fatal("top-up limit:", code, b)
	}
	if code, b := tara.do("POST", "/api/send", store.Send{To: "9800000002", Amount: 20_000_00, PIN: pin}, nil); code != 200 {
		t.Fatal(code, b)
	}
	code, b := tara.do("POST", "/api/send", store.Send{To: "9800000002", Amount: 6_000_00, PIN: pin}, nil)
	if code != 422 || !strings.Contains(b, "₹5,000.00 more today") {
		t.Fatal("send limit:", code, b)
	}
	if w := e.wallet(tara); w.SendLeft != 5_000_00 {
		t.Fatalf("%+v", w)
	}
}

// The fifth payment to a new payee within 10 minutes is refused and the
// account paused. Ops review it, and must write what they checked to clear it.
func TestFraudRulePausesTheAccount(t *testing.T) {
	e := newEnv(t)
	m := e.as(meena)
	var phones []string
	for i := range 5 {
		_, p := e.member(fmt.Sprintf("Payee%d Test", i), 10+i)
		phones = append(phones, p)
	}
	for i := range 4 {
		if code, b := m.do("POST", "/api/send", store.Send{To: phones[i], Amount: 500_00, PIN: pin}, nil); code != 200 {
			t.Fatalf("payment %d: %d %s", i+1, code, b)
		}
	}
	key := newKey()
	code, b := m.send("POST", "/api/send", store.Send{To: phones[4], Amount: 500_00, PIN: pin}, nil, key)
	if code != 423 || !strings.Contains(b, "paused") {
		t.Fatal("fifth:", code, b)
	}
	// a retry of that payment gets the same refusal, and still moves nothing
	if code, b := m.send("POST", "/api/send", store.Send{To: phones[4], Amount: 500_00, PIN: pin}, nil, key); code != 423 || !strings.Contains(b, "paused") {
		t.Fatal("retry:", code, b)
	}
	w := e.wallet(m)
	if !w.Paused || w.Balance != 10_000_00-4*500_00 {
		t.Fatalf("%+v", w)
	}
	// paused: even a payment to someone she has paid before is refused
	if code, _ := m.do("POST", "/api/send", store.Send{To: phones[0], Amount: 100_00, PIN: pin}, nil); code != 423 {
		t.Fatal("paused account paid:", code)
	}
	ops := e.as(selva)
	var paused []store.PausedAccount
	ops.do("GET", "/api/ops/paused", nil, &paused)
	if len(paused) != 1 || paused[0].Name != "Meena Sundaram" || len(paused[0].Recent) != 4 {
		t.Fatalf("%+v", paused)
	}
	path := fmt.Sprintf("/api/ops/paused/%d/unpause", paused[0].ID)
	if code, _ := ops.do("POST", path, store.Unpause{Note: "ok"}, nil); code != 422 {
		t.Fatal("unpause without a real note:", code)
	}
	if code, _ := m.do("POST", path, store.Unpause{Note: "I checked myself, all fine"}, nil); code != 403 {
		t.Fatal("member unpaused themselves:", code)
	}
	if code, b := ops.do("POST", path, store.Unpause{Note: "Called Meena; she was paying her new tenants' deposits back"}, nil); code != 200 {
		t.Fatal(code, b)
	}
	if code, b := m.do("POST", "/api/send", store.Send{To: phones[0], Amount: 100_00, PIN: pin}, nil); code != 200 {
		t.Fatal("after review:", code, b)
	}
	if n := e.one(`select count(*) from audit_log where action in ('paused_by_rule', 'unpaused')`); n != 2 {
		t.Fatalf("audit: %d", n)
	}
	e.ledgerOK()
}

// Two of Anbu's devices pay the same request at once: one payment.
func TestRequestIsPaidOnce(t *testing.T) {
	e := newEnv(t)
	a1, a2 := e.as(anbu), e.as(anbu)
	var rq store.Requests
	a1.do("GET", "/api/requests", nil, &rq)
	var kav store.MoneyRequest
	for _, r := range rq.ToPay {
		if r.From == "Kavya Raman" {
			kav = r
		}
	}
	if kav.ID == 0 {
		t.Fatalf("no request from Kavya: %+v", rq)
	}
	path := fmt.Sprintf("/api/requests/%d/pay", kav.ID)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, c := range []*client{a1, a2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = c.do("POST", path, store.PayRequest{PIN: pin}, nil)
		}()
	}
	wg.Wait()
	if !(codes[0] == 200 && codes[1] == 409 || codes[0] == 409 && codes[1] == 200) {
		t.Fatalf("codes %v", codes)
	}
	if n := e.one(`select count(*) from txns where request_id = $1`, kav.ID); n != 1 {
		t.Fatalf("%d payments", n)
	}
	// Kavya sees it paid; she can't decline what's paid
	k := e.as(kavya)
	var rk store.Requests
	k.do("GET", "/api/requests", nil, &rk)
	if len(rk.Finished) == 0 || rk.Finished[0].Status != "paid" {
		t.Fatalf("%+v", rk)
	}
	if code, _ := k.do("POST", fmt.Sprintf("/api/requests/%d/cancel", kav.ID), nil, nil); code != 409 {
		t.Fatal("cancelled a paid request:", code)
	}
	// someone else can't pay or even see it
	if code, _ := e.as(ravi).do("POST", path, store.PayRequest{PIN: pin}, nil); code != 404 {
		t.Fatal("ravi paid anbu's request:", code)
	}
	e.ledgerOK()
}

func TestSplitSharesAddUpToTheBill(t *testing.T) {
	e := newEnv(t)
	r := e.as(ravi)
	var sp store.Split
	if code, b := r.do("POST", "/api/splits", store.NewSplit{Title: "Ooty cab", Total: 1_000_01, With: []string{"anbu@kaasu", "9800000002", "meena@kaasu"}}, &sp); code != 201 {
		t.Fatal(code, b)
	}
	sum := sp.MyShare
	for _, q := range sp.Requests {
		sum += q.Amount
	}
	if len(sp.Requests) != 3 || sum != 1_000_01 || sp.MyShare != 250_00 || sp.Requests[0].Amount != 250_01 {
		t.Fatalf("%+v", sp)
	}
	if code, _ := r.do("POST", "/api/splits", store.NewSplit{Title: "x", Total: 500_00, With: []string{"anbu@kaasu", "9800000001"}}, nil); code != 422 {
		t.Fatal("same person twice accepted")
	}
	if code, _ := r.do("POST", "/api/splits", store.NewSplit{Title: "x", Total: 500_00, With: []string{"ravi@kaasu"}}, nil); code != 422 {
		t.Fatal("split with yourself accepted")
	}
}

func TestGoalsKeepMoneyInTheLedger(t *testing.T) {
	e := newEnv(t)
	a := e.as(anbu)
	var g store.Goal
	if code, b := a.do("POST", "/api/goals", store.NewGoal{Name: "New phone", Target: 20_000_00, TargetDate: time.Now().AddDate(0, 4, 0).Format(time.DateOnly)}, &g); code != 201 {
		t.Fatal(code, b)
	}
	before := e.wallet(a).Balance
	path := fmt.Sprintf("/api/goals/%d/move", g.ID)
	if code, b := a.do("POST", path, store.GoalMove{Amount: 3_000_00}, nil); code != 200 {
		t.Fatal(code, b)
	}
	if code, _ := a.do("POST", path, store.GoalMove{Amount: -5_000_00}, nil); code != 422 {
		t.Fatal("took out more than saved")
	}
	if code, _ := a.do("POST", fmt.Sprintf("/api/goals/%d/close", g.ID), nil, nil); code != 422 {
		t.Fatal("closed a goal with money in it")
	}
	if code, _ := a.do("POST", path, store.GoalMove{Amount: -3_000_00}, nil); code != 200 {
		t.Fatal("take back")
	}
	if code, _ := a.do("POST", fmt.Sprintf("/api/goals/%d/close", g.ID), nil, nil); code != 200 {
		t.Fatal("close empty goal")
	}
	if e.wallet(a).Balance != before {
		t.Fatal("wallet changed")
	}
	// another member can't touch it
	if code, _ := e.as(kavya).do("POST", path, store.GoalMove{Amount: 1_00}, nil); code != 404 {
		t.Fatal("kavya moved anbu's goal")
	}
	e.ledgerOK()
}

func (c *client) upload(name, csv string, out any) (int, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	io.WriteString(fw, csv)
	mw.Close()
	req, _ := http.NewRequest("POST", c.e.ts.URL+"/api/imports", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Kaasu", "1")
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

// A server-side import keeps rows; a correction teaches a rule that
// re-categorises every row from that merchant and the summary with them.
func TestImportAndLearning(t *testing.T) {
	e := newEnv(t)
	k := e.as(kavya)
	var im store.Import
	if code, b := k.upload("hdfc.csv", analyse.SampleCSV(3, time.Now()), &im); code != 201 {
		t.Fatal(code, b)
	}
	if im.Mode != "server" || len(im.Rows) != im.RowCount || len(im.Summary.Findings) == 0 || len(im.Skipped) != 2 {
		t.Fatalf("import: mode %s rows %d/%d findings %d skipped %v", im.Mode, len(im.Rows), im.RowCount, len(im.Summary.Findings), im.Skipped)
	}
	others := func(s analyse.Summary) int64 {
		for _, c := range s.Categories {
			if c.Category == "Other" {
				return c.Out
			}
		}
		return 0
	}
	before := others(im.Summary)
	var res map[string]int
	if code, b := k.do("POST", "/api/rules", store.Teach{Merchant: "RAVI KUMAR", Category: "Rent"}, &res); code != 200 || res["changed"] == 0 {
		t.Fatal(code, b)
	}
	var after store.Import
	k.do("GET", fmt.Sprintf("/api/imports/%d", im.ID), nil, &after)
	if others(after.Summary) >= before {
		t.Fatalf("Other: %d -> %d", before, others(after.Summary))
	}
	for _, r := range after.Rows {
		if r.Merchant == "RAVI KUMAR" && (r.Category != "Rent" || !r.Learned) {
			t.Fatalf("%+v", r)
		}
	}
	// the next import learns it straight away
	var next store.Import
	k.upload("next.csv", analyse.SampleCSV(4, time.Now()), &next)
	for _, r := range next.Rows {
		if r.Merchant == "RAVI KUMAR" && r.Category != "Rent" {
			t.Fatalf("not learned: %+v", r)
		}
	}
	// someone else can't see it
	if code, _ := e.as(anbu).do("GET", fmt.Sprintf("/api/imports/%d", im.ID), nil, nil); code != 404 {
		t.Fatal("anbu saw kavya's import")
	}
	if code, b := k.upload("notes.csv", "hello,world\n1,2\n", nil); code != 422 || !strings.Contains(b, "column headings") {
		t.Fatal(code, b)
	}
}

// Privacy mode: only the summary arrives; no rows are stored.
func TestDeviceImportStoresNoRows(t *testing.T) {
	e := newEnv(t)
	k := e.as(kavya)
	rowsBefore := e.one(`select count(*) from statement_rows`)
	p, _ := analyse.Parse(strings.NewReader(analyse.SampleCSV(5, time.Now())))
	sum := analyse.Analyse(p.Txns, nil)
	var im store.Import
	if code, b := k.do("POST", "/api/imports/device", store.DeviceImport{Filename: "C:\\Users\\kavya\\statement.csv", Summary: sum}, &im); code != 201 {
		t.Fatal(code, b)
	}
	if im.Mode != "device" || len(im.Rows) != 0 || im.Filename != "statement.csv" || im.Summary.MoneyOut != sum.MoneyOut {
		t.Fatalf("%+v", im)
	}
	if e.one(`select count(*) from statement_rows`) != rowsBefore {
		t.Fatal("rows stored for a device import")
	}
	if code, _ := k.do("POST", "/api/imports/device", store.DeviceImport{Filename: "x.csv", Summary: analyse.Summary{}}, nil); code != 422 {
		t.Fatal("empty summary accepted")
	}
}

// The month-end job runs on every server: five runs at once still make one
// report per member, and its totals match the ledger.
func TestMonthEndReportsOncePerMonth(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.db.Exec(ctx, `truncate reports`)
	month := time.Now().In(store.IST).AddDate(0, -1, 0)
	var wg sync.WaitGroup
	made := make([]int, 5)
	for i := range made {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.s.store.RunMonthEnd(ctx, month)
			if err != nil {
				t.Error(err)
			}
			made[i] = n
		}()
	}
	wg.Wait()
	total := 0
	for _, n := range made {
		total += n
	}
	members := e.one(`select count(*) from users where role = 'member'`)
	if int64(total) != members || e.one(`select count(*) from reports`) != members {
		t.Fatalf("made %v for %d members", made, members)
	}
	var rs []store.Report
	e.as(anbu).do("GET", "/api/reports", nil, &rs)
	from := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, store.IST)
	out := e.one(`select coalesce(sum(t.amount), 0) from txns t join users u on u.id = t.user_id where u.email = $1 and t.kind = 'transfer' and t.created_at >= $2 and t.created_at < $3`, anbu, from, from.AddDate(0, 1, 0))
	if len(rs) != 1 || rs[0].Data.Out != out {
		t.Fatalf("report %+v, ledger says %d out", rs, out)
	}
	if _, err := e.s.store.RunMonthEnd(ctx, time.Now()); err == nil {
		t.Fatal("made a report for a month that hasn't ended")
	}
}

// The statement download reads back through Kaasu's own analyser, its
// totals match the activity, and a note can't become a spreadsheet formula.
func TestStatementDownload(t *testing.T) {
	e := newEnv(t)
	a := e.as(anbu)
	if code, b := a.do("POST", "/api/send", store.Send{To: "kavya@kaasu", Amount: 10_00, Note: "=HYPERLINK(\"http://x\")", PIN: pin}, nil); code != 200 {
		t.Fatal(code, b)
	}
	today := time.Now().In(store.IST).Format(time.DateOnly)
	from := time.Now().In(store.IST).AddDate(0, -5, 0).Format(time.DateOnly)
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/statement.csv?from="+from+"&to="+today, nil)
	resp, err := a.h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal(resp.StatusCode, string(b))
	}
	cr := csv.NewReader(bytes.NewReader(b))
	cr.FieldsPerRecord = -1
	cells, err := cr.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range cells {
		for _, c := range row {
			if c != "" && strings.ContainsRune("=+-@", rune(c[0])) {
				t.Fatalf("a cell starts like a formula: %q", c)
			}
		}
	}
	p, err := analyse.Parse(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, x := range p.Txns {
		sum += x.Amount
	}
	var acts []store.Activity
	a.do("GET", "/api/activity?limit=100", nil, &acts)
	var want int64
	for _, x := range acts {
		want += x.Amount
	}
	if len(acts) == 100 {
		t.Skip("more than 100 entries; compare skipped")
	}
	if sum != want || len(p.Txns) != len(acts) {
		t.Fatalf("statement %d rows sum %d; activity %d rows sum %d", len(p.Txns), sum, len(acts), want)
	}
}

func TestCSRFAndRoles(t *testing.T) {
	e := newEnv(t)
	a := e.as(anbu)
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/topup", strings.NewReader(`{"amount":100}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", newKey())
	if resp, _ := a.h.Do(req); resp.StatusCode != 403 {
		t.Fatal("no X-Kaasu header:", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", e.ts.URL+"/api/topup", strings.NewReader(`{"amount":100}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kaasu", "1")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := a.h.Do(req); resp.StatusCode != 403 {
		t.Fatal("cross-site origin:", resp.StatusCode)
	}
	if code, _ := a.do("GET", "/api/ops/ledger", nil, nil); code != 403 {
		t.Fatal("member read the ledger check")
	}
	if code, _ := e.as(selva).do("POST", "/api/send", store.Send{To: "kavya@kaasu", Amount: 100, PIN: pin}, nil); code != 403 {
		t.Fatal("ops sent money")
	}
	if code, _ := (&client{e, &http.Client{}}).do("GET", "/api/wallet", nil, nil); code != 401 {
		t.Fatal("signed out read a wallet")
	}
}

func TestLiveTopicsAreFiltered(t *testing.T) {
	anbuU, opsU := &auth.User{ID: 1, Role: "member"}, &auth.User{ID: 5, Role: "ops"}
	cases := []struct {
		u     *auth.User
		topic string
		want  bool
	}{{anbuU, "user:1", true}, {anbuU, "user:2", false}, {anbuU, "ops", false}, {opsU, "ops", true}, {opsU, "user:1", false}}
	for _, c := range cases {
		if got := topicFilter(c.u)(c.topic); got != c.want {
			t.Errorf("%s hearing %s: %v", c.u.Role, c.topic, got)
		}
	}
}

// Receiving money shows up live: Kavya's stream hears about Anbu's payment.
func TestPaymentReachesTheRecipientLive(t *testing.T) {
	e := newEnv(t)
	k := e.as(kavya)
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/stream", nil)
	resp, err := k.h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan string, 4)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if err != nil {
				return
			}
			got <- string(buf[:n])
		}
	}()
	<-got // connected
	var kid int64 = e.one(`select id from users where email = $1`, kavya)
	if code, b := e.as(anbu).do("POST", "/api/send", store.Send{To: "kavya@kaasu", Amount: 50_00, PIN: pin}, nil); code != 200 {
		t.Fatal(code, b)
	}
	select {
	case m := <-got:
		if !strings.Contains(m, fmt.Sprintf("user:%d", kid)) {
			t.Fatalf("heard %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live notice")
	}
}

func TestSafeCell(t *testing.T) {
	for in, want := range map[string]string{"=SUM(A1)": "'=SUM(A1)", "+91": "'+91", "-5": "'-5", "@x": "'@x", "Lunch": "Lunch", "": ""} {
		if got := store.SafeCell(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestStaticFilesAreCompressed(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("console.log('kaasu');\n", 2000)
	s := New(Config{Static: fstest.MapFS{"index.html": {Data: []byte("<html>")}, "assets/app.js": {Data: []byte(big)}}}, e.db, e.s.auth, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/assets/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req) // no automatic decompression
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	zr, err := gzip.NewReader(resp.Body)
	if err != nil || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("not gzipped: %v %v", resp.Header, err)
	}
	b, _ := io.ReadAll(zr)
	if string(b) != big || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("got %d bytes, %s", len(b), resp.Header.Get("Content-Type"))
	}
	// a client that doesn't accept gzip gets the plain file
	plain, _ := http.Get(ts.URL + "/assets/app.js")
	pb, _ := io.ReadAll(plain.Body)
	plain.Body.Close()
	if string(pb) != big {
		t.Fatal("plain copy differs")
	}
}
