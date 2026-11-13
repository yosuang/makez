package store

import "testing"

// TestRepeatedSendingKeepsTheStartedFlag uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A repeated sending write is not a claim that the prompt bytes were written again, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestRepeatedSendingKeepsTheStartedFlag(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireRunningWithoutOutbox(t, db, fx)

	// saveProgress of sending leaves started false.
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	requireSendingPrompt(t, db, fx, false, 1)

	// MarkPromptStarted leaves started true and disposition sending.
	if err := db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashA); err != nil {
		t.Fatal(err)
	}
	requireSendingPrompt(t, db, fx, true, 1)

	// SaveAttemptProgress of sending again, for that same run, session, generation, and prompt hash, returns nil.
	// NativePrompt still has disposition sending, started true, the same run, and generation 1.
	if err := db.SaveAttemptProgress(ctx, fx.workspace, fx.runA, fx.session, 1, "sending", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}
	prompt := readNativePrompt(t, db, fx)
	want := sendingPrompt(fx.runA, true, 1)
	if prompt != want {
		t.Fatalf("repeated sending NativePrompt = %+v, want %+v", prompt, want)
	}

	// The task stays RUNNING. No new outbox row is inserted.
	requireRunningWithoutOutbox(t, db, fx)
}
