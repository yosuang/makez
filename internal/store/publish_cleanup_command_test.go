package store

import (
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

// TestPublishEnqueuesOneStartCleanup uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// One START_CLEANUP row is not a claim that cleanup already ran once.
// This is not exactly-once for an external action.
func TestPublishEnqueuesOneStartCleanup(t *testing.T) {
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
	fx := runInitFixture{db: db, ctx: WithPrincipal(ctx, principal), workspace: workspace}

	now := time.Now().UTC().Truncate(time.Microsecond)
	finished := now.Add(-2 * time.Minute)
	// Deadline is after the recorded exit, so a verified success request can be stored as SUCCEEDED.
	clock := publishClock{
		exit:     &core.ProcessExit{FinishedAt: finished},
		deadline: finished.Add(time.Minute),
		afterNow: false,
	}

	requireCleanupPublish(t, fx, clock, core.CleanupPending, core.TaskSucceeded, "DONE", 1)
	requireCleanupPublish(t, fx, clock, core.CleanupBlocked, core.TaskSucceeded, "DONE", 1)
	requireCleanupPublish(t, fx, clock, core.CleanupNotRequired, core.TaskSucceeded, "DONE", 0)
	requireCleanupPublish(t, fx, clock, core.CleanupDone, core.TaskSucceeded, "DONE", 0)
	// Selecting cleanup does not store SUCCEEDED when the caller asked for failure.
	requireCleanupPublish(t, fx, clock, core.CleanupPending, core.TaskFailed, "AGENT_FAILED", 1)
	requireCleanupPublish(t, fx, clock, core.CleanupBlocked, core.TaskFailed, "AGENT_FAILED", 1)
	requireSeededCleanupPublish(t, fx, clock)
}

func requireCleanupPublish(t *testing.T, fx runInitFixture, clock publishClock, cleanup core.CleanupState, wantTask core.TaskState, wantReason string, wantCleanups int) {
	t.Helper()
	task := openPublishRun(t, fx, core.RunFinalizing, clock)
	requireFinishClock(t, task, clock, true)
	before := readPublishCounts(t, fx.db, task.ID)
	requirePublishBaseline(t, before, core.RunFinalizing)
	publish := func() error {
		return fx.db.Publish(fx.ctx, fx.workspace, task.ID, wantTask, wantReason, true, cleanup, nil)
	}
	err := publish()
	after := readPublishCounts(t, fx.db, task.ID)
	stored, getErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if wantTask != core.TaskSucceeded && storedSucceeded(stored, after) {
		t.Fatalf("cleanup %s stored SUCCEEDED although the caller asked for %s: err=%v task %s reason %q run %s", cleanup, wantTask, err, after.task, after.reason, after.run)
	}
	wantRun := core.RunState(wantTask)
	wantCommands := "START_TASK"
	wantOutbox := 1
	if wantCleanups == 1 {
		wantCommands = "START_CLEANUP,START_TASK"
		wantOutbox = 2
	}
	if err != nil || after.task != string(wantTask) || after.reason != wantReason || after.run != string(wantRun) || after.cleanup != string(cleanup) || after.cleanups != wantCleanups || after.outbox != wantOutbox || after.commands != wantCommands || after.tasks != 1 || after.runs != 1 || after.runID != before.runID || after.cancel || !sameTaskView(stored, after) || stored.Run == nil || !stored.Run.VerifiedStopped {
		t.Fatalf("publish %s cleanup %s = %v task %s reason %q run %s cleanup %s START_CLEANUP %d outbox %d (%s) runs %d, want %s %q run %s cleanup %s START_CLEANUP %d outbox %d (%s) 1 run", wantTask, cleanup, err, after.task, after.reason, after.run, after.cleanup, after.cleanups, after.outbox, after.commands, after.runs, wantTask, wantReason, wantRun, cleanup, wantCleanups, wantOutbox, wantCommands)
	}
	if after.taskVer != before.taskVer+1 || after.runVer != before.runVer+1 {
		t.Fatalf("cleanup %s versions task %d run %d, want %d and %d", cleanup, after.taskVer, after.runVer, before.taskVer+1, before.runVer+1)
	}
	replayErr := publish()
	replay := readPublishCounts(t, fx.db, task.ID)
	replayed, replayGetErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if replayGetErr != nil {
		t.Fatal(replayGetErr)
	}
	if replayErr != nil || replay != after || !sameTaskView(replayed, replay) {
		t.Fatalf("second publish cleanup %s = %v START_CLEANUP %d rows %+v, want nil and %+v", cleanup, replayErr, replay.cleanups, replay, after)
	}
}

func requireSeededCleanupPublish(t *testing.T, fx runInitFixture, clock publishClock) {
	t.Helper()
	task := openPublishRun(t, fx, core.RunFinalizing, clock)
	requireFinishClock(t, task, clock, true)
	before := readPublishCounts(t, fx.db, task.ID)
	requirePublishBaseline(t, before, core.RunFinalizing)
	seedCleanupCommand(t, fx.db, fx.workspace, task.ID, before.runID)
	seeded := readPublishCounts(t, fx.db, task.ID)
	seedID := cleanupCommandID(t, fx.db, task.ID)
	if seeded.cleanups != 1 || seeded.outbox != 2 || seeded.commands != "START_CLEANUP,START_TASK" || seeded.runs != 1 || seeded.runID != before.runID || seeded.task != string(core.TaskRunning) || seedID == "" {
		t.Fatalf("seeded START_CLEANUP = %+v, want 1 START_CLEANUP beside 1 START_TASK and 1 run", seeded)
	}
	err := fx.db.Publish(fx.ctx, fx.workspace, task.ID, core.TaskSucceeded, "DONE", true, core.CleanupPending, nil)
	after := readPublishCounts(t, fx.db, task.ID)
	stored, getErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	kept := cleanupCommandID(t, fx.db, task.ID)
	if err != nil || after.cleanups != 1 || after.runs != 1 || after.runID != before.runID || after.outbox != 2 || after.commands != "START_CLEANUP,START_TASK" || after.task != string(core.TaskSucceeded) || after.reason != "DONE" || after.run != string(core.RunSucceeded) || after.cleanup != string(core.CleanupPending) || after.cancel || kept != seedID || !sameTaskView(stored, after) || stored.Run == nil || !stored.Run.VerifiedStopped || after.taskVer != seeded.taskVer+1 || after.runVer != seeded.runVer+1 {
		t.Fatalf("pre-seeded START_CLEANUP no longer keeps a single committed row: publish %v task %s reason %q run %s runs %d START_CLEANUP %d (%s) run %s id %s, want nil, SUCCEEDED, 1 run %s, 1 START_CLEANUP %s", err, after.task, after.reason, after.run, after.runs, after.cleanups, after.commands, after.runID, kept, before.runID, seedID)
	}
	replayErr := fx.db.Publish(fx.ctx, fx.workspace, task.ID, core.TaskSucceeded, "DONE", true, core.CleanupPending, nil)
	replay := readPublishCounts(t, fx.db, task.ID)
	if replayErr != nil || replay != after || cleanupCommandID(t, fx.db, task.ID) != seedID {
		t.Fatalf("second publish after seeded START_CLEANUP = %v START_CLEANUP %d rows %+v, want nil and one row", replayErr, replay.cleanups, replay)
	}
}

func seedCleanupCommand(t *testing.T, db *Store, workspace, taskID, runID string) {
	t.Helper()
	tag, err := db.pool.Exec(t.Context(), `
		INSERT INTO makez_outbox (id, workspace_id, task_id, run_id, command)
		VALUES ($1, $2, $3, $4, 'START_CLEANUP')`, newID(), workspace, taskID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("seeded %d START_CLEANUP rows", tag.RowsAffected())
	}
}

func cleanupCommandID(t *testing.T, db *Store, taskID string) string {
	t.Helper()
	var id string
	err := db.pool.QueryRow(t.Context(), `SELECT id FROM makez_outbox WHERE task_id=$1 AND command='START_CLEANUP'`, taskID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
