package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestUpdateRunKeepsTheFirstPreparedResult uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Keeping the first prepared result is not a claim that an external action ran once,
// and it is not exactly-once for that action.
func TestUpdateRunKeepsTheFirstPreparedResult(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "prepared-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "prepared-result")
	ids := preparedIDs{workspace: workspace, taskID: taskID, runID: runID}

	// An empty artifact list round-trips as the same JSON. A nil list does not.
	done := preparedResult(core.TaskSucceeded, "DONE")
	opened := readPreparedRow(t, db, ids)
	if opened.result != nil || opened.run != core.RunRunning || opened.task != core.TaskRunning || opened.reason != "" || opened.outbox != 0 || opened.tasks != 1 || opened.runs != 1 || opened.version < 1 {
		t.Fatalf("opened %s", describePrepared(opened))
	}

	// A RUNNING run rejects a terminal prepared result.
	// prepared_result stays null. The run version does not move.
	// The task stays RUNNING. No outbox command is inserted.
	rejected := requirePreparedConflict(t, db, ids, RunUpdate{Result: done}, opened, "RUNNING rejects a terminal prepared result")
	if rejected.result != nil || rejected.version != opened.version || rejected.run != core.RunRunning || rejected.task != core.TaskRunning || rejected.task == core.TaskSucceeded || rejected.outbox != 0 {
		t.Fatalf("RUNNING rejects a terminal prepared result wrote %s", describePrepared(rejected))
	}

	finalizing := core.RunFinalizing
	movedWant := opened
	movedWant.run = core.RunFinalizing
	movedWant.version++
	moved := requirePreparedWrite(t, db, ids, RunUpdate{State: &finalizing}, movedWant, "run moves to FINALIZING")
	if moved.result != nil || moved.run != core.RunFinalizing || moved.task != core.TaskRunning || moved.outbox != 0 {
		t.Fatalf("run moves to FINALIZING wrote %s", describePrepared(moved))
	}

	// A non-terminal result state conflicts. prepared_result stays null.
	for _, state := range []core.TaskState{core.TaskQueued, core.TaskRunning, core.TaskCanceling} {
		offer := preparedResult(state, "DONE")
		held := requirePreparedConflict(t, db, ids, RunUpdate{Result: offer}, moved, "non-terminal result "+string(state))
		if held.result != nil || held.version != moved.version || held.run != core.RunFinalizing || held.task != core.TaskRunning || held.outbox != 0 {
			t.Fatalf("non-terminal result %s wrote %s", state, describePrepared(held))
		}
	}

	// A SUCCEEDED result with reason DONE is stored.
	// The run stays FINALIZING. The task stays RUNNING and does not become SUCCEEDED.
	// No outbox command is inserted.
	storedWant := moved
	storedWant.result = done
	storedWant.version++
	stored := requirePreparedWrite(t, db, ids, RunUpdate{Result: done}, storedWant, "SUCCEEDED DONE")
	if stored.result == nil || stored.result.State != core.TaskSucceeded || stored.result.Reason != "DONE" || stored.run != core.RunFinalizing || stored.run == core.RunSucceeded || stored.task != core.TaskRunning || stored.task == core.TaskSucceeded || stored.outbox != 0 {
		t.Fatalf("SUCCEEDED DONE wrote %s", describePrepared(stored))
	}

	// The same JSON result is accepted again. The stored state and reason stay SUCCEEDED / DONE.
	replayWant := stored
	replayWant.version++
	replayed := requirePreparedWrite(t, db, ids, RunUpdate{Result: done}, replayWant, "same result")
	if replayed.result == nil || replayed.result.State != core.TaskSucceeded || replayed.result.Reason != "DONE" || replayed.run != core.RunFinalizing || replayed.task != core.TaskRunning || replayed.task == core.TaskSucceeded || replayed.outbox != 0 {
		t.Fatalf("same result wrote %s", describePrepared(replayed))
	}

	// A different reason, or a different terminal state, conflicts.
	// The stored result stays SUCCEEDED / DONE. The run version does not move.
	// The task does not become SUCCEEDED. No outbox command is inserted.
	differentReason := requirePreparedConflict(t, db, ids, RunUpdate{Result: preparedResult(core.TaskSucceeded, "OTHER")}, replayed, "different reason")
	if differentReason.result == nil || differentReason.result.State != core.TaskSucceeded || differentReason.result.Reason != "DONE" || differentReason.version != replayed.version || differentReason.task == core.TaskSucceeded || differentReason.outbox != 0 {
		t.Fatalf("different reason wrote %s", describePrepared(differentReason))
	}
	for _, state := range []core.TaskState{core.TaskFailed, core.TaskCanceled} {
		held := requirePreparedConflict(t, db, ids, RunUpdate{Result: preparedResult(state, "DONE")}, replayed, "different terminal state "+string(state))
		if held.result == nil || held.result.State != core.TaskSucceeded || held.result.Reason != "DONE" || held.version != replayed.version || held.run != core.RunFinalizing || held.task != core.TaskRunning || held.task == core.TaskSucceeded || held.outbox != 0 {
			t.Fatalf("different terminal state %s wrote %s", state, describePrepared(held))
		}
	}
}

