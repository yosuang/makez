package store

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// TestSaveCheckpointRejectsAnOversizedCheckpoint uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Saving a checkpoint records bytes Pi already wrote. It is not a claim that
// Pi wrote them, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestSaveCheckpointRejectsAnOversizedCheckpoint(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)

	// A checkpoint of exactly 1 MiB is stored.
	// The task stays RUNNING. No outbox row is inserted.
	exact := bytes.Repeat([]byte{0x01}, 1<<20)
	if err := db.SaveCheckpoint(ctx, fx.workspace, fx.session, fx.runA, 1, exact); err != nil {
		t.Fatal(err)
	}
	requireCheckpoint(t, db, fx, exact)
	requireRunningWithoutOutbox(t, db, fx)

	// One byte past 1 MiB returns ErrInvalidInput.
	// The stored checkpoint stays the 1 MiB bytes.
	oversized := bytes.Repeat([]byte{0x02}, 1<<20+1)
	requireCheckpointRejected(t, db, fx, oversized, exact)

	// An empty slice returns ErrInvalidInput.
	// The stored checkpoint stays the 1 MiB bytes.
	requireCheckpointRejected(t, db, fx, []byte{}, exact)
}

func requireCheckpoint(t *testing.T, db *Store, fx leasedSession, want []byte) {
	t.Helper()
	got, err := db.LoadCheckpoint(t.Context(), fx.workspace, fx.session)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("checkpoint len=%d mark=%s, want len=%d mark=%s err=%v", len(got), checkpointMark(got), len(want), checkpointMark(want), err)
	}
}

func requireCheckpointRejected(t *testing.T, db *Store, fx leasedSession, data, stored []byte) {
	t.Helper()
	err := db.SaveCheckpoint(t.Context(), fx.workspace, fx.session, fx.runA, 1, data)
	got, loadErr := db.LoadCheckpoint(t.Context(), fx.workspace, fx.session)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if err == nil || !bytes.Equal(got, stored) {
		t.Fatalf("%d-byte SaveCheckpoint accepted or replaced the stored bytes: err=%v loaded=%d mark=%s want=%d mark=%s", len(data), err, len(got), checkpointMark(got), len(stored), checkpointMark(stored))
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("%d-byte SaveCheckpoint = %v, want ErrInvalidInput; loaded=%d mark=%s", len(data), err, len(got), checkpointMark(got))
	}
}

func checkpointMark(data []byte) string {
	if len(data) == 0 {
		return "empty"
	}
	return fmt.Sprintf("%02x", data[0])
}
