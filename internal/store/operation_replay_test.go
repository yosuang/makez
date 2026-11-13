package store

import (
	"errors"
	"testing"
	"time"
)

// TestSameOperationDoesNotDispatchTwice uses Postgres. It does not start a sandbox, call a model, or accept a Pi session.
func TestSameOperationDoesNotDispatchTwice(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	const step = "sandbox"
	const hash = "request-hash-same"
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "op-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "operation-replay")

	if err := db.ConfirmOperation(ctx, runID, step); !errors.Is(err, ErrNotFound) {
		t.Fatalf("confirm before begin = %v", err)
	}
	if n := operationCount(t, db, runID, step); n != 0 {
		t.Fatalf("rows before begin = %d", n)
	}

	phase, created, err := db.BeginOperation(ctx, runID, step, hash)
	if err != nil || !created || phase != "DISPATCHING" {
		t.Fatalf("first begin = %q created=%v err=%v", phase, created, err)
	}
	if n := operationCount(t, db, runID, step); n != 1 {
		t.Fatalf("rows after first begin = %d", n)
	}

	phase, created, err = db.BeginOperation(ctx, runID, step, hash)
	if err != nil || created || phase != "DISPATCHING" {
		t.Fatalf("replay begin = %q created=%v err=%v", phase, created, err)
	}
	if n := operationCount(t, db, runID, step); n != 1 {
		t.Fatalf("rows after replay begin = %d", n)
	}
	if got := operationHash(t, db, runID, step); got != hash {
		t.Fatalf("request hash = %q", got)
	}
	phase, created, err = db.BeginOperation(ctx, runID, step, "request-hash-other")
	if !errors.Is(err, ErrConflict) || created {
		t.Fatalf("different hash = %q created=%v err=%v", phase, created, err)
	}
	if n := operationCount(t, db, runID, step); n != 1 {
		t.Fatalf("rows after different hash = %d", n)
	}
	if got := operationHash(t, db, runID, step); got != hash {
		t.Fatalf("request hash after conflict = %q", got)
	}

	if err = db.ConfirmOperation(ctx, runID, step); err != nil {
		t.Fatal(err)
	}
	if got, err := db.OperationPhase(ctx, runID, step); err != nil || got != "CONFIRMED" {
		t.Fatalf("phase after confirm = %q err=%v", got, err)
	}

	pinned := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	tag, err := db.pool.Exec(ctx, `UPDATE makez_operations SET updated_at=$3 WHERE run_id=$1 AND step=$2 AND phase='CONFIRMED'`, runID, step, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("pinned %d rows", tag.RowsAffected())
	}
	if err = db.ConfirmOperation(ctx, runID, step); err != nil {
		t.Fatal(err)
	}
	updated := operationUpdated(t, db, runID, step)
	if !updated.Equal(pinned) {
		t.Fatalf("second confirm rewrote the confirmed operation at %s", updated.UTC())
	}
	if n := operationCount(t, db, runID, step); n != 1 {
		t.Fatalf("rows after second confirm = %d", n)
	}
	if got, err := db.OperationPhase(ctx, runID, step); err != nil || got != "CONFIRMED" {
		t.Fatalf("phase after second confirm = %q err=%v", got, err)
	}

	phase, created, err = db.BeginOperation(ctx, runID, step, hash)
	if err != nil || created || phase != "CONFIRMED" {
		t.Fatalf("begin after confirm = %q created=%v err=%v", phase, created, err)
	}
	if n := operationCount(t, db, runID, step); n != 1 {
		t.Fatalf("rows after confirmed replay = %d", n)
	}
	if got := operationUpdated(t, db, runID, step); !got.Equal(pinned) {
		t.Fatalf("confirmed replay rewrote updated_at to %s", got.UTC())
	}
}

func operationCount(t *testing.T, db *Store, runID, step string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_operations WHERE run_id=$1 AND step=$2`, runID, step).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func operationHash(t *testing.T, db *Store, runID, step string) string {
	t.Helper()
	var hash string
	if err := db.pool.QueryRow(t.Context(), `SELECT request_hash FROM makez_operations WHERE run_id=$1 AND step=$2`, runID, step).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func operationUpdated(t *testing.T, db *Store, runID, step string) time.Time {
	t.Helper()
	var updated time.Time
	if err := db.pool.QueryRow(t.Context(), `SELECT updated_at FROM makez_operations WHERE run_id=$1 AND step=$2`, runID, step).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	return updated
}
