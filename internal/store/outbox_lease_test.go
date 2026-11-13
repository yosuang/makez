package store

import (
	"errors"
	"testing"
)

// TestStaleOutboxLeaseDoesNotAckOrCountTwice uses Postgres. It does not start a sandbox, call a model, or accept a Pi session.
func TestStaleOutboxLeaseDoesNotAckOrCountTwice(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "outbox-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "outbox-lease")

	// Each row is one committed delivery lease, so its attempt is already counted.
	stale := seedOutboxLease(t, db, workspace, taskID, runID, "START_TASK", true)
	current := seedOutboxLease(t, db, workspace, taskID, runID, "CANCEL_TASK", false)
	acked := seedOutboxLease(t, db, workspace, taskID, runID, "START_CLEANUP", false)

	if err := db.Delivered(ctx, stale); !errors.Is(err, ErrStale) {
		t.Fatalf("expired lease ack = %v", err)
	}
	requireDelivery(t, db, stale.ID, heldLease(stale))

	other := current
	other.LeaseOwner = "other-" + newID()
	if err := db.Delivered(ctx, other); !errors.Is(err, ErrStale) {
		t.Fatalf("other owner ack = %v", err)
	}
	requireDelivery(t, db, current.ID, heldLease(current))
	wrongToken := current
	wrongToken.LeaseToken = outboxLeaseToken - 1
	if err := db.Delivered(ctx, wrongToken); !errors.Is(err, ErrStale) {
		t.Fatalf("old token ack = %v", err)
	}
	requireDelivery(t, db, current.ID, heldLease(current))

	const reason = "delivery unavailable"
	if err := db.RetryOutbox(ctx, current, reason); err != nil {
		t.Fatal(err)
	}
	retried := heldLease(current)
	retried.owner = ""
	retried.lastError = reason
	requireDelivery(t, db, current.ID, retried)
	if err := db.RetryOutbox(ctx, current, "delivery unavailable again"); !errors.Is(err, ErrStale) {
		t.Fatalf("second retry = %v", err)
	}
	requireDelivery(t, db, current.ID, retried)

	if err := db.Delivered(ctx, acked); err != nil {
		t.Fatal(err)
	}
	delivered := heldLease(acked)
	delivered.owner = ""
	delivered.delivered = true
	requireDelivery(t, db, acked.ID, delivered)
	if err := db.Delivered(ctx, acked); !errors.Is(err, ErrStale) {
		t.Fatalf("second ack = %v", err)
	}
	requireDelivery(t, db, acked.ID, delivered)
}

func heldLease(row Outbox) deliveryView {
	return deliveryView{attempts: 1, token: row.LeaseToken, owner: row.LeaseOwner}
}

const outboxLeaseToken int64 = 4

func seedOutboxLease(t *testing.T, db *Store, workspace, taskID, runID, command string, expired bool) Outbox {
	t.Helper()
	id := newID()
	owner := newID()
	tag, err := db.pool.Exec(t.Context(), `
		INSERT INTO makez_outbox (
			id, workspace_id, task_id, run_id, command,
			attempts, lease_owner, lease_token, lease_until, next_attempt_at
		) VALUES (
			$1, $2, $3, $4, $5,
			1, $6, $7,
			CASE WHEN $8 THEN now() - interval '1 minute' ELSE now() + interval '30 seconds' END,
			now() + interval '1 day'
		)`, id, workspace, taskID, runID, command, owner, outboxLeaseToken, expired)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("inserted %d outbox rows", tag.RowsAffected())
	}
	return Outbox{ID: id, WorkspaceID: workspace, TaskID: taskID, RunID: runID, Command: command, Attempts: 1, LeaseOwner: owner, LeaseToken: outboxLeaseToken}
}

type deliveryView struct {
	attempts  int
	token     int64
	owner     string
	delivered bool
	dead      bool
	lastError string
}

func readDelivery(t *testing.T, db *Store, id string) deliveryView {
	t.Helper()
	var got deliveryView
	var owner *string
	err := db.pool.QueryRow(t.Context(), `
		SELECT attempts, lease_token, lease_owner, delivered_at IS NOT NULL, dead_at IS NOT NULL, last_error
		FROM makez_outbox WHERE id=$1`, id).Scan(&got.attempts, &got.token, &owner, &got.delivered, &got.dead, &got.lastError)
	if err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		got.owner = *owner
	}
	return got
}

func requireDelivery(t *testing.T, db *Store, id string, want deliveryView) {
	t.Helper()
	if got := readDelivery(t, db, id); got != want {
		t.Fatalf("delivery = %+v, want %+v", got, want)
	}
}
