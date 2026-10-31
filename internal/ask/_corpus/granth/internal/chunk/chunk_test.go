package chunk

import (
	"strings"
	"testing"

	"github.com/Aravindh97/granth/internal/doc"
)

func testDoc() *doc.Document {
	rows := [][]string{{"SKU", "Price"}}
	for i := 0; i < 40; i++ {
		rows = append(rows, []string{"KT-" + strings.Repeat("9", 3), "100"})
	}
	var tb strings.Builder
	tb.WriteString("SKU | Price")
	for range rows[1:] {
		tb.WriteString("\nSKU: KT-999; Price: 100")
	}
	return &doc.Document{ID: "d1", ContentHash: "h1", SourceURI: "x.md", Pages: []doc.Page{{Number: 1, Blocks: []doc.Block{
		{Kind: doc.Heading, Level: 1, Text: "Policy"},
		{Kind: doc.Heading, Level: 2, Text: "Leave"},
		{Kind: doc.Paragraph, Text: "Fifteen days per year."},
		{Kind: doc.Heading, Level: 2, Text: "Travel"},
		{Kind: doc.Paragraph, Text: "Economy class by default."},
		{Kind: doc.Table, Text: tb.String(), Rows: rows},
	}}}}
}

func TestChunksFollowHeadings(t *testing.T) {
	cs := Split(testDoc(), Options{MaxWords: 50})
	if len(cs) < 3 {
		t.Fatalf("got %d chunks", len(cs))
	}
	if got := strings.Join(cs[0].Citation.HeadingPath, ">"); got != "Policy>Leave" {
		t.Errorf("chunk 0 path = %q", got)
	}
	if !strings.HasPrefix(cs[0].Text, "Policy > Leave\n") {
		t.Errorf("chunk 0 text lacks heading header: %q", cs[0].Text)
	}
	if strings.Contains(cs[0].Text, "Economy") {
		t.Error("chunk crossed a heading boundary")
	}
	if got := strings.Join(cs[1].Citation.HeadingPath, ">"); got != "Policy>Travel" {
		t.Errorf("chunk 1 path = %q (sibling heading must replace, not nest)", got)
	}
}

func TestLongTableSplitRepeatsHeader(t *testing.T) {
	cs := Split(testDoc(), Options{MaxWords: 50})
	var tables []doc.Chunk
	for _, c := range cs {
		if c.IsTable {
			tables = append(tables, c)
		}
	}
	if len(tables) < 2 {
		t.Fatalf("table not split: %d pieces", len(tables))
	}
	for _, c := range tables {
		if !strings.Contains(c.Text, "SKU | Price\n") {
			t.Errorf("piece missing header row: %.60q", c.Text)
		}
	}
}

func TestChunkIDsAreDeterministic(t *testing.T) {
	a, b := Split(testDoc(), Default()), Split(testDoc(), Default())
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("chunk %d id differs between runs", i)
		}
	}
	d := testDoc()
	d.ContentHash = "h2"
	if Split(d, Default())[0].ID == a[0].ID {
		t.Error("new document version must produce new chunk ids")
	}
}
