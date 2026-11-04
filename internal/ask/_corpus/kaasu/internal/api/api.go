// Package api is Kaasu's HTTP layer.
//
// Sessions are HttpOnly SameSite=Lax cookies; every state-changing request
// also needs "X-Kaasu: 1" and a same-site Origin, which a page on another
// site cannot produce (CSRF). Requests that move money also need an
// Idempotency-Key header, so a retry never moves money twice.
package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/kaasu/internal/analyse"
	"github.com/Aravindh97/kaasu/internal/auth"
	"github.com/Aravindh97/kaasu/internal/store"
)

const cookie = "kaasu_session"

type Config struct {
	Demo       bool
	TrustProxy bool
	Static     fs.FS
}

type Server struct {
	cfg   Config
	db    *pgxpool.Pool
	auth  *auth.Auth
	store *store.Store
	log   *slog.Logger

	mu   sync.Mutex
	subs map[chan string]func(topic string) bool
	gz   sync.Map // static path -> gzipped bytes
}

func New(cfg Config, db *pgxpool.Pool, a *auth.Auth, log *slog.Logger, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	s := &Server{cfg: cfg, db: db, auth: a, log: log, subs: map[chan string]func(string) bool{}}
	// PINs use bcrypt cost 8, not the password cost. A 4-digit PIN has only
	// 10,000 values, so no hash cost makes a leaked hash safe; the 3-tries
	// lock protects it. Cost 10 made each payment spend 54 ms of CPU (cost 8: 14 ms)
	// on the hash, which the load test showed slowing every other request.
	s.store = &store.Store{DB: db, Now: func() time.Time { return now().In(store.IST) }, PINCost: min(a.Cost, store.PINCost)}
	return s
}

func (s *Server) Store() *store.Store { return s.store }

// Reset loads the demo data.
func (s *Server) Reset(ctx context.Context) error { return s.store.ResetDemo(ctx, s.auth.Cost) }

type handler func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error)

