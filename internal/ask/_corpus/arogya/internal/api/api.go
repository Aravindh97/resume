// Package api is Arogya's HTTP layer.
//
// Sessions are HttpOnly SameSite=Lax cookies; every state-changing request
// also needs "X-Arogya: 1" and a same-site Origin, which a page on another
// site cannot produce (CSRF). The waiting-room token board is public on
// purpose: like the screen in a hospital hall, it shows token numbers only,
// never names. Everything else requires signing in.
package api

import (
	"context"
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
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/arogya/internal/auth"
	"github.com/Aravindh97/arogya/internal/domain"
	"github.com/Aravindh97/arogya/internal/seed"
	"github.com/Aravindh97/arogya/internal/store"
)

const cookie = "arogya_session"

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
	clock *seed.Clock // demo hospital time; nil = real time

	mu   sync.Mutex
	subs map[chan string]func(topic string) bool
}

var IST, _ = time.LoadLocation("Asia/Kolkata")

func New(cfg Config, db *pgxpool.Pool, a *auth.Auth, log *slog.Logger, clock *seed.Clock) *Server {
	s := &Server{cfg: cfg, db: db, auth: a, log: log, clock: clock, subs: map[chan string]func(string) bool{}}
	now := func() time.Time { return time.Now().In(IST) }
	if clock != nil {
		now = func() time.Time { return s.clock.Now() }
	}
	s.store = &store.Store{DB: db, Now: now}
	return s
}

func (s *Server) Store() *store.Store { return s.store }

type handler func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error)

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
		users := []seed.DemoUser{}
		if s.cfg.Demo {
			users = seed.Users
		}
		writeJSON(w, 200, map[string]any{"demo": s.cfg.Demo, "demo_users": users, "now": s.store.Now()})
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
	route("POST /api/logout", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		if c, err := r.Cookie(cookie); err == nil {
			s.auth.Logout(r.Context(), c.Value)
		}
		s.setCookie(w, r, "", -1)
		return ok, nil
	})
	route("GET /api/me", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) { return u, nil })

	// OPD
	open("GET /api/board", func(w http.ResponseWriter, r *http.Request) {
		b, err := s.store.Board(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"rows": b, "now": s.store.Now()})
	})
	open("GET /api/board/stream", func(w http.ResponseWriter, r *http.Request) {
		s.stream(w, r, func(t string) bool { return strings.HasPrefix(t, "opd:") })
	})
	route("GET /api/doctors", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		day, err := time.ParseInLocation(time.DateOnly, r.URL.Query().Get("day"), IST)
		if err != nil {
			n := s.store.Now()
			day = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, IST)
		}
		return s.store.Doctors(r.Context(), day)
	})
	route("GET /api/patients", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.SearchPatients(r.Context(), u, r.URL.Query().Get("q"))
	})
	route("POST /api/patients", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.NewPatient
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.RegisterPatient(r.Context(), u, in)
	})
	route("POST /api/visits", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.Booking
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.Book(r.Context(), u, in)
	})
	route("POST /api/visits/{id}/checkin", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return ok, s.store.CheckIn(r.Context(), u, id(r))
	})
	route("POST /api/visits/{id}/cancel", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return ok, s.store.Cancel(r.Context(), u, id(r))
	})
	route("GET /api/reception", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.TodayAtReception(r.Context(), u)
	})
	route("GET /api/queue", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		d, _ := strconv.ParseInt(r.URL.Query().Get("doctor"), 10, 64)
		return s.store.Queue(r.Context(), u, d)
	})
	route("POST /api/queue/next", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		v, err := s.store.CallNext(r.Context(), u)
		if err == nil && v == nil {
			return map[string]any{"called": nil}, nil
		}
		return map[string]any{"called": v}, err
	})

	// Patients' own pages
	route("GET /api/my/visits", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.MyVisits(r.Context(), u)
	})
	route("GET /api/my/record", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.MyRecord(r.Context(), u)
	})
	route("GET /api/my/access", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.WhoViewed(r.Context(), u)
	})
	route("POST /api/my/consents", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var c store.Consents
		if err := readJSON(r, &c); err != nil {
			return nil, err
		}
		return ok, s.store.SetConsents(r.Context(), u, c)
	})

	// Records (opening is a POST: it may carry an emergency reason, and it is logged)
	route("POST /api/patients/{id}/open", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in struct {
			BreakGlass string `json:"break_glass"`
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.OpenRecord(r.Context(), u, id(r), in.BreakGlass)
	})
	route("POST /api/patients/{id}/encounters", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.NewEncounter
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return ok, s.store.AddEncounter(r.Context(), u, id(r), in)
	})
	route("GET /api/patients/{id}/fhir", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		b, err := s.store.FHIR(r.Context(), u, id(r))
		if err == nil {
			w.Header().Set("Content-Type", "application/fhir+json")
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(b)
		}
		return nil, err
	})

	// Beds
	route("GET /api/wards", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Wards(r.Context(), u)
	})
	route("POST /api/admissions", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.AdmitRequest
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.RequestAdmission(r.Context(), u, in)
	})
	route("POST /api/admissions/{id}/bed", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in struct {
			BedID int64 `json:"bed_id"`
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return ok, s.store.AssignBed(r.Context(), u, id(r), in.BedID)
	})
	route("POST /api/admissions/{id}/discharge", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return ok, s.store.Discharge(r.Context(), u, id(r))
	})

	// Blood bank
	route("GET /api/bloodbank", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.BloodBank(r.Context(), u)
	})
	route("POST /api/blood-requests", 201, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in store.NewBloodRequest
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return s.store.RequestBlood(r.Context(), u, in)
	})
	route("POST /api/blood-requests/{id}/reserve", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Reserve(r.Context(), u, id(r))
	})
	route("POST /api/blood-requests/{id}/issue", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Issue(r.Context(), u, id(r))
	})
	route("GET /api/blood-requests/{id}/donors", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Donors(r.Context(), u, id(r))
	})
	route("POST /api/blood-requests/{id}/ask", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in struct {
			Donors []int64 `json:"donors"`
		}
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		n, err := s.store.AskDonors(r.Context(), u, id(r), in.Donors)
		return map[string]int{"sent": n}, err
	})
	route("POST /api/bloodbank/discard", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		n, err := s.store.DiscardExpired(r.Context(), u)
		return map[string]int64{"discarded": n}, err
	})

	// Oversight
	route("GET /api/access", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		return s.store.AccessLog(r.Context(), u, r.URL.Query().Get("glass") == "1", before)
	})
	route("POST /api/access/{id}/review", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		var in struct{ Outcome, Note string }
		if err := readJSON(r, &in); err != nil {
			return nil, err
		}
		return ok, s.store.ReviewGlass(r.Context(), u, id(r), in.Outcome, in.Note)
	})
	route("GET /api/outbox", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		return s.store.Outbox(r.Context(), u)
	})
	route("POST /api/demo/reset", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		if !s.cfg.Demo {
			return nil, store.ErrNotFound
		}
		if u.Role != domain.Admin {
			return nil, store.ErrForbidden
		}
		if err := s.Reset(r.Context()); err != nil {
			return nil, err
		}
		s.setCookie(w, r, "", -1)
		return ok, nil
	})
	route("GET /api/stream", 200, func(w http.ResponseWriter, r *http.Request, u *auth.User) (any, error) {
		s.stream(w, r, topicFilter(u))
		return nil, nil
	})

	if s.cfg.Static != nil {
		mux.Handle("GET /", s.spa())
	}
	return s.middleware(mux)
}

