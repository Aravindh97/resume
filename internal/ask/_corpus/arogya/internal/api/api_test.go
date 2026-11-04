package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

	"github.com/Aravindh97/arogya/internal/auth"
	"github.com/Aravindh97/arogya/internal/db"
	"github.com/Aravindh97/arogya/internal/domain"
	"github.com/Aravindh97/arogya/internal/seed"
	"github.com/Aravindh97/arogya/internal/store"
)

type env struct {
	t  *testing.T
	ts *httptest.Server
	db *pgxpool.Pool
	s  *Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("AROGYA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AROGYA_TEST_DATABASE_URL not set")
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
	s := New(Config{Demo: true}, pool, a, slog.New(slog.NewTextHandler(io.Discard, nil)), &seed.Clock{})
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

func (c *client) do(method, path string, body, out any) (int, string) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.ts.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Arogya", "1")
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

func (e *env) one(sql string, args ...any) int64 {
	var v int64
	if err := e.db.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

const (
	admin   = "anand@arogya.demo"
	recep   = "malar@arogya.demo"
	priya   = "priya@arogya.demo"
	senthil = "senthil@arogya.demo"
	kavitha = "kavitha@arogya.demo"
	suresh  = "suresh@arogya.demo"
	meena   = "meena@arogya.demo"
)

// Twenty receptionists' screens book the same 12:45 slot at once; exactly
// one booking is made, the rest are told it was taken.
func TestLastSlotGoesToExactlyOne(t *testing.T) {
	e := newEnv(t)
	doctor := e.one(`select id from users where email = $1`, priya)
	day := e.s.store.Now().Format(time.DateOnly)
	e.db.Exec(context.Background(), `delete from visits where doctor_id = $1 and day = $2 and slot = '12:45'`, doctor, day)
	var ok, taken atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := e.as(recep).do("POST", "/api/visits", map[string]any{"patient_id": 20 + i, "doctor_id": doctor, "day": day, "slot": "12:45"}, nil)
			switch code {
			case 201:
				ok.Add(1)
			case 409:
				taken.Add(1)
			default:
				t.Errorf("status %d", code)
			}
		}(i)
	}
	wg.Wait()
	n := e.one(`select count(*) from visits where doctor_id = $1 and day = $2 and slot = '12:45' and status <> 'cancelled'`, doctor, day)
	if ok.Load() != 1 || taken.Load() != 19 || n != 1 {
		t.Fatalf("booked %d, refused %d, rows %d", ok.Load(), taken.Load(), n)
	}
	// tokens stayed unique under the same race
	if dup := e.one(`select count(*) from (select token from visits where doctor_id = $1 and day = $2 group by token having count(*) > 1) x`, doctor, day); dup != 0 {
		t.Fatal("duplicate tokens")
	}
}

func TestWalkinWaitingLongIsCalledFirst(t *testing.T) {
	e := newEnv(t)
	doctor := e.one(`select id from users where email = $1`, priya)
	day := e.s.store.Now()
	// one walk-in has waited 50 minutes; booked patients are checked in and due
	e.db.Exec(context.Background(), `update visits set arrived_at = $3 where doctor_id = $1 and day = $2::date and kind = 'walkin' and token = (select min(token) from visits where doctor_id = $1 and day = $2::date and kind = 'walkin' and status = 'waiting')`,
		doctor, day.Format(time.DateOnly), day.Add(-50*time.Minute))
	var r struct {
		Called *store.Visit `json:"called"`
	}
	e.as(priya).do("POST", "/api/queue/next", nil, &r)
	if r.Called == nil || r.Called.Kind != "walkin" || !strings.Contains(r.Called.Why, "waiting 50 min") {
		t.Fatalf("called %+v", r.Called)
	}
}

func TestBoardShowsTokensNotNames(t *testing.T) {
	e := newEnv(t)
	resp, _ := http.Get(e.ts.URL + "/api/board")
	b, _ := io.ReadAll(resp.Body)
	var names []string
	rows, _ := e.db.Query(context.Background(), `select name from patients`)
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		if strings.Contains(string(b), n) {
			t.Fatalf("board shows %q", n)
		}
	}
	if !strings.Contains(string(b), "now_serving") {
		t.Fatal(string(b))
	}
}

