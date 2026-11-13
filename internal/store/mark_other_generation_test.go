package store

import (
	"errors"
	"testing"
)

// TestMarkPromptStartedLeavesAnotherGeneration uses Postgres.
// The same run stays the writer. It records a sending row at generation 1
// that is not started. MarkPromptStarted for generation 2 returns ErrConflict.
// NativePrompt stays unstarted on that run at generation 1.
// That is not a second prompt write and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestMarkPromptStartedLeavesAnotherGeneration(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	held := recordUnstartedSending(t, db, fx)

	markErr := db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 2, fx.hashA)
	row, err := db.NativePrompt(ctx, fx.workspace, fx.session, fx.hashA)
	if err != nil || !errors.Is(markErr, ErrConflict) || row != held {
		t.Fatalf("generation 2 MarkPromptStarted = %v NativePrompt %+v %v, want ErrConflict %+v", markErr, row, err, held)
	}
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}
