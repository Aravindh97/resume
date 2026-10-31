package pipeline

import (
	"errors"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const TaskQueue = "granth-ingest"

type IngestInput struct {
	URI      string            `json:"uri"`
	Metadata map[string]string `json:"metadata,omitempty"`
	// Force re-indexes even if the same content is already indexed (used by
	// Reindex after an embedding model change).
	Force bool `json:"force,omitempty"`
}

type IngestResult struct {
	DocID      string `json:"doc_id"`
	URI        string `json:"uri"`
	Status     string `json:"status"` // indexed | skipped | failed
	Chunks     int    `json:"chunks,omitempty"`
	FailedStep string `json:"failed_step,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Transient failures (index or embedding service down, network errors) are
// retried with exponential backoff capped at one minute, for up to
// RetryBudget per step. Only after that does a document go to the
// dead-letter state. Permanent errors skip retries entirely.
var retry = &temporal.RetryPolicy{
	InitialInterval:        time.Second,
	BackoffCoefficient:     2,
	MaximumInterval:        time.Minute,
	NonRetryableErrorTypes: []string{ErrPermanent},
}

// RetryBudget bounds how long one step may keep retrying.
var RetryBudget = time.Hour

func opts(ctx workflow.Context, timeout time.Duration, heartbeat time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    timeout,
		ScheduleToCloseTimeout: RetryBudget,
		HeartbeatTimeout:       heartbeat,
		RetryPolicy:            retry,
	})
}

// IngestDocument runs one document through the pipeline. It never fails the
// workflow for a bad document: after retries are exhausted it records the
// failing step in the ledger (the dead-letter state) and returns a "failed"
// result, so a batch keeps going.
func IngestDocument(ctx workflow.Context, in IngestInput) (*IngestResult, error) {
	var a *Activities // nil receiver: only used to name activity methods
	res := &IngestResult{URI: in.URI}
	short := opts(ctx, 2*time.Minute, 0)

	fail := func(step string, err error) (*IngestResult, error) {
		res.Status, res.FailedStep, res.Error = "failed", step, rootMessage(err)
		docID := res.DocID
		if docID == "" {
			docID = docIDFor(in.URI)
		}
		if merr := workflow.ExecuteActivity(short, a.MarkFailed, docID, in.URI, step, res.Error).Get(ctx, nil); merr != nil {
			return nil, merr // ledger unreachable after retries: surface it
		}
		return res, nil
	}

	var f FetchResult
	if err := workflow.ExecuteActivity(short, a.Fetch, in.URI).Get(ctx, &f); err != nil {
		return fail("fetch", err)
	}
	res.DocID = f.DocID

	if !in.Force {
		var done bool
		if err := workflow.ExecuteActivity(short, a.AlreadyIndexed, f.DocID, f.Hash).Get(ctx, &done); err != nil {
			return fail("check", err)
		}
		if done {
			res.Status = "skipped"
			return res, nil
		}
	}
	if err := workflow.ExecuteActivity(short, a.MarkStarted, f.DocID, in.URI, f.Hash).Get(ctx, nil); err != nil {
		return fail("ledger", err)
	}

	var parsed string
	// OCR of a long scanned PDF can take a while.
	if err := workflow.ExecuteActivity(opts(ctx, 20*time.Minute, 0), a.Parse, in.URI, f.Hash).Get(ctx, &parsed); err != nil {
		return fail("parse", err)
	}

	var cr ChunkResult
	if err := workflow.ExecuteActivity(opts(ctx, 30*time.Minute, time.Minute), a.ChunkAndEmbed, parsed, in.Metadata).Get(ctx, &cr); err != nil {
		return fail("embed", err)
	}
	if err := workflow.ExecuteActivity(opts(ctx, 5*time.Minute, 0), a.IndexChunks, f.DocID, f.Hash, cr.ChunksRef).Get(ctx, nil); err != nil {
		return fail("index", err)
	}
	if err := workflow.ExecuteActivity(short, a.MarkIndexed, f.DocID, f.Hash, cr.Count).Get(ctx, nil); err != nil {
		return fail("ledger", err)
	}
	res.Status, res.Chunks = "indexed", cr.Count
	return res, nil
}

type BatchInput struct {
	Docs        []IngestInput `json:"docs"`
	Concurrency int           `json:"concurrency,omitempty"`
}

type BatchResult struct {
	Indexed, Skipped, Failed int
	Results                  []IngestResult
}

// IngestBatch runs IngestDocument as child workflows with a bounded number in
// flight. The child workflow id is derived from the document id, so two
// batches that contain the same file cannot ingest it concurrently.
func IngestBatch(ctx workflow.Context, in BatchInput) (*BatchResult, error) {
	conc := in.Concurrency
	if conc <= 0 {
		conc = 8
	}
	out := &BatchResult{}
	type pending struct {
		f  workflow.ChildWorkflowFuture
		in IngestInput
	}
	var inflight []pending
	collect := func(p pending) {
		var r IngestResult
		if err := p.f.Get(ctx, &r); err != nil {
			r = IngestResult{URI: p.in.URI, Status: "failed", FailedStep: "workflow", Error: rootMessage(err)}
		}
		switch r.Status {
		case "indexed":
			out.Indexed++
		case "skipped":
			out.Skipped++
		default:
			out.Failed++
		}
		out.Results = append(out.Results, r)
	}
	for _, d := range in.Docs {
		if len(inflight) == conc {
			collect(inflight[0])
			inflight = inflight[1:]
		}
		cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID:            "ingest-" + docIDFor(d.URI),
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
			ParentClosePolicy:     enums.PARENT_CLOSE_POLICY_ABANDON,
		})
		inflight = append(inflight, pending{workflow.ExecuteChildWorkflow(cctx, IngestDocument, d), d})
	}
	for _, p := range inflight {
		collect(p)
	}
	return out, nil
}

type ReindexInput struct {
	After    string `json:"after"`
	PageSize int    `json:"page_size"`
	Done     int    `json:"done"`
}

// Reindex re-ingests every indexed document page by page (for example after
// switching embedding model). It continues-as-new after each page, so the
// workflow history stays small and a crash resumes from the last cursor.
func Reindex(ctx workflow.Context, in ReindexInput) (int, error) {
	if in.PageSize <= 0 {
		in.PageSize = 100
	}
	var a *Activities
	var page []recordRef
	if err := workflow.ExecuteActivity(opts(ctx, time.Minute, 0), a.ListIndexed, in.After, in.PageSize).Get(ctx, &page); err != nil {
		return in.Done, err
	}
	if len(page) == 0 {
		return in.Done, nil
	}
	b := BatchInput{}
	for _, r := range page {
		b.Docs = append(b.Docs, IngestInput{URI: r.SourceURI, Force: true})
	}
	var br BatchResult
	if err := workflow.ExecuteChildWorkflow(ctx, IngestBatch, b).Get(ctx, &br); err != nil {
		return in.Done, err
	}
	next := ReindexInput{After: page[len(page)-1].DocID, PageSize: in.PageSize, Done: in.Done + br.Indexed}
	return 0, workflow.NewContinueAsNewError(ctx, Reindex, next)
}

// recordRef decodes the fields Reindex needs from store.Record.
type recordRef struct {
	DocID     string `json:"doc_id"`
	SourceURI string `json:"source_uri"`
}

func rootMessage(err error) string {
	var ae *temporal.ApplicationError
	if errors.As(err, &ae) {
		return ae.Message()
	}
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err.Error()
		}
		err = u
	}
}
