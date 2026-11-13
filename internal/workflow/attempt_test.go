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

// TestLostAttemptIsNotFinishedSuccessAndLaunchesAgain records each lost
// observation and the reason passed to Finalize. A lost attempt launches again
// before the attempt limit, and that loss stays the finished reason.
func TestLostAttemptIsNotFinishedSuccessAndLaunchesAgain(t *testing.T) {
	stub := &lostFinishStub{}
	runAttemptWorkflow(t, stub)
	if len(stub.seen) != 3 || stub.seen[0] != 1 || stub.seen[1] != 2 || stub.seen[2] != 3 {
		t.Fatalf("observed launches = %v", stub.seen)
	}
	if stub.reason != "ATTEMPT_LOST" {
		t.Fatalf("reason = %q", stub.reason)
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

type lostFinishStub struct {
	launches int
	seen     []int
	reason   string
}

func (s *lostFinishStub) Initialize(context.Context, Input) (Initialized, error) {
	return Initialized{Deadline: time.Now().Add(time.Hour)}, nil
}

func (s *lostFinishStub) Prepare(context.Context, Input) error { return nil }

func (s *lostFinishStub) Launch(context.Context, Input) error {
	s.launches++
	return nil
}

func (s *lostFinishStub) Observe(context.Context, Input) error {
	s.seen = append(s.seen, s.launches)
	return temporal.NewNonRetryableApplicationError("lost", "ATTEMPT_LOST", nil)
}

func (s *lostFinishStub) Finalize(_ context.Context, in FinishInput) error {
	s.reason = in.Reason
	return nil
}

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
