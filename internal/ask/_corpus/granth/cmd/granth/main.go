// Command granth ingests documents and serves hybrid retrieval to agents.
//
//	granth worker                      run the Temporal worker
//	granth ingest [--local] PATH...    ingest files or directories
//	granth search QUERY                hybrid search with citations
//	granth mcp                         MCP server on stdio
//	granth failed                      list documents in the dead-letter state
//	granth retry                       re-run every failed document
//	granth reindex                     re-ingest every indexed document
//	granth eval                        run the retrieval eval
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"github.com/Aravindh97/granth/internal/blob"
	"github.com/Aravindh97/granth/internal/chunk"
	"github.com/Aravindh97/granth/internal/embed"
	"github.com/Aravindh97/granth/internal/eval"
	"github.com/Aravindh97/granth/internal/index"
	"github.com/Aravindh97/granth/internal/mcp"
	"github.com/Aravindh97/granth/internal/parse"
	"github.com/Aravindh97/granth/internal/pipeline"
	"github.com/Aravindh97/granth/internal/search"
	"github.com/Aravindh97/granth/internal/store"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type deps struct {
	acts     *pipeline.Activities
	searcher *search.Searcher
	index    index.Index
	store    store.Store
	close    func()
}

// build wires backends from the environment. Without GRANTH_OPENSEARCH_URL
// and DATABASE_URL it falls back to in-memory backends, which only live for
// the duration of one command.
func build(ctx context.Context) (*deps, error) {
	dim, _ := strconv.Atoi(env("EMBED_DIM", "384"))
	var emb embed.Embedder = embed.NewHashing(dim)
	if u := os.Getenv("EMBED_URL"); u != "" {
		emb = embed.NewHTTP(u, env("EMBED_MODEL", "text-embedding-3-small"), os.Getenv("EMBED_API_KEY"), dim)
	}
	d := &deps{close: func() {}}
	if u := os.Getenv("GRANTH_OPENSEARCH_URL"); u != "" {
		ox, err := index.NewOpenSearch(index.OpenSearchConfig{URL: u, Index: env("GRANTH_INDEX", "granth-chunks"),
			User: env("OPENSEARCH_USER", ""), Password: env("OPENSEARCH_PASSWORD", ""), Dim: dim,
			CACertFile: env("OPENSEARCH_CA_CERT", ""), InsecureSkipVerify: env("OPENSEARCH_INSECURE_SKIP_VERIFY", "") == "true"})
		if err != nil {
			return nil, err
		}
		if err := ox.EnsureIndex(ctx); err != nil {
			return nil, fmt.Errorf("opensearch: %w", err)
		}
		d.index = ox
	} else {
		d.index = index.NewMemory()
	}
	if u := os.Getenv("DATABASE_URL"); u != "" {
		pg, err := store.OpenPostgres(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("postgres: %w", err)
		}
		d.store, d.close = pg, pg.DB.Close
	} else {
		d.store = store.NewMemory()
	}
	blobs, err := blob.New(env("GRANTH_BLOB_DIR", filepath.Join(os.TempDir(), "granth-blobs")))
	if err != nil {
		return nil, err
	}
	var ocr parse.OCR
	if env("OCR", "tesseract") == "tesseract" {
		ocr = parse.Tesseract{Lang: env("OCR_LANG", "eng+hin+tam")}
	}
	d.acts = &pipeline.Activities{Blobs: blobs, Parser: parse.New(ocr), Chunk: chunk.Default(),
		Embedder: emb, Index: d.index, Store: d.store, EmbedBatch: 32}
	d.searcher = search.New(d.index, emb)
	return d, nil
}

