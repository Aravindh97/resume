// Command server runs online Pallanguzhi.
//
//	go run ./cmd/server                      # http://127.0.0.1:8080
//
// Environment:
//
//	ADDR         listen address (default 127.0.0.1:8080). Hosting platforms
//	             that route public traffic into the container set this
//	             explicitly, e.g. 0.0.0.0:10000 on Render (see render.yaml).
//	ORIGINS      comma-separated extra origins allowed to connect, e.g.
//	             https://aravindh97.github.io
//	TRUST_PROXY  "1" to take the client IP from X-Forwarded-For (only behind
//	             a proxy that sets it)
//	STATIC_DIR   directory with the built React app to serve at /
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Aravindh97/pallanguzhi/internal/server"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := server.DefaultConfig()
	for _, o := range strings.Split(os.Getenv("ORIGINS"), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			cfg.Origins = append(cfg.Origins, o)
		}
	}
	cfg.TrustProxy = os.Getenv("TRUST_PROXY") == "1"
	if d := os.Getenv("STATIC_DIR"); d != "" {
		cfg.Static = os.DirFS(d)
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	s := server.New(cfg, log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.Run(ctx)
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", addr, "origins", cfg.Origins, "static", cfg.Static != nil)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
