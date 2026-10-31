// Package search runs hybrid retrieval: lexical (BM25) and vector search in
// parallel, fused with reciprocal rank fusion. Every result carries its
// citation and a trace of how it was ranked, so a missing result can be
// debugged ("was it never retrieved, or retrieved and outranked?").
package search

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Aravindh97/granth/internal/doc"
	"github.com/Aravindh97/granth/internal/embed"
	"github.com/Aravindh97/granth/internal/index"
)

type Mode string

const (
	Hybrid  Mode = "hybrid"
	Lexical Mode = "lexical"
	Vector  Mode = "vector"
)

type Request struct {
	Query  string       `json:"query"`
	K      int          `json:"k"`
	Mode   Mode         `json:"mode,omitempty"`
	Filter index.Filter `json:"filter,omitempty"`
	// MaxTokens caps the total snippet size returned (approx. 4 chars/token),
	// so an agent can call search many times without flooding its context.
	MaxTokens int `json:"max_tokens,omitempty"`
}

type Trace struct {
	LexicalRank int     `json:"lexical_rank,omitempty"` // 1-based, 0 = not retrieved
	VectorRank  int     `json:"vector_rank,omitempty"`
	Fused       float64 `json:"fused_score"`
}

type Result struct {
	ChunkID  string       `json:"chunk_id"`
	Snippet  string       `json:"snippet"`
	Citation doc.Citation `json:"citation"`
	Trace    Trace        `json:"trace"`
}

type Response struct {
	Results   []Result      `json:"results"`
	Truncated bool          `json:"truncated,omitempty"`
	Took      time.Duration `json:"took_ns"`
}

type Searcher struct {
	Index    index.Index
	Embedder embed.Embedder
	// Depth is how many candidates each retriever returns before fusion.
	Depth int
	// RRFK is the rank constant in 1/(k+rank); 60 is the value from the
	// original RRF paper and works well without tuning.
	RRFK float64
}

func New(ix index.Index, e embed.Embedder) *Searcher {
	return &Searcher{Index: ix, Embedder: e, Depth: 50, RRFK: 60}
}

func (s *Searcher) Search(ctx context.Context, r Request) (*Response, error) {
	start := time.Now()
	if r.K <= 0 {
		r.K = 5
	}
	if r.Mode == "" {
		r.Mode = Hybrid
	}
	depth := max(s.Depth, r.K)
	var lex, vec []index.Hit
	var lexErr, vecErr error
	var wg sync.WaitGroup
	if r.Mode != Vector {
		wg.Add(1)
		go func() { defer wg.Done(); lex, lexErr = s.Index.Lexical(ctx, r.Query, r.Filter, depth) }()
	}
	if r.Mode != Lexical {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.Embedder.Embed(ctx, []string{r.Query})
			if err != nil {
				vecErr = err
				return
			}
			vec, vecErr = s.Index.Vector(ctx, v[0], r.Filter, depth)
		}()
	}
	wg.Wait()
	// Degrade instead of failing: if one retriever is down in hybrid mode,
	// return the other's results.
	if lexErr != nil && (r.Mode == Lexical || vecErr != nil) {
		return nil, lexErr
	}
	if vecErr != nil && (r.Mode == Vector || lexErr != nil) {
		return nil, vecErr
	}

	type cand struct {
		c doc.Chunk
		t Trace
	}
	byID := map[string]*cand{}
	addAll := func(hits []index.Hit, set func(*Trace, int)) {
		for i, h := range hits {
			c, ok := byID[h.Chunk.ID]
			if !ok {
				c = &cand{c: h.Chunk}
				byID[h.Chunk.ID] = c
			}
			set(&c.t, i+1)
			c.t.Fused += 1 / (s.RRFK + float64(i+1))
		}
	}
	addAll(lex, func(t *Trace, r int) { t.LexicalRank = r })
	addAll(vec, func(t *Trace, r int) { t.VectorRank = r })

	cands := make([]*cand, 0, len(byID))
	for _, c := range byID {
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].t.Fused != cands[j].t.Fused {
			return cands[i].t.Fused > cands[j].t.Fused
		}
		return cands[i].c.ID < cands[j].c.ID
	})

	resp := &Response{}
	budget := r.MaxTokens * 4
	for _, c := range cands {
		if len(resp.Results) == r.K {
			break
		}
		snip := c.c.Text
		if budget > 0 {
			remaining := budget
			for _, x := range resp.Results {
				remaining -= len(x.Snippet)
			}
			if remaining <= 40 {
				resp.Truncated = true
				break
			}
			if len(snip) > remaining {
				snip, resp.Truncated = cut(snip, remaining), true
			}
		}
		resp.Results = append(resp.Results, Result{ChunkID: c.c.ID, Snippet: snip, Citation: c.c.Citation, Trace: c.t})
	}
	resp.Took = time.Since(start)
	return resp, nil
}

// cut trims s to at most n bytes on a word boundary without splitting a rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && (s[len(s)-1]&0xC0) == 0x80 {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] >= 0xC0 {
		s = s[:len(s)-1]
	}
	if i := strings.LastIndexAny(s, " \n"); i > n/2 {
		s = s[:i]
	}
	return s + " …"
}
