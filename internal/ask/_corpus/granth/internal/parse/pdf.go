package parse

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ledongthuc/pdf"

	"github.com/Aravindh97/granth/internal/doc"
)

// parsePDF reads the text layer page by page. A page with fewer than
// MinTextChars characters (typically a scan) is sent to OCR when available.
// A panic inside the PDF library (it happens on malformed xref tables) is
// converted into ErrCorrupt so one bad file cannot crash the worker.
func (p *Parser) parsePDF(ctx context.Context, data []byte) (pages []doc.Page, err error) {
	defer func() {
		if r := recover(); r != nil {
			pages, err = nil, fmt.Errorf("%w: pdf reader panic: %v", ErrCorrupt, r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	n := r.NumPage()
	if n == 0 {
		return nil, fmt.Errorf("%w: pdf has no pages", ErrCorrupt)
	}
	for i := 1; i <= n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text := ""
		if pg := r.Page(i); !pg.V.IsNull() {
			if t, terr := pg.GetPlainText(nil); terr == nil {
				text = t
			}
		}
		page := doc.Page{Number: i}
		if len(strings.TrimSpace(text)) < p.MinTextChars && p.OCR != nil {
			ocrText, oerr := p.OCR.PDFPage(ctx, data, i)
			if oerr != nil {
				// OCR failure is transient (service/binary hiccup): let the
				// activity retry instead of silently indexing an empty page.
				return nil, fmt.Errorf("ocr page %d: %w", i, oerr)
			}
			text, page.OCR = ocrText, true
		}
		page.Blocks = textToBlocks(text)
		pages = append(pages, page)
	}
	return pages, nil
}

var blankLines = regexp.MustCompile(`\n\s*\n`)

// textToBlocks splits extracted page text into paragraphs, treating short
// title-case lines without a final period as headings.
func textToBlocks(text string) []doc.Block {
	var out []doc.Block
	for _, para := range blankLines.Split(text, -1) {
		t := strings.Join(strings.Fields(para), " ")
		if t == "" {
			continue
		}
		kind := doc.Paragraph
		if len(t) < 70 && !strings.ContainsAny(t[len(t)-1:], ".,;:") && !strings.Contains(para, "\n") {
			kind = doc.Heading
		}
		b := doc.Block{Kind: kind, Text: t}
		if kind == doc.Heading {
			b.Level = 2
		}
		out = append(out, b)
	}
	return out
}

// Tesseract implements OCR with the poppler `pdftoppm` and `tesseract` CLIs,
// which the worker container image installs.
type Tesseract struct {
	Lang string // e.g. "eng+hin+tam"
	DPI  int
}

func (t Tesseract) PDFPage(ctx context.Context, data []byte, n int) (string, error) {
	dir, err := os.MkdirTemp("", "granth-ocr-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return "", err
	}
	dpi := t.DPI
	if dpi == 0 {
		dpi = 300
	}
	img := filepath.Join(dir, "page")
	pageArg := fmt.Sprint(n)
	if out, err := exec.CommandContext(ctx, "pdftoppm", "-r", fmt.Sprint(dpi), "-f", pageArg, "-l", pageArg,
		"-singlefile", "-png", in, img).CombinedOutput(); err != nil {
		return "", fmt.Errorf("pdftoppm: %v: %s", err, out)
	}
	lang := t.Lang
	if lang == "" {
		lang = "eng"
	}
	out, err := exec.CommandContext(ctx, "tesseract", img+".png", "stdout", "-l", lang).Output()
	if err != nil {
		return "", fmt.Errorf("tesseract: %w", err)
	}
	return string(out), nil
}
