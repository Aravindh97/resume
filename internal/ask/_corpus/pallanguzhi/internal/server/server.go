// Package server is the online Pallanguzhi service: a small JSON API to
// create rooms, a WebSocket per player or spectator, and (optionally) the
// built React app.
//
// The game is public and anonymous, so there are no accounts. What the
// server does guard against:
//   - other websites driving it: WebSocket and API calls must come from an
//     allowed Origin;
//   - abuse: rooms created per IP per minute, connections per IP, messages
//     per connection per second, and message size are all capped;
//   - cheating: the room accepts only legal moves, in turn, against the
//     current version (see internal/room).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Aravindh97/pallanguzhi/internal/room"
)

type Config struct {
	// Origins allowed to open WebSockets and call the API, e.g.
	// https://aravindh97.github.io. The server's own origin is always allowed.
	Origins []string
	// TrustProxy uses the last X-Forwarded-For entry as the client IP. Only
	// set it behind a proxy that writes that header (Render does).
	TrustProxy bool
	// Static, if set, is the built React app, served at /.
	Static fs.FS

	RoomsPerMinute int // per IP
	ConnsPerIP     int
	MsgsPerSecond  int // per connection, with a burst of the same size
}

func DefaultConfig() Config {
	return Config{RoomsPerMinute: 10, ConnsPerIP: 20, MsgsPerSecond: 10}
}

type Server struct {
	cfg   Config
	hub   *room.Hub
	log   *slog.Logger
	mu    sync.Mutex
	conns map[string]int         // open WebSockets per IP
	made  map[string][]time.Time // room creations per IP in the last minute
	total int
}

func New(cfg Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, hub: room.NewHub(), log: log, conns: map[string]int{}, made: map[string][]time.Time{}}
}

func (s *Server) Hub() *room.Hub { return s.hub }

// Run sweeps expired rooms until ctx is done.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.hub.Sweep(); n > 0 {
				s.log.Info("swept rooms", "removed", n, "open", s.hub.Rooms())
			}
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/rooms", s.create)
	mux.HandleFunc("GET /api/rooms/{code}", s.peek)
	mux.HandleFunc("GET /api/rooms/{code}/ws", s.ws)
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := s.total
		s.mu.Unlock()
		writeJSON(w, 200, map[string]int{"rooms": s.hub.Rooms(), "connections": n})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	if s.cfg.Static != nil {
		mux.Handle("GET /", http.FileServerFS(s.cfg.Static))
	}
	return s.headers(mux)
}

func (s *Server) headers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if o := r.Header.Get("Origin"); o != "" {
				if !s.originOK(r) {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin not allowed"})
					return
				}
				hd.Set("Access-Control-Allow-Origin", o)
				hd.Set("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				hd.Set("Access-Control-Allow-Methods", "GET, POST")
				hd.Set("Access-Control-Allow-Headers", "Content-Type")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		} else {
			hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		}
		h.ServeHTTP(w, r)
	})
}

// originOK allows requests with no Origin (curl, server-to-server), the
// server's own origin, and the configured ones.
func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" && s.cfg.TrustProxy {
		scheme = "https"
	}
	if o == scheme+"://"+r.Host {
		return true
	}
	for _, a := range s.cfg.Origins {
		if o == a {
			return true
		}
	}
	return false
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	now := time.Now()
	s.mu.Lock()
	recent := s.made[ip][:0]
	for _, t := range s.made[ip] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= s.cfg.RoomsPerMinute {
		s.made[ip] = recent
		s.mu.Unlock()
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many new rooms; wait a minute"})
		return
	}
	s.made[ip] = append(recent, now)
	if len(s.made) > 10000 { // forget old entries so the map cannot grow forever
		for k, v := range s.made {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > time.Minute {
				delete(s.made, k)
			}
		}
	}
	s.mu.Unlock()
	code, err := s.hub.Create()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"code": code})
}

func (s *Server) peek(w http.ResponseWriter, r *http.Request) {
	st, err := s.hub.Peek(r.PathValue("code"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, st)
}

// ClientMsg is what a browser sends on the socket; ServerMsg what it receives.
type ClientMsg struct {
	Type    string `json:"type"` // move | rematch | ping
	Pit     int    `json:"pit"`
	Version int    `json:"version"`
}

type ServerMsg struct {
	Type  string      `json:"type"` // hello | state | error | pong
	Seat  *int        `json:"seat,omitempty"`
	Token string      `json:"token,omitempty"`
	State *room.State `json:"state,omitempty"`
	Error string      `json:"error,omitempty"`
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	ip := s.clientIP(r)
	s.mu.Lock()
	if s.conns[ip] >= s.cfg.ConnsPerIP {
		s.mu.Unlock()
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	s.conns[ip]++
	s.total++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.conns[ip]--; s.conns[ip] <= 0 {
			delete(s.conns, ip)
		}
		s.total--
		s.mu.Unlock()
	}()

	q := r.URL.Query()
	sub, err := s.hub.Join(r.PathValue("code"), q.Get("name"), q.Get("token"), q.Get("play") == "1")
	if err != nil {
		code := http.StatusNotFound
		if errors.Is(err, room.ErrFull) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	defer sub.Leave(time.Now())

	// Origin is checked above; the library's own check is replaced by ours so
	// configured cross-origin hosts (GitHub Pages) work.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(512)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	send := func(m ServerMsg) error {
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		b, _ := json.Marshal(m)
		return c.Write(wctx, websocket.MessageText, b)
	}
	seat := sub.Seat
	if err := send(ServerMsg{Type: "hello", Seat: &seat, Token: sub.Token}); err != nil {
		return
	}

	// Reader: client messages, rate limited with a token bucket.
	errs := make(chan string, 4)
	go func() {
		defer cancel()
		tokens, last := float64(s.cfg.MsgsPerSecond), time.Now()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			now := time.Now()
			tokens = min(float64(s.cfg.MsgsPerSecond), tokens+now.Sub(last).Seconds()*float64(s.cfg.MsgsPerSecond))
			last = now
			if tokens < 1 {
				c.Close(websocket.StatusPolicyViolation, "too many messages")
				return
			}
			tokens--
			var m ClientMsg
			if json.Unmarshal(data, &m) != nil {
				errs <- "bad message"
				continue
			}
			var e error
			switch m.Type {
			case "move":
				e = sub.Move(m.Pit, m.Version)
			case "rematch":
				e = sub.Rematch()
			case "ping":
				select {
				case errs <- "":
				default:
				}
				continue
			default:
				e = errors.New("unknown message type")
			}
			if e != nil {
				select {
				case errs <- e.Error():
				default:
				}
			}
		}
	}()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusNormalClosure, "")
			return
		case _, ok := <-sub.Notify:
			if !ok { // the room expired
				c.Close(websocket.StatusGoingAway, "room closed")
				return
			}
			if st, has := sub.Take(); has {
				if send(ServerMsg{Type: "state", State: &st}) != nil {
					return
				}
			}
		case e := <-errs:
			m := ServerMsg{Type: "error", Error: e}
			if e == "" {
				m = ServerMsg{Type: "pong"}
			}
			if send(m) != nil {
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}
