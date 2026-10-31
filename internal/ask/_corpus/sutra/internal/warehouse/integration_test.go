//go:build integration

package warehouse_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Aravindh97/sutra/internal/connector/rest"
	"github.com/Aravindh97/sutra/internal/fakecrm"
	"github.com/Aravindh97/sutra/internal/ontology"
	"github.com/Aravindh97/sutra/internal/warehouse"
)

func open(t *testing.T) (*warehouse.DB, string) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	db, err := warehouse.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf("test_%d", time.Now().UnixNano())
	t.Cleanup(db.Pool.Close)
	t.Cleanup(func() {
		db.Pool.Exec(context.Background(), `DELETE FROM source_records WHERE source=$1`, src)
		db.Pool.Exec(context.Background(), `DELETE FROM connector_state WHERE source=$1`, src)
	})
	return db, src
}

func TestApplyNeverRollsBack(t *testing.T) {
	db, src := open(t)
	ctx := context.Background()
	r := func(name string, seq uint64) warehouse.Versioned {
		return warehouse.Versioned{Seq: seq, Record: ontology.Record{Source: src, ID: "1", Entity: "Company",
			Attrs: map[string]string{"name": name}, Position: fmt.Sprint(seq)}}
	}
	db.Apply(ctx, src, "20", []warehouse.Versioned{r("new", 20)})
	if n, _ := db.Apply(ctx, src, "", []warehouse.Versioned{r("old", 10)}); n != 0 {
		t.Fatal("older version overwrote newer")
	}
	recs, _ := db.Records(ctx, "Company")
	for _, x := range recs {
		if x.Source == src && x.Attrs["name"] != "new" {
			t.Fatalf("got %q", x.Attrs["name"])
		}
	}
	if cp, _ := db.Checkpoint(ctx, src); cp != "20" {
		t.Fatalf("checkpoint = %q", cp)
	}
}

func TestRESTSyncRetriesAndResumes(t *testing.T) {
	db, src := open(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var rows []map[string]string
	for i := 0; i < 25; i++ {
		// Several records share a timestamp, which breaks naive "updated_since" sync.
		rows = append(rows, map[string]string{"id": fmt.Sprintf("A%02d", i), "account_name": fmt.Sprintf("Account %d", i),
			"updated_at": base.Add(time.Duration(i/3) * time.Hour).Format(time.RFC3339)})
	}
	crm := fakecrm.New("tok", rows)
	crm.FailEvery = 2 // every other request is a 429 or 503
	srv := httptest.NewServer(crm)
	defer srv.Close()
	s := ontology.Source{Name: src, Kind: "rest", Entity: "Company", Path: "/api/v1/accounts", ID: "id",
		Map: map[string]string{"name": "account_name"}}
	c := &rest.Connector{BaseURL: srv.URL, Token: "tok", Source: s, WH: db, Limit: 4,
		Sleep: func(context.Context, time.Duration) error { return nil }}
	n, err := c.Sync(ctx)
	if err != nil || n != 25 {
		t.Fatalf("first sync: %d %v", n, err)
	}
	if n, _ := c.Sync(ctx); n != 0 {
		t.Fatalf("second sync re-read %d records", n)
	}
	crm.Update("A03", map[string]any{"account_name": "Renamed"}, base.Add(48*time.Hour))
	if n, _ := c.Sync(ctx); n != 1 {
		t.Fatalf("incremental sync read %d records, want 1", n)
	}
	bad := &rest.Connector{BaseURL: srv.URL, Token: "nope", Source: s, WH: db}
	if _, err := bad.Sync(ctx); err == nil {
		t.Fatal("wrong token accepted")
	}
}
