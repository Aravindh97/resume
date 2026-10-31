// Command site runs one node of the resume mesh.
//
// Single node (local):
//
//	go run ./cmd/site
//
// Four-node mesh on one machine: see scripts/mesh-local.sh.
//
// Flags default to loopback. In production the public address is the
// platform's HTTP port and the raft address must be a private network
// address (on Fly.io, the fly-local-6pn address), never a public one.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Aravindh97/resume/internal/site"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// parsePeers reads "id@region=host:port,..." (region optional).
func parsePeers(s string) ([]site.Peer, error) {
	var out []site.Peer
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		left, addr, ok := strings.Cut(p, "=")
		if !ok || addr == "" {
			return nil, fmt.Errorf("bad peer %q, want id[@region]=host:port", p)
		}
		id, region, _ := strings.Cut(left, "@")
		out = append(out, site.Peer{ID: id, Region: region, RaftAddr: addr})
	}
	return out, nil
}

func main() {
	id := flag.String("id", env("NODE_ID", "node-1"), "node id")
	region := flag.String("region", env("NODE_REGION", "local"), "display region")
	public := flag.String("addr", env("PUBLIC_ADDR", "127.0.0.1:8080"), "public HTTP address")
	private := flag.String("raft-addr", env("RAFT_ADDR", "127.0.0.1:9080"), "private raft address (loopback or private network only)")
	peers := flag.String("peers", env("PEERS", ""), "other nodes: id[@region]=raft-host:port,...")
	flag.Parse()
	ps, err := parsePeers(*peers)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("node", *id)
	s, err := site.New(site.Config{ID: *id, Region: *region, Peers: ps, Public: *public, Private: *private, Log: log})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
