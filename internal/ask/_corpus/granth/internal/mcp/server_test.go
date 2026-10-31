package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aravindh97/granth/internal/doc"
	"github.com/Aravindh97/granth/internal/embed"
	"github.com/Aravindh97/granth/internal/index"
	"github.com/Aravindh97/granth/internal/search"
)

func TestMCPToolsRoundTrip(t *testing.T) {
	ix, e := index.NewMemory(), embed.NewHashing(64)
	texts := []string{"Travel > Booking\nbook flights 14 days ahead", "Travel > Hotels\nhotel limit 7500", "Travel > Mileage\n12 per km"}
	vecs, _ := e.Embed(context.Background(), texts)
	var cs []doc.Chunk
	for i, tx := range texts {
		cs = append(cs, doc.Chunk{ID: doc.ChunkID("t", "h", i), DocID: "t", DocHash: "h", Ordinal: i, Text: tx, Embedding: vecs[i],
			Citation: doc.Citation{DocID: "t", SourceURI: "travel.md", Page: 1}})
	}
	ix.ReplaceDoc(context.Background(), "t", "h", cs)
	s := &Server{Searcher: search.New(ix, e), Index: ix, DefaultMaxTokens: 500}

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"hotel limit","k":1}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"open_source","arguments":{"chunk_id":"` + cs[1].ID + `","window":1}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"search","arguments":{"query":"  "}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"nope"}`,
	}, "\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, m)
	}
	if len(resps) != 6 {
		t.Fatalf("want 6 responses (notification gets none), got %d", len(resps))
	}
	if n := len(resps[1]["result"].(map[string]any)["tools"].([]any)); n != 3 {
		t.Errorf("tools = %d", n)
	}
	sr := resps[2]["result"].(map[string]any)["structuredContent"].(map[string]any)["results"].([]any)
	if len(sr) != 1 || !strings.Contains(sr[0].(map[string]any)["snippet"].(string), "7500") {
		t.Errorf("search = %v", sr)
	}
	ps := resps[3]["result"].(map[string]any)["structuredContent"].(map[string]any)["passages"].([]any)
	if len(ps) != 3 {
		t.Errorf("open_source passages = %d", len(ps))
	}
	if resps[4]["result"].(map[string]any)["isError"] != true {
		t.Errorf("empty query should be a tool error: %v", resps[4])
	}
	if resps[5]["error"] == nil {
		t.Errorf("unknown method should be a JSON-RPC error")
	}
}