// preparedResult builds a result whose artifact list is an empty slice.
// That JSON matches again after Postgres reads it back.
func preparedResult(state core.TaskState, reason string) *PreparedResult {
	return &PreparedResult{State: state, Reason: reason, Artifacts: []Artifact{}}
}

type preparedIDs struct {
	workspace string
	taskID    string
	runID     string
}

type preparedRow struct {
	version int64
	run     core.RunState
	result  *PreparedResult
	task    core.TaskState
	reason  string
	tasks   int
	runs    int
	outbox  int
}

func requirePreparedConflict(t *testing.T, db *Store, ids preparedIDs, update RunUpdate, before preparedRow, label string) preparedRow {
	t.Helper()
	err := db.UpdateRun(t.Context(), ids.runID, update)
	got := readPreparedRow(t, db, ids)
	if !errors.Is(err, ErrConflict) || !samePreparedRow(got, before) || got.task == core.TaskSucceeded || got.outbox != 0 {
		t.Fatalf("%s: UpdateRun returned %v; got %s, want ErrConflict with %s", label, err, describePrepared(got), describePrepared(before))
	}
	return got
}

func requirePreparedWrite(t *testing.T, db *Store, ids preparedIDs, update RunUpdate, want preparedRow, label string) preparedRow {
	t.Helper()
	err := db.UpdateRun(t.Context(), ids.runID, update)
	got := readPreparedRow(t, db, ids)
	if err != nil || !samePreparedRow(got, want) || got.task != core.TaskRunning || got.task == core.TaskSucceeded || got.outbox != 0 {
		t.Fatalf("%s: UpdateRun returned %v; got %s, want %s", label, err, describePrepared(got), describePrepared(want))
	}
	return got
}

func readPreparedRow(t *testing.T, db *Store, ids preparedIDs) preparedRow {
	t.Helper()
	var row preparedRow
	var run, task string
	var raw []byte
	err := db.pool.QueryRow(t.Context(), `
		SELECT
			r.version,
			r.state,
			r.prepared_result,
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
	row.result = jsonRef[PreparedResult](t, raw)
	return row
}

func samePreparedRow(got, want preparedRow) bool {
	return got.version == want.version && got.run == want.run && got.task == want.task && got.reason == want.reason && got.tasks == want.tasks && got.runs == want.runs && got.outbox == want.outbox && reflect.DeepEqual(got.result, want.result)
}

func describePrepared(row preparedRow) string {
	result := "null"
	if row.result != nil {
		result = fmt.Sprintf("%s/%s artifacts %d", row.result.State, row.result.Reason, len(row.result.Artifacts))
	}
	return fmt.Sprintf("prepared %s run %s task %s reason %q outbox %d tasks %d runs %d version %d", result, row.run, row.task, row.reason, row.outbox, row.tasks, row.runs, row.version)
}
