// Package store persists memories: in PostgreSQL with pgvector (Store, in
// postgres.go) or in memory for the browser demo and fast tests (Mem). Both
// apply the same rules.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/Aravindh97/smriti/internal/embed"
	"github.com/Aravindh97/smriti/internal/memory"
)

// Memories is what the eval, the browser demo and tools need from a store.
type Memories interface {
	Write(ctx context.Context, p memory.Principal, cands []memory.Candidate) ([]WriteResult, error)
	Recall(ctx context.Context, p memory.Principal, query string, o RecallOptions) ([]Recalled, error)
	History(ctx context.Context, p memory.Principal, subject string) ([]memory.Memory, error)
	Forget(ctx context.Context, p memory.Principal, id string) (int64, error)
	ExpireDue(ctx context.Context) (int64, error)
	Consolidate(ctx context.Context, threshold float64, limit int) (int, error)
	SetClock(now func() time.Time)
	Embeds() embed.Embedder
}

type WriteResult struct {
	Candidate memory.Candidate `json:"candidate"`
	Action    memory.Action    `json:"action"` // add | confirm | supersede | ignore | duplicate | denied
	MemoryID  string           `json:"memory_id,omitempty"`
	Replaced  string           `json:"replaced,omitempty"`
	Reason    string           `json:"reason,omitempty"`
}

const Duplicate memory.Action = "duplicate"
const Denied memory.Action = "denied"

type RecallOptions struct {
	K      int
	AsOf   *time.Time     // what was believed at this time; nil = now
	Scopes []memory.Scope // restrict to these scopes; empty = all visible
}

type Recalled struct {
	Memory     memory.Memory `json:"memory"`
	Similarity float64       `json:"similarity"`
	Score      float64       `json:"score"`
}

var ErrNotFound = errors.New("memory not found")

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
