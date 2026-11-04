// Package api is Thari's HTTP layer: JSON endpoints under /api, a
// Server-Sent Events stream for live updates, and the React app.
//
// Sessions live in an HttpOnly, SameSite=Lax cookie. Every request that
// changes something must also carry the header "X-Thari: 1" and, if the
// browser sends an Origin, come from this site. A page on another site
// cannot add custom headers to a cross-site request without a CORS
// preflight, which this server never approves, so it cannot act as a
// signed-in user (CSRF).
package api

import (
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
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/thari/internal/auth"
	"github.com/Aravindh97/thari/internal/domain"
	"github.com/Aravindh97/thari/internal/live"
	"github.com/Aravindh97/thari/internal/seed"
	"github.com/Aravindh97/thari/internal/store"
)

const cookieName = "thari_session"

type Config struct {
	Demo       bool  // one-click demo logins and the reset button
	TrustProxy bool  // client IP and scheme from X-Forwarded-* (only behind a proxy that sets them)
	Static     fs.FS // the built React app
	// Today overrides the date, for tests.
	Today func() time.Time
}

type Server struct {
	cfg   Config
	db    *pgxpool.Pool
	store *store.Store
	auth  *auth.Auth
	hub   *live.Hub
	log   *slog.Logger
}

func New(cfg Config, db *pgxpool.Pool, a *auth.Auth, hub *live.Hub, log *slog.Logger) *Server {
	if cfg.Today == nil {
		ist, _ := time.LoadLocation("Asia/Kolkata")
		cfg.Today = func() time.Time {
			y, m, d := time.Now().In(ist).Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) // the factory's calendar date
		}
	}
	return &Server{cfg: cfg, db: db, store: &store.Store{DB: db}, auth: a, hub: hub, log: log}
}

// Hub is where committed changes are published to connected browsers.
func (s *Server) Hub() *live.Hub { return s.hub }

