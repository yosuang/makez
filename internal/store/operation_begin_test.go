package store

import (
	"errors"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

// TestBeginOperationInsertsOneDispatchingRowOnlyWhileRunning uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Refusing to begin an operation is not a claim that an external action already ran once.
// A new operation row is not exactly-once for an external action and not Pi acceptance.
func TestBeginOperationInsertsOneDispatchingRowOnlyWhileRunning(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "fence-"+workspace); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if !operationFenceFuture.After(now) || operationFencePast.After(now) {
		t.Fatalf("fixture clock %s does not put %s after now and %s at or before now", now.UTC().Format(time.RFC3339Nano), operationFenceFuture.Format(time.RFC3339Nano), operationFencePast.Format(time.RFC3339Nano))
	}

	requireOneDispatchingOperation(t, db, workspace, "sandbox")
	requireOneDispatchingOperation(t, db, workspace, "agent")
	cases := refusedBeginCases()
	for _, tc := range cases {
		requireBeginRefused(t, db, workspace, tc)
	}
	requireOperationFenceCounts(t, db, workspace, len(cases)+2)
}

var (
	operationFenceFuture = time.Date(2099, 3, 14, 16, 0, 0, 0, time.UTC)
	operationFencePast   = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
)

type beginCase struct {
	name     string
	state    core.TaskState
	reason   string
	deadline time.Time
	step     string
	hash     string
	live     bool
}

func refusedBeginCases() []beginCase {
	future, past := operationFenceFuture, operationFencePast
	return []beginCase{
		{name: "QUEUED", state: core.TaskQueued, reason: "kept-QUEUED", deadline: future, step: "sandbox", hash: "request-hash-queued", live: true},
		{name: "CANCELING", state: core.TaskCanceling, reason: "kept-CANCELING", deadline: future, step: "sandbox", hash: "request-hash-canceling", live: true},
		{name: "CANCELED", state: core.TaskCanceled, reason: "kept-CANCELED", deadline: future, step: "sandbox", hash: "request-hash-canceled", live: true},
		{name: "SUCCEEDED", state: core.TaskSucceeded, reason: "kept-SUCCEEDED", deadline: future, step: "sandbox", hash: "request-hash-succeeded", live: true},
		{name: "FAILED", state: core.TaskFailed, reason: "kept-FAILED", deadline: future, step: "sandbox", hash: "request-hash-failed", live: true},
		{name: "expired", state: core.TaskRunning, reason: "kept-RUNNING", deadline: past, step: "agent", hash: "request-hash-expired", live: false},
		{name: "unknown-step", state: core.TaskRunning, reason: "kept-unknown", deadline: future, step: "bash", hash: "request-hash-unknown", live: true},
		{name: "empty-hash", state: core.TaskRunning, reason: "kept-empty", deadline: future, step: "sandbox", hash: "", live: true},
	}
}

func requireOneDispatchingOperation(t *testing.T, db *Store, workspace, step string) {
	t.Helper()
	taskID, runID := newID(), newID()
	insertRun(t, db, workspace, taskID, runID, "fence-live-"+step)
	moveTask(t, db, taskID, time.Now().UTC(), operationFenceFuture)
	before := readTaskState(t, db, taskID)
	deadline := readTaskDeadline(t, db, taskID)
	if before.state != string(core.TaskRunning) || before.reason != "" || !deadline.Equal(operationFenceFuture) || !deadline.After(time.Now()) {
		t.Fatalf("%s before begin = state %s reason %q deadline %s", step, before.state, before.reason, deadline.UTC().Format(time.RFC3339Nano))
	}
	hash := "request-hash-" + step
	phase, created, err := db.BeginOperation(t.Context(), runID, step, hash)
	after := readTaskState(t, db, taskID)
	rows := readRunOperations(t, db, runID)
	outbox := outboxCommands(t, db, taskID, runID)
	if err != nil || !created || phase != "DISPATCHING" || rows.total != 1 || rows.dispatching != 1 || rows.phase != "DISPATCHING" || rows.hash != hash {
		t.Fatalf("%s begin = %q created=%v err=%v operations=%d dispatching=%d phase=%q hash=%q", step, phase, created, err, rows.total, rows.dispatching, rows.phase, rows.hash)
	}
	if n := operationCount(t, db, runID, step); n != 1 {
		t.Fatalf("%s step rows = %d", step, n)
	}
	if after.state == string(core.TaskSucceeded) {
		t.Fatalf("%s begin marked the task SUCCEEDED", step)
	}
	if after != before {
		t.Fatalf("%s task = state %s reason %q, want state %s reason %q", step, after.state, after.reason, before.state, before.reason)
	}
	if outbox != 0 {
		t.Fatalf("%s outbox commands = %d", step, outbox)
	}
}

