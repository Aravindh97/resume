// Package server stores and relays notebook operations.
//
// A notebook (a "space") is identified by a random 128-bit secret that
// lives only in the share link's #fragment and in an X-Space header, never
// in a URL path or query, so it does not end up in server or proxy logs.
// The database stores only its SHA-256.
//
// Each space has its own counter. Storing ops takes the space's row lock,
// numbers the ops, and commits, so numbers become visible in order: a
// client that has seen everything up to n and asks for "after n" can never
// miss an op that commits later with a smaller number. (With one global
// identity column that guarantee does not hold: two transactions can take
// numbers 7 and 8 and commit 8 first.)
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/kurippu/internal/crdt"
)

const Schema = `
create table if not exists spaces (
  key        bytea primary key,          -- sha256 of the secret
  next_n     bigint not null default 1,
  created_at timestamptz not null default now(),
  active_at  timestamptz not null default now()
);
create table if not exists ops (
  space   bytea not null references spaces (key),
  n       bigint not null,               -- order within the space
  replica text not null,
  seq     bigint not null,
  op      jsonb not null,
  primary key (space, n),
  unique (space, replica, seq)
);`

const (
	MaxOpsPerSync  = 1000
	MaxOpsPerSpace = 200000
	PageSize       = 2000
)

type Config struct {
	TrustProxy      bool
	Static          fs.FS
	SpacesPerMinute int // per IP
	SyncsPerSecond  int // per IP, burst of 3x
}

func DefaultConfig() Config { return Config{SpacesPerMinute: 5, SyncsPerSecond: 20} }

type Server struct {
	cfg  Config
	db   *pgxpool.Pool
	log  *slog.Logger
	mu   sync.Mutex
	subs map[string]map[chan int64]struct{} // space key (hex) -> listeners
	lim  map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
	made   []time.Time
}

func New(cfg Config, db *pgxpool.Pool, log *slog.Logger) *Server {
	return &Server{cfg: cfg, db: db, log: log, subs: map[string]map[chan int64]struct{}{}, lim: map[string]*bucket{}}
}

func Migrate(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, Schema)
	return err
}

func keyOf(secret string) ([]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(b) != 16 {
		return nil, false
	}
	h := sha256.Sum256(b)
	return h[:], true
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /api/spaces", s.create)
	mux.HandleFunc("POST /api/sync", s.sync)
	mux.HandleFunc("GET /api/events", s.events)
	if s.cfg.Static != nil {
		mux.Handle("GET /", s.static())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer") // the share link's fragment is never sent, but be strict anyway
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
			if r.Method == http.MethodPost {
				// JSON only: a cross-site form cannot send it without a CORS preflight,
				// which this server never approves.
				if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
					writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send JSON"})
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// static serves the app; the service worker must not be cached by the
// browser's HTTP cache or updates would never arrive.
func (s *Server) static() http.Handler {
	files := http.FileServerFS(s.cfg.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		switch {
		case p == "sw.js" || p == "" || p == "index.html":
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasPrefix(p, "assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		if p != "" {
			if f, err := s.cfg.Static.Open(p); err == nil {
				f.Close()
				files.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFileFS(w, r, s.cfg.Static, "index.html")
	})
}

func (s *Server) ip(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			p := strings.Split(xff, ",")
			return strings.TrimSpace(p[len(p)-1])
		}
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (s *Server) allow(ip string, newSpace bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	b := s.lim[ip]
	if b == nil {
		if len(s.lim) > 50000 {
			s.lim = map[string]*bucket{}
		}
		b = &bucket{tokens: float64(3 * s.cfg.SyncsPerSecond), last: now}
		s.lim[ip] = b
	}
	if newSpace {
		recent := b.made[:0]
		for _, t := range b.made {
			if now.Sub(t) < time.Minute {
				recent = append(recent, t)
			}
		}
		b.made = recent
		if len(recent) >= s.cfg.SpacesPerMinute {
			return false
		}
		b.made = append(b.made, now)
		return true
	}
	b.tokens = min(float64(3*s.cfg.SyncsPerSecond), b.tokens+now.Sub(b.last).Seconds()*float64(s.cfg.SyncsPerSecond))
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	if !s.allow(s.ip(r), true) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many new notebooks; wait a minute"})
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	secret := base64.RawURLEncoding.EncodeToString(b)
	key, _ := keyOf(secret)
	if _, err := s.db.Exec(r.Context(), `insert into spaces (key) values ($1)`, key); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"space": secret})
}

type numbered struct {
	N  int64   `json:"n"`
	Op crdt.Op `json:"op"`
}

type SyncRequest struct {
	Since int64     `json:"since"`
	Ops   []crdt.Op `json:"ops"`
}

type SyncResponse struct {
	Ops      []numbered `json:"ops"`
	Latest   int64      `json:"latest"`   // highest n returned (or since)
	More     bool       `json:"more"`     // call again for the rest
	Accepted int        `json:"accepted"` // new ops stored from this request
}

func (s *Server) space(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	key, ok := keyOf(r.Header.Get("X-Space"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such notebook"})
		return nil, false
	}
	var exists bool
	if err := s.db.QueryRow(r.Context(), `select exists (select 1 from spaces where key = $1)`, key).Scan(&exists); err != nil || !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such notebook"})
		return nil, false
	}
	return key, true
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	if !s.allow(s.ip(r), false) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow down"})
		return
	}
	key, ok := s.space(w, r)
	if !ok {
		return
	}
	var in SyncRequest
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if len(in.Ops) > MaxOpsPerSync {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("send at most %d ops at a time", MaxOpsPerSync)})
		return
	}
	for i, o := range in.Ops {
		if err := o.Validate(); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("op %d: %v", i, err)})
			return
		}
	}
	accepted, err := s.store(r.Context(), key, in.Ops)
	if errors.Is(err, errFull) {
		writeJSON(w, http.StatusInsufficientStorage, map[string]string{"error": "this notebook is full"})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := SyncResponse{Ops: []numbered{}, Latest: in.Since, Accepted: accepted}
	rows, err := s.db.Query(r.Context(), `select n, op from ops where space = $1 and n > $2 order by n limit $3`, key, in.Since, PageSize+1)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var x numbered
		if err := rows.Scan(&x.N, &x.Op); err != nil {
			s.fail(w, err)
			return
		}
		resp.Ops = append(resp.Ops, x)
	}
	if len(resp.Ops) > PageSize {
		resp.Ops, resp.More = resp.Ops[:PageSize], true
	}
	if len(resp.Ops) > 0 {
		resp.Latest = resp.Ops[len(resp.Ops)-1].N
	}
	writeJSON(w, http.StatusOK, resp)
}

