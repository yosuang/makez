package store

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

// TestRecoverableListsUnfinishedTasksAndPendingCleanup uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Selecting tasks for recovery is not exactly-once for an external action and not Pi acceptance.
func TestRecoverableListsUnfinishedTasksAndPendingCleanup(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "recoverable-" + newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "recover-"+newID()); err != nil {
		t.Fatal(err)
	}

	// The cursor sits before this sweep. Through is inclusive, so a task at that instant is inside it.
	// Each call takes an empty slot so a previous sweep cannot share the page.
	cursorAt, through := openRecoverySweep(t, db)
	const cursorID = "recoverable-cursor"
	rows := recoveryFixture(t, db, workspace, cursorAt, through)
	requireRecoveryFixture(t, db, workspace, rows, cursorAt, cursorID, through)

	before := readRecoveryRows(t, db, rows)
	requireRecoveryStored(t, rows, before)
	got, err := db.Recoverable(ctx, RecoveryCursor{CreatedAt: cursorAt, ID: cursorID, Through: through})
	if err != nil {
		t.Fatal(err)
	}
	requireRecoveryUnchanged(t, db, workspace, rows, before, cursorAt, cursorID, through)
	requireRecoverySelection(t, workspace, got, rows, through, before)
}

const (
	wantRecoveryReturned = 5
	wantRecoveryExcluded = 10
	wantRecoveryAfter    = 1
	wantRecoveryRuns     = 13
	wantRecoveryTasks    = wantRecoveryReturned + wantRecoveryExcluded + wantRecoveryAfter
)

func openRecoverySweep(t *testing.T, db *Store) (time.Time, time.Time) {
	t.Helper()
	base := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	const slot = 2 * time.Minute
	slots := int64((364 * 24 * time.Hour) / slot)
	for range 8 {
		cursorAt := base.Add(time.Duration(rand.Int64N(slots)) * slot)
		through := cursorAt.Add(40 * time.Second)
		var n int
		err := db.pool.QueryRow(t.Context(), `
			SELECT count(*) FROM makez_tasks
			WHERE created_at >= $1::timestamptz AND created_at <= $2::timestamptz`,
			cursorAt, through.Add(time.Second)).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return cursorAt, through
		}
	}
	t.Fatal("recovery sweep window was not empty")
	return time.Time{}, time.Time{}
}

func recoveryFixture(t *testing.T, db *Store, workspace string, cursorAt, through time.Time) []recoverySeed {
	t.Helper()
	var rows []recoverySeed
	add := func(state core.TaskState, hasRun bool, runState core.RunState, cleanup core.CleanupState, created time.Time, want bool) {
		rows = append(rows, insertRecoveryTask(t, db, workspace, recoverySeed{
			state: state, hasRun: hasRun, runState: runState, cleanup: cleanup, created: created, want: want,
		}))
	}
	// QUEUED has no run. RUNNING is the live run, placed on the inclusive Through bound.
	add(core.TaskQueued, false, "", "", cursorAt.Add(time.Second), true)
	add(core.TaskRunning, true, core.RunRunning, core.CleanupNotRequired, through, true)
	for _, state := range []core.TaskState{core.TaskSucceeded, core.TaskFailed, core.TaskCanceled} {
		add(state, true, core.RunState(state), core.CleanupPending, cursorAt.Add(2*time.Second), true)
	}
	step := 3
	for _, state := range []core.TaskState{core.TaskSucceeded, core.TaskFailed, core.TaskCanceled} {
		for _, cleanup := range []core.CleanupState{core.CleanupDone, core.CleanupNotRequired, core.CleanupBlocked} {
			add(state, true, core.RunState(state), cleanup, cursorAt.Add(time.Duration(step)*time.Second), false)
			step++
		}
	}
	// A terminal task with no run has no PENDING cleanup.
	add(core.TaskCanceled, false, "", "", cursorAt.Add(time.Duration(step)*time.Second), false)
	// This task would be recoverable, and the time bound keeps it out of the sweep.
	add(core.TaskQueued, false, "", "", through.Add(time.Second), false)
	return rows
}

