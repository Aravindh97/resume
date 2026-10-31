//go:build integration

package index

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Aravindh97/granth/internal/doc"
	"github.com/Aravindh97/granth/internal/embed"
)

// Run with: go test -tags integration ./internal/index (see README for env).
func TestOpenSearchIndex(t *testing.T) {
	url := os.Getenv("GRANTH_OPENSEARCH_URL")
	if url == "" {
		t.Skip("GRANTH_OPENSEARCH_URL not set")
	}
	ctx := context.Background()
	e := embed.NewHashing(64)
	o, err := NewOpenSearch(OpenSearchConfig{URL: url, Index: fmt.Sprintf("granth-test-%d", time.Now().UnixNano()),
		User: os.Getenv("OPENSEARCH_USER"), Password: os.Getenv("OPENSEARCH_PASSWORD"), Dim: 64,
		CACertFile: os.Getenv("OPENSEARCH_CA_CERT"), InsecureSkipVerify: os.Getenv("OPENSEARCH_INSECURE_SKIP_VERIFY") == "true"})
	if err != nil {
		t.Fatal(err)
	}
	o.Refresh = "wait_for"
	if err := o.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.jsonDo(ctx, "DELETE", "/"+o.Index, nil, nil) })

	put := func(docID, hash string, meta map[string]string, texts ...string) {
		vecs, _ := e.Embed(ctx, texts)
		var cs []doc.Chunk
		for i, tx := range texts {
			cs = append(cs, doc.Chunk{ID: doc.ChunkID(docID, hash, i), DocID: docID, DocHash: hash, Ordinal: i, Text: tx,
				Embedding: vecs[i], Metadata: meta, Citation: doc.Citation{DocID: docID, SourceURI: docID + ".md", Page: 1}})
		}
		if err := o.ReplaceDoc(ctx, docID, hash, cs); err != nil {
			t.Fatal(err)
		}
	}
	put("hr", "v1", map[string]string{"dept": "hr"}, "earned leave is 18 days per year", "sick leave is 12 days")
	put("hr", "v1", map[string]string{"dept": "hr"}, "earned leave is 18 days per year", "sick leave is 12 days")
	put("travel", "v1", map[string]string{"dept": "travel"}, "hotel limit is 7500 rupees per night")
	put("hi", "v1", map[string]string{"dept": "hr"}, "कार्यालय का समय सुबह 9 बजे से शाम 6 बजे तक है")

	hits, err := o.Lexical(ctx, "leave", Filter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("replayed upsert should not duplicate: %d hits", len(hits))
	}
	hits, _ = o.Lexical(ctx, "कार्यालय समय", Filter{}, 3)
	if len(hits) == 0 || hits[0].Chunk.DocID != "hi" {
		t.Fatalf("hindi query: %+v", hits)
	}
	q, _ := e.Embed(ctx, []string{"hotel limit"})
	hits, err = o.Vector(ctx, q[0], Filter{Metadata: map[string]string{"dept": "travel"}}, 3)
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Chunk.Text, "7500") {
		t.Fatalf("filtered knn: %v %+v", err, hits)
	}
	put("hr", "v2", map[string]string{"dept": "hr"}, "earned leave is 20 days per year")
	hits, _ = o.Lexical(ctx, "leave", Filter{DocIDs: []string{"hr"}}, 10)
	if len(hits) != 1 || !strings.Contains(hits[0].Chunk.Text, "20") {
		t.Fatalf("old version not removed: %+v", hits)
	}
	c, err := o.Get(ctx, hits[0].Chunk.ID)
	if err != nil || c.Citation.SourceURI != "hr.md" {
		t.Fatalf("get: %v %+v", err, c)
	}
	if _, err := o.Get(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("missing chunk: %v", err)
	}
}
