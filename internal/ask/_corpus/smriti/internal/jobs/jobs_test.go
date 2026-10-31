package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/Aravindh97/smriti/internal/memory"
	"github.com/Aravindh97/smriti/internal/store"
)

func TestRememberRetriesExtractionThenWrites(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	var a *Activities
	cands := []memory.Candidate{{Scope: memory.User, Subject: "manager", Content: "manager is Arjun", Confidence: 0.9, SourceRef: "c/0"}}
	calls := 0
	env.RegisterActivity(a)
	env.OnActivity(a.Extract, mock.Anything, mock.Anything).Return(func(context.Context, RememberInput) ([]memory.Candidate, error) {
		if calls++; calls < 3 {
			return nil, errors.New("llm timeout")
		}
		return cands, nil
	})
	env.OnActivity(a.Write, mock.Anything, mock.Anything, cands).Return([]store.WriteResult{{Action: memory.Add}}, nil).Once()
	env.ExecuteWorkflow(RememberConversation, RememberInput{Principal: memory.Principal{OrgID: "o", UserID: "u"}})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res []store.WriteResult
	env.GetWorkflowResult(&res)
	if calls != 3 || len(res) != 1 || res[0].Action != memory.Add {
		t.Fatalf("calls=%d res=%+v", calls, res)
	}
	env.AssertExpectations(t)
}

func TestMaintainRunsExpireThenConsolidate(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	var a *Activities
	env.RegisterActivity(a)
	env.OnActivity(a.Expire, mock.Anything).Return(int64(4), nil).Once()
	env.OnActivity(a.Consolidate, mock.Anything, 0.95).Return(2, nil).Once()
	env.ExecuteWorkflow(Maintain, 0.0)
	var r MaintainResult
	if err := env.GetWorkflowResult(&r); err != nil || r.Expired != 4 || r.Merged != 2 {
		t.Fatalf("%v %+v", err, r)
	}
}