type recoverySeed struct {
	taskID   string
	state    core.TaskState
	hasRun   bool
	runID    string
	runState core.RunState
	cleanup  core.CleanupState
	created  time.Time
	want     bool
}

func insertRecoveryTask(t *testing.T, db *Store, workspace string, row recoverySeed) recoverySeed {
	t.Helper()
	row.taskID = newID()
	ctx := t.Context()
	tag, err := db.pool.Exec(ctx, `
		INSERT INTO makez_tasks (
			id, workspace_id, idempotency_key, request_hash, prompt, agent_id, agent_version,
			profile_id, profile_version, state, deadline, created_at
		) VALUES ($1, $2, $3, $4, 'recoverable-selection', 'pi', '1', 'default', '1', $5, $6, $7)`,
		row.taskID, workspace, "key-"+row.taskID, promptDigest(row.taskID), string(row.state),
		time.Date(2099, 3, 14, 16, 0, 0, 0, time.UTC), row.created)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("inserted %d tasks", tag.RowsAffected())
	}
	var stored time.Time
	if err = db.pool.QueryRow(ctx, `SELECT created_at FROM makez_tasks WHERE id=$1`, row.taskID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(row.created) {
		t.Fatalf("task %s created_at %s, want %s", row.taskID, stored.UTC(), row.created.UTC())
	}
	if !row.hasRun {
		return row
	}
	row.runID = newID()
	tag, err = db.pool.Exec(ctx, `
		INSERT INTO makez_runs (id, task_id, state, backend_id, workspace_id, cleanup)
		VALUES ($1, $2, $3, 'test', $4, $5)`,
		row.runID, row.taskID, string(row.runState), workspace, string(row.cleanup))
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("inserted %d runs", tag.RowsAffected())
	}
	return row
}

type recoveryStored struct {
	state    string
	reason   string
	version  int64
	runs     int
	runID    string
	runState string
	cleanup  string
	outbox   int
}

type recoveryCounts struct {
	tasks  int
	runs   int
	outbox int
	window int
}

func requireRecoveryFixture(t *testing.T, db *Store, workspace string, rows []recoverySeed, cursorAt time.Time, cursorID string, through time.Time) {
	t.Helper()
	if cursorID == "" {
		t.Fatal("empty recovery cursor disables the lower bound")
	}
	returned, excluded, after, runs := 0, 0, 0, 0
	for _, row := range rows {
		if row.hasRun {
			runs++
		}
		switch {
		case row.want:
			returned++
			if row.created.After(through) {
				t.Fatalf("included task %s is after Through", row.taskID)
			}
			if row.created.Equal(through) && row.state != core.TaskRunning {
				t.Fatalf("Through bound task is %s", row.state)
			}
		case row.created.After(through):
			after++
			if row.state != core.TaskQueued {
				t.Fatalf("task after Through is %s", row.state)
			}
		default:
			excluded++
			if row.state != core.TaskSucceeded && row.state != core.TaskFailed && row.state != core.TaskCanceled {
				t.Fatalf("excluded task %s is %s", row.taskID, row.state)
			}
		}
	}
	onBound := 0
	for _, row := range rows {
		if row.want && row.created.Equal(through) {
			onBound++
		}
	}
	if len(rows) != wantRecoveryTasks || returned != wantRecoveryReturned || excluded != wantRecoveryExcluded || after != wantRecoveryAfter || runs != wantRecoveryRuns || onBound != 1 {
		t.Fatalf("fixture tasks=%d returned=%d excluded=%d after=%d runs=%d on Through=%d", len(rows), returned, excluded, after, runs, onBound)
	}
	counts := readRecoveryCounts(t, db, workspace, cursorAt, cursorID, through)
	// The window count is every task inside the sweep, included and excluded.
	if counts.tasks != wantRecoveryTasks || counts.runs != wantRecoveryRuns || counts.outbox != 0 || counts.window != wantRecoveryTasks-wantRecoveryAfter {
		t.Fatalf("stored counts tasks=%d runs=%d outbox=%d window=%d", counts.tasks, counts.runs, counts.outbox, counts.window)
	}
}

