//go:build integration

package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresLedger(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pg, err := OpenPostgres(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.DB.Close) // registered first, so it runs after the row cleanup
	id := fmt.Sprintf("test-%d", time.Now().UnixNano())
	t.Cleanup(func() { pg.DB.Exec(ctx, `DELETE FROM documents WHERE doc_id LIKE $1`, id+"%") })

	if err := pg.MarkStarted(ctx, id, "/x/"+id, "h1"); err != nil {
		t.Fatal(err)
	}
	if err := pg.MarkStarted(ctx, id, "/x/"+id, "h1"); err != nil { // retry
		t.Fatal(err)
	}
	if err := pg.MarkIndexed(ctx, id, "h1", 7, "hashing-384"); err != nil {
		t.Fatal(err)
	}
	r, err := pg.Get(ctx, id)
	if err != nil || r.Status != Indexed || r.Attempts != 2 || r.ChunkCount != 7 || r.IndexedAt == nil {
		t.Fatalf("%v %+v", err, r)
	}
	// Re-ingest keeps 'indexed' until the new version lands.
	pg.MarkStarted(ctx, id, "/x/"+id, "h2")
	if r, _ = pg.Get(ctx, id); r.Status != Indexed {
		t.Fatalf("status during re-ingest = %s", r.Status)
	}
	if err := pg.MarkFailed(ctx, id+"b", "/x/"+id+"b", "parse", "corrupt"); err != nil {
		t.Fatal(err)
	}
	failed, err := pg.List(ctx, Failed, id, 10)
	if err != nil || len(failed) != 1 || failed[0].FailedStep != "parse" {
		t.Fatalf("%v %+v", err, failed)
	}
	if err := pg.MarkIndexed(ctx, "nope-"+id, "h", 1, "e"); err != ErrNotFound {
		t.Fatalf("missing doc: %v", err)
	}
}
