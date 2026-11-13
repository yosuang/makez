package store

import (
	"context"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

// TestInitializeRunKeepsOneRun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
func TestInitializeRunKeepsOneRun(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "run-" + newID()
	token := "run-token-" + newID()
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
	if n := runTaskIDUniqueCount(t, db); n != 1 {
		t.Fatalf("makez_runs.task_id unique constraints = %d", n)
	}

	const backend = "test"
	task := submitFreshTask(t, fx)
	first, err := db.InitializeRun(ctx, workspace, task.ID, backend)
	if err != nil || first.State != core.TaskRunning || first.Reason != "" || first.Run == nil || first.Run.ID == "" || first.Run.TaskID != task.ID || first.Run.State != core.RunPreparing || first.Run.BackendID != backend {
		t.Fatalf("first InitializeRun = %+v err=%v", first, err)
	}
	runID := first.Run.ID
	version := taskVersion(t, db, task.ID)
	started := runStartedAt(t, db, runID)
	if n, id, state, gotBackend := runsForTask(t, db, task.ID); n != 1 || id != runID || state != string(core.RunPreparing) || gotBackend != backend {
		t.Fatalf("runs after first InitializeRun = %d id %s state %s backend %s", n, id, state, gotBackend)
	}
	if row := readTaskState(t, db, task.ID); row.state != string(core.TaskRunning) || row.reason != "" {
		t.Fatalf("task row after first InitializeRun = %+v", row)
	}

	replay, err := db.InitializeRun(ctx, workspace, task.ID, backend)
	if err != nil || replay.State != core.TaskRunning || replay.Reason != "" || replay.Run == nil || replay.Run.ID != runID || replay.Run.State != core.RunPreparing {
		t.Fatalf("second InitializeRun = %+v err=%v", replay, err)
	}
	if n, id, state, gotBackend := runsForTask(t, db, task.ID); n != 1 || id != runID || state != string(core.RunPreparing) || gotBackend != backend {
		t.Fatalf("runs after second InitializeRun = %d id %s state %s backend %s", n, id, state, gotBackend)
	}
	if got := taskVersion(t, db, task.ID); got != version {
		t.Fatalf("task version after second InitializeRun = %d, want %d", got, version)
	}
	if got := runStartedAt(t, db, runID); !got.Equal(started) {
		t.Fatalf("second InitializeRun rewrote started_at to %s", got.UTC())
	}
	stored, err := db.Get(ctx, workspace, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != core.TaskRunning || stored.Reason != "" || stored.Run == nil || stored.Run.ID != runID {
		t.Fatalf("stored task = state %s reason %q run %v", stored.State, stored.Reason, stored.Run)
	}

	late := submitFreshTask(t, fx)
	moveTask(t, db, late.ID, late.CreatedAt, time.Now().Add(-time.Minute))
	requireClock(t, fx, late.ID, false, true)
	requireDeadlineExceeded(t, fx, late.ID, backend)

	aged := submitFreshTask(t, fx)
	moveTask(t, db, aged.ID, time.Now().Add(-11*time.Minute), time.Now().Add(30*time.Minute))
	requireClock(t, fx, aged.ID, true, false)
	requireDeadlineExceeded(t, fx, aged.ID, backend)

	for _, terminal := range []struct {
		state  core.TaskState
		reason string
	}{
		{core.TaskSucceeded, "DONE"},
		{core.TaskFailed, "STOP_UNCONFIRMED"},
		{core.TaskCanceled, "CANCEL_REQUESTED"},
	} {
		requireTerminalWithoutRun(t, fx, backend, terminal.state, terminal.reason)
	}
}

type runInitFixture struct {
	db        *Store
	ctx       context.Context
	workspace string
}

func submitFreshTask(t *testing.T, fx runInitFixture) Task {
	t.Helper()
	task, created, err := fx.db.Submit(fx.ctx, fx.workspace, "run-key-"+newID(), SubmitInput{
		Prompt: "initialize-run", AgentID: "demo", AgentVersion: "1", ProfileID: "default", ProfileVersion: "1", TimeoutSeconds: 3600,
	})
	if err != nil || !created || task.ID == "" || task.State != core.TaskQueued || task.Run != nil || task.Reason != "" {
		t.Fatalf("submit = %+v created=%v err=%v", task, created, err)
	}
	if n, _, _, _ := runsForTask(t, fx.db, task.ID); n != 0 {
		t.Fatalf("runs after submit = %d", n)
	}
	return task
}

func requireDeadlineExceeded(t *testing.T, fx runInitFixture, id, backend string) {
	t.Helper()
	for range 2 {
		got, err := fx.db.InitializeRun(fx.ctx, fx.workspace, id, backend)
		row := readTaskState(t, fx.db, id)
		if got.State == core.TaskSucceeded || row.state == string(core.TaskSucceeded) {
			t.Fatalf("expired task wrote SUCCEEDED")
		}
		n, runID, _, _ := runsForTask(t, fx.db, id)
		if n != 0 || got.Run != nil || runID != "" {
			t.Fatalf("expired task inserted a run: count %d id %s run %v", n, runID, got.Run)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.State != core.TaskFailed || got.Reason != "DEADLINE_EXCEEDED" || row.state != string(core.TaskFailed) || row.reason != "DEADLINE_EXCEEDED" {
			t.Fatalf("expired task = %s %q row %+v", got.State, got.Reason, row)
		}
	}
}

func requireTerminalWithoutRun(t *testing.T, fx runInitFixture, backend string, state core.TaskState, reason string) {
	t.Helper()
	task := submitFreshTask(t, fx)
	moveTaskState(t, fx.db, task.ID, state, reason)
	for range 2 {
		got, err := fx.db.InitializeRun(fx.ctx, fx.workspace, task.ID, backend)
		if err != nil {
			t.Fatal(err)
		}
		row := readTaskState(t, fx.db, task.ID)
		n, runID, _, _ := runsForTask(t, fx.db, task.ID)
		if n != 0 || got.Run != nil || runID != "" {
			t.Fatalf("terminal %s gained a run: count %d id %s run %v", state, n, runID, got.Run)
		}
		if got.State != state || got.Reason != reason || row.state != string(state) || row.reason != reason {
			t.Fatalf("terminal %s became %s %q row %+v", state, got.State, got.Reason, row)
		}
	}
}

// requireClock checks the fixture that selects the deadline clause or the age clause.
// older is created more than 10 minutes ago. past means the deadline is not still ahead.
func requireClock(t *testing.T, fx runInitFixture, id string, older, past bool) {
	t.Helper()
	task, err := fx.db.Get(fx.ctx, fx.workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Run != nil || task.State != core.TaskQueued {
		t.Fatalf("clock fixture = state %s run %v", task.State, task.Run)
	}
	if isOlder := time.Since(task.CreatedAt) > 10*time.Minute; isOlder != older {
		t.Fatalf("created %s older=%v, want %v", task.CreatedAt.UTC(), isOlder, older)
	}
	if isPast := !task.Deadline.After(time.Now()); isPast != past {
		t.Fatalf("deadline %s past=%v, want %v", task.Deadline.UTC(), isPast, past)
	}
}

func moveTask(t *testing.T, db *Store, id string, createdAt, deadline time.Time) {
	t.Helper()
	tag, err := db.pool.Exec(t.Context(), `UPDATE makez_tasks SET created_at=$2, deadline=$3 WHERE id=$1`, id, createdAt.UTC(), deadline.UTC())
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("moved %d tasks", tag.RowsAffected())
	}
}

func moveTaskState(t *testing.T, db *Store, id string, state core.TaskState, reason string) {
	t.Helper()
	tag, err := db.pool.Exec(t.Context(), `UPDATE makez_tasks SET state=$2, reason=$3 WHERE id=$1`, id, string(state), reason)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("updated %d task states", tag.RowsAffected())
	}
}

type taskStateRow struct {
	state  string
	reason string
}

func readTaskState(t *testing.T, db *Store, id string) taskStateRow {
	t.Helper()
	var row taskStateRow
	err := db.pool.QueryRow(t.Context(), `SELECT state, reason FROM makez_tasks WHERE id=$1`, id).Scan(&row.state, &row.reason)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func runsForTask(t *testing.T, db *Store, taskID string) (n int, id, state, backend string) {
	t.Helper()
	err := db.pool.QueryRow(t.Context(), `
		SELECT count(*), COALESCE(min(id), ''), COALESCE(min(state), ''), COALESCE(min(backend_id), '')
		FROM makez_runs WHERE task_id=$1`, taskID).Scan(&n, &id, &state, &backend)
	if err != nil {
		t.Fatal(err)
	}
	return n, id, state, backend
}

func taskVersion(t *testing.T, db *Store, id string) int64 {
	t.Helper()
	var version int64
	if err := db.pool.QueryRow(t.Context(), `SELECT version FROM makez_tasks WHERE id=$1`, id).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func runStartedAt(t *testing.T, db *Store, id string) time.Time {
	t.Helper()
	var started time.Time
	if err := db.pool.QueryRow(t.Context(), `SELECT started_at FROM makez_runs WHERE id=$1`, id).Scan(&started); err != nil {
		t.Fatal(err)
	}
	return started
}

func runTaskIDUniqueCount(t *testing.T, db *Store) int {
	t.Helper()
	var n int
	err := db.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM (
			SELECT tc.constraint_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
			  ON tc.constraint_schema = kcu.constraint_schema
			 AND tc.constraint_name = kcu.constraint_name
			 AND tc.table_schema = kcu.table_schema
			 AND tc.table_name = kcu.table_name
			WHERE tc.table_schema = current_schema()
			  AND tc.table_name = 'makez_runs'
			  AND tc.constraint_type = 'UNIQUE'
			GROUP BY tc.constraint_name
			HAVING count(*) = 1 AND bool_or(kcu.column_name = 'task_id')
		) unique_task`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