func requireRecoveryUnchanged(t *testing.T, db *Store, workspace string, rows []recoverySeed, before map[string]recoveryStored, cursorAt time.Time, cursorID string, through time.Time) {
	t.Helper()
	after := readRecoveryRows(t, db, rows)
	for _, row := range rows {
		got := after[row.taskID]
		prior := before[row.taskID]
		if got != prior {
			if prior.state != string(core.TaskSucceeded) && got.state == string(core.TaskSucceeded) {
				t.Fatalf("Recoverable selected %s and stored SUCCEEDED; row %+v", row.taskID, got)
			}
			t.Fatalf("Recoverable changed %s from %+v to %+v", row.taskID, prior, got)
		}
		if row.state != core.TaskSucceeded && got.state == string(core.TaskSucceeded) {
			t.Fatalf("Recoverable stored %s as SUCCEEDED", row.taskID)
		}
		if !row.hasRun && (got.runs != 0 || got.runID != "") {
			t.Fatalf("Recoverable created a run for %s: %+v", row.taskID, got)
		}
		if got.outbox != 0 {
			t.Fatalf("Recoverable inserted %d outbox commands for %s", got.outbox, row.taskID)
		}
	}
	counts := readRecoveryCounts(t, db, workspace, cursorAt, cursorID, through)
	if counts.tasks != wantRecoveryTasks || counts.runs != wantRecoveryRuns || counts.outbox != 0 || counts.window != wantRecoveryTasks-wantRecoveryAfter {
		t.Fatalf("counts after Recoverable tasks=%d runs=%d outbox=%d window=%d", counts.tasks, counts.runs, counts.outbox, counts.window)
	}
}

func requireRecoveryStored(t *testing.T, rows []recoverySeed, stored map[string]recoveryStored) {
	t.Helper()
	for _, row := range rows {
		got := stored[row.taskID]
		runs := 0
		runID, runState, cleanup := "", "", ""
		if row.hasRun {
			runs = 1
			runID, runState, cleanup = row.runID, string(row.runState), string(row.cleanup)
		}
		want := recoveryStored{state: string(row.state), version: 1, runs: runs, runID: runID, runState: runState, cleanup: cleanup}
		if got != want {
			t.Fatalf("stored %s = %+v, want %+v", row.taskID, got, want)
		}
	}
}

func requireRecoverySelection(t *testing.T, workspace string, got []Task, rows []recoverySeed, through time.Time, before map[string]recoveryStored) {
	t.Helper()
	byID := make(map[string]Task, len(got))
	for _, task := range got {
		if _, ok := byID[task.ID]; ok {
			t.Fatalf("Recoverable returned %d rows and duplicated %s", len(got), task.ID)
		}
		byID[task.ID] = task
	}
	leaked := 0
	var first recoverySeed
	missing := 0
	afterReturned := 0
	nonTerminal := 0
	pending := 0
	for _, row := range rows {
		task, present := byID[row.taskID]
		switch {
		case row.want:
			if !present {
				missing++
				continue
			}
			requireReturnedRecoveryTask(t, workspace, row, task, before[row.taskID])
			if row.state == core.TaskQueued || row.state == core.TaskRunning {
				nonTerminal++
			}
			if row.cleanup == core.CleanupPending {
				pending++
			}
		case row.created.After(through):
			if present {
				afterReturned++
			}
		default:
			if present {
				leaked++
				if leaked == 1 {
					first = row
				}
			}
		}
	}
	if leaked != 0 {
		cleanup := "none"
		if first.hasRun {
			cleanup = string(first.cleanup)
		}
		t.Fatalf("Recoverable returned %d rows, want %d; %d excluded terminal rows were returned, including task %s state %s cleanup %s", len(got), wantRecoveryReturned, leaked, first.taskID, first.state, cleanup)
	}
	if afterReturned != 0 {
		t.Fatalf("Recoverable returned %d rows, want %d; %d tasks created after Through were returned", len(got), wantRecoveryReturned, afterReturned)
	}
	if missing != 0 || len(got) != wantRecoveryReturned || nonTerminal != 2 || pending != 3 {
		t.Fatalf("Recoverable returned %d rows, want %d; missing %d, non-terminal %d, pending cleanup %d", len(got), wantRecoveryReturned, missing, nonTerminal, pending)
	}
}

