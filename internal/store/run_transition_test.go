package store

import (
	"errors"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestUpdateRunRejectsSkippedBackwardAndTerminal uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A transition guard is not exactly-once for an external action.
func TestUpdateRunRejectsSkippedBackwardAndTerminal(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "transition-" + newID()
	token := "transition-token-" + newID()
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
	ctx = WithPrincipal(ctx, principal)
	fx := runInitFixture{db: db, ctx: ctx, workspace: workspace}

	run := openPreparingRun(t, fx)
	// PREPARING to RUNNING skips STARTING.
	requireRunRejected(t, fx, run, core.RunRunning)
	run = requireRunCommitted(t, fx, run, core.RunStarting)
	run = requireRunCommitted(t, fx, run, core.RunRunning)
	// RUNNING to PREPARING is backward.
	requireRunRejected(t, fx, run, core.RunPreparing)
	run = requireRunCommitted(t, fx, run, core.RunFinalizing)
	requireOneRun(t, fx.db, run, core.RunFinalizing)

	// Same-state is allowed for a live run. SUCCEEDED is rejected before that rule.
	terminal := openPreparingRun(t, fx)
	terminal = markRunSucceeded(t, fx, terminal)
	requireRunRejected(t, fx, terminal, core.RunSucceeded)
	requireOneRun(t, fx.db, terminal, core.RunSucceeded)
}

type transitionRun struct {
	taskID  string
	runID   string
	state   core.RunState
	version int64
}

func openPreparingRun(t *testing.T, fx runInitFixture) transitionRun {
	t.Helper()
	task := submitFreshTask(t, fx)
	started, err := fx.db.InitializeRun(fx.ctx, fx.workspace, task.ID, "test")
	if err != nil || started.State != core.TaskRunning || started.Reason != "" || started.Run == nil || started.Run.ID == "" || started.Run.TaskID != task.ID || started.Run.State != core.RunPreparing {
		t.Fatalf("InitializeRun = %+v err=%v", started, err)
	}
	state, version := readRun(t, fx.db, started.Run.ID)
	if state != core.RunPreparing || version < 1 {
		t.Fatalf("opened run = %s version %d", state, version)
	}
	requireTaskRunning(t, fx.db, task.ID)
	return transitionRun{taskID: task.ID, runID: started.Run.ID, state: state, version: version}
}

func requireRunCommitted(t *testing.T, fx runInitFixture, before transitionRun, next core.RunState) transitionRun {
	t.Helper()
	err := fx.db.UpdateRun(fx.ctx, before.runID, RunUpdate{State: &next})
	state, version := readRun(t, fx.db, before.runID)
	if err != nil || state != next || version != before.version+1 {
		t.Fatalf("UpdateRun %s to %s returned %v; row %s version %d, want %s version %d", before.state, next, err, state, version, next, before.version+1)
	}
	requireTaskRunning(t, fx.db, before.taskID)
	return transitionRun{taskID: before.taskID, runID: before.runID, state: state, version: version}
}

func requireRunRejected(t *testing.T, fx runInitFixture, before transitionRun, next core.RunState) {
	t.Helper()
	err := fx.db.UpdateRun(fx.ctx, before.runID, RunUpdate{State: &next})
	state, version := readRun(t, fx.db, before.runID)
	if !errors.Is(err, ErrConflict) || state != before.state || version != before.version {
		t.Fatalf("UpdateRun %s to %s returned %v; row %s version %d, want ErrConflict with row %s version %d", before.state, next, err, state, version, before.state, before.version)
	}
	requireTaskRunning(t, fx.db, before.taskID)
}

// markRunSucceeded moves only the run row to SUCCEEDED. The task stays RUNNING.
func markRunSucceeded(t *testing.T, fx runInitFixture, run transitionRun) transitionRun {
	t.Helper()
	tag, err := fx.db.pool.Exec(t.Context(), `UPDATE makez_runs SET state=$2 WHERE id=$1 AND state=$3`, run.runID, string(core.RunSucceeded), string(core.RunPreparing))
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("marked %d runs succeeded", tag.RowsAffected())
	}
	state, version := readRun(t, fx.db, run.runID)
	if state != core.RunSucceeded || version != run.version {
		t.Fatalf("terminal fixture = %s version %d, want %s version %d", state, version, core.RunSucceeded, run.version)
	}
	requireTaskRunning(t, fx.db, run.taskID)
	run.state = state
	return run
}

func requireTaskRunning(t *testing.T, db *Store, id string) {
	t.Helper()
	row := readTaskState(t, db, id)
	if row.state != string(core.TaskRunning) || row.reason != "" {
		t.Fatalf("task row = %+v", row)
	}
}

func requireOneRun(t *testing.T, db *Store, run transitionRun, state core.RunState) {
	t.Helper()
	n, id, got, backend := runsForTask(t, db, run.taskID)
	if n != 1 || id != run.runID || got != string(state) || backend != "test" {
		t.Fatalf("runs = %d id %s state %s backend %s, want 1 %s %s test", n, id, got, backend, run.runID, state)
	}
	requireTaskRunning(t, db, run.taskID)
}

func readRun(t *testing.T, db *Store, id string) (core.RunState, int64) {
	t.Helper()
	var state string
	var version int64
	if err := db.pool.QueryRow(t.Context(), `SELECT state, version FROM makez_runs WHERE id=$1`, id).Scan(&state, &version); err != nil {
		t.Fatal(err)
	}
	return core.RunState(state), version
}