func requireBeginRefused(t *testing.T, db *Store, workspace string, tc beginCase) {
	t.Helper()
	taskID, runID := newID(), newID()
	insertRun(t, db, workspace, taskID, runID, "fence-"+tc.name)
	moveTask(t, db, taskID, time.Now().UTC(), tc.deadline)
	moveTaskState(t, db, taskID, tc.state, tc.reason)
	before := readTaskState(t, db, taskID)
	deadline := readTaskDeadline(t, db, taskID)
	if before.state != string(tc.state) || before.reason != tc.reason || !deadline.Equal(tc.deadline) || deadline.After(time.Now()) != tc.live {
		t.Fatalf("%s before begin = state %s reason %q deadline %s live=%v", tc.name, before.state, before.reason, deadline.UTC().Format(time.RFC3339Nano), deadline.After(time.Now()))
	}
	phase, created, err := db.BeginOperation(t.Context(), runID, tc.step, tc.hash)
	after := readTaskState(t, db, taskID)
	rows := readRunOperations(t, db, runID)
	outbox := outboxCommands(t, db, taskID, runID)
	becameSucceeded := before.state != string(core.TaskSucceeded) && after.state == string(core.TaskSucceeded)
	refused := errors.Is(err, ErrConflict) && !created && phase == "" && rows.total == 0 && rows.dispatching == 0 && rows.phase == "" && rows.hash == "" && after == before && outbox == 0 && !becameSucceeded
	if refused {
		return
	}
	lead := "refused incorrectly"
	if rows.total > 0 {
		lead = "gained an operation row"
	}
	if becameSucceeded {
		lead = "became SUCCEEDED because begin was called; " + lead
	}
	t.Errorf("%s %s: operations=%d dispatching=%d phase=%q begin=%q created=%v err=%v task=%s reason=%q was=%s was_reason=%q outbox=%d deadline=%s",
		tc.name, lead, rows.total, rows.dispatching, rows.phase, phase, created, err, after.state, after.reason, before.state, before.reason, outbox, deadline.UTC().Format(time.RFC3339Nano))
}

type runOperations struct {
	total       int
	dispatching int
	phase       string
	hash        string
}

func readRunOperations(t *testing.T, db *Store, runID string) runOperations {
	t.Helper()
	var rows runOperations
	err := db.pool.QueryRow(t.Context(), `
		SELECT count(*),
			count(*) FILTER (WHERE phase = 'DISPATCHING'),
			COALESCE(min(phase), ''),
			COALESCE(min(request_hash), '')
		FROM makez_operations WHERE run_id = $1`, runID).Scan(&rows.total, &rows.dispatching, &rows.phase, &rows.hash)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func readTaskDeadline(t *testing.T, db *Store, id string) time.Time {
	t.Helper()
	var deadline time.Time
	if err := db.pool.QueryRow(t.Context(), `SELECT deadline FROM makez_tasks WHERE id = $1`, id).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	return deadline
}

func outboxCommands(t *testing.T, db *Store, taskID, runID string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_outbox WHERE task_id = $1 OR run_id = $2`, taskID, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func requireOperationFenceCounts(t *testing.T, db *Store, workspace string, tasks int) {
	t.Helper()
	var gotTasks, runs, operations, dispatching, outbox int
	err := db.pool.QueryRow(t.Context(), `
		SELECT
			(SELECT count(*) FROM makez_tasks WHERE workspace_id = $1),
			(SELECT count(*) FROM makez_runs WHERE workspace_id = $1),
			(SELECT count(*) FROM makez_operations WHERE workspace_id = $1),
			(SELECT count(*) FROM makez_operations WHERE workspace_id = $1 AND phase = 'DISPATCHING'),
			(SELECT count(*) FROM makez_outbox WHERE workspace_id = $1)`, workspace).Scan(&gotTasks, &runs, &operations, &dispatching, &outbox)
	if err != nil {
		t.Fatal(err)
	}
	if gotTasks != tasks || runs != tasks || operations != 2 || dispatching != 2 || outbox != 0 {
		t.Fatalf("workspace rows tasks=%d runs=%d operations=%d dispatching=%d outbox=%d, want tasks=%d runs=%d operations=2 dispatching=2 outbox=0", gotTasks, runs, operations, dispatching, outbox, tasks, tasks)
	}
}