func requireReturnedRecoveryTask(t *testing.T, workspace string, row recoverySeed, task Task, stored recoveryStored) {
	t.Helper()
	if task.State != row.state || task.WorkspaceID != workspace || !task.CreatedAt.Equal(row.created) || task.DispatchError != "" {
		t.Fatalf("returned task %s = state %s workspace %s created %s error %q", row.taskID, task.State, task.WorkspaceID, task.CreatedAt.UTC(), task.DispatchError)
	}
	if row.state != core.TaskSucceeded && task.State == core.TaskSucceeded {
		t.Fatalf("Recoverable reported %s as SUCCEEDED", row.taskID)
	}
	if !row.hasRun {
		if task.Run != nil {
			t.Fatalf("Recoverable returned a run for %s: %+v", row.taskID, task.Run)
		}
		return
	}
	if task.Run == nil || task.Run.ID != stored.runID || task.Run.TaskID != row.taskID || task.Run.State != row.runState || task.Run.Cleanup != row.cleanup || task.Run.BackendID != "test" {
		t.Fatalf("returned run for %s = %+v, want id %s cleanup %s state %s", row.taskID, task.Run, stored.runID, row.cleanup, row.runState)
	}
}

func readRecoveryRows(t *testing.T, db *Store, rows []recoverySeed) map[string]recoveryStored {
	t.Helper()
	out := make(map[string]recoveryStored, len(rows))
	for _, row := range rows {
		var got recoveryStored
		err := db.pool.QueryRow(t.Context(), `
			SELECT t.state, t.reason, t.version,
				(SELECT count(*) FROM makez_runs r WHERE r.task_id=t.id),
				COALESCE((SELECT r.id FROM makez_runs r WHERE r.task_id=t.id), ''),
				COALESCE((SELECT r.state FROM makez_runs r WHERE r.task_id=t.id), ''),
				COALESCE((SELECT r.cleanup FROM makez_runs r WHERE r.task_id=t.id), ''),
				(SELECT count(*) FROM makez_outbox o WHERE o.task_id=t.id)
			FROM makez_tasks t WHERE t.id=$1`, row.taskID).Scan(
			&got.state, &got.reason, &got.version, &got.runs, &got.runID, &got.runState, &got.cleanup, &got.outbox)
		if err != nil {
			t.Fatal(err)
		}
		out[row.taskID] = got
	}
	return out
}

func readRecoveryCounts(t *testing.T, db *Store, workspace string, cursorAt time.Time, cursorID string, through time.Time) recoveryCounts {
	t.Helper()
	var counts recoveryCounts
	err := db.pool.QueryRow(t.Context(), `
		SELECT
			(SELECT count(*) FROM makez_tasks WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_runs WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_outbox WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_tasks t WHERE (t.created_at, t.id) > ($2::timestamptz, $3::text) AND t.created_at <= $4::timestamptz)`,
		workspace, cursorAt, cursorID, through).Scan(&counts.tasks, &counts.runs, &counts.outbox, &counts.window)
	if err != nil {
		t.Fatal(err)
	}
	return counts
}
