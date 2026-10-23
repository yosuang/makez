// Package workflow owns durable orchestration; all external I/O lives in Activities.
package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	tw "go.temporal.io/sdk/workflow"
)

// Queue is the task queue used by Makez workflows and activities.
const Queue = "makez-v1"

// Input identifies a task within its workspace.
type Input struct{ WorkspaceID, TaskID string }

// Initialized returns the task deadline and whether execution is already complete.
type Initialized struct {
	Done     bool
	Deadline time.Time
}

// FinishInput carries the task identity and finalization reason.
type FinishInput struct {
	Input
	Reason string
}

// ID returns the stable workflow ID for a task.
func ID(workspace, task string) string { return "makez/task/" + workspace + "/" + task }

// CleanupID returns the stable cleanup workflow ID for a task.
func CleanupID(workspace, task string) string { return "makez/cleanup/" + workspace + "/" + task }

// Task orchestrates preparation, execution, observation, and finalization.
func Task(ctx tw.Context, in Input) error {
	opts := tw.ActivityOptions{StartToCloseTimeout: 3 * time.Minute, WaitForCancellation: true, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 10 * time.Second, MaximumAttempts: 5}}
	ctx = tw.WithActivityOptions(ctx, opts)
	var init Initialized
	err := tw.ExecuteActivity(ctx, "Initialize", in).Get(ctx, &init)
	if err == nil && init.Done {
		return nil
	}
	reason := ""
	if err == nil {
		execution, cancel := tw.WithCancel(ctx)
		timerCtx, stopTimer := tw.WithCancel(ctx)
		tw.Go(timerCtx, func(timerCtx tw.Context) {
			if tw.NewTimer(timerCtx, init.Deadline.Sub(tw.Now(timerCtx))).Get(timerCtx, nil) == nil {
				reason = "DEADLINE_EXCEEDED"
				cancel()
			}
		})
		for _, name := range []string{"Prepare", "Launch", "Observe"} {
			activityCtx := execution
			if name == "Observe" {
				waitOpts := opts
				waitOpts.StartToCloseTimeout = init.Deadline.Sub(tw.Now(ctx)) + time.Minute
				if waitOpts.StartToCloseTimeout < time.Minute {
					waitOpts.StartToCloseTimeout = time.Minute
				}
				waitOpts.HeartbeatTimeout = 30 * time.Second
				waitOpts.WaitForCancellation = true
				activityCtx = tw.WithActivityOptions(execution, waitOpts)
			}
			err = tw.ExecuteActivity(activityCtx, name, in).Get(activityCtx, nil)
			if err != nil {
				break
			}
		}
		stopTimer()
		cancel()
	}
	if err != nil && reason == "" {
		if temporal.IsCanceledError(err) {
			reason = "CANCEL_REQUESTED"
		} else {
			reason = "EXECUTION_FAILED"
			var app *temporal.ApplicationError
			if findApplicationError(err, &app) {
				reason = app.Type()
			}
		}
	}
	// Business cancellation never cuts off stop/archive/delete/publish.
	finalCtx, _ := tw.NewDisconnectedContext(ctx)
	finalCtx = tw.WithActivityOptions(finalCtx, tw.ActivityOptions{StartToCloseTimeout: 5 * time.Minute, HeartbeatTimeout: 30 * time.Second, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 10 * time.Second, MaximumAttempts: 5}})
	return tw.ExecuteActivity(finalCtx, "Finalize", FinishInput{Input: in, Reason: reason}).Get(finalCtx, nil)
}

// Cleanup retries sandbox disposal and records blocked cleanup when retries expire.
func Cleanup(ctx tw.Context, in Input) error {
	ctx = tw.WithActivityOptions(ctx, tw.ActivityOptions{StartToCloseTimeout: 2 * time.Minute, ScheduleToCloseTimeout: 30 * time.Minute, RetryPolicy: &temporal.RetryPolicy{InitialInterval: 5 * time.Second, MaximumInterval: time.Minute}})
	err := tw.ExecuteActivity(ctx, "Reap", in).Get(ctx, nil)
	if err == nil {
		return nil
	}
	ctx = tw.WithActivityOptions(ctx, tw.ActivityOptions{StartToCloseTimeout: 30 * time.Second, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 5}})
	return tw.ExecuteActivity(ctx, "BlockCleanup", in).Get(ctx, nil)
}
