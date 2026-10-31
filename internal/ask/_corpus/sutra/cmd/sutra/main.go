// Command sutra syncs enterprise systems into one entity model and serves it
// to agents.
//
//	sutra seed-source [--reset]     create and fill the demo ERP vendors table (SOURCE_DATABASE_URL)
//	sutra fake-crm                  run the demo CRM API on 127.0.0.1 (needs SUTRA_CRM_TOKEN)
//	sutra sync SOURCE [--once]      run a connector (postgres_cdc or rest) from config
//	sutra resolve                   entity resolution -> golden records, lineage, review queue
//	sutra find ATTR VALUE           look up entities
//	sutra get ENTITY_ID             golden record with lineage and members
//	sutra mcp                       MCP server on stdio
//	sutra eval                      entity-resolution eval on labelled synthetic data (no DB needed)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Aravindh97/sutra/internal/connector/pgcdc"
	"github.com/Aravindh97/sutra/internal/connector/rest"
	"github.com/Aravindh97/sutra/internal/eval"
	"github.com/Aravindh97/sutra/internal/fakecrm"
	"github.com/Aravindh97/sutra/internal/mcp"
	"github.com/Aravindh97/sutra/internal/normalize"
	"github.com/Aravindh97/sutra/internal/ontology"
	"github.com/Aravindh97/sutra/internal/resolve"
	"github.com/Aravindh97/sutra/internal/synth"
	"github.com/Aravindh97/sutra/internal/warehouse"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func need(k string) (string, error) {
	if v := os.Getenv(k); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("set %s", k)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sutra seed-source|fake-crm|sync|resolve|find|get|mcp|eval")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func arg(pos []string, i int) string {
	if i < len(pos) {
		return pos[i]
	}
	return ""
}

func loadOntology() (*ontology.Ontology, error) {
	return ontology.Load(env("SUTRA_ONTOLOGY", "config/ontology.yaml"))
}

func openWH(ctx context.Context) (*warehouse.DB, error) {
	u, err := need("DATABASE_URL")
	if err != nil {
		return nil, err
	}
	return warehouse.Open(ctx, u)
}

