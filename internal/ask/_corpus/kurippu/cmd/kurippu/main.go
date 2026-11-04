// Command kurippu runs the notes sync server.
//
//	DATABASE_URL=postgres://... go run ./cmd/kurippu      # http://127.0.0.1:8080
//
// Environment:
//
//	DATABASE_URL  Postgres connection string (required)
//	ADDR          listen address, default 127.0.0.1:8080. Hosting platforms
//	              that route public traffic into the container set it
//	              explicitly (0.0.0.0:10000 on Render, see render.yaml).
//	TRUST_PROXY   "1" behind a proxy that sets X-Forwarded-For
//	STATIC_DIR    the built React app to serve
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aravindh97/kurippu/internal/server"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err == nil {
		err = db.Ping(ctx)
	}
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := server.Migrate(ctx, db); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	cfg := server.DefaultConfig()
	cfg.TrustProxy = os.Getenv("TRUST_PROXY") == "1"
	if d := os.Getenv("STATIC_DIR"); d != "" {
		cfg.Static = os.DirFS(d)
	}
	s := server.New(cfg, db, log)
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
	log.Info("listening", "addr", addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
