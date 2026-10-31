// Package doc holds the parsed-document and chunk model shared by every stage
// of the pipeline. Everything here is plain data so it can be serialised into
// the blob store and passed between Temporal activities by reference.
package doc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// BlockKind is the layout role of a block of text inside a page.
type BlockKind string

const (
	Heading   BlockKind = "heading"
	Paragraph BlockKind = "paragraph"
	Table     BlockKind = "table"
)

// Block is one layout element. For tables Rows holds the cells and Text holds
// a flattened rendering used for search.
type Block struct {
	Kind  BlockKind  `json:"kind"`
	Level int        `json:"level,omitempty"` // heading depth, 1 = top
	Text  string     `json:"text"`
	Rows  [][]string `json:"rows,omitempty"`
}

// Page is a physical page for paged formats (PDF) or a single logical page
// for flowing formats (Markdown, HTML, text).
type Page struct {
	Number int     `json:"number"`
	Blocks []Block `json:"blocks"`
	// OCR is true when the text came from OCR instead of a text layer.
	OCR bool `json:"ocr,omitempty"`
}

// Document is the parser output.
type Document struct {
	ID          string            `json:"id"`           // stable id derived from the source URI
	SourceURI   string            `json:"source_uri"`   // where the file came from
	ContentHash string            `json:"content_hash"` // sha256 of the raw bytes
	Title       string            `json:"title"`
	Format      string            `json:"format"`
	Pages       []Page            `json:"pages"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Citation says exactly where a chunk came from.
type Citation struct {
	DocID       string   `json:"doc_id"`
	SourceURI   string   `json:"source_uri"`
	Title       string   `json:"title"`
	Page        int      `json:"page"`
	HeadingPath []string `json:"heading_path,omitempty"`
	BlockStart  int      `json:"block_start"` // index of first block on the page
	BlockEnd    int      `json:"block_end"`   // index of last block on the page (inclusive)
}

func (c Citation) String() string {
	return fmt.Sprintf("%s p.%d blocks %d-%d", c.SourceURI, c.Page, c.BlockStart, c.BlockEnd)
}

// Chunk is the unit that is embedded and indexed.
type Chunk struct {
	ID        string            `json:"id"`
	DocID     string            `json:"doc_id"`
	DocHash   string            `json:"doc_hash"` // content hash of the document version
	Ordinal   int               `json:"ordinal"`
	Text      string            `json:"text"`
	IsTable   bool              `json:"is_table,omitempty"`
	Citation  Citation          `json:"citation"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Embedding []float32         `json:"embedding,omitempty"`
}

// Hash returns the hex sha256 of b.
func Hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// DocID derives a stable document id from its source URI, so re-ingesting the
// same URI replaces the old version instead of creating a second document.
func DocID(sourceURI string) string { return Hash([]byte(sourceURI))[:24] }

// ChunkID is deterministic in (doc, content hash, ordinal): replaying the same
// ingestion produces the same ids, so index writes are idempotent upserts.
func ChunkID(docID, contentHash string, ordinal int) string {
	return Hash([]byte(fmt.Sprintf("%s|%s|%d", docID, contentHash, ordinal)))[:32]
}
