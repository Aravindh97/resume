// Command smriti is a long-term memory service for LLM agents.
//
//	smriti worker                 Temporal worker (write path + maintenance)
//	smriti remember FILE.json     write a conversation through Temporal (--local to run in-process)
//	smriti recall QUERY           recall memories for the configured principal
//	smriti history SUBJECT        every version of a subject
//	smriti maintain               expire + consolidate once (--local) or via Temporal
//	smriti schedule               create the nightly maintenance schedule
//	smriti mcp                    MCP server on stdio
//	smriti eval                   run the multi-session memory eval
//
// The principal comes from SMRITI_ORG, SMRITI_USER, SMRITI_TEAMS (comma
// separated) and SMRITI_ORG_WRITER=true, set by the launching application.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/Aravindh97/smriti/internal/embed"
	"github.com/Aravindh97/smriti/internal/eval"
	"github.com/Aravindh97/smriti/internal/extract"
	"github.com/Aravindh97/smriti/internal/jobs"
	"github.com/Aravindh97/smriti/internal/mcp"
	"github.com/Aravindh97/smriti/internal/memory"
	"github.com/Aravindh97/smriti/internal/store"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func principal() (memory.Principal, error) {
	p := memory.Principal{OrgID: os.Getenv("SMRITI_ORG"), UserID: os.Getenv("SMRITI_USER"),
		OrgWriter: os.Getenv("SMRITI_ORG_WRITER") == "true"}
	if t := os.Getenv("SMRITI_TEAMS"); t != "" {
		p.Teams = strings.Split(t, ",")
	}
	if p.OrgID == "" || p.UserID == "" {
		return p, fmt.Errorf("set SMRITI_ORG and SMRITI_USER")
	}
	return p, nil
}

func extractor() extract.Extractor {
	if u := os.Getenv("SMRITI_LLM_URL"); u != "" {
		return extract.NewLLM(u, env("SMRITI_LLM_MODEL", "gpt-4o-mini"), os.Getenv("SMRITI_LLM_API_KEY"))
	}
	return extract.Rules{}
}

func open(ctx context.Context) (*store.Store, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, fmt.Errorf("set DATABASE_URL (Postgres with pgvector)")
	}
	dim, _ := strconv.Atoi(env("EMBED_DIM", "384"))
	return store.Open(ctx, url, embed.NewHashing(dim))
}

