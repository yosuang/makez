package store

import (
	"context"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestCancelAcceptanceDoesNotWriteTerminalSuccessOrFailure uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
func TestCancelAcceptanceDoesNotWriteTerminalSuccessOrFailure(t *testing.T) {
	fx := cancelWorkspace(t)

	queued := submitCancelTask(t, fx, "cancel-before-run")
	if queued.State != core.TaskQueued || queued.Run != nil || queued.CancelRequested || queued.Reason != "" {
		t.Fatalf("queued task = %+v", queued)
	}
	noRun := cancelView{task: core.TaskCanceled, reason: "CANCEL_REQUESTED"}
	requireCancelRequest(t, fx, queued.ID, noRun)
	requireCancelRequest(t, fx, queued.ID, noRun)

	running := advanceCancelTask(t, fx, "cancel-while-running", core.RunRunning, nil)
	openRun := cancelView{task: core.TaskCanceling, run: core.RunRunning}
	requireCancelRequest(t, fx, running.ID, openRun)
	requireCancelRequest(t, fx, running.ID, openRun)

	prepared := advanceCancelTask(t, fx, "cancel-with-prepared-success", core.RunFinalizing, &PreparedResult{State: core.TaskSucceeded, Reason: "SHOULD_NOT_PUBLISH"})
	held := cancelView{task: core.TaskCanceling, run: core.RunFinalizing, result: core.TaskSucceeded, resultReason: "SHOULD_NOT_PUBLISH"}
	requireCancelRequest(t, fx, prepared.ID, held)
	requireCancelRequest(t, fx, prepared.ID, held)
}

type cancelView struct {
	task         core.TaskState
	reason       string
	run          core.RunState
	result       core.TaskState
	resultReason string
}

type cancelFixture struct {
	db        *Store
	ctx       context.Context
	workspace string
}

func requireCancelRequest(t *testing.T, fx cancelFixture, id string, want cancelView) {
	t.Helper()
	accepted, err := fx.db.Cancel(fx.ctx, fx.workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := fx.db.Get(fx.ctx, fx.workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	row := readCancelRow(t, fx.db, id)
	if row.task != string(stored.State) || row.reason != stored.Reason || row.cancel != stored.CancelRequested {
		t.Fatalf("stored task %+v does not match row %+v", stored, row)
	}
	if accepted.State != stored.State || accepted.Reason != stored.Reason || accepted.CancelRequested != stored.CancelRequested {
		t.Fatalf("cancel return state %s reason %q requested %v, stored state %s reason %q requested %v", accepted.State, accepted.Reason, accepted.CancelRequested, stored.State, stored.Reason, stored.CancelRequested)
	}
	if stored.State == core.TaskSucceeded || stored.State == core.TaskFailed {
		t.Fatalf("cancel acceptance wrote task %s", stored.State)
	}
	if stored.State != want.task || stored.Reason != want.reason || !stored.CancelRequested {
		t.Fatalf("task = %s reason %q requested %v, want %s reason %q requested true", stored.State, stored.Reason, stored.CancelRequested, want.task, want.reason)
	}
	requireRunUntouched(t, accepted.Run, stored.Run, row, want)
	commands := cancelCommandCount(t, fx.db, id)
	wantCommands := 0
	if want.run != "" {
		wantCommands = 1
	}
	if commands != wantCommands {
		t.Fatalf("CANCEL_TASK rows = %d, want %d", commands, wantCommands)
	}
}

func requireRunUntouched(t *testing.T, accepted, stored *Run, row cancelRow, want cancelView) {
	t.Helper()
	if want.run == "" {
		if accepted != nil || stored != nil || row.run != "" || row.prepared != "" {
			t.Fatalf("cancel acceptance created a run: return %v stored %v row %q prepared %q", accepted != nil, stored != nil, row.run, row.prepared)
		}
		return
	}
	if accepted == nil || stored == nil || row.run == "" {
		t.Fatal("cancel acceptance dropped the run")
	}
	if accepted.State != stored.State || accepted.ID != stored.ID {
		t.Fatalf("cancel return run %s %s, stored %s %s", accepted.ID, accepted.State, stored.ID, stored.State)
	}
	if stored.State == core.RunSucceeded || stored.State == core.RunFailed || row.run == string(core.RunSucceeded) || row.run == string(core.RunFailed) {
		t.Fatalf("cancel acceptance wrote run %s", stored.State)
	}
	if stored.State != want.run || row.run != string(want.run) {
		t.Fatalf("run = %s row %s, want %s", stored.State, row.run, want.run)
	}
	if want.result == "" {
		if stored.Result != nil || accepted.Result != nil || row.prepared != "" {
			t.Fatalf("cancel acceptance wrote prepared result %+v", stored.Result)
		}
		return
	}
	if stored.Result == nil || accepted.Result == nil || stored.Result.State != want.result || stored.Result.Reason != want.resultReason || accepted.Result.State != stored.Result.State || accepted.Result.Reason != stored.Result.Reason || row.prepared != string(want.result) {
		t.Fatalf("prepared result = %+v row %q, want %s %s", stored.Result, row.prepared, want.result, want.resultReason)
	}
	if stored.State == core.RunState(want.result) {
		t.Fatalf("cancel acceptance published prepared %s onto the run", want.result)
	}
}

type cancelRow struct {
	task     string
	reason   string
	cancel   bool
	run      string
	prepared string
}

func readCancelRow(t *testing.T, db *Store, id string) cancelRow {
	t.Helper()
	var row cancelRow
	var run, prepared *string
	err := db.pool.QueryRow(t.Context(), `
		SELECT t.state, t.reason, t.cancel_requested, r.state, r.prepared_result->>'state'
		FROM makez_tasks t
		LEFT JOIN makez_runs r ON r.task_id = t.id
		WHERE t.id = $1`, id).Scan(&row.task, &row.reason, &row.cancel, &run, &prepared)
	if err != nil {
		t.Fatal(err)
	}
	if run != nil {
		row.run = *run
	}
	if prepared != nil {
		row.prepared = *prepared
	}
	return row
}

func cancelCommandCount(t *testing.T, db *Store, taskID string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_outbox WHERE task_id=$1 AND command='CANCEL_TASK'`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func cancelWorkspace(t *testing.T) cancelFixture {
	t.Helper()
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "cancel-" + newID()
	token := "cancel-token-" + newID()
	if err := db.RegisterMachines(ctx, map[string]string{token: workspace}); err != nil {
		t.Fatal(err)
	}
	principal, bound, err := db.AuthenticateMachine(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if bound != workspace || principal.Kind != "machine" || principal.ID == "" {
		t.Fatalf("principal = %+v workspace %s", principal, bound)
	}
	return cancelFixture{db: db, ctx: WithPrincipal(ctx, principal), workspace: workspace}
}

func submitCancelTask(t *testing.T, fx cancelFixture, prompt string) Task {
	t.Helper()
	task, created, err := fx.db.Submit(fx.ctx, fx.workspace, "cancel-key-"+newID(), SubmitInput{
		Prompt: prompt, AgentID: "demo", AgentVersion: "1", ProfileID: "default", ProfileVersion: "1", TimeoutSeconds: 60,
	})
	if err != nil || !created || task.State != core.TaskQueued || task.Run != nil || task.CancelRequested {
		t.Fatalf("submit %s = %+v created=%v err=%v", prompt, task, created, err)
	}
	return task
}

func advanceCancelTask(t *testing.T, fx cancelFixture, prompt string, state core.RunState, result *PreparedResult) Task {
	t.Helper()
	task := submitCancelTask(t, fx, prompt)
	started, err := fx.db.InitializeRun(fx.ctx, fx.workspace, task.ID, "test")
	if err != nil || started.Run == nil || started.State != core.TaskRunning || started.Run.State != core.RunPreparing || started.Run.Result != nil {
		t.Fatalf("initialize = %+v err=%v", started, err)
	}
	runID := started.Run.ID
	moveRun(t, fx, runID, core.RunStarting, nil)
	moveRun(t, fx, runID, core.RunRunning, nil)
	if state != core.RunRunning {
		moveRun(t, fx, runID, state, result)
	}
	got, err := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.TaskRunning || got.CancelRequested || got.Reason != "" || got.Run == nil || got.Run.State != state {
		t.Fatalf("before cancel = state %s reason %q run %v", got.State, got.Reason, got.Run)
	}
	if result == nil {
		if got.Run.Result != nil {
			t.Fatalf("before cancel prepared = %+v", got.Run.Result)
		}
		return got
	}
	if got.Run.Result == nil || got.Run.Result.State != result.State || got.Run.Result.Reason != result.Reason {
		t.Fatalf("before cancel prepared = %+v, want %+v", got.Run.Result, result)
	}
	return got
}

func moveRun(t *testing.T, fx cancelFixture, runID string, state core.RunState, result *PreparedResult) {
	t.Helper()
	if err := fx.db.UpdateRun(fx.ctx, runID, RunUpdate{State: &state, Result: result}); err != nil {
		t.Fatal(err)
	}
}
