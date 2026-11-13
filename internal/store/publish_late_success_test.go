package store

import (
	"errors"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

// TestPublishRejectsLateSuccessAndNonFinalizing uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Finish time is the run exit time when that time is set, otherwise time.Now().
// Refusing a late success is not a claim that an external action already ran once.
func TestPublishRejectsLateSuccessAndNonFinalizing(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "publish-" + newID()
	token := "publish-token-" + newID()
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
	finishedEarly := now.Add(-2 * time.Minute)
	// Exit cases sit on the other side of time.Now, so they lock that finish time. No exit locks time.Now.
	requireVerifiedPublish(t, fx, "on time, finish is the exit", publishClock{
		exit:     &core.ProcessExit{FinishedAt: finishedEarly},
		deadline: finishedEarly.Add(time.Minute),
		afterNow: false,
	}, core.TaskSucceeded, "DONE", core.RunSucceeded)
	requireVerifiedPublish(t, fx, "on time, finish is now", publishClock{afterNow: true}, core.TaskSucceeded, "DONE", core.RunSucceeded)
	deadline := now.Add(30 * time.Minute)
	requireVerifiedPublish(t, fx, "late, finish is the exit", publishClock{
		exit:     &core.ProcessExit{FinishedAt: deadline.Add(time.Minute)},
		deadline: deadline,
		afterNow: true,
	}, core.TaskFailed, "DEADLINE_EXCEEDED", core.RunFailed)
	requireVerifiedPublish(t, fx, "late, finish is now", publishClock{
		deadline: now.Add(-time.Minute),
		afterNow: false,
	}, core.TaskFailed, "DEADLINE_EXCEEDED", core.RunFailed)

	requirePublishConflict(t, fx, core.RunPreparing)
	requirePublishConflict(t, fx, core.RunRunning)
}

// publishClock selects the deadline and, when exit is set, the recorded finish time.
// afterNow says whether that deadline is still ahead of the publish clock.
type publishClock struct {
	exit     *core.ProcessExit
	deadline time.Time
	afterNow bool
}

func requireVerifiedPublish(t *testing.T, fx runInitFixture, name string, clock publishClock, wantTask core.TaskState, wantReason string, wantRun core.RunState) {
	t.Helper()
	task := openPublishRun(t, fx, core.RunFinalizing, clock)
	requireFinishClock(t, task, clock, wantTask == core.TaskSucceeded)
	before := readPublishCounts(t, fx.db, task.ID)
	requirePublishBaseline(t, before, core.RunFinalizing)
	publish := func() error {
		// NOT_REQUIRED keeps START_CLEANUP out of this assertion.
		return fx.db.Publish(fx.ctx, fx.workspace, task.ID, core.TaskSucceeded, "DONE", true, core.CleanupNotRequired, nil)
	}
	err := publish()
	after := readPublishCounts(t, fx.db, task.ID)
	stored, getErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if wantTask != core.TaskSucceeded && storedSucceeded(stored, after) {
		t.Fatalf("%s: caller asked for SUCCEEDED and the row stored it: err=%v task %s reason %q run %s", name, err, after.task, after.reason, after.run)
	}
	if err != nil || after.task != string(wantTask) || after.reason != wantReason || after.run != string(wantRun) || !sameTaskView(stored, after) {
		t.Fatalf("%s publish = %v stored %s %q run %s rows %+v, want %s %q run %s", name, err, stored.State, stored.Reason, runStateOf(stored), after, wantTask, wantReason, wantRun)
	}
	requireNoNewPublishRows(t, before, after)
	if after.taskVer != before.taskVer+1 || after.runVer != before.runVer+1 {
		t.Fatalf("%s versions task %d run %d, want %d and %d", name, after.taskVer, after.runVer, before.taskVer+1, before.runVer+1)
	}
	if stored.CancelRequested || stored.Run.Cleanup != core.CleanupNotRequired {
		t.Fatalf("%s cancel %v cleanup %s", name, stored.CancelRequested, stored.Run.Cleanup)
	}
	replayErr := publish()
	replay := readPublishCounts(t, fx.db, task.ID)
	replayed, replayGetErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if replayGetErr != nil {
		t.Fatal(replayGetErr)
	}
	if replayErr != nil || replay != after || !sameTaskView(replayed, replay) {
		t.Fatalf("%s second publish = %v stored %s %q run %s rows %+v, want nil and %+v", name, replayErr, replayed.State, replayed.Reason, runStateOf(replayed), replay, after)
	}
}

func requirePublishConflict(t *testing.T, fx runInitFixture, state core.RunState) {
	t.Helper()
	task := openPublishRun(t, fx, state, publishClock{})
	if !task.Deadline.After(time.Now()) || (task.Run.Exit != nil && !task.Run.Exit.FinishedAt.IsZero()) {
		t.Fatalf("%s fixture deadline %s exit %+v", state, task.Deadline.UTC(), task.Run.Exit)
	}
	before := readPublishCounts(t, fx.db, task.ID)
	requirePublishBaseline(t, before, state)
	err := fx.db.Publish(fx.ctx, fx.workspace, task.ID, core.TaskSucceeded, "DONE", true, core.CleanupNotRequired, nil)
	after := readPublishCounts(t, fx.db, task.ID)
	stored, getErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if storedSucceeded(stored, after) {
		t.Fatalf("%s stored SUCCEEDED (run was %s): publish %v task %s reason %q run %s", state, before.run, err, after.task, after.reason, after.run)
	}
	if !errors.Is(err, ErrConflict) || after != before || stored.State != core.TaskRunning || stored.Reason != "" || !sameTaskView(stored, after) {
		t.Fatalf("%s publish = %v stored %s %q run %s rows %+v, want ErrConflict and %+v", state, err, stored.State, stored.Reason, runStateOf(stored), after, before)
	}
}

func requireFinishClock(t *testing.T, task Task, clock publishClock, onTime bool) {
	t.Helper()
	now := time.Now()
	finished := now
	hasExit := task.Run != nil && task.Run.Exit != nil && !task.Run.Exit.FinishedAt.IsZero()
	if hasExit {
		finished = task.Run.Exit.FinishedAt
	}
	if hasExit != (clock.exit != nil) || task.Run == nil || task.Run.State != core.RunFinalizing {
		t.Fatalf("finish source exit=%v run %+v, want exit=%v", hasExit, task.Run, clock.exit != nil)
	}
	afterFinish := task.Deadline.After(finished)
	afterNow := task.Deadline.After(now)
	if afterFinish != onTime || afterNow != clock.afterNow {
		t.Fatalf("deadline %s finish %s now %s afterFinish=%v afterNow=%v, want afterFinish=%v afterNow=%v", task.Deadline.UTC(), finished.UTC(), now.UTC(), afterFinish, afterNow, onTime, clock.afterNow)
	}
	if hasExit && afterFinish == afterNow {
		t.Fatal("recorded exit agrees with now, so this case does not lock which clock publish uses")
	}
}

func openPublishRun(t *testing.T, fx runInitFixture, state core.RunState, clock publishClock) Task {
	t.Helper()
	task := submitFreshTask(t, fx)
	started, err := fx.db.InitializeRun(fx.ctx, fx.workspace, task.ID, "test")
	if err != nil || started.State != core.TaskRunning || started.Reason != "" || started.CancelRequested || started.Run == nil || started.Run.State != core.RunPreparing || started.Run.Cleanup != core.CleanupNotRequired || started.Run.Exit != nil {
		t.Fatalf("InitializeRun = %+v err=%v", started, err)
	}
	runID := started.Run.ID
	var path []core.RunState
	switch state {
	case core.RunPreparing:
		// InitializeRun already opened this run.
	case core.RunRunning:
		path = []core.RunState{core.RunStarting, core.RunRunning}
	case core.RunFinalizing:
		path = []core.RunState{core.RunStarting, core.RunRunning, core.RunFinalizing}
	default:
		t.Fatalf("unsupported run %s", state)
	}
	for _, next := range path {
		update := RunUpdate{State: &next}
		if next == core.RunFinalizing {
			update.Exit = clock.exit
		}
		if err = fx.db.UpdateRun(fx.ctx, runID, update); err != nil {
			t.Fatal(err)
		}
	}
	if !clock.deadline.IsZero() {
		if task.CreatedAt.IsZero() {
			t.Fatal("task created_at is zero")
		}
		moveTask(t, fx.db, task.ID, task.CreatedAt, clock.deadline)
	}
	got, err := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.TaskRunning || got.Reason != "" || got.CancelRequested || got.Run == nil || got.Run.ID != runID || got.Run.State != state || got.Run.Cleanup != core.CleanupNotRequired {
		t.Fatalf("before publish = state %s reason %q cancel %v run %+v", got.State, got.Reason, got.CancelRequested, got.Run)
	}
	return got
}

func storedSucceeded(stored Task, row publishCounts) bool {
	if row.task == string(core.TaskSucceeded) || row.run == string(core.RunSucceeded) || stored.State == core.TaskSucceeded {
		return true
	}
	return stored.Run != nil && stored.Run.State == core.RunSucceeded
}

func sameTaskView(stored Task, row publishCounts) bool {
	if string(stored.State) != row.task || stored.Reason != row.reason || stored.CancelRequested != row.cancel || stored.Run == nil {
		return false
	}
	return stored.Run.ID == row.runID && string(stored.Run.State) == row.run && string(stored.Run.Cleanup) == row.cleanup
}

func runStateOf(task Task) string {
	if task.Run == nil {
		return ""
	}
	return string(task.Run.State)
}

type publishCounts struct {
	tasks    int
	runs     int
	outbox   int
	cleanups int
	commands string
	task     string
	reason   string
	cancel   bool
	taskVer  int64
	runID    string
	run      string
	runVer   int64
	cleanup  string
}

func readPublishCounts(t *testing.T, db *Store, id string) publishCounts {
	t.Helper()
	var row publishCounts
	err := db.pool.QueryRow(t.Context(), `
		SELECT
			(SELECT count(*) FROM makez_tasks WHERE id=$1),
			(SELECT count(*) FROM makez_runs WHERE task_id=$1),
			(SELECT count(*) FROM makez_outbox WHERE task_id=$1),
			(SELECT count(*) FROM makez_outbox WHERE task_id=$1 AND command='START_CLEANUP'),
			(SELECT COALESCE(string_agg(command, ',' ORDER BY command), '') FROM makez_outbox WHERE task_id=$1),
			t.state, t.reason, t.cancel_requested, t.version,
			r.id, r.state, r.version, r.cleanup
		FROM makez_tasks t
		JOIN makez_runs r ON r.task_id = t.id
		WHERE t.id=$1`, id).Scan(
		&row.tasks, &row.runs, &row.outbox, &row.cleanups, &row.commands,
		&row.task, &row.reason, &row.cancel, &row.taskVer,
		&row.runID, &row.run, &row.runVer, &row.cleanup,
	)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func requirePublishBaseline(t *testing.T, row publishCounts, run core.RunState) {
	t.Helper()
	// Submit inserts the one START_TASK row. Publish has not run yet.
	if row.tasks != 1 || row.runs != 1 || row.outbox != 1 || row.cleanups != 0 || row.commands != "START_TASK" || row.task != string(core.TaskRunning) || row.reason != "" || row.cancel || row.run != string(run) || row.cleanup != string(core.CleanupNotRequired) || row.taskVer < 1 || row.runVer < 1 || row.runID == "" {
		t.Fatalf("before publish = %+v, want 1 task, 1 %s run, 1 START_TASK, 0 START_CLEANUP", row, run)
	}
}

func requireNoNewPublishRows(t *testing.T, before, after publishCounts) {
	t.Helper()
	if after.tasks != 1 || after.runs != 1 || after.outbox != 1 || after.cleanups != 0 || after.commands != "START_TASK" || after.runID != before.runID {
		t.Fatalf("row counts tasks %d runs %d outbox %d (%s) START_CLEANUP %d run %s, want 1 task, 1 run %s, 1 START_TASK, 0 START_CLEANUP", after.tasks, after.runs, after.outbox, after.commands, after.cleanups, after.runID, before.runID)
	}
}