func tclient() (client.Client, error) {
	return client.Dial(client.Options{HostPort: env("TEMPORAL_ADDRESS", "127.0.0.1:7233")})
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: smriti worker|remember|recall|history|maintain|schedule|mcp|eval")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func run(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "worker":
		st, err := open(ctx)
		if err != nil {
			return err
		}
		c, err := tclient()
		if err != nil {
			return err
		}
		defer c.Close()
		w := worker.New(c, jobs.TaskQueue, worker.Options{})
		w.RegisterWorkflow(jobs.RememberConversation)
		w.RegisterWorkflow(jobs.Maintain)
		w.RegisterActivity(&jobs.Activities{Store: st, Extractor: extractor()})
		return w.Run(worker.InterruptCh())

	case "remember":
		local := fs.Bool("local", false, "run in-process")
		team := fs.String("team", "", "team context")
		fs.Parse(args)
		p, err := principal()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		var conv memory.Conversation
		if err := json.Unmarshal(b, &conv); err != nil {
			return err
		}
		in := jobs.RememberInput{Principal: p, Conversation: conv, Team: *team}
		var res []store.WriteResult
		if *local {
			st, err := open(ctx)
			if err != nil {
				return err
			}
			a := &jobs.Activities{Store: st, Extractor: extractor()}
			cands, err := a.Extract(ctx, in)
			if err != nil {
				return err
			}
			if res, err = a.Write(ctx, p, cands); err != nil {
				return err
			}
		} else {
			c, err := tclient()
			if err != nil {
				return err
			}
			defer c.Close()
			// Workflow id from the conversation id: submitting the same
			// conversation twice while it runs is rejected by Temporal.
			r, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "remember-" + p.OrgID + "-" + conv.ID,
				TaskQueue: jobs.TaskQueue}, jobs.RememberConversation, in)
			if err != nil {
				return err
			}
			if err := r.Get(ctx, &res); err != nil {
				return err
			}
		}
		return printJSON(res)

	case "recall":
		k := fs.Int("k", 5, "results")
		asOf := fs.String("as-of", "", "RFC3339 time")
		fs.Parse(args)
		p, err := principal()
		if err != nil {
			return err
		}
		st, err := open(ctx)
		if err != nil {
			return err
		}
		o := store.RecallOptions{K: *k}
		if *asOf != "" {
			t, err := time.Parse(time.RFC3339, *asOf)
			if err != nil {
				return err
			}
			o.AsOf = &t
		}
		res, err := st.Recall(ctx, p, strings.Join(fs.Args(), " "), o)
		if err != nil {
			return err
		}
		for _, r := range res {
			fmt.Printf("%.3f  [%s/%s] %s  (subject=%s, confirmed %dx, since %s, source %s)\n", r.Score, r.Memory.Scope,
				r.Memory.OwnerID, r.Memory.Content, r.Memory.Subject, r.Memory.Confirmations, r.Memory.ValidFrom.Format("2006-01-02"), r.Memory.SourceRef)
		}
		return nil

	case "history":
		fs.Parse(args)
		p, err := principal()
		if err != nil {
			return err
		}
		st, err := open(ctx)
		if err != nil {
			return err
		}
		h, err := st.History(ctx, p, strings.Join(fs.Args(), " "))
		if err != nil {
			return err
		}
		for _, m := range h {
			to := "current"
			if m.ValidTo != nil {
				to = m.ValidTo.Format("2006-01-02")
			}
			fmt.Printf("%s .. %-10s [%s/%s] %s (source %s)\n", m.ValidFrom.Format("2006-01-02"), to, m.Scope, m.OwnerID, m.Content, m.SourceRef)
		}
		return nil

	case "maintain":
		local := fs.Bool("local", false, "run in-process")
		fs.Parse(args)
		if *local {
			st, err := open(ctx)
			if err != nil {
				return err
			}
			e, err := st.ExpireDue(ctx)
			if err != nil {
				return err
			}
			m, err := st.Consolidate(ctx, 0.95, 500)
			fmt.Printf("expired=%d merged=%d\n", e, m)
			return err
		}
		c, err := tclient()
		if err != nil {
			return err
		}
		defer c.Close()
		r, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: jobs.TaskQueue}, jobs.Maintain, 0.95)
		if err != nil {
			return err
		}
		var res jobs.MaintainResult
		if err := r.Get(ctx, &res); err != nil {
			return err
		}
		return printJSON(res)

	case "schedule":
		c, err := tclient()
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.ScheduleClient().Create(ctx, client.ScheduleOptions{
			ID:   "smriti-maintain",
			Spec: client.ScheduleSpec{CronExpressions: []string{env("SMRITI_MAINTAIN_CRON", "0 2 * * *")}},
			Action: &client.ScheduleWorkflowAction{ID: "smriti-maintain", Workflow: jobs.Maintain,
				Args: []any{0.95}, TaskQueue: jobs.TaskQueue},
		})
		if err == nil {
			fmt.Println("created schedule smriti-maintain")
		}
		return err

	case "mcp":
		team := fs.String("team", os.Getenv("SMRITI_TEAM"), "team context")
		fs.Parse(args)
		p, err := principal()
		if err != nil {
			return err
		}
		st, err := open(ctx)
		if err != nil {
			return err
		}
		return (&mcp.Server{Store: st, Extractor: extractor(), Principal: p, Team: *team}).Serve(ctx, os.Stdin, os.Stdout)

	case "eval":
		path := fs.String("scenario", "testdata/eval/scenario.json", "scenario file")
		verbose := fs.Bool("v", false, "per-question rows")
		fs.Parse(args)
		st, err := open(ctx)
		if err != nil {
			return err
		}
		sc, err := eval.Load(*path)
		if err != nil {
			return err
		}
		rep, err := eval.Run(ctx, st, sc, strconv.FormatInt(time.Now().UnixNano(), 36))
		if err != nil {
			return err
		}
		eval.Print(os.Stdout, rep, *verbose)
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}
