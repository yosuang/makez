package store

import (
	"errors"
	"testing"
	"time"
)

// TestLiveCommandReplayDoesNotRequeue uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
func TestLiveCommandReplayDoesNotRequeue(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "replay-"+workspace); err != nil {
		t.Fatal(err)
	}
	liveTask, liveRun := newID(), newID()
	deadTask, deadRun := newID(), newID()
	insertRun(t, db, workspace, liveTask, liveRun, "live-command")
	insertRun(t, db, workspace, deadTask, deadRun, "dead-command")

	waiting := insertOutboxCommand(t, db, outboxSeed{
		workspace: workspace, taskID: liveTask, runID: liveRun, command: "START_TASK",
		attempts: 5, token: 4, lastError: "delivery lease expired; outcome may be unknown",
	})
	leased := insertOutboxCommand(t, db, outboxSeed{
		workspace: workspace, taskID: liveTask, runID: liveRun, command: "CANCEL_TASK",
		attempts: 2, token: 8, lastError: "delivery in progress", leased: true,
	})
	delivered := insertOutboxCommand(t, db, outboxSeed{
		workspace: workspace, taskID: liveTask, runID: liveRun, command: "START_CLEANUP",
		attempts: 1, token: 3, lastError: "acknowledged", delivered: true,
	})
	dead := insertOutboxCommand(t, db, outboxSeed{
		workspace: workspace, taskID: deadTask, runID: deadRun, command: "START_TASK",
		attempts: 12, token: 11, lastError: "delivery dead-lettered; operator replay required", dead: true,
	})

	err := db.ReplayOutbox(ctx, workspace, waiting.id)
	requireReplayRefused(t, db, waiting, err)
	err = db.ReplayDeadLetter(ctx, workspace, waiting.id, "task", "explicit operator replay")
	requireReplayRefused(t, db, waiting, err)

	err = db.ReplayOutbox(ctx, workspace, leased.id)
	requireReplayRefused(t, db, leased, err)
	err = db.ReplayDeadLetter(ctx, workspace, leased.id, "task", "explicit operator replay")
	requireReplayRefused(t, db, leased, err)

	err = db.ReplayDeadLetter(ctx, workspace, delivered.id, "task", "explicit operator replay")
	requireReplayRefused(t, db, delivered, err)

	if err = db.ReplayDeadLetter(ctx, workspace, dead.id, "task", "explicit operator replay"); err != nil {
		t.Fatal(err)
	}
	requeued := requireRequeuedOnce(t, db, dead)
	err = db.ReplayOutbox(ctx, workspace, dead.id)
	requireReplayRefused(t, db, requeued, err)
	err = db.ReplayDeadLetter(ctx, workspace, dead.id, "task", "explicit operator replay")
	requireReplayRefused(t, db, requeued, err)

	requireOutboxUnchanged(t, db, waiting)
	requireOutboxUnchanged(t, db, leased)
	requireOutboxUnchanged(t, db, delivered)
	if n := replayAuditCount(t, db, waiting.id); n != 0 {
		t.Fatalf("live replay audits = %d", n)
	}
	if n := replayAuditCount(t, db, leased.id); n != 0 {
		t.Fatalf("leased replay audits = %d", n)
	}
	if n := replayAuditCount(t, db, delivered.id); n != 0 {
		t.Fatalf("delivered replay audits = %d", n)
	}
	if n := replayAuditCount(t, db, dead.id); n != 1 {
		t.Fatalf("dead replay audits = %d", n)
	}
}

type outboxSeed struct {
	workspace, taskID, runID, command, lastError string
	attempts                                     int
	token                                        int64
	dead, delivered, leased                      bool
}

type outboxRow struct {
	id, workspaceID, taskID, runID, command, owner, lastError string
	attempts                                                  int
	token                                                     int64
	delivered, dead                                           bool
	nextAt, leaseUntil                                        time.Time
}

