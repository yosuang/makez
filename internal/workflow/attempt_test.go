package workflow

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

type attemptStub struct {
	launches int
	fail     bool
}

func (s *attemptStub) Initialize(context.Context, Input) (Initialized, error) {
	return Initialized{Deadline: time.Now().Add(time.Hour)}, nil
}

func (s *attemptStub) Prepare(context.Context, Input) error { return nil }

func (s *attemptStub) Launch(context.Context, Input) error {
	s.launches++
	return nil
}

func (s *attemptStub) Observe(context.Context, Input) error {
	if s.fail || s.launches < 3 {
		return temporal.NewNonRetryableApplicationError("lost", "ATTEMPT_LOST", nil)
	}
	return nil
}

func (s *attemptStub) Finalize(context.Context, FinishInput) error { return nil }

func TestLostAttemptLaunchesAgainUntilTheLimit(t *testing.T) {
	stub := &attemptStub{fail: true}
	runAttemptWorkflow(t, stub)
	if stub.launches != 3 {
		t.Fatalf("launches = %d", stub.launches)
	}
}

func TestLoopStopsAfterASuccessfulObserve(t *testing.T) {
	stub := &attemptStub{}
	runAttemptWorkflow(t, stub)
	if stub.launches != 3 {
		t.Fatalf("launches = %d, want the third attempt to settle", stub.launches)
	}
}

func TestOtherFailuresDoNotStartAnotherAttempt(t *testing.T) {
	stub := &failureStub{}
	runAttemptWorkflow(t, stub)
	if stub.launches != 1 {
		t.Fatalf("launches = %d", stub.launches)
	}
}

type failureStub struct{ launches int }

func (s *failureStub) Initialize(context.Context, Input) (Initialized, error) {
	return Initialized{Deadline: time.Now().Add(time.Hour)}, nil
}
func (s *failureStub) Prepare(context.Context, Input) error { return nil }
func (s *failureStub) Launch(context.Context, Input) error {
	s.launches++
	return nil
}
func (s *failureStub) Observe(context.Context, Input) error {
	return temporal.NewNonRetryableApplicationError("bad output", "INVALID_AGENT_OUTPUT", nil)
}
func (s *failureStub) Finalize(context.Context, FinishInput) error { return nil }

func runAttemptWorkflow(t *testing.T, activities any) {
	t.Helper()
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(Task)
	env.RegisterActivity(activities)
	env.ExecuteWorkflow(Task, Input{WorkspaceID: "w", TaskID: "t"})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
}
