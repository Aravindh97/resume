//go:build js && wasm

// Command web runs Granth's parse -> chunk -> embed -> index -> search path
// in the browser, on the embedded test corpus plus any file you upload.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aravindh97/granth/internal/chunk"
	gembed "github.com/Aravindh97/granth/internal/embed"
	"github.com/Aravindh97/granth/internal/eval"
	"github.com/Aravindh97/granth/internal/index"
	"github.com/Aravindh97/granth/internal/mcp"
	"github.com/Aravindh97/granth/internal/parse"
	"github.com/Aravindh97/granth/internal/search"
)

//go:embed embed/corpus embed/bad embed/questions.jsonl
var files embed.FS

type docInfo struct {
	URI    string `json:"uri"`
	Title  string `json:"title"`
	Format string `json:"format"`
	Pages  int    `json:"pages"`
	Chunks int    `json:"chunks"`
	Bytes  int    `json:"bytes"`
	Mine   bool   `json:"uploaded,omitempty"`
}

var (
	mu   sync.Mutex
	ix   = index.NewMemory()
	emb  = gembed.NewHashing(384)
	prs  = parse.New(nil) // no OCR in the browser
	docs = map[string]docInfo{}
	srch = search.New(ix, emb)
)

// ingest runs the same steps as the pipeline's activities, in process.
func ingest(ctx context.Context, uri string, data []byte, mine bool) (docInfo, error) {
	d, err := prs.Parse(ctx, uri, data)
	if err != nil {
		return docInfo{}, err
	}
	cs := chunk.Split(d, chunk.Default())
	texts := make([]string, len(cs))
	for i, c := range cs {
		texts[i] = c.Text
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		return docInfo{}, err
	}
	for i := range cs {
		cs[i].Embedding = vecs[i]
	}
	if err := ix.ReplaceDoc(ctx, d.ID, d.ContentHash, cs); err != nil {
		return docInfo{}, err
	}
	info := docInfo{URI: uri, Title: d.Title, Format: d.Format, Pages: len(d.Pages), Chunks: len(cs), Bytes: len(data), Mine: mine}
	docs[d.ID] = info
	return info, nil
}

func classify(err error) string {
	switch {
	case errors.Is(err, parse.ErrUnsupported):
		return "permanent: unsupported format (the pipeline sends this to the dead-letter ledger, no retries)"
	case errors.Is(err, parse.ErrCorrupt):
		return "permanent: corrupt or unreadable (dead-letter ledger, no retries)"
	}
	return "transient (the pipeline would retry with backoff)"
}

func main() {
	ctx := context.Background()
	t0 := time.Now()
	ents, _ := fs.ReadDir(files, "embed/corpus")
	for _, e := range ents {
		b, _ := files.ReadFile("embed/corpus/" + e.Name())
		ingest(ctx, "testdata/corpus/"+e.Name(), b, false)
	}
	loadTook := time.Since(t0)
	server := &mcp.Server{Searcher: srch, Index: ix, DefaultMaxTokens: 1500}

	serve(map[string]handler{
		"GET docs": func(url.Values, []byte) resp {
			mu.Lock()
			defer mu.Unlock()
			var out []docInfo
			for _, d := range docs {
				out = append(out, d)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
			return ok(map[string]any{"docs": out, "chunks": ix.Len(), "ingest_ms": loadTook.Milliseconds(), "embedder": emb.Name()})
		},
		"GET search": func(q url.Values, _ []byte) resp {
			k, _ := strconv.Atoi(q.Get("k"))
			mode := search.Mode(q.Get("mode"))
			if mode != search.Lexical && mode != search.Vector {
				mode = search.Hybrid
			}
			if strings.TrimSpace(q.Get("q")) == "" {
				return fail(400, "type a question")
			}
			mu.Lock()
			defer mu.Unlock()
			r, err := srch.Search(ctx, search.Request{Query: q.Get("q"), K: min(max(k, 1), 10), Mode: mode, MaxTokens: 2000})
			if err != nil {
				return fail(500, err.Error())
			}
			return ok(map[string]any{"results": r.Results, "truncated": r.Truncated, "took_us": r.Took.Microseconds()})
		},
		"POST upload": func(_ url.Values, b []byte) resp {
			var in struct{ Name, Data string }
			if json.Unmarshal(b, &in) != nil || in.Name == "" {
				return fail(400, "bad request")
			}
			raw, err := base64.StdEncoding.DecodeString(in.Data)
			if err != nil {
				return fail(400, "bad file encoding")
			}
			if len(raw) > 5<<20 {
				return fail(400, "files up to 5 MB in the browser demo")
			}
			mu.Lock()
			defer mu.Unlock()
			info, err := ingest(ctx, "upload/"+path.Base(in.Name), raw, true)
			if err != nil {
				return ok(map[string]any{"error": err.Error(), "class": classify(err)})
			}
			return ok(map[string]any{"doc": info})
		},
		"POST bad": func(q url.Values, _ []byte) resp {
			// The repo's deliberately broken files, run through the same parser.
			var out []map[string]string
			ents, _ := fs.ReadDir(files, "embed/bad")
			for _, e := range ents {
				b, _ := files.ReadFile("embed/bad/" + e.Name())
				_, err := prs.Parse(ctx, e.Name(), b)
				row := map[string]string{"file": e.Name(), "result": "parsed"}
				if err != nil {
					row["result"], row["class"] = err.Error(), classify(err)
				}
				out = append(out, row)
			}
			return ok(out)
		},
		"POST eval": func(url.Values, []byte) resp {
			b, _ := files.ReadFile("embed/questions.jsonl")
			var qs []eval.Question
			for _, line := range bytes.Split(b, []byte("\n")) {
				var x eval.Question
				if json.Unmarshal(line, &x) == nil && x.ID != "" {
					qs = append(qs, x)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			var rows []map[string]any
			var misses []map[string]any
			for _, m := range []search.Mode{search.Lexical, search.Vector, search.Hybrid} {
				r, err := eval.Run(ctx, srch, qs, m)
				if err != nil {
					return fail(500, err.Error())
				}
				rows = append(rows, map[string]any{"mode": m, "n": r.N, "hit1": r.HitAt1, "hit5": r.HitAt5, "mrr": r.MRR, "cite1": r.CitationAt1, "p50_us": r.P50.Microseconds()})
				for _, x := range r.Misses {
					top := ""
					if len(x.Top) > 0 {
						top = path.Base(x.Top[0].Citation.SourceURI)
					}
					misses = append(misses, map[string]any{"mode": m, "id": x.Question.ID, "query": x.Question.Query, "want": x.Question.Doc, "got": top})
				}
			}
			return ok(map[string]any{"rows": rows, "misses": misses, "questions": len(qs)})
		},
		"POST mcp": func(_ url.Values, b []byte) resp {
			// One JSON-RPC line in, one out: the exact protocol `granth mcp` speaks over stdio.
			var outBuf bytes.Buffer
			if err := server.Serve(ctx, bytes.NewReader(append(b, '\n')), &outBuf); err != nil {
				return fail(500, err.Error())
			}
			var v any
			json.Unmarshal(outBuf.Bytes(), &v)
			return ok(v)
		},
	})
}
