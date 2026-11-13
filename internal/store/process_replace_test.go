package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

// TestUpdateRunReplacesAProcessOnlyAfterExit uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Replacing a process record is not a claim that an external action ran once,
// and it is not exactly-once for that action.
func TestUpdateRunReplacesAProcessOnlyAfterExit(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "process-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "process-replace")
	ids := processIDs{workspace: workspace, taskID: taskID, runID: runID}

	key := core.OperationKey{WorkspaceID: workspace, TaskID: taskID, RunID: runID, Step: "agent"}
	sandbox := core.SandboxRef{BackendID: "test", ID: "sandbox-1", Key: key}
	first := core.ProcessRef{Sandbox: sandbox, ID: "process-1", Key: key}
	replacement := first
	replacement.ID = "process-2"

	opened := readProcessRow(t, db, ids)
	if opened.process != nil || opened.exit != nil || opened.sandbox != nil || opened.run != core.RunRunning || opened.task != core.TaskRunning || opened.reason != "" || opened.outbox != 0 || opened.tasks != 1 || opened.runs != 1 || opened.version < 1 {
		t.Fatalf("opened %s", describeProcess(opened))
	}

	// A run with no sandbox rejects a process. The receipt stays empty and the version does not move.
	empty := requireProcessConflict(t, db, ids, RunUpdate{Process: &first}, opened, "run with no sandbox")
	if empty.process != nil || empty.exit != nil || empty.version != opened.version || empty.task != core.TaskRunning || empty.outbox != 0 {
		t.Fatalf("run with no sandbox wrote %s", describeProcess(empty))
	}

	// A matching sandbox is stored before any process. The exit stays empty.
	withSandbox := opened
	withSandbox.sandbox = &sandbox
	withSandbox.version++
	sandboxed := requireProcessWrite(t, db, ids, RunUpdate{Sandbox: &sandbox}, withSandbox, "matching sandbox")

	// The first matching process is stored. The exit stays empty.
	withProcess := sandboxed
	withProcess.process = &first
	withProcess.version++
	stored := requireProcessWrite(t, db, ids, RunUpdate{Process: &first}, withProcess, "first matching process")

	// A different process id conflicts while the exit is empty.
	// The stored process id stays, the task stays RUNNING, and no outbox command is inserted.
	held := requireProcessConflict(t, db, ids, RunUpdate{Process: &replacement}, stored, "different process id while exit is empty")
	if held.process == nil || held.process.ID != first.ID || held.exit != nil || held.version != stored.version || held.task != core.TaskRunning || held.task == core.TaskSucceeded || held.outbox != 0 {
		t.Fatalf("different process id while exit is empty wrote %s", describeProcess(held))
	}

	// The receipt id matches. The conflict is the run id, task id, or sandbox id.
	wrongRun := first
	wrongRun.Key.RunID = newID()
	wrongTask := first
	wrongTask.Key.TaskID = newID()
	wrongSandbox := first
	wrongSandbox.Sandbox.ID = "sandbox-other"
	requireProcessConflict(t, db, ids, RunUpdate{Process: &wrongRun}, held, "mismatched run id")
	requireProcessConflict(t, db, ids, RunUpdate{Process: &wrongTask}, held, "mismatched task id")
	requireProcessConflict(t, db, ids, RunUpdate{Process: &wrongSandbox}, held, "mismatched sandbox id")

	// After an exit is stored, a different process id replaces the receipt and clears the exit.
	// The task stays RUNNING, and replacing the receipt inserts no outbox command.
	exit := core.ProcessExit{Code: 7, Signal: "SIGTERM", FinishedAt: time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC)}
	withExit := held
	withExit.exit = &exit
	withExit.version++
	exited := requireProcessWrite(t, db, ids, RunUpdate{Exit: &exit}, withExit, "stored exit")
	replaced := withExit
	replaced.process = &replacement
	replaced.exit = nil
	replaced.version++
	wrote := requireProcessWrite(t, db, ids, RunUpdate{Process: &replacement}, replaced, "process id after exit")
	if wrote.exit != nil || wrote.process == nil || wrote.process.ID != replacement.ID || wrote.task != core.TaskRunning || wrote.task == core.TaskSucceeded || wrote.outbox != 0 || wrote.run != core.RunRunning {
		t.Fatalf("process id after exit wrote %s", describeProcess(wrote))
	}
	if exited.process == nil || exited.process.ID != first.ID || exited.exit == nil {
		t.Fatalf("stored exit wrote %s", describeProcess(exited))
	}
}

