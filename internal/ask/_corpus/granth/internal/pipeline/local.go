package pipeline

import (
	"context"
	"errors"

	"go.temporal.io/sdk/temporal"

	"github.com/Aravindh97/granth/internal/doc"
	"github.com/Aravindh97/granth/internal/resilience"
)

func docIDFor(uri string) string { return doc.DocID(uri) }

// RunLocal runs the same activities in-process without a Temporal server,
// with in-process retries. It is used by `granth ingest --local`, tests and
// the eval harness. Production ingestion goes through IngestDocument.
func RunLocal(ctx context.Context, a *Activities, in IngestInput) IngestResult {
	if a.Heartbeat == nil {
		a.Heartbeat = func(context.Context, ...any) {}
	}
	r := resilience.DefaultRetry()
	res := IngestResult{URI: in.URI, DocID: docIDFor(in.URI)}
	step := func(name string, fn func(context.Context) error) bool {
		err := r.Do(ctx, func(ctx context.Context) error {
			err := fn(ctx)
			var ae *temporal.ApplicationError
			if errors.As(err, &ae) && ae.NonRetryable() {
				return resilience.Permanent{Err: err}
			}
			return err
		})
		if err != nil {
			res.Status, res.FailedStep, res.Error = "failed", name, err.Error()
			_ = a.MarkFailed(ctx, res.DocID, in.URI, name, res.Error)
			return false
		}
		return true
	}
	var f *FetchResult
	if !step("fetch", func(ctx context.Context) (err error) { f, err = a.Fetch(ctx, in.URI); return }) {
		return res
	}
	if !in.Force {
		var done bool
		if !step("check", func(ctx context.Context) (err error) { done, err = a.AlreadyIndexed(ctx, f.DocID, f.Hash); return }) {
			return res
		}
		if done {
			res.Status = "skipped"
			return res
		}
	}
	var parsed string
	var cr *ChunkResult
	ok := step("ledger", func(ctx context.Context) error { return a.MarkStarted(ctx, f.DocID, in.URI, f.Hash) }) &&
		step("parse", func(ctx context.Context) (err error) { parsed, err = a.Parse(ctx, in.URI, f.Hash); return }) &&
		step("embed", func(ctx context.Context) (err error) { cr, err = a.ChunkAndEmbed(ctx, parsed, in.Metadata); return }) &&
		step("index", func(ctx context.Context) error { return a.IndexChunks(ctx, f.DocID, f.Hash, cr.ChunksRef) }) &&
		step("ledger", func(ctx context.Context) error { return a.MarkIndexed(ctx, f.DocID, f.Hash, cr.Count) })
	if ok {
		res.Status, res.Chunks = "indexed", cr.Count
	}
	return res
}
