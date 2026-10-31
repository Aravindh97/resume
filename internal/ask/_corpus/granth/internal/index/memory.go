// Package index defines the chunk index used for retrieval, with two
// implementations: OpenSearch for production and Memory (BM25 + exact cosine)
// for tests, local runs and evals.
package index

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"

	"github.com/Aravindh97/granth/internal/doc"
	"github.com/Aravindh97/granth/internal/embed"
	"github.com/Aravindh97/granth/internal/textproc"
)

var ErrNotFound = errors.New("not found")

// Filter restricts a search. Empty fields match everything.
type Filter struct {
	DocIDs     []string          `json:"doc_ids,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"` // exact match on every key
	TablesOnly bool              `json:"tables_only,omitempty"`
}

func (f Filter) match(c *doc.Chunk) bool {
	if f.TablesOnly && !c.IsTable {
		return false
	}
	if len(f.DocIDs) > 0 {
		ok := false
		for _, id := range f.DocIDs {
			if id == c.DocID {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for k, v := range f.Metadata {
		if c.Metadata[k] != v {
			return false
		}
	}
	return true
}

type Hit struct {
	Chunk doc.Chunk
	Score float64
}

type Index interface {
	// ReplaceDoc upserts chunks by id and removes chunks of older versions of
	// the same document. Calling it twice with the same input is a no-op.
	ReplaceDoc(ctx context.Context, docID, docHash string, chunks []doc.Chunk) error
	DeleteDoc(ctx context.Context, docID string) error
	Lexical(ctx context.Context, query string, f Filter, k int) ([]Hit, error)
	Vector(ctx context.Context, vec []float32, f Filter, k int) ([]Hit, error)
	Get(ctx context.Context, chunkID string) (*doc.Chunk, error)
}

// Memory is a thread-safe in-process index using Okapi BM25 (k1=1.2, b=0.75)
// and exact cosine similarity.
type Memory struct {
	mu     sync.RWMutex
	chunks map[string]*doc.Chunk
	terms  map[string][]string // chunk id -> tokens
	df     map[string]int
	total  int // sum of token counts
}

func NewMemory() *Memory {
	return &Memory{chunks: map[string]*doc.Chunk{}, terms: map[string][]string{}, df: map[string]int{}}
}

func (m *Memory) add(c doc.Chunk) {
	if _, ok := m.chunks[c.ID]; ok {
		m.remove(c.ID)
	}
	toks := textproc.Tokens(c.Text)
	m.chunks[c.ID] = &c
	m.terms[c.ID] = toks
	m.total += len(toks)
	seen := map[string]bool{}
	for _, t := range toks {
		if !seen[t] {
			seen[t] = true
			m.df[t]++
		}
	}
}

func (m *Memory) remove(id string) {
	toks := m.terms[id]
	seen := map[string]bool{}
	for _, t := range toks {
		if !seen[t] {
			seen[t] = true
			if m.df[t]--; m.df[t] == 0 {
				delete(m.df, t)
			}
		}
	}
	m.total -= len(toks)
	delete(m.terms, id)
	delete(m.chunks, id)
}

func (m *Memory) ReplaceDoc(_ context.Context, docID, docHash string, chunks []doc.Chunk) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range chunks {
		m.add(c)
	}
	for id, c := range m.chunks {
		if c.DocID == docID && c.DocHash != docHash {
			m.remove(id)
		}
	}
	return nil
}

func (m *Memory) DeleteDoc(_ context.Context, docID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.chunks {
		if c.DocID == docID {
			m.remove(id)
		}
	}
	return nil
}

func (m *Memory) Get(_ context.Context, id string) (*doc.Chunk, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.chunks[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.chunks)
}

func (m *Memory) Lexical(_ context.Context, query string, f Filter, k int) ([]Hit, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	q := textproc.Tokens(query)
	n := float64(len(m.chunks))
	if n == 0 || len(q) == 0 {
		return nil, nil
	}
	avg := float64(m.total) / n
	const k1, b = 1.2, 0.75
	var hits []Hit
	for id, toks := range m.terms {
		c := m.chunks[id]
		if !f.match(c) {
			continue
		}
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		var s float64
		for _, t := range q {
			f := float64(tf[t])
			if f == 0 {
				continue
			}
			df := float64(m.df[t])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			s += idf * f * (k1 + 1) / (f + k1*(1-b+b*float64(len(toks))/avg))
		}
		if s > 0 {
			hits = append(hits, Hit{Chunk: *c, Score: s})
		}
	}
	return top(hits, k), nil
}

func (m *Memory) Vector(_ context.Context, vec []float32, f Filter, k int) ([]Hit, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var hits []Hit
	for _, c := range m.chunks {
		if !f.match(c) || len(c.Embedding) != len(vec) {
			continue
		}
		hits = append(hits, Hit{Chunk: *c, Score: float64(embed.Cosine(vec, c.Embedding))})
	}
	return top(hits, k), nil
}

func top(h []Hit, k int) []Hit {
	sort.Slice(h, func(i, j int) bool {
		if h[i].Score != h[j].Score {
			return h[i].Score > h[j].Score
		}
		return h[i].Chunk.ID < h[j].Chunk.ID
	})
	if len(h) > k {
		h = h[:k]
	}
	return h
}
