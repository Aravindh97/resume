package parse

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Aravindh97/granth/internal/doc"
)

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func kinds(p doc.Page) []doc.BlockKind {
	var k []doc.BlockKind
	for _, b := range p.Blocks {
		k = append(k, b.Kind)
	}
	return k
}

func TestMarkdownHeadingsAndTables(t *testing.T) {
	d, err := New(nil).Parse(context.Background(), "travel.md", mustRead(t, "../../testdata/corpus/travel-policy.md"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Business Travel and Expense Policy" {
		t.Errorf("title = %q", d.Title)
	}
	var table *doc.Block
	for i, b := range d.Pages[0].Blocks {
		if b.Kind == doc.Table {
			table = &d.Pages[0].Blocks[i]
		}
	}
	if table == nil {
		t.Fatal("no table block")
	}
	if len(table.Rows) != 4 || table.Rows[2][0] != "Tier 2" {
		t.Errorf("rows = %v", table.Rows)
	}
	if !strings.Contains(table.Text, "Nightly hotel limit (INR): 5,000") {
		t.Errorf("table text lacks header-qualified cell: %q", table.Text)
	}
}

func TestHTMLDropsChromeAndKeepsTables(t *testing.T) {
	d, err := New(nil).Parse(context.Background(), "v.html", mustRead(t, "../../testdata/corpus/vendor-onboarding.html"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Vendor Onboarding Guide" {
		t.Errorf("title = %q", d.Title)
	}
	all := ""
	for _, b := range d.Pages[0].Blocks {
		all += b.Text + "\n"
	}
	for _, bad := range []string{"analytics", "Copyright", "Home"} {
		if strings.Contains(all, bad) {
			t.Errorf("chrome text %q leaked into blocks", bad)
		}
	}
	if !strings.Contains(all, "Payment terms: 15 days") {
		t.Errorf("table not rendered: %s", all)
	}
}

func TestCSV(t *testing.T) {
	d, err := New(nil).Parse(context.Background(), "c.csv", mustRead(t, "../../testdata/corpus/product-catalog.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if b := d.Pages[0].Blocks[0]; b.Kind != doc.Table || len(b.Rows) != 7 {
		t.Fatalf("got %v with %d rows", b.Kind, len(b.Rows))
	}
}

func TestPDFTextLayerPerPage(t *testing.T) {
	d, err := New(nil).Parse(context.Background(), "q.pdf", mustRead(t, "../../testdata/corpus/quality-manual.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Pages) != 2 {
		t.Fatalf("pages = %d", len(d.Pages))
	}
	p2 := ""
	for _, b := range d.Pages[1].Blocks {
		p2 += b.Text + " "
	}
	if !strings.Contains(p2, "AQL 2.5") || d.Pages[1].OCR {
		t.Errorf("page 2 = %q (ocr=%v)", p2, d.Pages[1].OCR)
	}
}

type fakeOCR struct{ calls int }

func (f *fakeOCR) PDFPage(_ context.Context, _ []byte, n int) (string, error) {
	f.calls++
	return "Scanned Invoice\n\nInvoice total is INR 4,200 payable in 30 days.", nil
}

func TestPDFWithoutTextLayerUsesOCR(t *testing.T) {
	ocr := &fakeOCR{}
	d, err := New(ocr).Parse(context.Background(), "s.pdf", mustRead(t, "../../testdata/scanned.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if ocr.calls != 1 || !d.Pages[0].OCR {
		t.Fatalf("ocr calls = %d, page.OCR = %v", ocr.calls, d.Pages[0].OCR)
	}
	if got := kinds(d.Pages[0]); len(got) != 2 || got[0] != doc.Heading {
		t.Errorf("blocks = %v", got)
	}
}

type failingOCR struct{}

func (failingOCR) PDFPage(context.Context, []byte, int) (string, error) {
	return "", errors.New("ocr service unavailable")
}

func TestOCRFailureIsTransient(t *testing.T) {
	_, err := New(failingOCR{}).Parse(context.Background(), "s.pdf", mustRead(t, "../../testdata/scanned.pdf"))
	if err == nil || errors.Is(err, ErrCorrupt) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("want transient error, got %v", err)
	}
}

func TestBadFilesArePermanent(t *testing.T) {
	cases := map[string]error{
		"../../testdata/bad/broken.pdf":  ErrCorrupt,
		"../../testdata/bad/archive.zip": ErrUnsupported,
		"../../testdata/bad/latin1.txt":  ErrCorrupt,
	}
	for path, want := range cases {
		_, err := New(nil).Parse(context.Background(), path, mustRead(t, path))
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", path, err, want)
		}
	}
}
