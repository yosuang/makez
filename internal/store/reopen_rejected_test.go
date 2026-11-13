package store

import (
	"errors"
	"testing"
)

// TestReopenUnsentPromptLeavesARejectedRow uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// ReopenUnsentPrompt updates a command row only when disposition is sending.
// A rejected row returns ErrConflict and stays on run A at generation 1.
// That is not a claim that Pi rejected the prompt bytes, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestReopenUnsentPromptLeavesARejectedRow(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	saveProgress(t, db, fx, fx.runA, 1, "rejected", fx.hashA, "")
	if err := db.ReleaseSessionWriter(ctx, fx.workspace, fx.session, fx.runA); err != nil {
		t.Fatal(err)
	}

	want := NativePrompt{Disposition: "rejected", Started: false, RunID: fx.runA, Generation: 1, Found: true}
	rejected, err := db.NativePrompt(ctx, fx.workspace, fx.session, fx.hashA)
	if err != nil || rejected != want {
		t.Fatalf("released NativePrompt = %+v %v, want %+v", rejected, err, want)
	}

	openGeneration(t, db, fx, fx.runB, fx.hashA)
	err = db.ReopenUnsentPrompt(ctx, fx.workspace, fx.runB, fx.session, 1, fx.hashA)
	left, readErr := db.NativePrompt(ctx, fx.workspace, fx.session, fx.hashA)
	if readErr != nil || !errors.Is(err, ErrConflict) || left != want {
		t.Fatalf("ReopenUnsentPrompt = %v prompt %+v %v, want ErrConflict and %+v", err, left, readErr, want)
	}
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}
