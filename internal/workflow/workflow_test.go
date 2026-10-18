package workflow

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

var testInput = Input{WorkspaceID: "workspace", TaskID: "task"}

func workflowEnvironment(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetStartTime(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
	env.RegisterActivityWithOptions(func(context.Context, Input) (Initialized, error) {
		return Initialized{}, errors.New("unexpected Initialize invocation")
	}, activity.RegisterOptions{Name: "Initialize"})
	for _, name := range []string{"Prepare", "Launch", "Observe", "Reap", "BlockCleanup"} {
		env.RegisterActivityWithOptions(func(context.Context, Input) error {
			return temporal.NewNonRetryableApplicationError("unexpected activity invocation", "TEST_UNEXPECTED", nil)
		}, activity.RegisterOptions{Name: name})
	}
	env.RegisterActivityWithOptions(func(context.Context, FinishInput) error {
		return temporal.NewNonRetryableApplicationError("unexpected Finalize invocation", "TEST_UNEXPECTED", nil)
	}, activity.RegisterOptions{Name: "Finalize"})
	t.Cleanup(func() { env.AssertExpectations(t) })
	return env
}

func assertWorkflowSucceeded(t *testing.T, env *testsuite.TestWorkflowEnvironment) {
	t.Helper()
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not finish")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
}

func TestTaskNormalLifecycleInOrder(t *testing.T) {
	env := workflowEnvironment(t)
	var mutex sync.Mutex
	var calls []string
	record := func(name string) { mutex.Lock(); calls = append(calls, name); mutex.Unlock() }
	env.OnActivity("Initialize", mock.Anything, testInput).Return(Initialized{Deadline: env.Now().Add(time.Hour)}, nil).Once().Run(func(mock.Arguments) { record("Initialize") })
	for _, name := range []string{"Prepare", "Launch", "Observe"} {
		env.OnActivity(name, mock.Anything, testInput).Return(nil).Once().Run(func(mock.Arguments) { record(name) })
	}
	env.OnActivity("Finalize", mock.Anything, FinishInput{Input: testInput}).Return(nil).Once().Run(func(mock.Arguments) { record("Finalize") })
	env.ExecuteWorkflow(Task, testInput)
	assertWorkflowSucceeded(t, env)
	mutex.Lock()
	defer mutex.Unlock()
	if !reflect.DeepEqual(calls, []string{"Initialize", "Prepare", "Launch", "Observe", "Finalize"}) {
		t.Fatalf("unexpected lifecycle: %v", calls)
	}
}

func TestTaskCanceledStillFinalizes(t *testing.T) {
	env := workflowEnvironment(t)
	env.OnActivity("Initialize", mock.Anything, testInput).Return(Initialized{Deadline: env.Now().Add(time.Hour)}, nil).Once()
	env.OnActivity("Prepare", mock.Anything, testInput).Return(nil).Once()
	env.OnActivity("Launch", mock.Anything, testInput).Return(nil).Once()
	env.OnActivity("Observe", mock.Anything, testInput).After(time.Minute).Return(temporal.NewCanceledError()).Once()
	env.OnActivity("Finalize", mock.Anything, FinishInput{Input: testInput, Reason: "CANCEL_REQUESTED"}).Return(func(ctx context.Context, _ FinishInput) error {
		if ctx.Err() != nil {
			return errors.New("finalizer inherited workflow cancellation")
		}
		return nil
	}).Once()
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Second)
	env.ExecuteWorkflow(Task, testInput)
	assertWorkflowSucceeded(t, env)
}

func TestTaskDeadlineUsesTimeoutReason(t *testing.T) {
	env := workflowEnvironment(t)
	env.OnActivity("Initialize", mock.Anything, testInput).Return(Initialized{Deadline: env.Now().Add(10 * time.Second)}, nil).Once()
	env.OnActivity("Prepare", mock.Anything, testInput).Return(nil).Once()
	env.OnActivity("Launch", mock.Anything, testInput).Return(nil).Once()
	env.OnActivity("Observe", mock.Anything, testInput).After(20 * time.Second).Return(temporal.NewCanceledError()).Once()
	env.OnActivity("Finalize", mock.Anything, FinishInput{Input: testInput, Reason: "DEADLINE_EXCEEDED"}).Return(nil).Once()
	env.ExecuteWorkflow(Task, testInput)
	assertWorkflowSucceeded(t, env)
}

