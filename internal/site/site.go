// Package site serves the resume: a public HTTP listener (page, JSON API,
// metrics) and a private one for Raft traffic between mesh nodes.
//
// The public side is anonymous by design (it is a resume), so it is
// read-mostly, rate-limited per client, size-limited and time-limited. The
// two state-changing public actions, chaos and "replicate an update", are
// rate-limited cluster-wide and can never take the cluster below quorum. Raft
// RPCs and node isolation are only served on the private listener, which
// binds to loopback by default and to the platform's private network in
// production; they are never routed through the public listener.
package site

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Aravindh97/resume/internal/ask"
	"github.com/Aravindh97/resume/internal/career"
	"github.com/Aravindh97/resume/internal/profile"
	"github.com/Aravindh97/resume/internal/raft"
)

//go:embed static
var assets embed.FS

type Peer struct{ ID, RaftAddr, Region string }

type Config struct {
	ID       string
	Region   string
	Peers    []Peer // other nodes
	Public   string // public listen address
	Private  string // raft listen address
	ChaosFor time.Duration
	Log      *slog.Logger
}

type Server struct {
	cfg     Config
	node    *raft.Node
	index   *ask.Index
	client  *http.Client
	started time.Time

	mu        sync.Mutex
	version   int // replicated resume version (state machine)
	history   []string
	lastChaos time.Time
	lastWrite time.Time

	reqs, limited atomic.Int64
	limiter       *limiter
}

func New(cfg Config) (*Server, error) {
	if cfg.ChaosFor == 0 {
		cfg.ChaosFor = 20 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	ix, err := ask.Load()
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, index: ix, started: time.Now(), limiter: newLimiter(20, 40),
		client: &http.Client{Timeout: 2 * time.Second}}
	var peers []string
	for _, p := range cfg.Peers {
		peers = append(peers, p.ID)
	}
	s.node = raft.New(raft.Config{ID: cfg.ID, Peers: peers, Transport: &httpTransport{s},
		ElectionTimeout: 500 * time.Millisecond, HeartbeatTimeout: 100 * time.Millisecond,
		Apply: s.apply, Logf: func(f string, a ...any) { cfg.Log.Info(fmt.Sprintf(f, a...)) }})
	return s, nil
}

type command struct {
	Op   string `json:"op"`
	Note string `json:"note"`
	By   string `json:"by"`
	At   string `json:"at"`
}

func (s *Server) apply(i int, e raft.Entry) {
	var c command
	if json.Unmarshal(e.Cmd, &c) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	s.history = append(s.history, fmt.Sprintf("v%d %s (term %d, via %s)", s.version, c.Note, e.Term, c.By))
	if len(s.history) > 20 {
		s.history = s.history[len(s.history)-20:]
	}
}

// Run serves both listeners until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go s.node.Run(ctx)
	pub := &http.Server{Addr: s.cfg.Public, Handler: s.publicHandler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	priv := &http.Server{Addr: s.cfg.Private, Handler: s.privateHandler(), ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- serve(pub) }()
	go func() { errc <- serve(priv) }()
	s.cfg.Log.Info("node up", "id", s.cfg.ID, "public", s.cfg.Public, "raft", s.cfg.Private, "peers", len(s.cfg.Peers))
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub.Shutdown(sc)
	priv.Shutdown(sc)
	return nil
}

func serve(h *http.Server) error {
	if err := h.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---------- private (raft) side ----------

func (s *Server) privateHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /raft/vote", func(w http.ResponseWriter, r *http.Request) {
		var a raft.VoteArgs
		if !decode(w, r, &a) {
			return
		}
		rep, err := s.node.HandleVote(a)
		reply(w, rep, err)
	})
	mux.HandleFunc("POST /raft/append", func(w http.ResponseWriter, r *http.Request) {
		var a raft.AppendArgs
		if !decode(w, r, &a) {
			return
		}
		rep, err := s.node.HandleAppend(a)
		reply(w, rep, err)
	})
	mux.HandleFunc("GET /raft/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.localStatus())
	})
	mux.HandleFunc("POST /raft/isolate", func(w http.ResponseWriter, r *http.Request) {
		s.node.Isolate(s.cfg.ChaosFor)
		writeJSON(w, http.StatusOK, map[string]any{"isolated_for_s": s.cfg.ChaosFor.Seconds()})
	})
	mux.HandleFunc("POST /raft/propose", func(w http.ResponseWriter, r *http.Request) {
		var c command
		if !decode(w, r, &c) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		b, _ := json.Marshal(c)
		idx, err := s.node.Propose(ctx, b)
		reply(w, map[string]int{"index": idx}, err)
	})
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

type httpTransport struct{ s *Server }

