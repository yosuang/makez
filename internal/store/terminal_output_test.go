package store

import (
	"errors"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestAppendOutputRejectsATerminalRun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Rejecting output on a finished run is not a claim that an external action already ran once.
// This is not exactly-once for an external action and not Pi acceptance.
func TestAppendOutputRejectsATerminalRun(t *testing.T) {
	db := leaseDB(t)
	// Same-cursor replay and a stale epoch stay in TestSameRunEpochAndCursorDoesNotAppendTwice.
	for _, state := range []core.RunState{core.RunSucceeded, core.RunFailed, core.RunCanceled} {
		run := acceptCurrentOutput(t, db)
		requireTerminalOutputRejected(t, db, run, state)
	}
}

type acceptedOutput struct {
	workspace string
	taskID    string
	runID     string
	epoch     int64
	cursor    string
	rows      outputRows
}

// acceptCurrentOutput stores one event on a non-terminal run at the current observer epoch.
func acceptCurrentOutput(t *testing.T, db *Store) acceptedOutput {
	t.Helper()
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "terminal-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "terminal-output")
	epoch, err := db.ClaimObserver(ctx, runID)
	if err != nil || epoch != 1 {
		t.Fatalf("epoch = %d err=%v", epoch, err)
	}
	const cursor = "stdout:16"
	const added = "stdout:32"
	opened := readOutputRows(t, db, workspace, taskID, runID, cursor, added)
	if opened.events != 0 || opened.kept != 0 || opened.added != 0 || opened.cursor != "" || opened.epoch != epoch || opened.run != string(core.RunRunning) || opened.task != string(core.TaskRunning) || opened.reason != "" || opened.outbox != 0 {
		t.Fatalf("opened events %d kept %d added %d cursor %q epoch %d run %s task %s outbox %d, want 0 events, empty cursor, epoch %d, run RUNNING, task RUNNING, outbox 0", opened.events, opened.kept, opened.added, opened.cursor, opened.epoch, opened.run, opened.task, opened.outbox, epoch)
	}
	event := core.AgentEvent{SourceOffset: cursor, Kind: "log", Payload: []byte(`{"text":"live"}`)}
	if err = db.AppendOutput(ctx, runID, epoch, event.SourceOffset, []byte(`{}`), []core.AgentEvent{event}); err != nil {
		t.Fatal(err)
	}
	rows := readOutputRows(t, db, workspace, taskID, runID, cursor, added)
	if rows.events != 1 || rows.kept != 1 || rows.added != 0 || rows.cursor != event.SourceOffset || rows.epoch != epoch || rows.run != string(core.RunRunning) || rows.task != string(core.TaskRunning) || rows.reason != "" || rows.outbox != 0 || rows.text != "live" || rows.version != opened.version+1 {
		t.Fatalf("accepted events %d kept %d added %d cursor %q text %q run %s task %s outbox %d version %d, want 1 event cursor %q task RUNNING outbox 0", rows.events, rows.kept, rows.added, rows.cursor, rows.text, rows.run, rows.task, rows.outbox, rows.version, event.SourceOffset)
	}
	return acceptedOutput{workspace: workspace, taskID: taskID, runID: runID, epoch: epoch, cursor: cursor, rows: rows}
}

func requireTerminalOutputRejected(t *testing.T, db *Store, run acceptedOutput, terminal core.RunState) {
	t.Helper()
	ctx := t.Context()
	fx := runInitFixture{db: db, ctx: ctx, workspace: run.workspace}
	marked := markRunTerminal(t, fx, transitionRun{taskID: run.taskID, runID: run.runID, state: core.RunState(run.rows.run), version: run.rows.version}, terminal)
	const added = "stdout:32"
	before := readOutputRows(t, db, run.workspace, run.taskID, run.runID, run.cursor, added)
	if marked.state != terminal || before.events != 1 || before.kept != 1 || before.added != 0 || before.cursor != run.cursor || before.epoch != run.epoch || before.run != string(terminal) || before.task != string(core.TaskRunning) || before.reason != "" || before.outbox != 0 || before.text != "live" || before.version != run.rows.version || before.task == string(core.TaskSucceeded) {
		t.Fatalf("terminal %s events %d cursor %q task %s outbox %d version %d, want task RUNNING with 1 event cursor %q outbox 0", terminal, before.events, before.cursor, before.task, before.outbox, before.version, run.cursor)
	}
	event := core.AgentEvent{SourceOffset: added, Kind: "log", Payload: []byte(`{"text":"late"}`)}
	err := db.AppendOutput(ctx, run.runID, run.epoch, event.SourceOffset, []byte(`{}`), []core.AgentEvent{event})
	after := readOutputRows(t, db, run.workspace, run.taskID, run.runID, run.cursor, added)
	if !errors.Is(err, ErrConflict) || after != before || after.events != 1 || after.cursor != run.cursor || after.task != string(core.TaskRunning) || after.task == string(core.TaskSucceeded) || after.outbox != 0 || after.added != 0 {
		t.Fatalf("AppendOutput %s returned %v; events %d cursor %q text %q added %d task %s outbox %d, want ErrConflict with events %d cursor %q task RUNNING outbox 0", terminal, err, after.events, after.cursor, after.text, after.added, after.task, after.outbox, before.events, before.cursor)
	}
}

type outputRows struct {
	events  int
	kept    int
	added   int
	cursor  string
	epoch   int64
	run     string
	task    string
	reason  string
	outbox  int
	text    string
	version int64
}

func readOutputRows(t *testing.T, db *Store, workspace, taskID, runID, keptOffset, addedOffset string) outputRows {
	t.Helper()
	var rows outputRows
	err := db.pool.QueryRow(t.Context(), `
		SELECT
			(SELECT count(*) FROM makez_events WHERE run_id=$1),
			(SELECT count(*) FROM makez_events WHERE run_id=$1 AND source_offset=$2),
			(SELECT count(*) FROM makez_events WHERE run_id=$1 AND source_offset=$3),
			r.cursor,
			r.observer_epoch,
			r.state,
			tk.state,
			tk.reason,
			(SELECT count(*) FROM makez_outbox WHERE workspace_id=$4 OR task_id=$5 OR run_id=$1),
			COALESCE((SELECT string_agg(e.payload->>'text', ',' ORDER BY e.seq) FROM makez_events e WHERE e.run_id=$1), ''),
			r.version
		FROM makez_runs r
		JOIN makez_tasks tk ON tk.id=r.task_id
		WHERE r.id=$1 AND tk.id=$5`,
		runID, keptOffset, addedOffset, workspace, taskID).Scan(
		&rows.events, &rows.kept, &rows.added, &rows.cursor, &rows.epoch, &rows.run, &rows.task, &rows.reason, &rows.outbox, &rows.text, &rows.version)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