func expand(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			out = append(out, a)
			continue
		}
		_ = filepath.WalkDir(a, func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				abs, _ := filepath.Abs(p)
				out = append(out, abs)
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

func temporalClient() (client.Client, error) {
	return client.Dial(client.Options{HostPort: env("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		Namespace: env("TEMPORAL_NAMESPACE", "default"), Logger: tlog.NewStructuredLogger(slogAdapter())})
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: granth worker|ingest|search|mcp|failed|retry|reindex|eval")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "worker":
		fs.Parse(args)
		d, err := build(ctx)
		if err != nil {
			return err
		}
		defer d.close()
		c, err := temporalClient()
		if err != nil {
			return err
		}
		defer c.Close()
		w := worker.New(c, pipeline.TaskQueue, worker.Options{MaxConcurrentActivityExecutionSize: 16})
		w.RegisterWorkflow(pipeline.IngestDocument)
		w.RegisterWorkflow(pipeline.IngestBatch)
		w.RegisterWorkflow(pipeline.Reindex)
		w.RegisterActivity(d.acts)
		return w.Run(worker.InterruptCh())

	case "ingest", "retry":
		local := fs.Bool("local", false, "run in-process instead of on Temporal")
		meta := fs.String("meta", "", "metadata for every document, k=v,k=v")
		conc := fs.Int("concurrency", 8, "documents in flight")
		fs.Parse(args)
		md := map[string]string{}
		for _, kv := range strings.Split(*meta, ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				md[k] = v
			}
		}
		// Starting workflows needs only Temporal; the worker talks to the
		// index and ledger. Local runs and `retry` need the backends here.
		var d *deps
		if *local || cmd == "retry" {
			var err error
			if d, err = build(ctx); err != nil {
				return err
			}
			defer d.close()
		}
		var uris []string
		if cmd == "retry" {
			failed, err := d.store.List(ctx, store.Failed, "", 10000)
			if err != nil {
				return err
			}
			for _, r := range failed {
				uris = append(uris, r.SourceURI)
			}
		} else {
			uris = expand(fs.Args())
		}
		docs := make([]pipeline.IngestInput, len(uris))
		for i, u := range uris {
			docs[i] = pipeline.IngestInput{URI: u, Metadata: md}
		}
		var results []pipeline.IngestResult
		if *local {
			for _, in := range docs {
				results = append(results, pipeline.RunLocal(ctx, d.acts, in))
			}
		} else {
			c, err := temporalClient()
			if err != nil {
				return err
			}
			defer c.Close()
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: fmt.Sprintf("batch-%d", time.Now().UnixNano()),
				TaskQueue: pipeline.TaskQueue}, pipeline.IngestBatch, pipeline.BatchInput{Docs: docs, Concurrency: *conc})
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "started workflow %s\n", run.GetID())
			var br pipeline.BatchResult
			if err := run.Get(ctx, &br); err != nil {
				return err
			}
			results = br.Results
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "STATUS\tCHUNKS\tSTEP\tURI\tERROR")
		for _, r := range results {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.80s\n", r.Status, r.Chunks, r.FailedStep, r.URI, r.Error)
		}
		return tw.Flush()

	case "search":
		k := fs.Int("k", 5, "results")
		mode := fs.String("mode", "hybrid", "hybrid|lexical|vector")
		asJSON := fs.Bool("json", false, "print JSON")
		fs.Parse(args)
		d, err := build(ctx)
		if err != nil {
			return err
		}
		defer d.close()
		resp, err := d.searcher.Search(ctx, search.Request{Query: strings.Join(fs.Args(), " "), K: *k, Mode: search.Mode(*mode)})
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(resp)
		}
		for i, r := range resp.Results {
			fmt.Printf("%d. [%s] (lex=%d vec=%d)\n   %s\n\n", i+1, r.Citation, r.Trace.LexicalRank, r.Trace.VectorRank,
				strings.ReplaceAll(r.Snippet, "\n", "\n   "))
		}
		return nil

	case "mcp":
		fs.Parse(args)
		d, err := build(ctx)
		if err != nil {
			return err
		}
		defer d.close()
		return (&mcp.Server{Searcher: d.searcher, Index: d.index, DefaultMaxTokens: 1500}).Serve(ctx, os.Stdin, os.Stdout)

	case "failed":
		fs.Parse(args)
		d, err := build(ctx)
		if err != nil {
			return err
		}
		defer d.close()
		recs, err := d.store.List(ctx, store.Failed, "", 1000)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "DOC\tSTEP\tATTEMPTS\tURI\tERROR")
		for _, r := range recs {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%.80s\n", r.DocID, r.FailedStep, r.Attempts, r.SourceURI, r.LastError)
		}
		return tw.Flush()

	case "reindex":
		fs.Parse(args)
		c, err := temporalClient()
		if err != nil {
			return err
		}
		defer c.Close()
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "reindex", TaskQueue: pipeline.TaskQueue},
			pipeline.Reindex, pipeline.ReindexInput{PageSize: 100})
		if err != nil {
			return err
		}
		var n int
		if err := run.Get(ctx, &n); err != nil {
			return err
		}
		fmt.Printf("reindexed %d documents\n", n)
		return nil

	case "eval":
		corpus := fs.String("corpus", "testdata/corpus", "documents to index")
		qpath := fs.String("questions", "testdata/eval/questions.jsonl", "labelled questions")
		verbose := fs.Bool("v", false, "print a trace for every miss")
		fs.Parse(args)
		d, err := build(ctx)
		if err != nil {
			return err
		}
		defer d.close()
		d.acts.Heartbeat = func(context.Context, ...any) {}
		if ox, ok := d.index.(*index.OpenSearch); ok {
			ox.Refresh = "wait_for"
		}
		for _, u := range expand([]string{*corpus}) {
			if r := pipeline.RunLocal(ctx, d.acts, pipeline.IngestInput{URI: u}); r.Status == "failed" {
				return fmt.Errorf("ingest %s: %s: %s", u, r.FailedStep, r.Error)
			}
		}
		qs, err := eval.Load(*qpath)
		if err != nil {
			return err
		}
		var reports []*eval.Report
		for _, m := range []search.Mode{search.Lexical, search.Vector, search.Hybrid} {
			r, err := eval.Run(ctx, d.searcher, qs, m)
			if err != nil {
				return err
			}
			reports = append(reports, r)
		}
		fmt.Printf("embedder=%s index=%T questions=%d\n\n", d.acts.Embedder.Name(), d.index, len(qs))
		eval.Print(os.Stdout, reports, *verbose)
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func slogAdapter() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