func (s *Server) req(r *http.Request) store.Request {
	return store.Request{Key: r.Header.Get("Idempotency-Key"), IP: s.ip(r)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	open := func(p string, h http.HandlerFunc) { mux.HandleFunc(p, h) }
	route := func(p string, code int, h handler) {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			u, err := s.user(r)
			if err != nil {
				writeErr(w, err)
				return
			}
			v, err := h(w, r, u)
			if err != nil {
				writeErr(w, err)
				return
			}
			if v != nil {
				writeJSON(w, code, v)
			}
		})
	}
	ok := map[string]bool{"ok": true}

	open("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	open("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		c := map[string]any{"demo": s.cfg.Demo, "demo_users": []store.DemoUser{}, "send_categories": store.SendCategories(), "budget_categories": store.BudgetCategories(),
			"statement_categories": analyse.Categories(), "limits": map[string]int64{"daily_send": store.DailySendLimit, "daily_topup": store.DailyTopUpLimit, "wallet_cap": store.WalletCap},
			"now": s.store.Now().Format(time.RFC3339)}
		if s.cfg.Demo {
			c["demo_users"], c["demo_pin"] = store.DemoUsers, store.DemoPIN
		}
		writeJSON(w, 200, c)
	})
	open("GET /api/sample-statement.csv", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="sample-bank-statement.csv"`)
		io.WriteString(w, analyse.SampleCSV(7, s.store.Now()))
	})
	open("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Email, Password string }
		if err := readJSON(r, &in); err != nil {
			writeErr(w, err)
			return
		}
		tok, u, err := s.auth.Login(r.Context(), in.Email, in.Password, s.ip(r))
		s.signedIn(w, r, tok, u, err)
	})
	open("POST /api/demo-login", func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Demo {
			writeErr(w, store.ErrNotFound)
			return
		}
		var in struct{ Email string }
		if err := readJSON(r, &in); err != nil {
			writeErr(w, err)
			return
		}
		tok, u, err := s.auth.DemoLogin(r.Context(), in.Email)
		s.signedIn(w, r, tok, u, err)
	})
	open("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookie); err == nil {
			s.auth.Logout(r.Context(), c.Value)
		}
		s.setCookie(w, r, "", -1)
		writeJSON(w, 200, ok)
	})

	route("GET /api/me", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) { return u, nil })
	route("GET /api/stream", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		s.stream(w, r, topicFilter(u))
		return nil, nil
	})

	// Wallet
	route("GET /api/wallet", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Wallet(r.Context(), u)
	})
	route("GET /api/activity", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		return s.store.Activity(r.Context(), u, before, limit)
	})
	route("POST /api/topup", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.TopUp
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.AddMoney(r.Context(), u, s.req(r), in)
	})
	route("GET /api/lookup", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Lookup(r.Context(), u, r.URL.Query().Get("to"))
	})
	route("POST /api/send", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.Send
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.SendMoney(r.Context(), u, s.req(r), in)
	})
	route("POST /api/pin", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in struct {
			Current string `json:"current"`
			New     string `json:"new"`
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		if u.HasPIN {
			if err := s.store.CheckPIN(r.Context(), u, in.Current, s.ip(r)); err != nil {
				return nil, err
			}
		}
		return ok, s.store.SetPIN(r.Context(), u, in.New, s.ip(r))
	})
	route("GET /api/statement.csv", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		from, err1 := time.ParseInLocation(time.DateOnly, r.URL.Query().Get("from"), store.IST)
		to, err2 := time.ParseInLocation(time.DateOnly, r.URL.Query().Get("to"), store.IST)
		if err1 != nil || err2 != nil {
			return nil, &store.Problem{Field: "from", Msg: "choose the dates"}
		}
		b, err := s.store.StatementCSV(r.Context(), u, from, to)
		if err != nil {
			return nil, err
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="kaasu-statement-%s-to-%s.csv"`, from.Format("20060102"), to.Format("20060102")))
		w.Write(b)
		return nil, nil
	})

	// Requests and splits
	route("GET /api/requests", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Requests(r.Context(), u)
	})
	route("POST /api/requests", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.NewRequest
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.RequestMoney(r.Context(), u, in, s.ip(r))
	})
	route("POST /api/requests/{id}/pay", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.PayRequest
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.PayRequest(r.Context(), u, s.req(r), id(r), in)
	})
	route("POST /api/requests/{id}/{action}", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.DecideRequest(r.Context(), u, id(r), r.PathValue("action"), s.ip(r))
	})
	route("POST /api/splits", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.NewSplit
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.SplitBill(r.Context(), u, in, s.ip(r))
	})

	// Planning
	route("GET /api/budgets", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Budgets(r.Context(), u)
	})
	route("PUT /api/budgets", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.SetBudget
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		if err := s.store.SetBudget(r.Context(), u, in); err != nil {
			return nil, err
		}
		return s.store.Budgets(r.Context(), u)
	})
	route("GET /api/goals", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Goals(r.Context(), u)
	})
	route("POST /api/goals", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.NewGoal
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.CreateGoal(r.Context(), u, in)
	})
	route("POST /api/goals/{id}/move", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.GoalMove
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.MoveGoal(r.Context(), u, s.req(r), id(r), in)
	})
	route("POST /api/goals/{id}/close", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return ok, s.store.CloseGoal(r.Context(), u, id(r))
	})
	route("GET /api/reports", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Reports(r.Context(), u)
	})

	// Statement analysis
	route("GET /api/imports", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Imports(r.Context(), u)
	})
	route("POST /api/imports", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		f, hdr, err := r.FormFile("file")
		if err != nil {
			return nil, &store.Problem{Field: "file", Msg: "choose a CSV file of up to 2 MB"}
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		if err != nil {
			return nil, &store.Problem{Field: "file", Msg: "the file is too big; the limit is 2 MB"}
		}
		return s.store.ImportOnServer(r.Context(), u, hdr.Filename, b)
	})
	route("POST /api/imports/device", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.DeviceImport
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.ImportFromDevice(r.Context(), u, in)
	})
	route("GET /api/imports/{id}", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.GetImport(r.Context(), u, id(r))
	})
	route("PUT /api/imports/{id}/summary", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.DeviceImport
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.UpdateDeviceSummary(r.Context(), u, id(r), in)
	})
	route("DELETE /api/imports/{id}", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return ok, s.store.DeleteImport(r.Context(), u, id(r))
	})
	route("GET /api/rules", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Rules(r.Context(), u)
	})
	route("POST /api/rules", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.Teach
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		n, err := s.store.Teach(r.Context(), u, in)
		return map[string]int{"changed": n}, err
	})

	// Audit and ops
	route("GET /api/audit", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Audit(r.Context(), u, 100)
	})
	route("GET /api/ops/paused", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Paused(r.Context(), u)
	})
	route("POST /api/ops/paused/{id}/unpause", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.Unpause
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return ok, s.store.Unpause(r.Context(), u, id(r), in, s.ip(r))
	})
	route("GET /api/ops/ledger", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.CheckLedger(r.Context(), u)
	})
	route("POST /api/demo/reset", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		if !s.cfg.Demo {
			return nil, store.ErrNotFound
		}
		if u.Role != "ops" {
			return nil, store.ErrForbidden
		}
		return ok, s.Reset(r.Context())
	})

	if s.cfg.Static != nil {
		mux.Handle("/", s.spa())
	}
	return s.middleware(mux)
}

// topicFilter: a member hears only about their own money; ops hear about
// pauses. Notices carry only a topic; the browser refetches through the API.
func topicFilter(u *auth.User) func(string) bool {
	mine := fmt.Sprintf("user:%d", u.ID)
	return func(t string) bool { return t == mine || t == "ops" && u.Role == "ops" }
}

