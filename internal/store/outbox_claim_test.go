package store

import (
	"testing"
	"time"
)

// TestPendingDoesNotClaimLeasedDeliveredOrDead uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A claim leases that command again. It does not prove the command's side effect ran once.
func TestPendingDoesNotClaimLeasedDeliveredOrDead(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "claim-"+workspace); err != nil {
		t.Fatal(err)
	}
	dueTask, dueRun := newID(), newID()
	blockedTask, blockedRun := newID(), newID()
	insertRun(t, db, workspace, dueTask, dueRun, "claim-due")
	insertRun(t, db, workspace, blockedTask, blockedRun, "claim-blocked")

	var dueAt time.Time
	if err := db.pool.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT min(next_attempt_at) FROM makez_outbox WHERE next_attempt_at <= now()),
			now()
		) - interval '1 second'`).Scan(&dueAt); err != nil {
		t.Fatal(err)
	}

	due := insertOutboxCommand(t, db, outboxSeed{
		workspace: workspace, taskID: dueTask, runID: dueRun, command: "START_TASK",
		attempts: 4, token: 9,
	})
	delivered := insertOutboxCommand(t, db, outboxSeed{
		workspace: workspace, taskID: dueTask, runID: dueRun, command: "CANCEL_TASK",
		attempts: 2, token: 5, lastError: "acknowledged", delivered: true,
	})
	dead := insertOutboxCommand(t, db, outboxSeed{
		workspace: workspace, taskID: dueTask, runID: dueRun, command: "START_CLEANUP",
		attempts: 3, token: 6, lastError: "delivery dead-lettered; operator replay required", dead: true,
	})
	exhausted := insertOutboxCommand(t, db, outboxSeed{
		workspace: workspace, taskID: blockedTask, runID: blockedRun, command: "START_TASK",
		attempts: 12, token: 15,
	})
	for _, id := range []string{due.id, delivered.id, dead.id, exhausted.id} {
		setOutboxDue(t, db, id, dueAt)
	}
	due = readOutboxCommand(t, db, due.id)
	delivered = readOutboxCommand(t, db, delivered.id)
	dead = readOutboxCommand(t, db, dead.id)
	exhausted = readOutboxCommand(t, db, exhausted.id)
	dueClock := readClaimClock(t, db, due.id)
	deliveredClock := readClaimClock(t, db, delivered.id)
	deadClock := readClaimClock(t, db, dead.id)
	requireClaimShape(t, db, due.id, "due")
	requireClaimShape(t, db, delivered.id, "delivered")
	requireClaimShape(t, db, dead.id, "dead")
	requireClaimShape(t, db, exhausted.id, "exhausted")

	first, err := db.Pending(ctx, claimRank(t, db, due.id, true))
	if err != nil {
		t.Fatal(err)
	}
	held := requireClaimedOnce(t, db, first, due, dueClock)
	requireCommandHeld(t, db, first, delivered, deliveredClock)
	requireCommandHeld(t, db, first, dead, deadClock)
	requireUnclaimedExhausted(t, db, first, exhausted)

	// This count includes the open lease. Pending stays inside that window, so
	// dropping lease_until IS NULL from ClaimOutbox returns this id again.
	second, err := db.Pending(ctx, claimRank(t, db, due.id, false))
	if err != nil {
		t.Fatal(err)
	}
	requireLeaseHeld(t, db, second, held, dueClock)
	requireCommandHeld(t, db, second, delivered, deliveredClock)
	requireCommandHeld(t, db, second, dead, deadClock)
	requireUnclaimedExhausted(t, db, second, exhausted)
}

func setOutboxDue(t *testing.T, db *Store, id string, at time.Time) {
	t.Helper()
	tag, err := db.pool.Exec(t.Context(), `UPDATE makez_outbox SET next_attempt_at=$2 WHERE id=$1`, id, at)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("scheduled %d outbox rows", tag.RowsAffected())
	}
}

func claimRank(t *testing.T, db *Store, id string, unleasedOnly bool) int {
	t.Helper()
	var n int
	err := db.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM makez_outbox o
		WHERE o.delivered_at IS NULL AND o.dead_at IS NULL
			AND o.next_attempt_at <= now() AND o.attempts < 12
			AND (NOT $2 OR o.lease_until IS NULL)
			AND (o.next_attempt_at, o.id) <= (SELECT next_attempt_at, id FROM makez_outbox WHERE id=$1)`,
		id, unleasedOnly).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 || n > 500 {
		t.Fatalf("claim window = %d", n)
	}
	return n
}

type claimClock struct {
	createdAt   time.Time
	deliveredAt *time.Time
	deadAt      *time.Time
}

func readClaimClock(t *testing.T, db *Store, id string) claimClock {
	t.Helper()
	var clock claimClock
	err := db.pool.QueryRow(t.Context(), `
		SELECT created_at, delivered_at, dead_at FROM makez_outbox WHERE id=$1`, id).Scan(
		&clock.createdAt, &clock.deliveredAt, &clock.deadAt)
	if err != nil {
		t.Fatal(err)
	}
	return clock
}

func sameInstant(got, want *time.Time) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got.Equal(*want)
}

