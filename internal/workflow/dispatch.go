package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	tw "go.temporal.io/sdk/workflow"
)

// Recovery reconciles unfinished execution and cleanup after interrupted dispatch.
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
	if _, ok := errors.AsType[*serviceerror.WorkflowExecutionAlreadyStarted](err); ok {
		return nil
	}
	return err
}

// Dispatch retries durable delivery only; Temporal remains the execution scheduler.
func Dispatch(ctx context.Context, db *store.Store, c client.Client) {
	var wg sync.WaitGroup
	wg.Go(func() {
		cursor := store.RecoveryCursor{}
		repeat(ctx, 15*time.Second, func() { cursor = reconcile(ctx, db, c, cursor) })
	})
	defer wg.Wait()
	repeat(ctx, time.Second, func() { dispatchOnce(ctx, db, c) })
}

func repeat(ctx context.Context, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		fn()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func dispatchOnce(ctx context.Context, db *store.Store, c client.Client) {
	rows, err := db.Pending(ctx, 50)
	if err != nil {
		slog.Error("outbox read failed", "error_type", fmt.Sprintf("%T", err))
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
			if _, ok := errors.AsType[*serviceerror.NotFound](err); ok {
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
				slog.Error("outbox acknowledgment failed", "error_type", fmt.Sprintf("%T", e))
			}
		} else {
			if db.Metrics != nil {
				db.Metrics.OutboxRetries.Add(ctx, 1)
			}
			if retryErr := db.RetryOutbox(ctx, row.ID, "Temporal delivery unavailable"); retryErr != nil {
				slog.Error("outbox retry persistence failed", "task_id", row.TaskID, "error_type", fmt.Sprintf("%T", retryErr))
			}
			slog.Warn("outbox delivery deferred", "task", row.TaskID, "command", row.Command, "error_type", fmt.Sprintf("%T", err))
		}
	}
}
func reconcile(ctx context.Context, db *store.Store, c client.Client, cursor store.RecoveryCursor) store.RecoveryCursor {
	if cursor.Through.IsZero() {
		cursor.Through = time.Now()
	}
	tasks, err := db.Recoverable(ctx, cursor)
	if err != nil {
		slog.Error("recovery scan failed", "error_type", fmt.Sprintf("%T", err))
		return cursor
	}
	for _, t := range tasks {
		if ctx.Err() != nil {
			return cursor
		}
		cursor.CreatedAt, cursor.ID = t.CreatedAt, t.ID
		if t.Run == nil {
			if !store.IsTerminal(t.State) && (!time.Now().Before(t.Deadline) || time.Since(t.CreatedAt) > 10*time.Minute) {
				if err := db.Publish(ctx, t.WorkspaceID, t.ID, core.TaskFailed, "DEADLINE_EXCEEDED", true, core.CleanupNotRequired, nil); err != nil {
					slog.Error("expired task publication failed", "task_id", t.ID, "error_type", fmt.Sprintf("%T", err))
				}
			}
			continue
		}
		if store.IsTerminal(t.State) {
			if t.Run.Cleanup == core.CleanupPending {
				checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := start(checkCtx, c, CleanupID(t.WorkspaceID, t.ID), Cleanup, Input{t.WorkspaceID, t.ID}); err != nil {
					slog.Warn("cleanup dispatch failed", "task_id", t.ID, "error_type", fmt.Sprintf("%T", err))
				}
				cancel()
			}
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		desc, err := c.DescribeWorkflowExecution(checkCtx, ID(t.WorkspaceID, t.ID), "")
		cancel()
		_, missing := errors.AsType[*serviceerror.NotFound](err)
		if err != nil && !missing {
			slog.Warn("workflow inspection failed", "task_id", t.ID, "error_type", fmt.Sprintf("%T", err))
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
			slog.Error("recovery dispatch failed", "task", t.ID, "error_type", fmt.Sprintf("%T", err))
		}
	}
	if len(tasks) < store.RecoveryPageSize {
		return store.RecoveryCursor{}
	}
	return cursor
}
