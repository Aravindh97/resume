// Package jobs runs Smriti's write path and maintenance on Temporal.
//
// RememberConversation extracts candidates (an LLM call that may fail or time
// out) and writes them. Both steps are safe to retry: extraction is a pure
// function of the conversation, and Write ignores (source, subject) pairs it
// has already processed.
//
// Maintain expires memories whose TTL passed and merges near-duplicates. It
// is started on a Temporal schedule (see `smriti schedule`).
package jobs

import (
	"context"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Aravindh97/smriti/internal/extract"
	"github.com/Aravindh97/smriti/internal/memory"
	"github.com/Aravindh97/smriti/internal/store"
)

const TaskQueue = "smriti"

type Activities struct {
	Store     *store.Store
	Extractor extract.Extractor
}

type RememberInput struct {
	Principal    memory.Principal    `json:"principal"`
	Conversation memory.Conversation `json:"conversation"`
	Team         string              `json:"team,omitempty"`
}

func (a *Activities) Extract(ctx context.Context, in RememberInput) ([]memory.Candidate, error) {
	return a.Extractor.Extract(ctx, in.Conversation, extract.Context{Team: in.Team})
}

func (a *Activities) Write(ctx context.Context, p memory.Principal, c []memory.Candidate) ([]store.WriteResult, error) {
	return a.Store.Write(ctx, p, c)
}

func (a *Activities) Expire(ctx context.Context) (int64, error) { return a.Store.ExpireDue(ctx) }

func (a *Activities) Consolidate(ctx context.Context, threshold float64) (int, error) {
	total := 0
	for {
		n, err := a.Store.Consolidate(ctx, threshold, 500)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

var retry = &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute}

func ao(ctx workflow.Context, d time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: d, ScheduleToCloseTimeout: time.Hour, RetryPolicy: retry})
}

func RememberConversation(ctx workflow.Context, in RememberInput) ([]store.WriteResult, error) {
	var a *Activities
	var cands []memory.Candidate
	if err := workflow.ExecuteActivity(ao(ctx, 2*time.Minute), a.Extract, in).Get(ctx, &cands); err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}
	var res []store.WriteResult
	err := workflow.ExecuteActivity(ao(ctx, time.Minute), a.Write, in.Principal, cands).Get(ctx, &res)
	return res, err
}

type MaintainResult struct {
	Expired int64 `json:"expired"`
	Merged  int   `json:"merged"`
}

func Maintain(ctx workflow.Context, threshold float64) (*MaintainResult, error) {
	var a *Activities
	if threshold == 0 {
		threshold = 0.95
	}
	r := &MaintainResult{}
	if err := workflow.ExecuteActivity(ao(ctx, 10*time.Minute), a.Expire).Get(ctx, &r.Expired); err != nil {
		return nil, err
	}
	if err := workflow.ExecuteActivity(ao(ctx, 30*time.Minute), a.Consolidate, threshold).Get(ctx, &r.Merged); err != nil {
		return nil, err
	}
	return r, nil
}