func TestTaskPreparationFailureDoesNotLaunch(t *testing.T) {
	env := workflowEnvironment(t)
	env.OnActivity("Initialize", mock.Anything, testInput).Return(Initialized{Deadline: env.Now().Add(time.Hour)}, nil).Once()
	env.OnActivity("Prepare", mock.Anything, testInput).Return(temporal.NewNonRetryableApplicationError("unsupported backend", "UNSUPPORTED_CAPABILITY", nil)).Once()
	env.OnActivity("Finalize", mock.Anything, FinishInput{Input: testInput, Reason: "UNSUPPORTED_CAPABILITY"}).Return(nil).Once()
	env.ExecuteWorkflow(Task, testInput)
	assertWorkflowSucceeded(t, env)
	env.AssertNotCalled(t, "Launch", mock.Anything, mock.Anything)
	env.AssertNotCalled(t, "Observe", mock.Anything, mock.Anything)
}

func TestTaskAlreadyTerminalDoesNotCreateResources(t *testing.T) {
	env := workflowEnvironment(t)
	env.OnActivity("Initialize", mock.Anything, testInput).Return(Initialized{Done: true}, nil).Once()
	env.ExecuteWorkflow(Task, testInput)
	assertWorkflowSucceeded(t, env)
	for _, name := range []string{"Prepare", "Launch", "Observe", "Finalize"} {
		env.AssertNotCalled(t, name, mock.Anything, mock.Anything)
	}
}

func TestCleanupRetriesTransientDeletion(t *testing.T) {
	env := workflowEnvironment(t)
	env.OnActivity("Reap", mock.Anything, testInput).Return(errors.New("temporarily unavailable")).Once()
	env.OnActivity("Reap", mock.Anything, testInput).Return(nil).Once()
	env.ExecuteWorkflow(Cleanup, testInput)
	assertWorkflowSucceeded(t, env)
	env.AssertNotCalled(t, "BlockCleanup", mock.Anything, mock.Anything)
}

func TestCleanupMarksBlockedAfterPermanentFailure(t *testing.T) {
	env := workflowEnvironment(t)
	env.OnActivity("Reap", mock.Anything, testInput).Return(temporal.NewNonRetryableApplicationError("backend access revoked", "PERMISSION_DENIED", nil)).Once()
	env.OnActivity("BlockCleanup", mock.Anything, testInput).Return(nil).Once()
	env.ExecuteWorkflow(Cleanup, testInput)
	assertWorkflowSucceeded(t, env)
}

func TestCleanupEventuallyMarksBlockedOnRepeatedFailure(t *testing.T) {
	env := workflowEnvironment(t)
	var attempts atomic.Int32
	// The Temporal test environment caps an unlimited retry policy at ten attempts.
	env.OnActivity("Reap", mock.Anything, testInput).Return(errors.New("sandbox backend unavailable")).Run(func(mock.Arguments) { attempts.Add(1) })
	env.OnActivity("BlockCleanup", mock.Anything, testInput).Return(nil).Once()
	env.ExecuteWorkflow(Cleanup, testInput)
	assertWorkflowSucceeded(t, env)
	if attempts.Load() < 2 {
		t.Fatal("transient cleanup failure blocked without retries")
	}
}

func TestFinalizeRetryDoesNotRelaunchAgent(t *testing.T) {
	env := workflowEnvironment(t)
	env.OnActivity("Initialize", mock.Anything, testInput).Return(Initialized{Deadline: env.Now().Add(time.Hour)}, nil).Once()
	for _, name := range []string{"Prepare", "Launch", "Observe"} {
		env.OnActivity(name, mock.Anything, testInput).Return(nil).Once()
	}
	env.OnActivity("Finalize", mock.Anything, FinishInput{Input: testInput}).Return(errors.New("artifact storage temporarily unavailable")).Once()
	env.OnActivity("Finalize", mock.Anything, FinishInput{Input: testInput}).Return(nil).Once()
	env.ExecuteWorkflow(Task, testInput)
	assertWorkflowSucceeded(t, env)
}
