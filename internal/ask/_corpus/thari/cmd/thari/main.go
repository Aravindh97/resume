// Command thari runs the garment order tracker.
//
//	DATABASE_URL=postgres://... go run ./cmd/thari            # http://127.0.0.1:8080
//	DATABASE_URL=... go run ./cmd/thari create-admin EMAIL NAME
//
// Environment:
//
//	DATABASE_URL  Postgres connection string (required)
//	ADDR          listen address, default 127.0.0.1:8080. Hosting platforms
//	              that route public traffic into the container set it
//	              explicitly (0.0.0.0:10000 on Render, see render.yaml).
//	DEMO          "1": seed demo data, enable one-click demo logins and reset
//	TRUST_PROXY   "1" behind a proxy that sets X-Forwarded-For/-Proto
//	STATIC_DIR    the built React app to serve
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Aravindh97/thari/internal/api"
	"github.com/Aravindh97/thari/internal/auth"
	"github.com/Aravindh97/thari/internal/db"
	"github.com/Aravindh97/thari/internal/live"
	"github.com/Aravindh97/thari/internal/seed"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(2)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	a := auth.New(pool)

	if len(os.Args) > 1 && os.Args[1] == "create-admin" {
		createAdmin(ctx, a, os.Args[2:])
		return
	}

	demo := os.Getenv("DEMO") == "1"
	cfg := api.Config{Demo: demo, TrustProxy: os.Getenv("TRUST_PROXY") == "1"}
	if d := os.Getenv("STATIC_DIR"); d != "" {
		cfg.Static = os.DirFS(d)
	}
	srv := api.New(cfg, pool, a, live.New(log), log)
	if demo {
		ist, _ := time.LoadLocation("Asia/Kolkata")
		y, m, d := time.Now().In(ist).Date()
		if err := seed.Seed(ctx, pool, time.Date(y, m, d, 0, 0, 0, 0, time.UTC), a.Cost); err != nil {
			log.Error("seed", "err", err)
			os.Exit(1)
		}
	}
	go srv.Hub().Listen(ctx, pool)

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", addr, "demo", demo)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

// createAdmin makes the first admin account of a new factory and prints
// its one-time password.
func createAdmin(ctx context.Context, a *auth.Auth, args []string) {
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: thari create-admin EMAIL NAME FACTORY_NAME")
		os.Exit(2)
	}
	email, name, factory := args[0], args[1], args[2]
	pw := auth.RandomPassword()
	h, err := auth.HashPassword(pw, a.Cost)
	if err == nil {
		_, err = a.DB.Exec(ctx, `with f as (insert into orgs (name, kind) values ($3, 'factory') returning id)
			insert into users (org_id, email, name, role, password_hash) select id, lower($1), $2, 'admin', $4 from f`, email, name, factory, h)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("created %s; temporary password (shown once): %s\n", email, pw)
}
