// Package chunk splits a parsed document into retrieval chunks that respect
// layout: a chunk never crosses a heading or a page, tables are kept apart
// from prose, and a table that is too long is split by rows with the header
// row repeated so every piece is still readable on its own.
package chunk

import (
	"strings"

	"github.com/Aravindh97/granth/internal/doc"
)

type Options struct {
	MaxWords int // soft limit for prose chunks
}

func Default() Options { return Options{MaxWords: 180} }

func Split(d *doc.Document, o Options) []doc.Chunk {
	if o.MaxWords <= 0 {
		o = Default()
	}
	var out []doc.Chunk
	var path []string // current heading path
	emit := func(text string, page, start, end int, table bool) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		hp := append([]string(nil), path...)
		// The heading path is prepended to the indexed text so a chunk such as
		// "Up to 15 days per year" still matches a query about "leave".
		header := strings.Join(hp, " > ")
		if header != "" {
			text = header + "\n" + text
		}
		ord := len(out)
		out = append(out, doc.Chunk{
			ID: doc.ChunkID(d.ID, d.ContentHash, ord), DocID: d.ID, DocHash: d.ContentHash, Ordinal: ord,
			Text: text, IsTable: table, Metadata: d.Metadata,
			Citation: doc.Citation{DocID: d.ID, SourceURI: d.SourceURI, Title: d.Title, Page: page,
				HeadingPath: hp, BlockStart: start, BlockEnd: end},
		})
	}

	for _, pg := range d.Pages {
		var buf []string
		words, start := 0, -1
		flush := func(end int) {
			if len(buf) > 0 {
				emit(strings.Join(buf, "\n"), pg.Number, start, end, false)
			}
			buf, words, start = buf[:0], 0, -1
		}
		for i, b := range pg.Blocks {
			switch b.Kind {
			case doc.Heading:
				flush(i - 1)
				lvl := b.Level
				if lvl < 1 {
					lvl = 1
				}
				if lvl-1 < len(path) {
					path = path[:lvl-1]
				}
				path = append(path, b.Text)
			case doc.Table:
				flush(i - 1)
				for _, piece := range splitTable(b, o.MaxWords) {
					emit(piece, pg.Number, i, i, true)
				}
			default:
				n := len(strings.Fields(b.Text))
				if words > 0 && words+n > o.MaxWords {
					flush(i - 1)
				}
				if start < 0 {
					start = i
				}
				buf = append(buf, b.Text)
				words += n
			}
		}
		flush(len(pg.Blocks) - 1)
	}
	return out
}

func splitTable(b doc.Block, maxWords int) []string {
	lines := strings.Split(b.Text, "\n")
	if len(lines) <= 1 || len(strings.Fields(b.Text)) <= maxWords {
		return []string{b.Text}
	}
	header, rows := lines[0], lines[1:]
	var out []string
	cur := []string{header}
	words := len(strings.Fields(header))
	for _, r := range rows {
		n := len(strings.Fields(r))
		if len(cur) > 1 && words+n > maxWords {
			out = append(out, strings.Join(cur, "\n"))
			cur, words = []string{header}, len(strings.Fields(header))
		}
		cur = append(cur, r)
		words += n
	}
	return append(out, strings.Join(cur, "\n"))
}
