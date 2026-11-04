// Command kaasu runs the personal finance app.
//
//	DATABASE_URL=postgres://... DEMO=1 go run ./cmd/kaasu     # http://127.0.0.1:8080
//
// Environment:
//
//	DATABASE_URL  Postgres connection string (required)
//	ADDR          listen address, default 127.0.0.1:8080. Hosting platforms
//	              that route public traffic into the container set it
//	              explicitly (0.0.0.0:10000 on Render, see render.yaml).
//	DEMO          "1": demo people and history, one-click demo logins, and a
//	              reset every 6 hours. All money is simulated either way.
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

	"github.com/Aravindh97/kaasu/internal/api"
	"github.com/Aravindh97/kaasu/internal/auth"
	"github.com/Aravindh97/kaasu/internal/db"
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
	s := api.New(cfg, pool, auth.New(pool), log, nil)
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
	// The month-end job: every hour, make last month's report for anyone who
	// doesn't have one. Safe on several instances (see store.RunMonthEnd).
	go func() {
		for {
			n := s.Store().Now()
			last := time.Date(n.Year(), n.Month()-1, 1, 0, 0, 0, 0, n.Location())
			if made, err := s.Store().RunMonthEnd(ctx, last); err != nil {
				log.Error("month-end", "err", err)
			} else if made > 0 {
				log.Info("month-end", "month", last.Format("2006-01"), "reports", made)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()
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
