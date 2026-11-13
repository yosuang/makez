package store

import (
	"errors"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestClaimObserverAdvancesALiveEpoch uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A new epoch fences a stale output writer. It does not prove an external action or an output batch ran once.
func TestClaimObserverAdvancesALiveEpoch(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "observer-" + newID()
	token := "observer-token-" + newID()
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

	// PREPARING is live. The returned epoch is one greater than the stored epoch, and the row stores that return.
	run := openPreparingRun(t, fx)
	opened := readObserver(t, fx.db, run.runID)
	if opened.state != core.RunPreparing || opened.version != run.version {
		t.Fatalf("opened run = %s version %d, want %s version %d", opened.state, opened.version, core.RunPreparing, run.version)
	}
	first := requireObserverClaimed(t, fx, run, opened.epoch+1)
	second := requireObserverClaimed(t, fx, run, first+1)
	if second != first+1 {
		t.Fatalf("ClaimObserver epochs = %d then %d, want a difference of 1", first, second)
	}

	// Each terminal state keeps the epoch already issued. The task stays RUNNING.
	for _, state := range []core.RunState{core.RunSucceeded, core.RunFailed, core.RunCanceled} {
		live := openPreparingRun(t, fx)
		issued := requireObserverClaimed(t, fx, live, readObserver(t, fx.db, live.runID).epoch+1)
		terminal := markRunTerminal(t, fx, live, state)
		requireObserverRejected(t, fx, terminal, issued)
	}
}

func requireObserverClaimed(t *testing.T, fx runInitFixture, before transitionRun, want int64) int64 {
	t.Helper()
	got, err := fx.db.ClaimObserver(fx.ctx, before.runID)
	row := readObserver(t, fx.db, before.runID)
	if err != nil || got != want || row.epoch != got || row.state != before.state || row.version != before.version {
		t.Fatalf("ClaimObserver %s returned %d %v; row %s version %d epoch %d, want %d with row %s version %d epoch %d", before.state, got, err, row.state, row.version, row.epoch, want, before.state, before.version, want)
	}
	requireClaimLeftTaskRunning(t, fx.db, before.taskID)
	return got
}

func requireObserverRejected(t *testing.T, fx runInitFixture, before transitionRun, epoch int64) {
	t.Helper()
	got, err := fx.db.ClaimObserver(fx.ctx, before.runID)
	row := readObserver(t, fx.db, before.runID)
	if !errors.Is(err, ErrConflict) || got != 0 || row.epoch != epoch || row.state != before.state || row.version != before.version {
		t.Fatalf("ClaimObserver %s returned %d %v; row %s version %d epoch %d, want ErrConflict with row %s version %d epoch %d", before.state, got, err, row.state, row.version, row.epoch, before.state, before.version, epoch)
	}
	requireClaimLeftTaskRunning(t, fx.db, before.taskID)
}

// markRunTerminal moves only the run row to a terminal state. The task stays RUNNING.
func markRunTerminal(t *testing.T, fx runInitFixture, run transitionRun, state core.RunState) transitionRun {
	t.Helper()
	before := readObserver(t, fx.db, run.runID)
	tag, err := fx.db.pool.Exec(t.Context(), `UPDATE makez_runs SET state=$2 WHERE id=$1 AND state=$3`, run.runID, string(state), string(run.state))
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("marked %d runs %s", tag.RowsAffected(), state)
	}
	row := readObserver(t, fx.db, run.runID)
	if row.state != state || row.version != before.version || row.epoch != before.epoch {
		t.Fatalf("terminal fixture = %s version %d epoch %d, want %s version %d epoch %d", row.state, row.version, row.epoch, state, before.version, before.epoch)
	}
	requireTaskRunning(t, fx.db, run.taskID)
	return transitionRun{taskID: run.taskID, runID: run.runID, state: row.state, version: row.version}
}

func requireClaimLeftTaskRunning(t *testing.T, db *Store, id string) {
	t.Helper()
	row := readTaskState(t, db, id)
	if row.state == string(core.TaskSucceeded) {
		t.Fatal("claim wrote task SUCCEEDED")
	}
	if row.state != string(core.TaskRunning) || row.reason != "" {
		t.Fatalf("task row = %+v", row)
	}
}

type observerRow struct {
	state   core.RunState
	version int64
	epoch   int64
}

func readObserver(t *testing.T, db *Store, id string) observerRow {
	t.Helper()
	var state string
	var row observerRow
	if err := db.pool.QueryRow(t.Context(), `SELECT state, version, observer_epoch FROM makez_runs WHERE id=$1`, id).Scan(&state, &row.version, &row.epoch); err != nil {
		t.Fatal(err)
	}
	row.state = core.RunState(state)
	return row
}