func requireClaimShape(t *testing.T, db *Store, id, kind string) {
	t.Helper()
	var ok bool
	var err error
	switch kind {
	case "due":
		err = db.pool.QueryRow(t.Context(), `
			SELECT delivered_at IS NULL AND dead_at IS NULL AND lease_until IS NULL
				AND next_attempt_at <= now() AND attempts < 12
			FROM makez_outbox WHERE id=$1`, id).Scan(&ok)
	case "delivered":
		err = db.pool.QueryRow(t.Context(), `
			SELECT delivered_at IS NOT NULL AND dead_at IS NULL AND lease_until IS NULL
				AND next_attempt_at <= now() AND attempts < 12
			FROM makez_outbox WHERE id=$1`, id).Scan(&ok)
	case "dead":
		err = db.pool.QueryRow(t.Context(), `
			SELECT dead_at IS NOT NULL AND delivered_at IS NULL AND lease_until IS NULL
				AND next_attempt_at <= now() AND attempts < 12
			FROM makez_outbox WHERE id=$1`, id).Scan(&ok)
	case "exhausted":
		err = db.pool.QueryRow(t.Context(), `
			SELECT delivered_at IS NULL AND dead_at IS NULL AND lease_until IS NULL
				AND next_attempt_at <= now() AND attempts >= 12
			FROM makez_outbox WHERE id=$1`, id).Scan(&ok)
	default:
		t.Fatalf("unknown claim fixture %s", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("command %s is not a %s claim fixture", id, kind)
	}
}

func requireClaimedOnce(t *testing.T, db *Store, rows []Outbox, before outboxRow, clock claimClock) outboxRow {
	t.Helper()
	found := claimedRow(t, rows, before.id)
	got := readOutboxCommand(t, db, before.id)
	if got.attempts != before.attempts+1 || got.token != before.token+1 || got.owner == "" || got.leaseUntil.IsZero() {
		t.Fatalf("claim = %+v, want attempts %d token %d and one lease owner", got, before.attempts+1, before.token+1)
	}
	requireClockHeld(t, db, before.id, clock)
	if found.Attempts != got.attempts || found.LeaseToken != got.token || found.LeaseOwner != got.owner {
		t.Fatalf("returned claim %+v does not match stored %+v", found, got)
	}
	if found.WorkspaceID != before.workspaceID || found.TaskID != before.taskID || found.RunID != before.runID || found.Command != before.command {
		t.Fatalf("claim changed identity: %+v", found)
	}
	if got.id != before.id || got.workspaceID != before.workspaceID || got.taskID != before.taskID || got.runID != before.runID || got.command != before.command {
		t.Fatalf("stored claim changed identity from %+v to %+v", before, got)
	}
	if got.delivered || got.dead || !got.nextAt.Equal(before.nextAt) || got.lastError != before.lastError {
		t.Fatalf("claim changed the command outside its lease: %+v", got)
	}
	if ids := outboxCommandIDs(t, db, before.taskID, before.command); len(ids) != 1 || ids[0] != before.id {
		t.Fatalf("command ids = %v, want [%s]", ids, before.id)
	}
	var open bool
	if err := db.pool.QueryRow(t.Context(), `SELECT lease_owner IS NOT NULL AND lease_until > now() FROM makez_outbox WHERE id=$1`, before.id).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if !open {
		t.Fatal("claim lease is not in the future")
	}
	return got
}

func claimedRow(t *testing.T, rows []Outbox, id string) Outbox {
	t.Helper()
	var found *Outbox
	for i := range rows {
		if rows[i].ID != id {
			continue
		}
		if found != nil {
			t.Fatalf("command %s returned more than once", id)
		}
		found = &rows[i]
	}
	if found == nil {
		t.Fatalf("due command %s was not claimed", id)
	}
	return *found
}

func requireLeaseHeld(t *testing.T, db *Store, rows []Outbox, held outboxRow, clock claimClock) {
	t.Helper()
	n := 0
	for _, row := range rows {
		if row.ID == held.id {
			n++
		}
	}
	got := readOutboxCommand(t, db, held.id)
	if n != 0 || !sameOutbox(got, held) {
		t.Fatalf("leased command was claimed again: returned %d times, got %+v, want %+v", n, got, held)
	}
	requireClockHeld(t, db, held.id, clock)
}

func requireClockHeld(t *testing.T, db *Store, id string, clock claimClock) {
	t.Helper()
	got := readClaimClock(t, db, id)
	if !got.createdAt.Equal(clock.createdAt) || !sameInstant(got.deliveredAt, clock.deliveredAt) || !sameInstant(got.deadAt, clock.deadAt) {
		t.Fatalf("timestamps changed: got %+v, want %+v", got, clock)
	}
}

func requireCommandHeld(t *testing.T, db *Store, rows []Outbox, before outboxRow, clock claimClock) {
	t.Helper()
	for _, row := range rows {
		if row.ID == before.id {
			t.Fatalf("command %s was returned: %+v", before.id, row)
		}
	}
	got := readOutboxCommand(t, db, before.id)
	if !sameOutbox(got, before) {
		t.Fatalf("command changed: got %+v, want %+v", got, before)
	}
	requireClockHeld(t, db, before.id, clock)
	if ids := outboxCommandIDs(t, db, before.taskID, before.command); len(ids) != 1 || ids[0] != before.id {
		t.Fatalf("command ids = %v, want [%s]", ids, before.id)
	}
}

func requireUnclaimedExhausted(t *testing.T, db *Store, rows []Outbox, before outboxRow) {
	t.Helper()
	for _, row := range rows {
		if row.ID == before.id {
			t.Fatalf("command with %d attempts was claimed: %+v", before.attempts, row)
		}
	}
	got := readOutboxCommand(t, db, before.id)
	clock := readClaimClock(t, db, before.id)
	if got.attempts != before.attempts || got.token != before.token || got.owner != "" || !got.leaseUntil.IsZero() || clock.deliveredAt != nil || got.delivered {
		t.Fatalf("command at the attempt limit was claimed or delivered: got %+v deliveredAt %v", got, clock.deliveredAt)
	}
	if ids := outboxCommandIDs(t, db, before.taskID, before.command); len(ids) != 1 || ids[0] != before.id {
		t.Fatalf("command ids = %v, want [%s]", ids, before.id)
	}
}