type handler func(w http.ResponseWriter, r *http.Request, u *auth.User) error

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	open := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	authed := func(pattern string, h handler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			u, err := s.user(r)
			if err != nil {
				writeErr(w, err)
				return
			}
			if err := h(w, r, u); err != nil {
				writeErr(w, err)
			}
		})
	}

	open("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	open("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		var demo []seed.DemoUser
		if s.cfg.Demo {
			demo = seed.Users
		}
		writeJSON(w, 200, map[string]any{"demo": s.cfg.Demo, "demo_users": demo, "stages": domain.Stages, "stage_labels": domain.StageLabel})
	})
	open("POST /api/login", s.login)
	open("POST /api/demo-login", s.demoLogin)
	authed("POST /api/logout", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		if c, err := r.Cookie(cookieName); err == nil {
			s.auth.Logout(r.Context(), c.Value)
		}
		s.setCookie(w, r, "", -1)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	})
	authed("GET /api/me", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		writeJSON(w, 200, u)
		return nil
	})
	authed("POST /api/me/password", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		var in struct{ Old, New string }
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if err := s.auth.ChangePassword(r.Context(), u, in.Old, in.New); err != nil {
			if errors.Is(err, auth.ErrBadLogin) {
				return &store.InvalidError{Field: "old", Msg: "current password is wrong"}
			}
			return &store.InvalidError{Field: "new", Msg: err.Error()}
		}
		s.setCookie(w, r, "", -1)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	})

	authed("GET /api/dashboard", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		d, err := s.store.Dashboard(r.Context(), u.Principal, s.cfg.Today())
		if err == nil {
			writeJSON(w, 200, d)
		}
		return err
	})
	authed("GET /api/orders", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		q := r.URL.Query()
		buyer, _ := strconv.ParseInt(q.Get("buyer"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		orders, next, err := s.store.ListOrders(r.Context(), u.Principal, store.Filter{Status: q.Get("status"), BuyerID: buyer,
			Risk: q.Get("risk"), Stage: q.Get("stage"), Q: q.Get("q"), Cursor: q.Get("cursor"), Limit: limit}, s.cfg.Today())
		if err == nil {
			if orders == nil {
				orders = []store.Order{}
			}
			writeJSON(w, 200, map[string]any{"orders": orders, "next_cursor": next})
		}
		return err
	})
	authed("POST /api/orders", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		var in store.OrderInput
		if err := readJSON(r, &in); err != nil {
			return err
		}
		o, err := s.store.CreateOrder(r.Context(), u.Principal, in, s.cfg.Today())
		if err == nil {
			writeJSON(w, 201, o)
		}
		return err
	})
	authed("GET /api/orders/{id}", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		d, err := s.store.GetOrder(r.Context(), u.Principal, pathID(r), s.cfg.Today())
		if err == nil {
			writeJSON(w, 200, d)
		}
		return err
	})
	authed("PATCH /api/orders/{id}", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		var in store.OrderPatch
		if err := readJSON(r, &in); err != nil {
			return err
		}
		o, err := s.store.UpdateOrder(r.Context(), u.Principal, pathID(r), in, s.cfg.Today())
		if err == nil {
			writeJSON(w, 200, o)
		}
		return err
	})
	authed("POST /api/orders/{id}/progress", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		var in struct {
			Stage string `json:"stage"`
			Delta int    `json:"delta"`
			Note  string `json:"note"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		o, err := s.store.AddProgress(r.Context(), u.Principal, pathID(r), in.Stage, in.Delta, in.Note, s.cfg.Today())
		if err == nil {
			writeJSON(w, 200, o)
		}
		return err
	})
	authed("POST /api/orders/{id}/comments", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		var in struct {
			Text     string `json:"text"`
			Internal bool   `json:"internal"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		err := s.store.Comment(r.Context(), u.Principal, pathID(r), in.Text, in.Internal, s.cfg.Today())
		if err == nil {
			writeJSON(w, 201, map[string]bool{"ok": true})
		}
		return err
	})
	authed("POST /api/orders/{id}/inspections", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		var in struct {
			Critical int    `json:"critical"`
			Major    int    `json:"major"`
			Minor    int    `json:"minor"`
			Notes    string `json:"notes"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		ins, err := s.store.Inspect(r.Context(), u.Principal, pathID(r), in.Critical, in.Major, in.Minor, in.Notes, s.cfg.Today())
		if err == nil {
			writeJSON(w, 201, ins)
		}
		return err
	})
	authed("POST /api/orders/{id}/ship", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		o, err := s.store.Ship(r.Context(), u.Principal, pathID(r), s.cfg.Today())
		if err == nil {
			writeJSON(w, 200, o)
		}
		return err
	})
	authed("POST /api/orders/{id}/files", s.upload)
	authed("GET /api/files/{id}", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		name, ct, data, err := s.store.File(r.Context(), u.Principal, pathID(r))
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("Content-Security-Policy", "sandbox")
		w.Write(data)
		return nil
	})
	authed("GET /api/aql", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		lot, _ := strconv.Atoi(r.URL.Query().Get("lot"))
		if lot < 1 || lot > 10000000 {
			return &store.InvalidError{Field: "lot", Msg: "lot size must be 1 to 10,000,000"}
		}
		writeJSON(w, 200, domain.PlanFor(lot))
		return nil
	})
	authed("GET /api/buyers", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		b, err := s.store.Buyers(r.Context(), u.Principal)
		if err == nil {
			writeJSON(w, 200, b)
		}
		return err
	})
	authed("GET /api/users", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		us, err := s.auth.Users(r.Context(), u)
		if errors.Is(err, domain.ErrNotAllowed) {
			return store.ErrForbidden
		}
		if err == nil {
			writeJSON(w, 200, us)
		}
		return err
	})
	authed("POST /api/users", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		var in auth.NewUser
		if err := readJSON(r, &in); err != nil {
			return err
		}
		nu, pw, err := s.auth.CreateUser(r.Context(), u, in)
		if errors.Is(err, domain.ErrNotAllowed) {
			return store.ErrForbidden
		}
		if err != nil {
			return &store.InvalidError{Msg: err.Error()}
		}
		writeJSON(w, 201, map[string]any{"user": nu, "password": pw})
		return nil
	})
	authed("POST /api/users/{id}/active", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		var in struct {
			Active bool `json:"active"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		err := s.auth.SetActive(r.Context(), u, pathID(r), in.Active)
		if errors.Is(err, domain.ErrNotAllowed) {
			return store.ErrForbidden
		}
		if err != nil {
			return &store.InvalidError{Msg: err.Error()}
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	})
	authed("GET /api/audit", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		ev, err := s.store.Audit(r.Context(), u.Principal, before, 50)
		if err == nil {
			writeJSON(w, 200, ev)
		}
		return err
	})
	authed("POST /api/demo/reset", func(w http.ResponseWriter, r *http.Request, u *auth.User) error {
		if !s.cfg.Demo {
			return store.ErrNotFound
		}
		if !u.CanDo(domain.ManageUsers) || u.Role == domain.Buyer {
			return store.ErrForbidden
		}
		if err := seed.Reset(r.Context(), s.db, s.cfg.Today(), s.auth.Cost); err != nil {
			return err
		}
		s.setCookie(w, r, "", -1) // every session was deleted
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	})
	authed("GET /api/stream", s.stream)

	if s.cfg.Static != nil {
		mux.Handle("GET /", s.spa())
	}
	return s.middleware(mux)
}

