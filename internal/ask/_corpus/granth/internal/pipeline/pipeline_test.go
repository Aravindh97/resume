package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/Aravindh97/granth/internal/blob"
	"github.com/Aravindh97/granth/internal/chunk"
	"github.com/Aravindh97/granth/internal/embed"
	"github.com/Aravindh97/granth/internal/index"
	"github.com/Aravindh97/granth/internal/parse"
	"github.com/Aravindh97/granth/internal/store"
)

// flakyEmbedder fails the first FailFirst calls, then works, and counts calls.
type flakyEmbedder struct {
	embed.Hashing
	FailFirst int32
	calls     atomic.Int32
}

func (f *flakyEmbedder) Embed(ctx context.Context, t []string) ([][]float32, error) {
	if f.calls.Add(1) <= f.FailFirst {
		return nil, errors.New("embedding service: 503")
	}
	return f.Hashing.Embed(ctx, t)
}

func newActs(t *testing.T, e embed.Embedder) (*Activities, *index.Memory, *store.Memory) {
	t.Helper()
	b, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ix, st := index.NewMemory(), store.NewMemory()
	return &Activities{Blobs: b, Parser: parse.New(nil), Chunk: chunk.Default(), Embedder: e, Index: ix, Store: st,
		EmbedBatch: 2, Heartbeat: func(context.Context, ...any) {}}, ix, st
}

func corpus(name string) string {
	p, _ := filepath.Abs(filepath.Join("../../testdata/corpus", name))
	return p
}

func runWorkflow(t *testing.T, a *Activities, wf any, in any, out any) {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(a)
	env.RegisterWorkflow(IngestDocument)
	env.RegisterWorkflow(IngestBatch)
	env.ExecuteWorkflow(wf, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if err := env.GetWorkflowResult(out); err != nil {
		t.Fatal(err)
	}
}

func TestIngestDocumentHappyPathAndReplaySkips(t *testing.T) {
	a, ix, st := newActs(t, embed.NewHashing(64))
	var r IngestResult
	runWorkflow(t, a, IngestDocument, IngestInput{URI: corpus("leave-policy.md")}, &r)
	if r.Status != "indexed" || r.Chunks == 0 || ix.Len() != r.Chunks {
		t.Fatalf("result = %+v, index has %d", r, ix.Len())
	}
	rec, _ := st.Get(context.Background(), r.DocID)
	if rec.Status != store.Indexed || rec.ChunkCount != r.Chunks {
		t.Fatalf("ledger = %+v", rec)
	}
	var again IngestResult
	runWorkflow(t, a, IngestDocument, IngestInput{URI: corpus("leave-policy.md")}, &again)
	if again.Status != "skipped" || ix.Len() != r.Chunks {
		t.Fatalf("second run = %+v, index has %d", again, ix.Len())
	}
}

func TestTransientEmbedFailureIsRetried(t *testing.T) {
	e := &flakyEmbedder{Hashing: embed.NewHashing(64), FailFirst: 2}
	a, _, _ := newActs(t, e)
	var r IngestResult
	runWorkflow(t, a, IngestDocument, IngestInput{URI: corpus("travel-policy.md")}, &r)
	if r.Status != "indexed" {
		t.Fatalf("result = %+v", r)
	}
}

func TestPermanentFailureGoesToDeadLetterWithoutRetries(t *testing.T) {
	a, _, st := newActs(t, embed.NewHashing(64))
	bad, _ := filepath.Abs("../../testdata/bad/broken.pdf")
	var r IngestResult
	runWorkflow(t, a, IngestDocument, IngestInput{URI: bad}, &r)
	if r.Status != "failed" || r.FailedStep != "parse" {
		t.Fatalf("result = %+v", r)
	}
	failed, _ := st.List(context.Background(), store.Failed, "", 10)
	if len(failed) != 1 || failed[0].FailedStep != "parse" || failed[0].LastError == "" {
		t.Fatalf("dead letter = %+v", failed)
	}
}

func TestBatchContinuesPastBadFiles(t *testing.T) {
	a, _, _ := newActs(t, embed.NewHashing(64))
	bad, _ := filepath.Abs("../../testdata/bad/archive.zip")
	in := BatchInput{Concurrency: 2, Docs: []IngestInput{
		{URI: corpus("leave-policy.md")}, {URI: bad}, {URI: corpus("infosec-policy.md")},
		{URI: "/does/not/exist.md"}, {URI: corpus("product-catalog.csv")},
	}}
	var br BatchResult
	runWorkflow(t, a, IngestBatch, in, &br)
	if br.Indexed != 3 || br.Failed != 2 || len(br.Results) != 5 {
		t.Fatalf("batch = %+v", br)
	}
}

func TestNewVersionReplacesOldChunks(t *testing.T) {
	a, ix, _ := newActs(t, embed.NewHashing(64))
	p := filepath.Join(t.TempDir(), "note.md")
	os.WriteFile(p, []byte("# Note\n\nVersion one text.\n\n## Extra\n\nMore text here."), 0o600)
	if r := RunLocal(context.Background(), a, IngestInput{URI: p}); r.Status != "indexed" || ix.Len() != 2 {
		t.Fatalf("v1: %+v, %d chunks", r, ix.Len())
	}
	os.WriteFile(p, []byte("# Note\n\nVersion two text."), 0o600)
	if r := RunLocal(context.Background(), a, IngestInput{URI: p}); r.Status != "indexed" || ix.Len() != 1 {
		t.Fatalf("v2: %+v, %d chunks", r, ix.Len())
	}
}

func TestEmbeddingCacheAvoidsRepeatCalls(t *testing.T) {
	e := &flakyEmbedder{Hashing: embed.NewHashing(64)}
	a, _, _ := newActs(t, e)
	in := IngestInput{URI: corpus("leave-policy.md")}
	RunLocal(context.Background(), a, in)
	first := e.calls.Load()
	in.Force = true // re-ingest same content, e.g. a retried activity
	if r := RunLocal(context.Background(), a, in); r.Status != "indexed" {
		t.Fatal(r)
	}
	if e.calls.Load() != first {
		t.Fatalf("embedder called again: %d -> %d", first, e.calls.Load())
	}
}
