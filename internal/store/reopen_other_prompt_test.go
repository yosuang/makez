package store

import (
	"errors"
	"testing"
)

// TestReopenUnsentPromptLeavesAnotherPrompt uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// The same run stays the writer. It records a sending row for prompt hash A
// at generation 1, and MarkPromptStarted has set that row started.
// ReopenUnsentPrompt for a different valid prompt hash B, same session and
// run, with generation 2, returns ErrConflict. NativePrompt for hash A stays
// started, sending, generation 1, on that run.
// No command row is inserted for hash B.
// That is not a second prompt write and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestReopenUnsentPromptLeavesAnotherPrompt(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	recordUnstartedSending(t, db, fx)
	if err := db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashA); err != nil {
		t.Fatal(err)
	}
	held := sendingPrompt(fx.runA, true, 1)
	requireSendingPrompt(t, db, fx, true, 1)

	reopenErr := db.ReopenUnsentPrompt(ctx, fx.workspace, fx.runA, fx.session, 2, fx.hashB)
	row, err := db.NativePrompt(ctx, fx.workspace, fx.session, fx.hashA)
	if err != nil || !errors.Is(reopenErr, ErrConflict) || row != held {
		t.Fatalf("hash B ReopenUnsentPrompt = %v NativePrompt %+v %v, want ErrConflict %+v", reopenErr, row, err, held)
	}
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}
