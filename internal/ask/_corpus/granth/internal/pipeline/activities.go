// Package pipeline implements document ingestion as Temporal activities and
// workflows. Each activity is idempotent: inputs and outputs are content
// hashes, index writes are upserts keyed by deterministic chunk ids, and the
// ledger update is a single-row upsert. A retry or a workflow replay can
// therefore run any step again without creating duplicates.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/Aravindh97/granth/internal/blob"
	"github.com/Aravindh97/granth/internal/chunk"
	"github.com/Aravindh97/granth/internal/doc"
	"github.com/Aravindh97/granth/internal/embed"
	"github.com/Aravindh97/granth/internal/index"
	"github.com/Aravindh97/granth/internal/parse"
	"github.com/Aravindh97/granth/internal/store"
)

// ErrPermanent is the Temporal application error type for failures that a
// retry cannot fix (unsupported format, corrupt file, file too large).
const ErrPermanent = "PermanentError"

const MaxFileBytes = 100 << 20

type Activities struct {
	Blobs    *blob.Store
	Parser   *parse.Parser
	Chunk    chunk.Options
	Embedder embed.Embedder
	Index    index.Index
	Store    store.Store
	HTTP     *http.Client
	// EmbedBatch is the number of chunks per embedding request.
	EmbedBatch int
	// Heartbeat is called after every embedding batch. It defaults to the
	// Temporal heartbeat and is replaced when running without Temporal.
	Heartbeat func(ctx context.Context, details ...any)
}

func permanent(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), ErrPermanent, err)
}

type FetchResult struct {
	DocID string `json:"doc_id"`
	Hash  string `json:"hash"`
	Size  int    `json:"size"`
}

// Fetch reads a local path, file:// URL or http(s) URL into the blob store.
func (a *Activities) Fetch(ctx context.Context, uri string) (*FetchResult, error) {
	data, err := a.read(ctx, uri)
	if err != nil {
		return nil, err
	}
	h, err := a.Blobs.Put(data)
	if err != nil {
		return nil, err
	}
	return &FetchResult{DocID: doc.DocID(uri), Hash: h, Size: len(data)}, nil
}

func (a *Activities) read(ctx context.Context, uri string) ([]byte, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, permanent(fmt.Errorf("bad uri %q: %w", uri, err))
	}
	switch u.Scheme {
	case "http", "https":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
		if err != nil {
			return nil, permanent(err)
		}
		c := a.HTTP
		if c == nil {
			c = &http.Client{Timeout: 60 * time.Second}
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err // network errors are retryable
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone ||
			resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			return nil, permanent(fmt.Errorf("fetch %s: HTTP %d", uri, resp.StatusCode))
		}
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("fetch %s: HTTP %d", uri, resp.StatusCode)
		}
		return readLimited(resp.Body)
	case "file", "":
		p := uri
		if u.Scheme == "file" {
			p = u.Path
		}
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil, permanent(err)
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return readLimited(f)
	}
	return nil, permanent(fmt.Errorf("unsupported scheme %q", u.Scheme))
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileBytes {
		return nil, permanent(fmt.Errorf("file larger than %d bytes", MaxFileBytes))
	}
	return b, nil
}

// AlreadyIndexed reports whether this exact content was already indexed with
// the current embedder, in which case the workflow stops early.
func (a *Activities) AlreadyIndexed(ctx context.Context, docID, hash string) (bool, error) {
	r, err := a.Store.Get(ctx, docID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.Status == store.Indexed && r.ContentHash == hash && r.Embedder == a.Embedder.Name(), nil
}

func (a *Activities) MarkStarted(ctx context.Context, docID, uri, hash string) error {
	return a.Store.MarkStarted(ctx, docID, uri, hash)
}

// Parse turns the raw blob into a doc.Document blob and returns its hash.
func (a *Activities) Parse(ctx context.Context, uri, rawHash string) (string, error) {
	raw, err := a.Blobs.Get(rawHash)
	if err != nil {
		return "", err
	}
	d, err := a.Parser.Parse(ctx, uri, raw)
	if errors.Is(err, parse.ErrUnsupported) || errors.Is(err, parse.ErrCorrupt) {
		return "", permanent(err)
	}
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return a.Blobs.Put(b)
}

type ChunkResult struct {
	ChunksRef string `json:"chunks_ref"`
	Count     int    `json:"count"`
}

// ChunkAndEmbed chunks the parsed document and embeds every chunk.
//
// Embedding results are cached in the blob store under
// hash(embedder name, batch texts). When the activity is retried after a
// crash or timeout, batches that already succeeded are read from the cache
// instead of paying for the API call again; re-ingesting a document where
// only a few sections changed re-embeds only the batches that changed.
func (a *Activities) ChunkAndEmbed(ctx context.Context, parsedRef string, metadata map[string]string) (*ChunkResult, error) {
	b, err := a.Blobs.Get(parsedRef)
	if err != nil {
		return nil, err
	}
	var d doc.Document
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, permanent(err)
	}
	if len(metadata) > 0 {
		d.Metadata = metadata
	}
	chunks := chunk.Split(&d, a.Chunk)
	bs := a.EmbedBatch
	if bs <= 0 {
		bs = 32
	}
	hb := a.Heartbeat
	if hb == nil {
		hb = activity.RecordHeartbeat
	}
	for i := 0; i < len(chunks); i += bs {
		end := min(i+bs, len(chunks))
		texts := make([]string, 0, end-i)
		for _, c := range chunks[i:end] {
			texts = append(texts, c.Text)
		}
		key := doc.Hash([]byte(a.Embedder.Name() + "\x00" + strings.Join(texts, "\x00")))
		var vecs [][]float32
		if cached, err := a.Blobs.Get(key); err == nil && json.Unmarshal(cached, &vecs) == nil && len(vecs) == len(texts) {
			// cache hit
		} else {
			vecs, err = a.Embedder.Embed(ctx, texts)
			if err != nil {
				return nil, fmt.Errorf("embed batch %d: %w", i/bs, err)
			}
			if eb, err := json.Marshal(vecs); err == nil {
				_ = a.Blobs.PutAt(key, eb)
			}
		}
		for j := range vecs {
			chunks[i+j].Embedding = vecs[j]
		}
		hb(ctx, end)
	}
	out, err := json.Marshal(chunks)
	if err != nil {
		return nil, err
	}
	ref, err := a.Blobs.Put(out)
	if err != nil {
		return nil, err
	}
	return &ChunkResult{ChunksRef: ref, Count: len(chunks)}, nil
}

// IndexChunks writes chunks and removes chunks of older versions.
func (a *Activities) IndexChunks(ctx context.Context, docID, docHash, chunksRef string) error {
	b, err := a.Blobs.Get(chunksRef)
	if err != nil {
		return err
	}
	var chunks []doc.Chunk
	if err := json.Unmarshal(b, &chunks); err != nil {
		return permanent(err)
	}
	return a.Index.ReplaceDoc(ctx, docID, docHash, chunks)
}

func (a *Activities) MarkIndexed(ctx context.Context, docID, hash string, n int) error {
	return a.Store.MarkIndexed(ctx, docID, hash, n, a.Embedder.Name())
}

func (a *Activities) MarkFailed(ctx context.Context, docID, uri, step, msg string) error {
	return a.Store.MarkFailed(ctx, docID, uri, step, msg)
}

func (a *Activities) ListIndexed(ctx context.Context, after string, limit int) ([]store.Record, error) {
	return a.Store.List(ctx, store.Indexed, after, limit)
}