func TestRecordAccessAndBreakGlass(t *testing.T) {
	e := newEnv(t)
	meenaID := e.one(`select patient_id from users where email = $1`, meena)
	path := fmt.Sprintf("/api/patients/%d/open", meenaID)
	// Dr. Priya sees Meena today: care team
	var c store.Chart
	if code, b := e.as(priya).do("POST", path, map[string]string{}, &c); code != 200 || c.Access != "care_team" || !c.CanWrite {
		t.Fatalf("priya: %d %s", code, b)
	}
	// Dr. Senthil has no relationship: refused, then allowed with a reason
	s := e.as(senthil)
	if code, b := s.do("POST", path, map[string]string{}, nil); code != 403 || !strings.Contains(b, "break_glass") {
		t.Fatalf("senthil without reason: %d %s", code, b)
	}
	if code, _ := s.do("POST", path, map[string]string{"break_glass": "Collapsed in the corridor, checking for allergies"}, &c); code != 200 || c.Access != "break_glass" || c.CanWrite {
		t.Fatalf("break glass: %d %+v", code, c.Access)
	}
	// he still cannot write to her record
	if code, _ := s.do("POST", fmt.Sprintf("/api/patients/%d/encounters", meenaID), map[string]any{"diagnosis": "x"}, nil); code != 403 {
		t.Fatalf("break-glass write: %d", code)
	}
	// receptionists and blood bank never open records, even with a reason
	for _, who := range []string{recep, suresh} {
		if code, _ := e.as(who).do("POST", path, map[string]string{"break_glass": "just checking something here"}, nil); code != 403 {
			t.Fatalf("%s opened a record", who)
		}
	}
	// Meena sees exactly who looked, including the emergency access
	var log []store.AccessEntry
	m := e.as(meena)
	m.do("GET", "/api/my/access", nil, &log)
	var glass *store.AccessEntry
	for i := range log {
		if log[i].Reason == "break_glass" && log[i].User == "Dr. Senthil Kumar" && strings.Contains(log[i].Detail, "corridor") {
			glass = &log[i]
		}
	}
	if glass == nil {
		t.Fatalf("Meena's log lacks the emergency access: %+v", log[:min(3, len(log))])
	}
	// she cannot open anyone else's record
	if code, _ := m.do("POST", "/api/patients/2/open", map[string]string{}, nil); code != 403 {
		t.Fatal("patient opened another patient's record")
	}
	// the admin reviews it once; the log itself can't be changed
	a := e.as(admin)
	if code, b := a.do("POST", fmt.Sprintf("/api/access/%d/review", glass.ID), map[string]string{"outcome": "justified", "note": "Confirmed with casualty"}, nil); code != 200 {
		t.Fatalf("review: %d %s", code, b)
	}
	if code, _ := a.do("POST", fmt.Sprintf("/api/access/%d/review", glass.ID), map[string]string{"outcome": "not_justified"}, nil); code != 409 {
		t.Fatal("reviewed twice")
	}
	if _, err := e.db.Exec(context.Background(), `update access_log set reason = 'care_team' where id = $1`, glass.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("access log changed: %v", err)
	}
	if _, err := e.db.Exec(context.Background(), `delete from access_log where id = $1`, glass.ID); err == nil {
		t.Fatal("access log row deleted")
	}
}

func TestNursesSeeTheirWardOnly(t *testing.T) {
	e := newEnv(t)
	inA := e.one(`select a.patient_id from admissions a join wards w on w.id = a.ward_id where w.name like 'Ward A%' and a.bed_id is not null and a.discharged_at is null limit 1`)
	inICU := e.one(`select a.patient_id from admissions a join wards w on w.id = a.ward_id where w.name = 'ICU' and a.discharged_at is null limit 1`)
	k := e.as(kavitha)
	if code, _ := k.do("POST", fmt.Sprintf("/api/patients/%d/open", inA), map[string]string{}, nil); code != 200 {
		t.Fatal("ward A nurse refused a ward A patient")
	}
	if code, _ := k.do("POST", fmt.Sprintf("/api/patients/%d/open", inICU), map[string]string{}, nil); code != 403 {
		t.Fatal("ward A nurse opened an ICU patient's record without a reason")
	}
	// on the bed board, other wards' patient names are hidden from her
	var wards []store.Ward
	k.do("GET", "/api/wards", nil, &wards)
	for _, w := range wards {
		for _, b := range w.Beds {
			if !strings.HasPrefix(w.Name, "Ward A") && b.Patient != "" {
				t.Fatalf("%s bed %s shows %s", w.Name, b.Label, b.Patient)
			}
		}
	}
}

