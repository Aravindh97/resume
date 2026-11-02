package search

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Aravindh97/granth/internal/doc"
	"github.com/Aravindh97/granth/internal/embed"
	"github.com/Aravindh97/granth/internal/index"
)

func seed(t *testing.T, ix *index.Memory, e embed.Embedder, docID, hash string, texts ...string) {
	t.Helper()
	var cs []doc.Chunk
	vecs, _ := e.Embed(context.Background(), texts)
	for i, tx := range texts {
		cs = append(cs, doc.Chunk{ID: doc.ChunkID(docID, hash, i), DocID: docID, DocHash: hash, Ordinal: i, Text: tx,
			Embedding: vecs[i], Metadata: map[string]string{"dept": docID},
			Citation: doc.Citation{DocID: docID, SourceURI: docID + ".md", Page: 1}})
	}
	if err := ix.ReplaceDoc(context.Background(), docID, hash, cs); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceDocIsIdempotentAndDropsOldVersions(t *testing.T) {
	ix, e := index.NewMemory(), embed.NewHashing(64)
	seed(t, ix, e, "hr", "v1", "leave is 15 days", "sick leave is 12 days")
	seed(t, ix, e, "hr", "v1", "leave is 15 days", "sick leave is 12 days")
	if ix.Len() != 2 {
		t.Fatalf("replay created duplicates: %d chunks", ix.Len())
	}
	seed(t, ix, e, "hr", "v2", "leave is 18 days")
	if ix.Len() != 1 {
		t.Fatalf("old version not removed: %d chunks", ix.Len())
	}
	hits, _ := ix.Lexical(context.Background(), "leave", index.Filter{}, 5)
	if len(hits) != 1 || !strings.Contains(hits[0].Chunk.Text, "18") {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestHybridFusesAndTraces(t *testing.T) {
	ix, e := index.NewMemory(), embed.NewHashing(256)
	seed(t, ix, e, "travel", "v1", "hotel limit in tier 1 cities is 7500 rupees", "daily allowance is 1200")
	seed(t, ix, e, "hr", "v1", "earned leave accrues monthly", "public holidays list")
	s := New(ix, e)
	resp, err := s.Search(context.Background(), Request{Query: "hotel limit", K: 3})
	if err != nil {
		t.Fatal(err)
	}
	top := resp.Results[0]
	if !strings.Contains(top.Snippet, "hotel") || top.Trace.LexicalRank != 1 || top.Trace.VectorRank == 0 {
		t.Fatalf("top = %+v", top)
	}
	if top.Citation.SourceURI != "travel.md" {
		t.Errorf("citation = %+v", top.Citation)
	}
}

func TestFilters(t *testing.T) {
	ix, e := index.NewMemory(), embed.NewHashing(64)
	seed(t, ix, e, "travel", "v1", "approval needed from VP")
	seed(t, ix, e, "finance", "v1", "approval needed from CFO")
	resp, _ := New(ix, e).Search(context.Background(), Request{Query: "approval", K: 5,
		Filter: index.Filter{Metadata: map[string]string{"dept": "finance"}}})
	if len(resp.Results) != 1 || !strings.Contains(resp.Results[0].Snippet, "CFO") {
		t.Fatalf("results = %+v", resp.Results)
	}
}

type brokenEmbedder struct{ embed.Hashing }

func (brokenEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embedding service down")
}

func TestHybridDegradesToLexicalWhenEmbedderFails(t *testing.T) {
	ix, e := index.NewMemory(), embed.NewHashing(64)
	seed(t, ix, e, "hr", "v1", "sick leave is 12 days")
	s := New(ix, brokenEmbedder{e})
	resp, err := s.Search(context.Background(), Request{Query: "sick leave"})
	if err != nil || len(resp.Results) != 1 {
		t.Fatalf("hybrid should degrade, got %v %+v", err, resp)
	}
	if _, err := s.Search(context.Background(), Request{Query: "sick leave", Mode: Vector}); err == nil {
		t.Fatal("vector-only mode must surface the embedder error")
	}
}

func TestTokenBudget(t *testing.T) {
	ix, e := index.NewMemory(), embed.NewHashing(64)
	long := strings.Repeat("leave policy details ", 100)
	seed(t, ix, e, "hr", "v1", long, long+"x", long+"y")
	resp, _ := New(ix, e).Search(context.Background(), Request{Query: "leave", K: 3, MaxTokens: 200})
	total := 0
	for _, r := range resp.Results {
		total += len(r.Snippet)
	}
	if !resp.Truncated || total > 200*4+10 {
		t.Fatalf("truncated=%v total=%d", resp.Truncated, total)
	}
}

func TestOrderDoesNotDependOnPaths(t *testing.T) {
	e := embed.NewHashing(128)
	build := func(prefix string) []string {
		ix := index.NewMemory()
		for i, txt := range []string{"hotel limit tier 1 cities", "hotel booking travel desk", "daily allowance hotel"} {
			id := prefix + "/doc" + string(rune('a'+i))
			v, _ := e.Embed(context.Background(), []string{txt})
			ix.ReplaceDoc(context.Background(), id, "h", []doc.Chunk{{ID: doc.ChunkID(id, "h", 0), DocID: id, DocHash: "h", Text: txt,
				Embedding: v[0], Citation: doc.Citation{SourceURI: id + ".md"}}})
		}
		r, _ := New(ix, e).Search(context.Background(), Request{Query: "hotel", K: 3})
		var out []string
		for _, x := range r.Results {
			out = append(out, x.Snippet)
		}
		return out
	}
	a, b := build("/home/me/corpus"), build("/runner/work/x")
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("order depends on path: %v vs %v", a, b)
		}
	}
}