func (t *httpTransport) addr(id string) string {
	for _, p := range t.s.cfg.Peers {
		if p.ID == id {
			return p.RaftAddr
		}
	}
	return ""
}

func (t *httpTransport) post(ctx context.Context, peer, path string, in, out any) error {
	if t.s.node.Isolated() {
		return raft.ErrIsolated
	}
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+t.addr(peer)+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s%s: HTTP %d", peer, path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (t *httpTransport) RequestVote(ctx context.Context, p string, a raft.VoteArgs) (raft.VoteReply, error) {
	var r raft.VoteReply
	return r, t.post(ctx, p, "/raft/vote", a, &r)
}

func (t *httpTransport) AppendEntries(ctx context.Context, p string, a raft.AppendArgs) (raft.AppendReply, error) {
	var r raft.AppendReply
	return r, t.post(ctx, p, "/raft/append", a, &r)
}

// ---------- cluster view ----------

type NodeStatus struct {
	raft.Status
	Region  string   `json:"region"`
	Version int      `json:"version"`
	History []string `json:"history,omitempty"`
	Up      bool     `json:"up"`
}

func (s *Server) localStatus() NodeStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return NodeStatus{Status: s.node.Status(), Region: s.cfg.Region, Version: s.version,
		History: append([]string(nil), s.history...), Up: true}
}

func (s *Server) clusterStatus(ctx context.Context) []NodeStatus {
	out := []NodeStatus{s.localStatus()}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range s.cfg.Peers {
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			st := NodeStatus{Region: p.Region}
			st.ID = p.ID
			c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(c, http.MethodGet, "http://"+p.RaftAddr+"/raft/status", nil)
			if resp, err := s.client.Do(req); err == nil {
				json.NewDecoder(resp.Body).Decode(&st)
				resp.Body.Close()
			}
			mu.Lock()
			out = append(out, st)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---------- public side ----------

func (s *Server) publicHandler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	// The page is the React app in web/, built into static/ui by `make ui`;
	// its assets/ and shots/ folders are served at the root, next to it.
	if ui, err := fs.Sub(assets, "static/ui"); err == nil {
		mux.Handle("GET /assets/", http.FileServerFS(ui))
		mux.Handle("GET /shots/", http.FileServerFS(ui))
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		page, err := assets.ReadFile("static/ui/index.html")
		if err != nil {
			http.Error(w, "frontend not built: run make ui", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("GET /api/v1/node", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"id": s.cfg.ID, "region": s.cfg.Region})
	})
	mux.HandleFunc("GET /api/v1/profile", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, profile.Profile) })
	mux.HandleFunc("GET /api/v1/evals", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, profile.Evals) })
	mux.HandleFunc("GET /api/v1/ask", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if len(q) < 3 || len(q) > 300 {
			writeJSON(w, 400, map[string]string{"error": "q must be 3-300 characters"})
			return
		}
		t := time.Now()
		res := s.index.Search(q, 3)
		low := len(res) == 0 || res[0].Score < 6
		writeJSON(w, 200, map[string]any{"query": q, "passages": res, "low_confidence": low,
			"took_ms": float64(time.Since(t).Microseconds()) / 1000, "served_by": s.cfg.ID})
	})
	mux.HandleFunc("GET /api/v1/career", func(w http.ResponseWriter, r *http.Request) {
		args := strings.Fields(r.URL.Query().Get("cmd"))
		if len(args) > 4 {
			args = args[:4]
		}
		if len(args) > 0 && args[0] == "init" {
			writeJSON(w, 400, map[string]string{"error": "run `career init` locally: go run github.com/Aravindh97/resume/cmd/career@latest init ./aravindh"})
			return
		}
		out, err := career.Run(args)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"output": out})
	})
	mux.HandleFunc("GET /api/v1/cluster", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"served_by": s.cfg.ID, "nodes": s.clusterStatus(r.Context())})
	})
	mux.HandleFunc("POST /api/v1/cluster/chaos", s.chaos)
	mux.HandleFunc("POST /api/v1/cluster/write", s.write)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return s.middleware(mux)
}