// parseFlags lets flags appear before or after positional arguments
// (`sutra sync crm_accounts --once`). The standard flag package stops at the
// first positional argument, which silently ignored --once.
func parseFlags(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			return pos
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	seed := fs.Uint64("seed", 7, "synthetic data seed")
	n := fs.Int("n", 300, "synthetic companies")
	switch cmd {
	case "seed-source":
		reset := fs.Bool("reset", false, "drop and recreate the table")
		parseFlags(fs, args)
		u, err := need("SOURCE_DATABASE_URL")
		if err != nil {
			return err
		}
		c, err := pgx.Connect(ctx, u)
		if err != nil {
			return err
		}
		defer c.Close(ctx)
		if *reset {
			if _, err := c.Exec(ctx, `DROP TABLE IF EXISTS vendors`); err != nil {
				return err
			}
		}
		if _, err := c.Exec(ctx, `CREATE TABLE IF NOT EXISTS vendors (
			vendor_id int PRIMARY KEY, vendor_name text NOT NULL, gst_no text, contact_phone text, city text, category text,
			updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		d := synth.Generate(*seed, *n)
		rows := make([][]any, 0, len(d.ERP))
		for _, r := range d.ERP {
			f := r.Fields
			var id int
			fmt.Sscan(f["vendor_id"], &id)
			rows = append(rows, []any{id, f["vendor_name"], f["gst_no"], f["contact_phone"], f["city"], f["category"]})
		}
		cnt, err := c.CopyFrom(ctx, pgx.Identifier{"vendors"}, []string{"vendor_id", "vendor_name", "gst_no", "contact_phone", "city", "category"}, pgx.CopyFromRows(rows))
		fmt.Printf("inserted %d vendors\n", cnt)
		return err

	case "fake-crm":
		addr := fs.String("addr", "127.0.0.1:8089", "listen address")
		failEvery := fs.Int("fail-every", 0, "fail every Nth request with 429/503")
		parseFlags(fs, args)
		tok, err := need("SUTRA_CRM_TOKEN")
		if err != nil {
			return err
		}
		host, _, err := net.SplitHostPort(*addr)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("fake-crm only listens on a loopback address")
		}
		d := synth.Generate(*seed, *n)
		var rows []map[string]string
		for _, r := range d.CRM {
			rows = append(rows, r.Fields)
		}
		srv := fakecrm.New(tok, rows)
		srv.FailEvery = *failEvery
		hs := &http.Server{Addr: *addr, Handler: srv, ReadHeaderTimeout: 5 * time.Second}
		go func() { <-ctx.Done(); hs.Shutdown(context.Background()) }()
		fmt.Fprintf(os.Stderr, "fake CRM with %d accounts on http://%s\n", len(rows), *addr)
		if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil

	case "sync":
		once := fs.Bool("once", false, "stop when caught up")
		pos := parseFlags(fs, args)
		o, err := loadOntology()
		if err != nil {
			return err
		}
		src, ok := o.Sources[arg(pos, 0)]
		if !ok {
			return fmt.Errorf("unknown source %q", arg(pos, 0))
		}
		wh, err := openWH(ctx)
		if err != nil {
			return err
		}
		defer wh.Pool.Close()
		switch src.Kind {
		case "postgres_cdc":
			u, err := need("SOURCE_DATABASE_URL")
			if err != nil {
				return err
			}
			c := &pgcdc.Connector{SourceURL: u, Source: src, WH: wh}
			if *once {
				c.IdleExit = 3 * time.Second
			}
			return c.Run(ctx)
		case "rest":
			base, err := need("SUTRA_CRM_URL")
			if err != nil {
				return err
			}
			tok, err := need("SUTRA_CRM_TOKEN")
			if err != nil {
				return err
			}
			c := &rest.Connector{BaseURL: base, Token: tok, Source: src, WH: wh}
			for {
				got, err := c.Sync(ctx)
				if err != nil {
					return err
				}
				slog.Info("rest sync", "source", src.Name, "records", got)
				if *once {
					return nil
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(30 * time.Second):
				}
			}
		}
		return fmt.Errorf("unsupported kind %q", src.Kind)

	case "resolve":
		entity := fs.String("entity", "Company", "entity type")
		parseFlags(fs, args)
		o, err := loadOntology()
		if err != nil {
			return err
		}
		wh, err := openWH(ctx)
		if err != nil {
			return err
		}
		defer wh.Pool.Close()
		recs, err := wh.Records(ctx, *entity)
		if err != nil {
			return err
		}
		prio := map[string]int{}
		for _, s := range o.SourcesFor(*entity) {
			prio[s.Name] = s.Priority
		}
		cfg := resolve.DefaultConfig()
		if cfg.Previous, err = wh.Assignments(ctx, *entity); err != nil {
			return err
		}
		t := time.Now()
		res := resolve.Resolve(recs, prio, cfg)
		if err := wh.SaveResolution(ctx, *entity, res); err != nil {
			return err
		}
		fmt.Printf("records=%d entities=%d review=%d pairs_scored=%d took=%s\n", len(recs), len(res.Entities), len(res.Review), res.Compared, time.Since(t).Round(time.Millisecond))
		return nil

	case "find":
		entity := fs.String("entity", "Company", "entity type")
		pos := parseFlags(fs, args)
		wh, err := openWH(ctx)
		if err != nil {
			return err
		}
		defer wh.Pool.Close()
		attr, val := arg(pos, 0), strings.Join(pos[min(1, len(pos)):], " ")
		switch attr {
		case "gstin":
			val, _ = normalize.GSTIN(val)
		case "pan":
			val, _ = normalize.PAN(val)
		case "phone":
			val = normalize.Phone(val)
		}
		res, err := wh.Find(ctx, *entity, attr, val, 20)
		if err != nil {
			return err
		}
		for _, e := range res {
			fmt.Printf("%s  %s  gstin=%s city=%s\n", e.ID, e.Golden["name"], e.Golden["gstin"], e.Golden["city"])
		}
		return nil

	case "get":
		pos := parseFlags(fs, args)
		wh, err := openWH(ctx)
		if err != nil {
			return err
		}
		defer wh.Pool.Close()
		e, err := wh.Get(ctx, arg(pos, 0))
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(e)

	case "mcp":
		parseFlags(fs, args)
		o, err := loadOntology()
		if err != nil {
			return err
		}
		wh, err := openWH(ctx)
		if err != nil {
			return err
		}
		defer wh.Pool.Close()
		return (&mcp.Server{WH: wh, Ontology: o}).Serve(ctx, os.Stdin, os.Stdout)

	case "eval":
		verbose := fs.Bool("v", false, "print sample errors")
		heldout := fs.Bool("heldout", false, "also run held-out seeds 101..505 at n and 2000")
		parseFlags(fs, args)
		o, err := loadOntology()
		if err != nil {
			return err
		}
		r, err := eval.Run(o, synth.Generate(*seed, *n), resolve.DefaultConfig())
		if err != nil {
			return err
		}
		fmt.Printf("dev set: seed=%d n=%d\n", *seed, *n)
		eval.Print(os.Stdout, r, *verbose)
		if *heldout {
			fmt.Printf("\nheld-out seeds (rules were not tuned on these)\n%-6s %-5s %-11s %9s %7s %6s %7s\n", "n", "seed", "method", "precision", "recall", "f1", "review")
			sizes := []int{*n, 2000}
			sort.Ints(sizes)
			for _, size := range sizes {
				for _, s := range []uint64{101, 202, 303, 404, 505} {
					r, err := eval.Run(o, synth.Generate(s, size), resolve.DefaultConfig())
					if err != nil {
						return err
					}
					for _, m := range r.Rows {
						if m.Name != "exact name" {
							fmt.Printf("%-6d %-5d %-11s %9.3f %7.3f %6.3f %7d\n", size, s, m.Name, m.Precision, m.Recall, m.F1, m.Review)
						}
					}
				}
			}
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}
