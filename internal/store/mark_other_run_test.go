package store

import (
	"errors"
	"testing"
)

// TestMarkPromptStartedLeavesAnotherRunsRow uses Postgres.
// Run A records sending and releases the writer. Run B opens that session and
// MarkPromptStarted returns ErrConflict. The row stays unstarted on run A.
// That is not a second prompt write and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestMarkPromptStartedLeavesAnotherRunsRow(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	if err := db.ReleaseSessionWriter(ctx, fx.workspace, fx.session, fx.runA); err != nil {
		t.Fatal(err)
	}
	held := sendingPrompt(fx.runA, false, 1)
	if row, err := db.NativePrompt(ctx, fx.workspace, fx.session, fx.hashA); err != nil || row != held {
		t.Fatalf("run A NativePrompt = %+v %v, want %+v", row, err, held)
	}

	openGeneration(t, db, fx, fx.runB, fx.hashA)
	markErr := db.MarkPromptStarted(ctx, fx.workspace, fx.runB, fx.session, 1, fx.hashA)
	row, err := db.NativePrompt(ctx, fx.workspace, fx.session, fx.hashA)
	if err != nil || !errors.Is(markErr, ErrConflict) || row != held {
		t.Fatalf("run B MarkPromptStarted = %v NativePrompt %+v %v, want ErrConflict %+v", markErr, row, err, held)
	}
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}