func (s *Server) chaos(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Target string `json:"target"` // node id or "leader"
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	if since := time.Since(s.lastChaos); since < 10*time.Second {
		s.mu.Unlock()
		writeJSON(w, 429, map[string]string{"error": fmt.Sprintf("chaos cooling down, try in %ds", int((10*time.Second-since).Seconds())+1)})
		return
	}
	s.mu.Unlock()
	nodes := s.clusterStatus(r.Context())
	down := 0
	target := in.Target
	for _, n := range nodes {
		if n.Isolated || n.Role == "" {
			down++
		}
		if in.Target == "leader" && n.Role == raft.Leader {
			target = n.ID
		}
	}
	// Never go below quorum: with 4 nodes at most one may be out at a time.
	if down >= (len(nodes)-1)/2 {
		writeJSON(w, 409, map[string]string{"error": "a node is already down; wait for it to rejoin so the cluster keeps a majority"})
		return
	}
	addr := s.cfg.Private
	if target != s.cfg.ID {
		addr = ""
		for _, p := range s.cfg.Peers {
			if p.ID == target {
				addr = p.RaftAddr
			}
		}
	}
	if addr == "" {
		writeJSON(w, 404, map[string]string{"error": "unknown node"})
		return
	}
	s.mu.Lock()
	s.lastChaos = time.Now()
	s.mu.Unlock()
	if target == s.cfg.ID {
		s.node.Isolate(s.cfg.ChaosFor)
	} else if err := s.privatePost(r.Context(), addr, "/raft/isolate", struct{}{}); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"isolated": target, "for_s": s.cfg.ChaosFor.Seconds()})
}

func (s *Server) write(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if time.Since(s.lastWrite) < 3*time.Second {
		s.mu.Unlock()
		writeJSON(w, 429, map[string]string{"error": "one update every 3 seconds"})
		return
	}
	s.lastWrite = time.Now()
	s.mu.Unlock()
	c := command{Op: "publish", Note: "resume update", By: s.cfg.ID, At: time.Now().UTC().Format(time.RFC3339)}
	leader := ""
	for _, n := range s.clusterStatus(r.Context()) {
		if n.Role == raft.Leader && !n.Isolated {
			leader = n.ID
		}
	}
	addr := s.cfg.Private
	for _, p := range s.cfg.Peers {
		if p.ID == leader {
			addr = p.RaftAddr
		}
	}
	if leader == "" {
		writeJSON(w, 503, map[string]string{"error": "no leader right now (election in progress)"})
		return
	}
	if err := s.privatePost(r.Context(), addr, "/raft/propose", c); err != nil {
		writeJSON(w, 503, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"committed_by": leader})
}

func (s *Server) privatePost(ctx context.Context, addr, path string, v any) error {
	b, _ := json.Marshal(v)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", path, strings.TrimSpace(string(b)))
	}
	return nil
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	st := s.node.Status()
	leader := 0
	if st.Role == raft.Leader {
		leader = 1
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# TYPE go_memstats_heap_alloc_bytes gauge\ngo_memstats_heap_alloc_bytes %d\n", m.HeapAlloc)
	fmt.Fprintf(w, "# TYPE go_gc_cycles_total counter\ngo_gc_cycles_total %d\n", m.NumGC)
	fmt.Fprintf(w, "# TYPE process_uptime_seconds gauge\nprocess_uptime_seconds %.0f\n", time.Since(s.started).Seconds())
	fmt.Fprintf(w, "# TYPE http_requests_total counter\nhttp_requests_total %d\n", s.reqs.Load())
	fmt.Fprintf(w, "# TYPE http_rate_limited_total counter\nhttp_rate_limited_total %d\n", s.limited.Load())
	fmt.Fprintf(w, "# TYPE raft_term gauge\nraft_term{node=%q} %d\n", s.cfg.ID, st.Term)
	fmt.Fprintf(w, "# TYPE raft_is_leader gauge\nraft_is_leader{node=%q} %d\n", s.cfg.ID, leader)
	fmt.Fprintf(w, "# TYPE raft_commit_index gauge\nraft_commit_index{node=%q} %d\n", s.cfg.ID, st.CommitIndex)
	fmt.Fprintf(w, "# TYPE raft_elections_started_total counter\nraft_elections_started_total{node=%q} %d\n", s.cfg.ID, st.Elections)
}

// ---------- middleware: limits and headers ----------

func clientIP(r *http.Request) string {
	// Fly.io sets Fly-Client-IP; elsewhere use the socket address. Arbitrary
	// X-Forwarded-For values are not trusted.
	if ip := r.Header.Get("Fly-Client-IP"); ip != "" && net.ParseIP(ip) != nil {
		return ip
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (s *Server) middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.reqs.Add(1)
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		// wasm-unsafe-eval is required to instantiate the Go WebAssembly module.
		hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		hd.Set("X-Served-By", s.cfg.ID)
		if !s.limiter.allow(clientIP(r)) {
			s.limited.Add(1)
			writeJSON(w, 429, map[string]string{"error": "too many requests"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		h.ServeHTTP(w, r)
	})
}

// limiter is a per-client token bucket.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perSec, burst float64) *limiter {
	return &limiter{rate: perSec, burst: burst, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.buckets) > 10000 { // bound memory under many clients
		for k, b := range l.buckets {
			if now.Sub(b.last) > time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
