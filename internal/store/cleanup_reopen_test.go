package store

import (
	"errors"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestSetCleanupRecordsDoneWithoutReopening uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Refusing to reopen cleanup is not exactly-once for an external action.
func TestSetCleanupRecordsDoneWithoutReopening(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "cleanup-" + newID()
	token := "cleanup-token-" + newID()
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

	// PREPARING is live. NOT_REQUIRED is not DONE, so DONE is stored and the version increases by 1.
	run := openPreparingRun(t, fx)
	opened := readCleanup(t, fx.db, run.runID)
	if opened.state != core.RunPreparing || opened.cleanup != core.CleanupNotRequired || opened.version != run.version {
		t.Fatalf("opened run = %s cleanup %s version %d, want %s cleanup %s version %d", opened.state, opened.cleanup, opened.version, core.RunPreparing, core.CleanupNotRequired, run.version)
	}
	done := requireCleanupCommitted(t, fx, run, core.CleanupDone)
	// A second DONE is allowed and bumps the version. Cleanup stays DONE, so the call does not reopen the run.
	closed := requireCleanupCommitted(t, fx, done, core.CleanupDone)
	requireCleanupRejected(t, fx, closed, core.CleanupPending)
	requireCleanupRejected(t, fx, closed, core.CleanupBlocked)
	// REOPENED is not NOT_REQUIRED, PENDING, DONE, or BLOCKED.
	requireCleanupRejected(t, fx, closed, core.CleanupState("REOPENED"))
}

func requireCleanupCommitted(t *testing.T, fx runInitFixture, before transitionRun, next core.CleanupState) transitionRun {
	t.Helper()
	prior := readCleanup(t, fx.db, before.runID)
	err := fx.db.SetCleanup(fx.ctx, before.runID, next)
	row := readCleanup(t, fx.db, before.runID)
	if err != nil || prior.state != before.state || prior.version != before.version || row.cleanup != next || row.version != prior.version+1 || row.state != prior.state {
		t.Fatalf("SetCleanup %s to %s returned %v; row %s cleanup %s version %d, want %s version %d", prior.cleanup, next, err, row.state, row.cleanup, row.version, next, prior.version+1)
	}
	requireCleanupLeftTaskRunning(t, fx.db, before.taskID)
	return transitionRun{taskID: before.taskID, runID: before.runID, state: row.state, version: row.version}
}

func requireCleanupRejected(t *testing.T, fx runInitFixture, before transitionRun, next core.CleanupState) {
	t.Helper()
	prior := readCleanup(t, fx.db, before.runID)
	err := fx.db.SetCleanup(fx.ctx, before.runID, next)
	row := readCleanup(t, fx.db, before.runID)
	if !errors.Is(err, ErrConflict) || prior.cleanup != core.CleanupDone || prior.state != before.state || prior.version != before.version || row.cleanup != prior.cleanup || row.version != prior.version || row.state != prior.state {
		t.Fatalf("SetCleanup %s to %s returned %v; row %s cleanup %s version %d, want ErrConflict with row %s cleanup %s version %d", prior.cleanup, next, err, row.state, row.cleanup, row.version, prior.state, prior.cleanup, prior.version)
	}
	requireCleanupLeftTaskRunning(t, fx.db, before.taskID)
}

func requireCleanupLeftTaskRunning(t *testing.T, db *Store, id string) {
	t.Helper()
	row := readTaskState(t, db, id)
	if row.state == string(core.TaskSucceeded) {
		t.Fatal("SetCleanup wrote task SUCCEEDED")
	}
	if row.state != string(core.TaskRunning) || row.reason != "" {
		t.Fatalf("task row = %+v", row)
	}
}

type cleanupRow struct {
	state   core.RunState
	version int64
	cleanup core.CleanupState
}

func readCleanup(t *testing.T, db *Store, id string) cleanupRow {
	t.Helper()
	var state, cleanup string
	var row cleanupRow
	if err := db.pool.QueryRow(t.Context(), `SELECT state, version, cleanup FROM makez_runs WHERE id=$1`, id).Scan(&state, &row.version, &cleanup); err != nil {
		t.Fatal(err)
	}
	row.state = core.RunState(state)
	row.cleanup = core.CleanupState(cleanup)
	return row
}