func (s *Server) publish(topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, want := range s.subs {
		if want(topic) {
			select {
			case ch <- topic:
			default: // a slow browser misses a notice; it refetches on reconnect
			}
		}
	}
}

// Listen relays Postgres notifications to connected browsers.
func (s *Server) Listen(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		err := func() error {
			c, err := s.db.Acquire(ctx)
			if err != nil {
				return err
			}
			defer c.Release()
			if _, err := c.Exec(ctx, "listen kaasu"); err != nil {
				return err
			}
			wait = time.Second
			for {
				n, err := c.Conn().WaitForNotification(ctx)
				if err != nil {
					return err
				}
				s.publish(n.Payload)
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

func (s *Server) stream(w http.ResponseWriter, r *http.Request, want func(string) bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	ch := make(chan string, 16)
	s.mu.Lock()
	s.subs[ch] = want
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 3000\n: connected\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case t := <-ch:
			fmt.Fprintf(w, "event: change\ndata: %s\n\n", t)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) spa() http.Handler {
	files := http.FileServerFS(s.cfg.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if strings.HasPrefix(p, "api/") {
			writeErr(w, store.ErrNotFound)
			return
		}
		if p != "" {
			if f, err := s.cfg.Static.Open(p); err == nil {
				f.Close()
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				if strings.HasSuffix(p, ".wasm") {
					w.Header().Set("Content-Type", "application/wasm")
				}
				if s.gzipped(w, r, p) {
					return
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, s.cfg.Static, "index.html")
	})
}

// gzipped serves a compressible static file compressed, made once and kept
// in memory (the analyser's .wasm goes from 3.7 MB to about 1 MB).
func (s *Server) gzipped(w http.ResponseWriter, r *http.Request, p string) bool {
	ext := path.Ext(p)
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || !map[string]bool{".wasm": true, ".js": true, ".css": true, ".svg": true}[ext] {
		return false
	}
	v, ok := s.gz.Load(p)
	if !ok {
		raw, err := fs.ReadFile(s.cfg.Static, p)
		if err != nil {
			return false
		}
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		zw.Write(raw)
		zw.Close()
		v, _ = s.gz.LoadOrStore(p, b.Bytes())
	}
	if ct := mime.TypeByExtension(ext); ct != "" && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	w.Write(v.([]byte))
	return true
}

func (s *Server) middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Frame-Options", "DENY")
		// 'wasm-unsafe-eval' lets the page compile the statement analyser
		// (WebAssembly) without allowing JavaScript eval.
		hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if s.https(r) {
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			hd.Set("Cache-Control", "no-store")
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if r.Header.Get("X-Kaasu") != "1" || !s.sameOrigin(r) {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
					return
				}
				limit := int64(64 << 10)
				if strings.HasPrefix(r.URL.Path, "/api/imports") {
					limit = 2<<20 + 64<<10 // a 2 MB statement plus the form around it
				}
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) https(r *http.Request) bool {
	return r.TLS != nil || s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https"
}

func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	scheme := "http"
	if s.https(r) {
		scheme = "https"
	}
	return o == scheme+"://"+r.Host
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

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, tok string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: cookie, Value: tok, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: s.https(r), SameSite: http.SameSiteLaxMode})
}

func (s *Server) signedIn(w http.ResponseWriter, r *http.Request, tok string, u *auth.User, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	s.setCookie(w, r, tok, int(auth.SessionTTL.Seconds()))
	writeJSON(w, 200, u)
}

func (s *Server) user(r *http.Request) (*auth.User, error) {
	c, err := r.Cookie(cookie)
	if err != nil {
		return nil, auth.ErrNoSession
	}
	return s.auth.Session(r.Context(), c.Value)
}

func id(r *http.Request) int64 {
	v, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return v
}

func readJSON(r *http.Request, v any) error {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		return &store.Problem{Msg: "send JSON"}
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			return &store.Problem{Msg: "request too large", Code: 413}
		}
		return &store.Problem{Msg: "invalid request: " + err.Error()}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var p *store.Problem
	switch {
	case errors.As(err, &p):
		code := p.Code
		if code == 0 {
			code = http.StatusUnprocessableEntity
		}
		writeJSON(w, code, p)
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, 404, map[string]string{"error": "not found"})
	case errors.Is(err, store.ErrForbidden):
		writeJSON(w, 403, map[string]string{"error": "not allowed for your account"})
	case errors.Is(err, auth.ErrNoSession):
		writeJSON(w, 401, map[string]string{"error": "please sign in"})
	case errors.Is(err, auth.ErrBadLogin):
		writeJSON(w, 401, map[string]string{"error": err.Error()})
	case errors.Is(err, auth.ErrThrottled):
		w.Header().Set("Retry-After", "600")
		writeJSON(w, 429, map[string]string{"error": err.Error()})
	case errors.Is(err, context.Canceled):
	default:
		slog.Error("request", "err", err)
		writeJSON(w, 500, map[string]string{"error": "something went wrong"})
	}
}
