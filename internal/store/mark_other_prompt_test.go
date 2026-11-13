package store

import (
	"errors"
	"testing"
)

// TestMarkPromptStartedLeavesAnotherPrompt uses Postgres.
// The same run stays the writer. It records a sending row for prompt hash A
// at generation 1 that is not started. MarkPromptStarted for a different
// valid prompt hash B, same session, run, and generation, returns ErrConflict.
// NativePrompt for hash A stays unstarted on that run at generation 1.
// That is not a second prompt write and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestMarkPromptStartedLeavesAnotherPrompt(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	held := recordUnstartedSending(t, db, fx)

	markErr := db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashB)
	row, err := db.NativePrompt(ctx, fx.workspace, fx.session, fx.hashA)
	if err != nil || !errors.Is(markErr, ErrConflict) || row != held {
		t.Fatalf("hash B MarkPromptStarted = %v NativePrompt %+v %v, want ErrConflict %+v", markErr, row, err, held)
	}
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}