var errFull = errors.New("space full")

// store numbers and saves ops that are new; ops already stored (a retried
// request, or the same op from two tabs) are skipped.
func (s *Server) store(ctx context.Context, key []byte, ops []crdt.Op) (int, error) {
	if len(ops) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var next int64
	if err := tx.QueryRow(ctx, `select next_n from spaces where key = $1 for update`, key).Scan(&next); err != nil {
		return 0, err
	}
	if next+int64(len(ops)) > MaxOpsPerSpace {
		return 0, errFull
	}
	accepted := 0
	for _, o := range ops {
		b, _ := json.Marshal(o)
		tag, err := tx.Exec(ctx, `insert into ops (space, n, replica, seq, op) values ($1, $2, $3, $4, $5) on conflict (space, replica, seq) do nothing`,
			key, next, o.Replica, o.Seq, b)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 1 {
			next++
			accepted++
		}
	}
	if _, err := tx.Exec(ctx, `update spaces set next_n = $2, active_at = now() where key = $1`, key, next); err != nil {
		return 0, err
	}
	if accepted > 0 {
		if _, err := tx.Exec(ctx, `select pg_notify('kurippu', $1)`, hex.EncodeToString(key)+":"+strconv.FormatInt(next-1, 10)); err != nil {
			return 0, err
		}
	}
	return accepted, tx.Commit(ctx)
}

// events streams "changed" notices for one space as Server-Sent Events.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	key, ok := s.space(w, r)
	if !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	k := hex.EncodeToString(key)
	ch := make(chan int64, 1)
	s.mu.Lock()
	if s.subs[k] == nil {
		s.subs[k] = map[chan int64]struct{}{}
	}
	s.subs[k][ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs[k], ch)
		if len(s.subs[k]) == 0 {
			delete(s.subs, k)
		}
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case n := <-ch:
			fmt.Fprintf(w, "event: changed\ndata: %d\n\n", n)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) publish(k string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs[k] {
		select {
		case ch <- n:
		default: // a notice is already waiting; the client will sync everything
		}
	}
}

// Listen relays Postgres notifications to this instance's listeners.
func (s *Server) Listen(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		err := func() error {
			c, err := s.db.Acquire(ctx)
			if err != nil {
				return err
			}
			defer c.Release()
			if _, err := c.Exec(ctx, "listen kurippu"); err != nil {
				return err
			}
			wait = time.Second
			for {
				n, err := c.Conn().WaitForNotification(ctx)
				if err != nil {
					return err
				}
				k, num, _ := strings.Cut(n.Payload, ":")
				v, _ := strconv.ParseInt(num, 10, 64)
				s.publish(k, v)
			}
		}()
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("listen", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, 30*time.Second)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such notebook"})
		return
	}
	s.log.Error("request", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "something went wrong"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