// Two nurses give the last free bed to two different patients at once.
func TestLastBedGoesToExactlyOne(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ward := e.one(`select id from wards where name like 'Ward A%'`)
	// leave exactly one free bed in ward A
	free := e.one(`select count(*) from beds b where b.ward_id = $1 and not exists (select 1 from admissions a where a.bed_id = b.id and a.discharged_at is null)`, ward)
	if free < 1 {
		t.Skip("no free bed")
	}
	rows, _ := e.db.Query(ctx, `select id from admissions where ward_id = $1 and bed_id is null and discharged_at is null order by id`, ward)
	var waiting []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		waiting = append(waiting, id)
	}
	rows.Close()
	bed := e.one(`select b.id from beds b where b.ward_id = $1 and not exists (select 1 from admissions a where a.bed_id = b.id and a.discharged_at is null) order by b.id limit 1`, ward)
	var codes [2]int
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = e.as(kavitha).do("POST", fmt.Sprintf("/api/admissions/%d/bed", waiting[i]), map[string]int64{"bed_id": bed}, nil)
		}(i)
	}
	wg.Wait()
	if !(codes[0] == 200 && codes[1] == 409 || codes[0] == 409 && codes[1] == 200) {
		t.Fatalf("codes %v", codes)
	}
	if n := e.one(`select count(*) from admissions where bed_id = $1 and discharged_at is null`, bed); n != 1 {
		t.Fatalf("%d patients in one bed", n)
	}
}

