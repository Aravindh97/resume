// Package parse turns raw bytes into a layout-aware doc.Document.
//
// Errors are classified: ErrUnsupported and ErrCorrupt are permanent (retrying
// will not help, the pipeline sends the file to the dead-letter state), any
// other error is treated as transient.
package parse

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Aravindh97/granth/internal/doc"
)

var (
	ErrUnsupported = errors.New("unsupported format")
	ErrCorrupt     = errors.New("corrupt or unreadable file")
)

// OCR extracts text from a page image. Implementations shell out to Tesseract
// or call a document-AI service; tests use a fake.
type OCR interface {
	// PDFPage returns the text of page n (1-based) of the PDF in data.
	PDFPage(ctx context.Context, data []byte, n int) (string, error)
}

// Parser dispatches on file extension.
type Parser struct {
	OCR OCR // optional; nil disables OCR fallback
	// MinTextChars is the threshold below which a PDF page is considered to
	// have no usable text layer and is sent to OCR.
	MinTextChars int
}

func New(ocr OCR) *Parser { return &Parser{OCR: ocr, MinTextChars: 20} }

// Format returns the parser format for a file name.
func Format(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return "markdown"
	case ".txt", ".text":
		return "text"
	case ".html", ".htm":
		return "html"
	case ".csv":
		return "csv"
	case ".pdf":
		return "pdf"
	}
	return ""
}

func (p *Parser) Parse(ctx context.Context, sourceURI string, data []byte) (*doc.Document, error) {
	format := Format(sourceURI)
	d := &doc.Document{
		ID:          doc.DocID(sourceURI),
		SourceURI:   sourceURI,
		ContentHash: doc.Hash(data),
		Format:      format,
		Title:       strings.TrimSuffix(filepath.Base(sourceURI), filepath.Ext(sourceURI)),
	}
	var err error
	switch format {
	case "markdown":
		d.Pages, err = parseMarkdown(data)
	case "text":
		d.Pages, err = parseText(data)
	case "html":
		d.Pages, d.Title, err = parseHTML(data, d.Title)
	case "csv":
		d.Pages, err = parseCSV(data)
	case "pdf":
		d.Pages, err = p.parsePDF(ctx, data)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, filepath.Ext(sourceURI))
	}
	if err != nil {
		return nil, err
	}
	if t := firstHeading(d.Pages); t != "" && format != "html" {
		d.Title = t
	}
	return d, nil
}

func requireUTF8(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("%w: not valid UTF-8", ErrCorrupt)
	}
	return nil
}

func firstHeading(pages []doc.Page) string {
	for _, pg := range pages {
		for _, b := range pg.Blocks {
			if b.Kind == doc.Heading {
				return b.Text
			}
		}
	}
	return ""
}

var (
	mdHeading  = regexp.MustCompile(`^(#{1,6})\s+(.*\S)\s*$`)
	mdTableSep = regexp.MustCompile(`^\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$`)
)

func parseMarkdown(data []byte) ([]doc.Page, error) {
	if err := requireUTF8(data); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var blocks []doc.Block
	var para []string
	flush := func() {
		if t := strings.TrimSpace(strings.Join(para, " ")); t != "" {
			blocks = append(blocks, doc.Block{Kind: doc.Paragraph, Text: t})
		}
		para = para[:0]
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trim := strings.TrimSpace(line)
		switch {
		case trim == "":
			flush()
		case mdHeading.MatchString(trim):
			flush()
			m := mdHeading.FindStringSubmatch(trim)
			blocks = append(blocks, doc.Block{Kind: doc.Heading, Level: len(m[1]), Text: m[2]})
		case strings.HasPrefix(trim, "|") && i+1 < len(lines) && mdTableSep.MatchString(strings.TrimSpace(lines[i+1])):
			flush()
			rows := [][]string{splitRow(trim)}
			i += 2
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				rows = append(rows, splitRow(strings.TrimSpace(lines[i])))
			}
			i--
			blocks = append(blocks, tableBlock(rows))
		default:
			para = append(para, strings.TrimLeft(trim, "-*+ "))
		}
	}
	flush()
	return []doc.Page{{Number: 1, Blocks: blocks}}, nil
}

func splitRow(line string) []string {
	line = strings.Trim(line, "|")
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// tableBlock renders rows as "header: value" lines so each cell is searchable
// together with its column name.
func tableBlock(rows [][]string) doc.Block {
	var sb strings.Builder
	if len(rows) > 0 {
		hdr := rows[0]
		sb.WriteString(strings.Join(hdr, " | "))
		for _, r := range rows[1:] {
			sb.WriteString("\n")
			parts := make([]string, 0, len(r))
			for j, c := range r {
				h := ""
				if j < len(hdr) {
					h = hdr[j] + ": "
				}
				parts = append(parts, h+c)
			}
			sb.WriteString(strings.Join(parts, "; "))
		}
	}
	return doc.Block{Kind: doc.Table, Text: sb.String(), Rows: rows}
}

func parseText(data []byte) ([]doc.Page, error) {
	if err := requireUTF8(data); err != nil {
		return nil, err
	}
	var blocks []doc.Block
	for _, p := range regexp.MustCompile(`\n\s*\n`).Split(string(data), -1) {
		if t := strings.Join(strings.Fields(p), " "); t != "" {
			blocks = append(blocks, doc.Block{Kind: doc.Paragraph, Text: t})
		}
	}
	return []doc.Page{{Number: 1, Blocks: blocks}}, nil
}

func parseCSV(data []byte) ([]doc.Page, error) {
	if err := requireUTF8(data); err != nil {
		return nil, err
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: empty csv", ErrCorrupt)
	}
	return []doc.Page{{Number: 1, Blocks: []doc.Block{tableBlock(rows)}}}, nil
}