// spa serves built files, and index.html for client-side routes like
// /orders/12 so deep links and reloads work.
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
				if r.Header.Get("X-Thari") != "1" || !s.sameOrigin(r) {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, store.MaxFile+64<<10)
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
		return true // not a browser cross-site request; the custom header is still required
	}
	scheme := "http"
	if s.https(r) {
		scheme = "https"
	}
	return o == scheme+"://"+r.Host
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, tok string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: s.https(r), SameSite: http.SameSiteLaxMode})
}

func (s *Server) user(r *http.Request) (*auth.User, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil, auth.ErrNoSession
	}
	return s.auth.Session(r.Context(), c.Value)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	tok, u, err := s.auth.Login(r.Context(), in.Email, in.Password, s.clientIP(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.setCookie(w, r, tok, int(auth.SessionTTL.Seconds()))
	writeJSON(w, 200, u)
}

func (s *Server) demoLogin(w http.ResponseWriter, r *http.Request) {
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
	if err != nil {
		writeErr(w, err)
		return
	}
	s.setCookie(w, r, tok, int(auth.SessionTTL.Seconds()))
	writeJSON(w, 200, u)
}

var allowedUploads = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, u *auth.User) error {
	f, hdr, err := r.FormFile("file")
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &store.InvalidError{Field: "file", Msg: "files can be up to 2 MB"}
		}
		return &store.InvalidError{Field: "file", Msg: "choose a file"}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, store.MaxFile+1))
	if err != nil {
		return err
	}
	// The type comes from the bytes, not the browser's claim.
	ct := http.DetectContentType(data)
	if !allowedUploads[ct] {
		return &store.InvalidError{Field: "file", Msg: "only PDF, PNG and JPEG files are accepted"}
	}
	fi, err := s.store.AddFile(r.Context(), u.Principal, pathID(r), hdr.Filename, ct, data, s.cfg.Today())
	if err == nil {
		writeJSON(w, 201, fi)
	}
	return err
}

// stream sends a server-sent event for each change the user may see. The
// browser refetches what it shows; the event itself carries only ids.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, u *auth.User) error {
	fl, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	sub := s.hub.Subscribe(u.Principal)
	defer s.hub.Unsubscribe(sub)
	fmt.Fprint(w, "retry: 3000\n: connected\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	// re-check the session now and then, so a disabled account stops receiving
	recheck := time.NewTicker(time.Minute)
	defer recheck.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case n := <-sub.C:
			b, _ := json.Marshal(map[string]any{"order_id": n.OrderID, "kind": n.Kind, "event_id": n.EventID})
			fmt.Fprintf(w, "event: change\ndata: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-recheck.C:
			if _, err := s.user(r); err != nil {
				return nil
			}
		}
	}
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func readJSON(r *http.Request, v any) error {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		return &store.InvalidError{Msg: "send JSON"}
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return &store.InvalidError{Msg: "invalid request: " + err.Error()}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var inv *store.InvalidError
	var conflict *store.ConflictError
	switch {
	case errors.As(err, &inv):
		writeJSON(w, http.StatusUnprocessableEntity, inv)
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": conflict.Error(), "current": conflict.Current})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, store.ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not allowed for your role"})
	case errors.Is(err, auth.ErrNoSession):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "please sign in"})
	case errors.Is(err, auth.ErrBadLogin):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
	case errors.Is(err, auth.ErrThrottled):
		w.Header().Set("Retry-After", "600")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
	case errors.Is(err, context.Canceled):
	default:
		slog.Error("request", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "something went wrong"})
	}
}