// Ten urgent requests compete for scarce O- red cells at the same moment.
// No unit is ever reserved twice, and every reserved unit is compatible.
func TestBloodIsNeverDoubleAllocated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	doctor := e.one(`select id from users where email = $1`, priya)
	var reqs []int64
	// three O- patients, whatever the seed produced
	if _, err := e.db.Exec(ctx, `update patients set blood_group = 'O-' where id in (select id from patients order by id offset 1 limit 3)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		p := e.one(`select id from patients where blood_group = 'O-' order by id offset $1 limit 1`, i%3)
		reqs = append(reqs, e.one(`insert into blood_requests (patient_id, doctor_id, blood_group, component, units, urgency) values ($1, $2, 'O-', 'rbc', 2, 'emergency') returning id`, p, doctor))
	}
	stock := e.one(`select count(*) from blood_units where status = 'available' and component = 'rbc' and blood_group = 'O-' and expires_on > $1`, e.s.store.Now())
	var wg sync.WaitGroup
	for _, id := range reqs {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if code, b := e.as(suresh).do("POST", fmt.Sprintf("/api/blood-requests/%d/reserve", id), nil, nil); code != 200 {
				t.Errorf("reserve: %d %s", code, b)
			}
		}(id)
	}
	wg.Wait()
	reserved := e.one(`select count(*) from blood_units where request_id = any($1)`, reqs)
	wrong := e.one(`select count(*) from blood_units where request_id = any($1) and (blood_group <> 'O-' or component <> 'rbc' or status <> 'reserved')`, reqs)
	if reserved != min(stock, 20) || wrong != 0 {
		t.Fatalf("stock %d, reserved %d, wrong %d", stock, reserved, wrong)
	}
	var partial int64
	e.db.QueryRow(ctx, `select count(*) from blood_requests where id = any($1) and status = 'partial'`, reqs).Scan(&partial)
	if stock < 20 && partial == 0 {
		t.Fatal("shortage not reported as partial")
	}
}

// Meena is O-, female, 34: Rh-positive platelets are held back unless it
// is an emergency, and the oldest compatible unit goes first.
func TestBloodRulesThroughTheAPI(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	meenaID := e.one(`select patient_id from users where email = $1`, meena)
	e.db.Exec(ctx, `update blood_units set status = 'discarded' where component = 'platelets'`) // only the units below
	today := e.s.store.Now()
	e.db.Exec(ctx, `insert into blood_units (unit_no, blood_group, component, collected_on, expires_on) values ('T-POS', 'O+', 'platelets', $1, $2), ('T-NEG-LATE', 'O-', 'platelets', $1, $3), ('T-NEG-SOON', 'O-', 'platelets', $1, $2)`,
		today.AddDate(0, 0, -2), today.AddDate(0, 0, 2), today.AddDate(0, 0, 4))
	p := e.as(priya)
	var r store.BloodRequest
	if code, b := p.do("POST", "/api/blood-requests", map[string]any{"patient_id": meenaID, "component": "platelets", "units": 3, "urgency": "urgent"}, &r); code != 201 || r.Group != "O-" {
		t.Fatalf("request: %d %s", code, b)
	}
	s := e.as(suresh)
	s.do("POST", fmt.Sprintf("/api/blood-requests/%d/reserve", r.ID), nil, &r)
	if r.Status != "partial" || len(r.Reserved) != 2 || r.Reserved[0] != "T-NEG-SOON" {
		t.Fatalf("urgent: %s %v", r.Status, r.Reserved)
	}
	if code, _ := s.do("POST", fmt.Sprintf("/api/blood-requests/%d/issue", r.ID), nil, nil); code != 409 {
		t.Fatal("issued a partial request")
	}
	// doctors can't order for patients they don't treat
	if code, _ := e.as(senthil).do("POST", "/api/blood-requests", map[string]any{"patient_id": meenaID, "component": "rbc", "units": 1, "urgency": "routine"}, nil); code != 403 {
		t.Fatal("blood ordered by a doctor without a care relationship")
	}
	// donors offered are compatible and past their donation interval
	var donors []store.DonorMatch
	s.do("GET", fmt.Sprintf("/api/blood-requests/%d/donors", r.ID), nil, &donors)
	// (O recipients can take plasma and platelets from any ABO group)
	if len(donors) == 0 {
		t.Fatal("no donors offered")
	}
	for _, d := range donors {
		if !domain.Compatible("platelets", "O-", d.Group) {
			t.Fatalf("incompatible donor %s for O- platelets", d.Group)
		}
	}
	var lastDonated int64
	e.db.QueryRow(ctx, `select count(*) from donors where id = any($1) and last_donated > $2::date - 90`, func() []int64 {
		ids := []int64{}
		for _, d := range donors {
			ids = append(ids, d.ID)
		}
		return ids
	}(), today).Scan(&lastDonated)
	if lastDonated != 0 {
		t.Fatal("offered a donor who gave blood less than 90 days ago")
	}
	if strings.Contains(fmt.Sprint(donors), "phone") {
		t.Fatal("donor phone numbers exposed")
	}
}

func TestFHIRExport(t *testing.T) {
	e := newEnv(t)
	meenaID := e.one(`select patient_id from users where email = $1`, meena)
	var b struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			Resource map[string]any `json:"resource"`
		} `json:"entry"`
	}
	if code, body := e.as(priya).do("GET", fmt.Sprintf("/api/patients/%d/fhir", meenaID), nil, &b); code != 200 || b.ResourceType != "Bundle" {
		t.Fatalf("%d %.200s", code, body)
	}
	if b.Entry[0].Resource["resourceType"] != "Patient" || len(b.Entry) < 2 {
		t.Fatalf("entries: %d", len(b.Entry))
	}
	obs := b.Entry[1].Resource
	coding := obs["code"].(map[string]any)["coding"].([]any)[0].(map[string]any)
	if obs["resourceType"] != "Observation" || coding["system"] != "http://loinc.org" || coding["code"] != "718-7" {
		t.Fatalf("observation: %v", obs)
	}
	if n := e.one(`select count(*) from access_log where patient_id = $1 and action = 'fhir_export'`, meenaID); n != 1 {
		t.Fatal("export not logged")
	}
}

func TestCSRFAndRoles(t *testing.T) {
	e := newEnv(t)
	c := e.as(recep)
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/patients", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, _ := c.h.Do(req); resp.StatusCode != 403 {
		t.Fatalf("no X-Arogya header: %d", resp.StatusCode)
	}
	if code, _ := e.as(meena).do("GET", "/api/reception", nil, nil); code != 403 {
		t.Fatal("patient read the reception list")
	}
	if code, _ := e.as(recep).do("GET", "/api/access", nil, nil); code != 403 {
		t.Fatal("receptionist read the access log")
	}
	resp, _ := http.Get(e.ts.URL + "/api/wards")
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous bed board: %d", resp.StatusCode)
	}
}

func TestLiveTopicsAreFiltered(t *testing.T) {
	cases := []struct {
		role, topic string
		want        bool
	}{
		{"receptionist", "blood", false}, {"bloodbank", "blood", true}, {"nurse", "beds", true}, {"patient", "beds", false},
		{"admin", "glass", true}, {"doctor", "glass", false}, {"receptionist", "opd:3", true},
	}
	for _, c := range cases {
		if got := topicFilter(&auth.User{Role: c.role})(c.topic); got != c.want {
			t.Errorf("%s hears %s: %v", c.role, c.topic, got)
		}
	}
	pid := int64(5)
	me := &auth.User{Role: "patient", PatientID: &pid}
	if !topicFilter(me)("patient:5") || topicFilter(me)("patient:6") {
		t.Error("patient topics")
	}
}

// Receptionists registering patients at the same moment each get a distinct
// MRN. The load test caught the first version, which counted rows and failed
// about half the time under contention.
func TestConcurrentRegistrationsGetDistinctMRNs(t *testing.T) {
	e := newEnv(t)
	desk := e.as(recep)
	const n = 30
	mrns := make([]string, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var p struct{ MRN string }
			codes[i], _ = desk.do("POST", "/api/patients", map[string]string{"name": fmt.Sprintf("Rush Patient %d", i), "birth_date": "1990-02-03", "sex": "female", "phone": "9876543210"}, &p)
			mrns[i] = p.MRN
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range n {
		if codes[i] != 201 || mrns[i] == "" || seen[mrns[i]] {
			t.Fatalf("registration %d: status %d, mrn %q (seen before: %v)", i, codes[i], mrns[i], seen[mrns[i]])
		}
		seen[mrns[i]] = true
	}
}
