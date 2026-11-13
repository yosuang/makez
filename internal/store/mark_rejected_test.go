package store

import (
	"errors"
	"testing"
)

// TestMarkPromptStartedLeavesARejectedRow uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// MarkPromptStarted sets started only when the command row is sending for this run and generation.
// A rejected row returns ErrConflict and stays unstarted. The writer lease stays with run A.
// That is not a claim that Pi rejected the prompt bytes, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestMarkPromptStartedLeavesARejectedRow(t *testing.T) {
	db := leaseDB(t)
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	saveProgress(t, db, fx, fx.runA, 1, "rejected", fx.hashA, "")

	want := rejectedPrompt(fx.runA, 1)
	prompt, err := db.NativePrompt(t.Context(), fx.workspace, fx.session, fx.hashA)
	if err != nil || prompt != want {
		t.Fatalf("rejected NativePrompt = %+v %v, want %+v", prompt, err, want)
	}

	markErr := db.MarkPromptStarted(t.Context(), fx.workspace, fx.runA, fx.session, 1, fx.hashA)
	prompt, err = db.NativePrompt(t.Context(), fx.workspace, fx.session, fx.hashA)
	if err != nil || !errors.Is(markErr, ErrConflict) || prompt != want {
		t.Fatalf("MarkPromptStarted = %v NativePrompt = %+v %v, want ErrConflict and %+v", markErr, prompt, err, want)
	}
	requireRunningWithoutOutbox(t, db, fx)
}