func insertOutboxCommand(t *testing.T, db *Store, seed outboxSeed) outboxRow {
	t.Helper()
	id := newID()
	owner := ""
	if seed.leased {
		owner = newID()
	}
	tag, err := db.pool.Exec(t.Context(), `
		INSERT INTO makez_outbox (
			id, workspace_id, task_id, run_id, command,
			attempts, last_error, next_attempt_at, delivered_at, dead_at,
			lease_owner, lease_token, lease_until
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7,
			CASE WHEN $8 THEN now() - interval '1 hour' ELSE now() + interval '1 day' END,
			CASE WHEN $9 THEN now() ELSE NULL END,
			CASE WHEN $8 THEN now() ELSE NULL END,
			CASE WHEN $10 THEN $11 ELSE NULL END,
			$12,
			CASE WHEN $10 THEN now() + interval '30 seconds' ELSE NULL END
		)`, id, seed.workspace, seed.taskID, seed.runID, seed.command,
		seed.attempts, seed.lastError, seed.dead, seed.delivered, seed.leased, owner, seed.token)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("inserted %d outbox rows", tag.RowsAffected())
	}
	return readOutboxCommand(t, db, id)
}

func readOutboxCommand(t *testing.T, db *Store, id string) outboxRow {
	t.Helper()
	var got outboxRow
	var owner *string
	var leaseUntil *time.Time
	err := db.pool.QueryRow(t.Context(), `
		SELECT id, workspace_id, task_id, run_id, command,
			attempts, lease_token, lease_owner, last_error,
			delivered_at IS NOT NULL, dead_at IS NOT NULL,
			next_attempt_at, lease_until
		FROM makez_outbox WHERE id=$1`, id).Scan(
		&got.id, &got.workspaceID, &got.taskID, &got.runID, &got.command,
		&got.attempts, &got.token, &owner, &got.lastError,
		&got.delivered, &got.dead, &got.nextAt, &leaseUntil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		got.owner = *owner
	}
	if leaseUntil != nil {
		got.leaseUntil = *leaseUntil
	}
	return got
}

func requireReplayRefused(t *testing.T, db *Store, before outboxRow, err error) {
	t.Helper()
	requireOutboxUnchanged(t, db, before)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay of command %s = %v", before.id, err)
	}
}

func requireOutboxUnchanged(t *testing.T, db *Store, before outboxRow) {
	t.Helper()
	got := readOutboxCommand(t, db, before.id)
	if !sameOutbox(got, before) {
		t.Fatalf("command that is not dead was requeued: got %+v, want %+v", got, before)
	}
	if ids := outboxCommandIDs(t, db, before.taskID, before.command); len(ids) != 1 || ids[0] != before.id {
		t.Fatalf("command ids = %v, want [%s]", ids, before.id)
	}
}

func requireRequeuedOnce(t *testing.T, db *Store, before outboxRow) outboxRow {
	t.Helper()
	got := readOutboxCommand(t, db, before.id)
	if got.id != before.id || got.workspaceID != before.workspaceID || got.taskID != before.taskID || got.runID != before.runID || got.command != before.command {
		t.Fatalf("replay changed identity from %+v to %+v", before, got)
	}
	if got.dead || got.delivered || got.attempts != 0 || got.token != before.token+1 || got.owner != "" || !got.leaseUntil.IsZero() || got.lastError != "" {
		t.Fatalf("requeued command = %+v", got)
	}
	var due bool
	if err := db.pool.QueryRow(t.Context(), `
		SELECT delivered_at IS NULL AND dead_at IS NULL AND lease_until IS NULL
			AND attempts = 0 AND next_attempt_at <= now()
		FROM makez_outbox WHERE id=$1`, got.id).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("dead command was not requeued")
	}
	if ids := outboxCommandIDs(t, db, before.taskID, before.command); len(ids) != 1 || ids[0] != before.id {
		t.Fatalf("command ids = %v, want [%s]", ids, before.id)
	}
	return got
}

func sameOutbox(got, want outboxRow) bool {
	return got.id == want.id && got.workspaceID == want.workspaceID && got.taskID == want.taskID &&
		got.runID == want.runID && got.command == want.command && got.attempts == want.attempts &&
		got.token == want.token && got.owner == want.owner && got.delivered == want.delivered &&
		got.dead == want.dead && got.lastError == want.lastError && got.nextAt.Equal(want.nextAt) &&
		got.leaseUntil.Equal(want.leaseUntil)
}

func outboxCommandIDs(t *testing.T, db *Store, taskID, command string) []string {
	t.Helper()
	rows, err := db.pool.Query(t.Context(), `SELECT id FROM makez_outbox WHERE task_id=$1 AND command=$2 ORDER BY id`, taskID, command)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func replayAuditCount(t *testing.T, db *Store, id string) int {
	t.Helper()
	var n int
	err := db.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM makez_audit_events
		WHERE resource_id=$1 AND action='replay_outbox' AND outcome='allowed'`, id).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