type processIDs struct {
	workspace string
	taskID    string
	runID     string
}

type processRow struct {
	version int64
	run     core.RunState
	sandbox *core.SandboxRef
	process *core.ProcessRef
	exit    *core.ProcessExit
	task    core.TaskState
	reason  string
	tasks   int
	runs    int
	outbox  int
}

func requireProcessConflict(t *testing.T, db *Store, ids processIDs, update RunUpdate, before processRow, label string) processRow {
	t.Helper()
	err := db.UpdateRun(t.Context(), ids.runID, update)
	got := readProcessRow(t, db, ids)
	if !errors.Is(err, ErrConflict) || !sameProcessRow(got, before) || got.task == core.TaskSucceeded || got.outbox != 0 {
		t.Fatalf("%s: UpdateRun returned %v; got %s, want ErrConflict with %s", label, err, describeProcess(got), describeProcess(before))
	}
	return got
}

func requireProcessWrite(t *testing.T, db *Store, ids processIDs, update RunUpdate, want processRow, label string) processRow {
	t.Helper()
	err := db.UpdateRun(t.Context(), ids.runID, update)
	got := readProcessRow(t, db, ids)
	if err != nil || !sameProcessRow(got, want) || got.task != core.TaskRunning || got.task == core.TaskSucceeded || got.outbox != 0 {
		t.Fatalf("%s: UpdateRun returned %v; got %s, want %s", label, err, describeProcess(got), describeProcess(want))
	}
	return got
}

func readProcessRow(t *testing.T, db *Store, ids processIDs) processRow {
	t.Helper()
	var row processRow
	var run, task string
	var sandboxRaw, processRaw, exitRaw []byte
	err := db.pool.QueryRow(t.Context(), `
		SELECT
			r.version,
			r.state,
			r.sandbox,
			r.process,
			r.process_exit,
			tk.state,
			tk.reason,
			(SELECT count(*) FROM makez_tasks WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_runs WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_outbox WHERE workspace_id=$1 OR task_id=$2 OR run_id=$3)
		FROM makez_runs r
		JOIN makez_tasks tk ON tk.id=r.task_id
		WHERE r.workspace_id=$1 AND r.id=$3 AND tk.id=$2`,
		ids.workspace, ids.taskID, ids.runID).Scan(
		&row.version, &run, &sandboxRaw, &processRaw, &exitRaw, &task, &row.reason, &row.tasks, &row.runs, &row.outbox)
	if err != nil {
		t.Fatal(err)
	}
	row.run = core.RunState(run)
	row.task = core.TaskState(task)
	row.sandbox = jsonRef[core.SandboxRef](t, sandboxRaw)
	row.process = jsonRef[core.ProcessRef](t, processRaw)
	row.exit = jsonRef[core.ProcessExit](t, exitRaw)
	return row
}

func jsonRef[T any](t *testing.T, raw []byte) *T {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var value *T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func sameProcessRow(got, want processRow) bool {
	return got.version == want.version && got.run == want.run && got.task == want.task && got.reason == want.reason && got.tasks == want.tasks && got.runs == want.runs && got.outbox == want.outbox && sameSandbox(got.sandbox, want.sandbox) && sameProcess(got.process, want.process) && sameExit(got.exit, want.exit)
}

func sameSandbox(got, want *core.SandboxRef) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func sameProcess(got, want *core.ProcessRef) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func sameExit(got, want *core.ProcessExit) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got.Code == want.Code && got.Signal == want.Signal && got.FinishedAt.Equal(want.FinishedAt)
}

func describeProcess(row processRow) string {
	processID := ""
	processSandbox := ""
	if row.process != nil {
		processID = row.process.ID
		processSandbox = row.process.Sandbox.ID
	}
	sandboxID := ""
	if row.sandbox != nil {
		sandboxID = row.sandbox.ID
	}
	exit := "empty"
	if row.exit != nil {
		exit = fmt.Sprintf("code %d at %s", row.exit.Code, row.exit.FinishedAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("process %q process-sandbox %q run-sandbox %q exit %s run %s task %s reason %q outbox %d version %d", processID, processSandbox, sandboxID, exit, row.run, row.task, row.reason, row.outbox, row.version)
}
