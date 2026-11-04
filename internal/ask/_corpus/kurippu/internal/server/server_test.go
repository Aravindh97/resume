package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/kurippu/internal/crdt"
)

func setup(t *testing.T) *httptest.Server {
	t.Helper()
	url := os.Getenv("KURIPPU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KURIPPU_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	t.Cleanup(cancel)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SpacesPerMinute, cfg.SyncsPerSecond = 1000, 100000
	s := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go s.Listen(ctx)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, ts *httptest.Server, path, space string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if space != "" {
		req.Header.Set("X-Space", space)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func newSpace(t *testing.T, ts *httptest.Server) string {
	var r struct{ Space string }
	if code := post(t, ts, "/api/spaces", "", map[string]any{}, &r); code != 201 {
		t.Fatal(code)
	}
	return r.Space
}

// device is a client: a CRDT replica, its unsent ops and its sync cursor.
type device struct {
	doc     *crdt.Doc
	pending []crdt.Op
	since   int64
}

func (d *device) sync(t *testing.T, ts *httptest.Server, space string) {
	t.Helper()
	for {
		var r SyncResponse
		if code := post(t, ts, "/api/sync", space, SyncRequest{Since: d.since, Ops: d.pending}, &r); code != 200 {
			t.Fatalf("sync: %d", code)
		}
		d.pending = nil
		for _, x := range r.Ops {
			if _, err := d.doc.Apply(x.Op); err != nil {
				t.Fatal(err)
			}
		}
		d.since = r.Latest
		if !r.More {
			return
		}
	}
}

// Three devices edit while offline at different times; syncing through the
// server in any order brings them to the same notes.
func TestOfflineDevicesConverge(t *testing.T) {
	ts := setup(t)
	space := newSpace(t, ts)
	devs := []*device{{doc: crdt.New("phone")}, {doc: crdt.New("laptop")}, {doc: crdt.New("tablet")}}
	c, _ := devs[0].doc.CreateNote("n1")
	devs[0].pending = append(devs[0].pending, c)
	e, _ := devs[0].doc.Edit("n1", "Buyer visit on Friday")
	devs[0].pending = append(devs[0].pending, e...)
	devs[0].sync(t, ts, space)
	for _, d := range devs[1:] {
		d.sync(t, ts, space)
	}
	r := rand.New(rand.NewPCG(1, 1))
	words := []string{" at 10", " bring samples", " with Arun", " (confirmed)", "!"}
	typed := len([]rune("Buyer visit on Friday"))
	for round := 0; round < 30; round++ {
		d := devs[r.IntN(3)]
		old := d.doc.Text("n1")
		p := r.IntN(len([]rune(old)) + 1)
		w := words[r.IntN(len(words))]
		typed += len([]rune(w))
		ops, err := d.doc.Edit("n1", string([]rune(old)[:p])+w+string([]rune(old)[p:]))
		if err != nil {
			t.Fatal(err)
		}
		d.pending = append(d.pending, ops...)
		if r.IntN(3) == 0 { // sometimes a device comes online mid-way
			devs[r.IntN(3)].sync(t, ts, space)
		}
	}
	for k := 0; k < 2; k++ {
		for _, d := range devs {
			d.sync(t, ts, space)
		}
	}
	want := devs[0].doc.Fingerprint()
	for i, d := range devs {
		if d.doc.Fingerprint() != want {
			t.Fatalf("device %d differs:\n%s\nvs\n%s", i, d.doc.Fingerprint(), want)
		}
	}
	// nothing typed on any device was lost or duplicated
	if got := len([]rune(devs[0].doc.Text("n1"))); got != typed {
		t.Fatalf("%d characters, %d typed", got, typed)
	}
}

// A retried request (the response was lost) stores nothing twice.
func TestRetriesAreIdempotent(t *testing.T) {
	ts := setup(t)
	space := newSpace(t, ts)
	d := crdt.New("a")
	c, _ := d.CreateNote("n")
	ops, _ := d.Edit("n", "hello")
	batch := append([]crdt.Op{c}, ops...)
	var r1, r2 SyncResponse
	post(t, ts, "/api/sync", space, SyncRequest{Ops: batch}, &r1)
	post(t, ts, "/api/sync", space, SyncRequest{Ops: batch}, &r2)
	if r1.Accepted != 2 || r2.Accepted != 0 || len(r2.Ops) != 2 {
		t.Fatalf("first %d, retry %d accepted, %d ops", r1.Accepted, r2.Accepted, len(r2.Ops))
	}
}

// Many writers at once: every reader that follows its cursor sees every op
// exactly once, with no gaps, even though transactions commit in any order.
func TestConcurrentWritersNeverHideOps(t *testing.T) {
	ts := setup(t)
	space := newSpace(t, ts)
	root := crdt.New("root")
	c, _ := root.CreateNote("n")
	post(t, ts, "/api/sync", space, SyncRequest{Ops: []crdt.Op{c}}, nil)
	var wg sync.WaitGroup
	writers, perWriter := 20, 15
	stop := make(chan struct{})
	seen := make(chan int, 1)
	go func() { // a reader polling with its cursor while writes happen
		since, got := int64(0), map[int64]bool{}
		for {
			var r SyncResponse
			post(t, ts, "/api/sync", space, SyncRequest{Since: since}, &r)
			for _, x := range r.Ops {
				if got[x.N] {
					t.Errorf("op %d seen twice", x.N)
				}
				got[x.N] = true
			}
			since = r.Latest
			select {
			case <-stop:
				if r.More {
					continue
				}
				seen <- len(got)
				return
			default:
			}
		}
	}()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			d := crdt.New(fmt.Sprintf("w%d", w))
			d.Apply(c)
			for i := 0; i < perWriter; i++ {
				ops, _ := d.Edit("n", d.Text("n")+"x")
				post(t, ts, "/api/sync", space, SyncRequest{Ops: ops}, nil)
			}
		}(w)
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	if n := <-seen; n != 1+writers*perWriter {
		t.Fatalf("reader saw %d of %d ops", n, 1+writers*perWriter)
	}
}

func TestSpacesAreIsolatedAndValidated(t *testing.T) {
	ts := setup(t)
	a, b := newSpace(t, ts), newSpace(t, ts)
	d := crdt.New("a")
	c, _ := d.CreateNote("secret-note")
	post(t, ts, "/api/sync", a, SyncRequest{Ops: []crdt.Op{c}}, nil)
	var r SyncResponse
	post(t, ts, "/api/sync", b, SyncRequest{}, &r)
	if len(r.Ops) != 0 {
		t.Fatal("another notebook's ops leaked")
	}
	if code := post(t, ts, "/api/sync", "AAAAAAAAAAAAAAAAAAAAAA", SyncRequest{}, nil); code != 404 {
		t.Fatalf("unknown notebook: %d", code)
	}
	if code := post(t, ts, "/api/sync", a, SyncRequest{Ops: []crdt.Op{{Replica: "x", Seq: 1, ID: crdt.ID{L: 1, R: "y"}, Note: "n", Kind: "create"}}}, nil); code != 422 {
		t.Fatalf("forged op id: %d", code)
	}
	if code := post(t, ts, "/api/sync", a, SyncRequest{Ops: make([]crdt.Op, MaxOpsPerSync+1)}, nil); code != 413 {
		t.Fatalf("too many ops: %d", code)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/sync", strings.NewReader("since=0"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Space", a)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 415 {
		t.Fatalf("form post: %d", resp.StatusCode)
	}
}

func TestLiveNotices(t *testing.T) {
	ts := setup(t)
	a, b := newSpace(t, ts), newSpace(t, ts)
	listen := func(space string) <-chan string {
		req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
		req.Header.Set("X-Space", space)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("events: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		ch := make(chan string, 4)
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
	ca, cb := listen(a), listen(b)
	time.Sleep(150 * time.Millisecond)
	d := crdt.New("x")
	c, _ := d.CreateNote("n")
	post(t, ts, "/api/sync", a, SyncRequest{Ops: []crdt.Op{c}}, nil)
	select {
	case m := <-ca:
		if m != "data: 1" {
			t.Fatal(m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no notice")
	}
	select {
	case m := <-cb:
		t.Fatalf("other notebook notified: %s", m)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestPaging(t *testing.T) {
	ts := setup(t)
	space := newSpace(t, ts)
	d := crdt.New("a")
	c, _ := d.CreateNote("n")
	all := []crdt.Op{c}
	for len(all) < PageSize+300 {
		ops, _ := d.Edit("n", d.Text("n")+"y")
		all = append(all, ops...)
	}
	for i := 0; i < len(all); i += MaxOpsPerSync {
		post(t, ts, "/api/sync", space, SyncRequest{Ops: all[i:min(len(all), i+MaxOpsPerSync)]}, nil)
	}
	fresh := &device{doc: crdt.New("new")}
	fresh.sync(t, ts, space)
	if fresh.doc.Text("n") != d.Text("n") {
		t.Fatalf("paged sync: %d of %d chars", len(fresh.doc.Text("n")), len(d.Text("n")))
	}
}
