package workflow

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	tw "go.temporal.io/sdk/workflow"
)

func Recovery(ctx tw.Context, in Input) error {
	ctx = tw.WithActivityOptions(ctx, tw.ActivityOptions{StartToCloseTimeout: 5 * time.Minute, HeartbeatTimeout: 30 * time.Second, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 10 * time.Second, MaximumAttempts: 5}})
	return tw.ExecuteActivity(ctx, "Finalize", FinishInput{Input: in, Reason: "WORKFLOW_INTERRUPTED"}).Get(ctx, nil)
}
func start(ctx context.Context, c client.Client, id string, wf any, in Input) error {
	policy := enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE
	if strings.HasPrefix(id, "makez/recover/") || strings.HasPrefix(id, "makez/cleanup/") {
		policy = enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY
	}
	_, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: Queue, WorkflowIDReusePolicy: policy, WorkflowExecutionErrorWhenAlreadyStarted: true}, wf, in)
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &already) {
		return nil
	}
	return err
}

// Dispatch retries durable delivery only; Temporal remains the execution scheduler.
func Dispatch(ctx context.Context, db *store.Store, c client.Client) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	iteration := 0
	for {
		dispatchOnce(ctx, db, c)
		if iteration%15 == 0 {
			reconcile(ctx, db, c)
		}
		iteration++
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func dispatchOnce(ctx context.Context, db *store.Store, c client.Client) {
	rows, err := db.Pending(ctx, 50)
	if err != nil {
		slog.Error("outbox read failed", "error", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		in := Input{row.WorkspaceID, row.TaskID}
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		switch row.Command {
		case "START_TASK":
			t, e := db.Get(opCtx, row.WorkspaceID, row.TaskID)
			if e != nil {
				err = e
			} else if store.IsTerminal(t.State) {
				err = nil
			} else {
				err = start(opCtx, c, ID(row.WorkspaceID, row.TaskID), Task, in)
			}
		case "CANCEL_TASK":
			err = c.CancelWorkflow(opCtx, ID(row.WorkspaceID, row.TaskID), "")
			var missing *serviceerror.NotFound
			if errors.As(err, &missing) {
				t, e := db.Get(opCtx, row.WorkspaceID, row.TaskID)
				if e == nil && store.IsTerminal(t.State) {
					err = nil
				}
			}
		case "START_CLEANUP":
			err = start(opCtx, c, CleanupID(row.WorkspaceID, row.TaskID), Cleanup, in)
		default:
			err = errors.New("unknown outbox command")
		}
		cancel()
		if err == nil {
			if e := db.Delivered(ctx, row.ID); e != nil {
				slog.Error("outbox acknowledgment failed", "error", e)
			}
		} else {
			db.RetryOutbox(ctx, row.ID, "Temporal delivery unavailable")
			slog.Warn("outbox delivery deferred", "task", row.TaskID, "command", row.Command, "error", err)
		}
	}
}
func reconcile(ctx context.Context, db *store.Store, c client.Client) {
	tasks, err := db.Recoverable(ctx)
	if err != nil {
		return
	}
	for _, t := range tasks {
		if ctx.Err() != nil {
			return
		}
		if t.Run == nil {
			if !store.IsTerminal(t.State) && (!time.Now().Before(t.Deadline) || time.Since(t.CreatedAt) > 10*time.Minute) {
				db.Publish(ctx, t.WorkspaceID, t.ID, core.TaskFailed, "DEADLINE_EXCEEDED", true, core.CleanupNotRequired, nil)
			}
			continue
		}
		if store.IsTerminal(t.State) {
			if t.Run.Cleanup == core.CleanupPending {
				checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = start(checkCtx, c, CleanupID(t.WorkspaceID, t.ID), Cleanup, Input{t.WorkspaceID, t.ID})
				cancel()
			}
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		desc, err := c.DescribeWorkflowExecution(checkCtx, ID(t.WorkspaceID, t.ID), "")
		cancel()
		var missing *serviceerror.NotFound
		if err != nil && !errors.As(err, &missing) {
			continue
		}
		if err == nil && desc.WorkflowExecutionInfo.Status == enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
			continue
		}
		in := Input{t.WorkspaceID, t.ID}
		recoveryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = start(recoveryCtx, c, "makez/recover/"+t.WorkspaceID+"/"+t.ID, Recovery, in)
		cancel()
		if err != nil {
			slog.Error("recovery dispatch failed", "task", t.ID, "error", err)
		}
	}
}