// Reset reseeds the demo and restarts the demo clock at 10:15.
func (s *Server) Reset(ctx context.Context) error {
	now := time.Now().In(IST)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, IST)
	c := &seed.Clock{Start: time.Now(), Day: day}
	if err := seed.Reset(ctx, s.db, c.Now(), s.auth.Cost); err != nil {
		return err
	}
	s.clock.Start, s.clock.Day = c.Start, c.Day
	for _, t := range []string{"beds", "blood", "glass", "opd:all"} {
		s.publish(t)
	}
	return nil
}

// topicFilter: which live topics a user hears about. Notices carry only a
// topic; the browser refetches through the API, which applies the rules.
func topicFilter(u *auth.User) func(string) bool {
	return func(t string) bool {
		switch {
		case strings.HasPrefix(t, "opd:"):
			return u.Role == domain.Admin || u.Role == domain.Receptionist || u.Role == domain.Doctor || u.Role == domain.Patient
		case strings.HasPrefix(t, "patient:"):
			if u.Role == domain.Patient {
				return u.PatientID != nil && t == fmt.Sprintf("patient:%d", *u.PatientID)
			}
			return u.Role == domain.Doctor || u.Role == domain.Nurse
		case t == "beds":
			return u.Role != domain.Patient && u.Role != domain.BloodBank
		case t == "blood":
			return u.Role == domain.BloodBank || u.Role == domain.Doctor || u.Role == domain.Admin
		case t == "glass":
			return u.Role == domain.Admin
		}
		return false
	}
}

func (s *Server) publish(topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, want := range s.subs {
		if want(topic) {
			select {
			case ch <- topic:
			default:
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
			if _, err := c.Exec(ctx, "listen arogya"); err != nil {
				return err
			}
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
		if p != "" && !strings.HasPrefix(p, "api/") {
			if f, err := s.cfg.Static.Open(p); err == nil {
				f.Close()
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		if strings.HasPrefix(p, "api/") {
			writeErr(w, store.ErrNotFound)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, s.cfg.Static, "index.html")
	})
}

func (s *Server) middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if s.https(r) {
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			hd.Set("Cache-Control", "no-store")
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if r.Header.Get("X-Arogya") != "1" || !s.sameOrigin(r) {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
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
		writeJSON(w, 403, map[string]string{"error": "not allowed for your role"})
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
