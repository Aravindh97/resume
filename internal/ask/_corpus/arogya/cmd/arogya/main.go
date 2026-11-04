// Command arogya runs the hospital platform.
//
//	DATABASE_URL=postgres://... DEMO=1 go run ./cmd/arogya     # http://127.0.0.1:8080
//
// Environment:
//
//	DATABASE_URL  Postgres connection string (required)
//	ADDR          listen address, default 127.0.0.1:8080. Hosting platforms
//	              that route public traffic into the container set it
//	              explicitly (0.0.0.0:10000 on Render, see render.yaml).
//	DEMO          "1": synthetic data, one-click demo logins, a demo clock that
//	              starts at 10:15 so the OPD is always mid-morning, and reset
//	TRUST_PROXY   "1" behind a proxy that sets X-Forwarded-For/-Proto
//	STATIC_DIR    the built React app
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Aravindh97/arogya/internal/api"
	"github.com/Aravindh97/arogya/internal/auth"
	"github.com/Aravindh97/arogya/internal/db"
	"github.com/Aravindh97/arogya/internal/seed"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	cfg := api.Config{Demo: os.Getenv("DEMO") == "1", TrustProxy: os.Getenv("TRUST_PROXY") == "1"}
	if d := os.Getenv("STATIC_DIR"); d != "" {
		cfg.Static = os.DirFS(d)
	}
	var clock *seed.Clock
	if cfg.Demo {
		clock = &seed.Clock{}
	}
	s := api.New(cfg, pool, auth.New(pool), log, clock)
	if cfg.Demo {
		// fresh demo data on every start, and every 6 hours
		if err := s.Reset(ctx); err != nil {
			log.Error("seed", "err", err)
			os.Exit(1)
		}
		go func() {
			for range time.Tick(6 * time.Hour) {
				if err := s.Reset(ctx); err != nil {
					log.Error("reset", "err", err)
				}
			}
		}()
	}
	go s.Listen(ctx)
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	hs := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(c)
	}()
	log.Info("listening", "addr", addr, "demo", cfg.Demo)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
