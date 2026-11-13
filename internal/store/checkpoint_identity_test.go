package store

import (
	"bytes"
	"errors"
	"testing"
)

// TestSaveCheckpointRejectsABlankRunOrGeneration uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Saving a checkpoint records bytes Pi already wrote. It is not a claim that
// Pi wrote them, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestSaveCheckpointRejectsABlankRunOrGeneration(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)

	// The current writer stores a few bytes at generation 1.
	stored := []byte("abc")
	if err := db.SaveCheckpoint(ctx, fx.workspace, fx.session, fx.runA, 1, stored); err != nil {
		t.Fatal(err)
	}

	// Generation 0 with those same bytes returns ErrInvalidInput.
	// LoadCheckpoint still returns the stored bytes.
	err := db.SaveCheckpoint(ctx, fx.workspace, fx.session, fx.runA, 0, stored)
	got, loadErr := db.LoadCheckpoint(ctx, fx.workspace, fx.session)
	if loadErr != nil || !bytes.Equal(got, stored) {
		t.Fatalf("generation 0 left %q, want %q, loadErr=%v", got, stored, loadErr)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("generation 0 = %v, want ErrInvalidInput; loaded %q", err, got)
	}

	// An empty run id at generation 1 with those same bytes returns ErrInvalidInput.
	// LoadCheckpoint still returns the stored bytes.
	err = db.SaveCheckpoint(ctx, fx.workspace, fx.session, "", 1, stored)
	got, loadErr = db.LoadCheckpoint(ctx, fx.workspace, fx.session)
	if loadErr != nil || !bytes.Equal(got, stored) {
		t.Fatalf("empty run left %q, want %q, loadErr=%v", got, stored, loadErr)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty run = %v, want ErrInvalidInput; loaded %q", err, got)
	}

	// The task stays RUNNING. No outbox row is inserted.
	requireRunningWithoutOutbox(t, db, fx)
}
