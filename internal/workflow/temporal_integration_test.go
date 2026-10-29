package workflow

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/store"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	tw "go.temporal.io/sdk/workflow"
)

type lostStartResponse struct {
	client.Client
	runID string
}

func (c *lostStartResponse) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, wf any, args ...any) (client.WorkflowRun, error) {
	run, err := c.Client.ExecuteWorkflow(ctx, options, wf, args...)
	if err != nil {
		return run, err
	}
	c.runID = run.GetRunID()
	// Temporal has committed the start; the dispatcher loses the response and dies before ack.
	return nil, context.DeadlineExceeded
}

func durableBoundaryWorkflow(ctx tw.Context, in Input) (string, error) {
	ctx = tw.WithActivityOptions(ctx, tw.ActivityOptions{StartToCloseTimeout: 5 * time.Second, RetryPolicy: &temporal.RetryPolicy{InitialInterval: 20 * time.Millisecond, MaximumAttempts: 3}})
	var run string
	if err := tw.ExecuteActivity(ctx, "durableInitialize", in).Get(ctx, &run); err != nil {
		return "", err
	}
	var finish bool
	tw.GetSignalChannel(ctx, "finish").Receive(ctx, &finish)
	return run, nil
}

func TestLiveTemporalLostAckLeaseExpiryAndWorkerRestart(t *testing.T) {
	address := os.Getenv("MAKEZ_TEST_TEMPORAL_ADDRESS")
	if address == "" {
		t.Skip("set MAKEZ_TEST_TEMPORAL_ADDRESS to an isolated Temporal service")
	}
	if os.Getenv("MAKEZ_TEST_DATABASE_URL") == "" {
		t.Fatal("live Temporal acceptance also requires real PostgreSQL")
	}
	db := activityStore(t)
	task := activityTask(t, db, false)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	remote, err := client.DialContext(ctx, client.Options{HostPort: address})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	var attempts atomic.Int32
	startWorker := func() worker.Worker {
		w := worker.New(remote, Queue, worker.Options{})
		w.RegisterWorkflowWithOptions(durableBoundaryWorkflow, tw.RegisterOptions{Name: "Task"})
		w.RegisterActivityWithOptions(func(ctx context.Context, in Input) (string, error) {
			saved, err := db.InitializeRun(ctx, in.WorkspaceID, in.TaskID, "live-temporal-test")
			if err != nil {
				return "", err
			}
			if attempts.Add(1) == 1 {
				return "", errors.New("injected activity response loss after database commit")
			}
			return saved.Run.ID, nil
		}, activity.RegisterOptions{Name: "durableInitialize"})
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		return w
	}
	w := startWorker()
	defer func() { w.Stop() }()
	in := Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID}
	id := ID(in.WorkspaceID, in.TaskID)
	defer func() { _ = remote.TerminateWorkflow(context.Background(), id, "", "isolated test cleanup") }()
	rows, err := db.Pending(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatal("initial claim", err)
	}
	old := rows[0]
	lossy := &lostStartResponse{Client: remote}
	if err = start(ctx, lossy, id, Task, in); !errors.Is(err, context.DeadlineExceeded) || lossy.runID == "" {
		t.Fatal("start did not commit before injected response loss", err)
	}
	t.Log("Temporal start committed; response lost; outbox remains unacknowledged")
	var reclaimed store.Outbox
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for reclaimed.ID == "" {
		rows, err = db.Pending(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			reclaimed = rows[0]
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	if reclaimed.ID != old.ID || reclaimed.LeaseToken <= old.LeaseToken {
		t.Fatal("lease did not retain event identity and advance fence")
	}
	if err = db.Delivered(ctx, old); !errors.Is(err, store.ErrStale) {
		t.Fatal("old dispatcher acknowledged new lease", err)
	}
	if err = start(ctx, remote, id, Task, in); err != nil {
		t.Fatal("redelivery could not verify existing workflow", err)
	}
	if err = db.Delivered(ctx, reclaimed); err != nil {
		t.Fatal(err)
	}
	saved, err := db.Get(ctx, in.WorkspaceID, in.TaskID)
	if err != nil || saved.Run == nil || attempts.Load() != 2 {
		t.Fatalf("activity retry did not converge: attempts=%d err=%v", attempts.Load(), err)
	}
	t.Log("lease expired; stale ack rejected; activity retried once into the same PostgreSQL run")
	w.Stop()
	w = startWorker()
	if err = remote.SignalWorkflow(ctx, id, lossy.runID, "finish", true); err != nil {
		t.Fatal(err)
	}
	var businessRun string
	if err = remote.GetWorkflow(ctx, id, lossy.runID).Get(ctx, &businessRun); err != nil || businessRun != saved.Run.ID {
		t.Fatal("worker restart changed business run", err)
	}
	if err = start(ctx, remote, id, Task, in); err != nil {
		t.Fatal("closed duplicate could not be verified", err)
	}
	description, err := remote.DescribeWorkflowExecution(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if description.WorkflowExecutionInfo.Execution.RunId != lossy.runID || attempts.Load() != 2 {
		t.Fatal("closed workflow was restarted")
	}
	t.Logf("verified stable workflow/task/run mapping; activity_attempts=%d; worker restarted; closed workflow not reused", attempts.Load())
}
