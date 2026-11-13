package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestUpdateRunKeepsTheFirstSandbox uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Storing a sandbox record is not a claim that an external sandbox was created once,
// and it is not exactly-once for that action.
func TestUpdateRunKeepsTheFirstSandbox(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "sandbox-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "sandbox-identity")
	ids := sandboxIDs{workspace: workspace, taskID: taskID, runID: runID}

	key := core.OperationKey{WorkspaceID: workspace, TaskID: taskID, RunID: runID, Step: "sandbox"}
	first := core.SandboxRef{BackendID: "test", ID: "sandbox-1", Key: key}
	replacement := first
	replacement.ID = "sandbox-2"

	opened := readSandboxRow(t, db, ids)
	if opened.sandbox != nil || opened.run != core.RunRunning || opened.task != core.TaskRunning || opened.reason != "" || opened.outbox != 0 || opened.tasks != 1 || opened.runs != 1 || opened.version < 1 {
		t.Fatalf("opened %s", describeSandbox(opened))
	}

	// A mismatched run id, task id, or backend id conflicts.
	// The stored sandbox stays empty. The run version does not move.
	// The task stays RUNNING. No outbox command is inserted.
	wrongRun := first
	wrongRun.Key.RunID = newID()
	wrongTask := first
	wrongTask.Key.TaskID = newID()
	wrongBackend := first
	wrongBackend.BackendID = "other"
	mismatchedRun := requireSandboxConflict(t, db, ids, RunUpdate{Sandbox: &wrongRun}, opened, "mismatched run id")
	mismatchedTask := requireSandboxConflict(t, db, ids, RunUpdate{Sandbox: &wrongTask}, opened, "mismatched task id")
	mismatchedBackend := requireSandboxConflict(t, db, ids, RunUpdate{Sandbox: &wrongBackend}, opened, "mismatched backend id")
	for _, held := range []sandboxRow{mismatchedRun, mismatchedTask, mismatchedBackend} {
		if held.sandbox != nil || held.version != opened.version || held.run != core.RunRunning || held.task != core.TaskRunning || held.task == core.TaskSucceeded || held.outbox != 0 {
			t.Fatalf("mismatched sandbox wrote %s", describeSandbox(held))
		}
	}

	// A matching sandbox with id sandbox-1 and backend test is stored.
	// The task stays RUNNING. No outbox command is inserted.
	storedWant := opened
	storedWant.sandbox = &first
	storedWant.version++
	stored := requireSandboxWrite(t, db, ids, RunUpdate{Sandbox: &first}, storedWant, "matching sandbox")
	if stored.sandbox == nil || stored.sandbox.ID != "sandbox-1" || stored.sandbox.BackendID != "test" || stored.run != core.RunRunning || stored.task != core.TaskRunning || stored.task == core.TaskSucceeded || stored.outbox != 0 {
		t.Fatalf("matching sandbox wrote %s", describeSandbox(stored))
	}

	// A different sandbox id conflicts. The stored id stays sandbox-1.
	// The run version does not move. The task does not become SUCCEEDED.
	// No outbox command is inserted.
	different := requireSandboxConflict(t, db, ids, RunUpdate{Sandbox: &replacement}, stored, "different sandbox id")
	if different.sandbox == nil || different.sandbox.ID != "sandbox-1" || different.version != stored.version || different.run != core.RunRunning || different.task != core.TaskRunning || different.task == core.TaskSucceeded || different.outbox != 0 {
		t.Fatalf("different sandbox id wrote %s", describeSandbox(different))
	}

	// The same sandbox id is accepted again. The stored id stays sandbox-1.
	replayWant := stored
	replayWant.version++
	replayed := requireSandboxWrite(t, db, ids, RunUpdate{Sandbox: &first}, replayWant, "same sandbox id")
	if replayed.sandbox == nil || replayed.sandbox.ID != "sandbox-1" || replayed.sandbox.BackendID != "test" || replayed.run != core.RunRunning || replayed.task != core.TaskRunning || replayed.task == core.TaskSucceeded || replayed.outbox != 0 {
		t.Fatalf("same sandbox id wrote %s", describeSandbox(replayed))
	}
}

type sandboxIDs struct {
	workspace string
	taskID    string
	runID     string
}

type sandboxRow struct {
	version int64
	run     core.RunState
	sandbox *core.SandboxRef
	task    core.TaskState
	reason  string
	tasks   int
	runs    int
	outbox  int
}

func requireSandboxConflict(t *testing.T, db *Store, ids sandboxIDs, update RunUpdate, before sandboxRow, label string) sandboxRow {
	t.Helper()
	err := db.UpdateRun(t.Context(), ids.runID, update)
	got := readSandboxRow(t, db, ids)
	if !errors.Is(err, ErrConflict) || !sameSandboxRow(got, before) || got.task == core.TaskSucceeded || got.outbox != 0 {
		t.Fatalf("%s: UpdateRun returned %v; got %s, want ErrConflict with %s", label, err, describeSandbox(got), describeSandbox(before))
	}
	return got
}

func requireSandboxWrite(t *testing.T, db *Store, ids sandboxIDs, update RunUpdate, want sandboxRow, label string) sandboxRow {
	t.Helper()
	err := db.UpdateRun(t.Context(), ids.runID, update)
	got := readSandboxRow(t, db, ids)
	if err != nil || !sameSandboxRow(got, want) || got.task != core.TaskRunning || got.task == core.TaskSucceeded || got.outbox != 0 {
		t.Fatalf("%s: UpdateRun returned %v; got %s, want %s", label, err, describeSandbox(got), describeSandbox(want))
	}
	return got
}

func readSandboxRow(t *testing.T, db *Store, ids sandboxIDs) sandboxRow {
	t.Helper()
	var row sandboxRow
	var run, task string
	var raw []byte
	err := db.pool.QueryRow(t.Context(), `
		SELECT
			r.version,
			r.state,
			r.sandbox,
			tk.state,
			tk.reason,
			(SELECT count(*) FROM makez_tasks WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_runs WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_outbox WHERE workspace_id=$1 OR task_id=$2 OR run_id=$3)
		FROM makez_runs r
		JOIN makez_tasks tk ON tk.id=r.task_id
		WHERE r.workspace_id=$1 AND r.id=$3 AND tk.id=$2`,
		ids.workspace, ids.taskID, ids.runID).Scan(
		&row.version, &run, &raw, &task, &row.reason, &row.tasks, &row.runs, &row.outbox)
	if err != nil {
		t.Fatal(err)
	}
	row.run = core.RunState(run)
	row.task = core.TaskState(task)
	row.sandbox = jsonRef[core.SandboxRef](t, raw)
	return row
}

func sameSandboxRow(got, want sandboxRow) bool {
	return got.version == want.version && got.run == want.run && got.task == want.task && got.reason == want.reason && got.tasks == want.tasks && got.runs == want.runs && got.outbox == want.outbox && sameSandbox(got.sandbox, want.sandbox)
}

func describeSandbox(row sandboxRow) string {
	id := ""
	backend := ""
	if row.sandbox != nil {
		id = row.sandbox.ID
		backend = row.sandbox.BackendID
	}
	return fmt.Sprintf("sandbox %q backend %q run %s task %s reason %q outbox %d tasks %d runs %d version %d", id, backend, row.run, row.task, row.reason, row.outbox, row.tasks, row.runs, row.version)
}
