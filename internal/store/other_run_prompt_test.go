package store

import "testing"

// TestStartedSendingStaysWithTheFirstRun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A started sending row stays with the first run when another run writes accepted.
// That is not a second prompt write and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestStartedSendingStaysWithTheFirstRun(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireRunningWithoutOutbox(t, db, fx)

	// Run A saveProgress of sending, then MarkPromptStarted, leaves disposition sending,
	// started true, and run A on the command row.
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	if err := db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashA); err != nil {
		t.Fatal(err)
	}
	want := sendingPrompt(fx.runA, true, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != want {
		t.Fatalf("started sending NativePrompt = %+v, want %+v", prompt, want)
	}

	// Run A ReleaseSessionWriter. The command row stays as it was.
	if err := db.ReleaseSessionWriter(ctx, fx.workspace, fx.session, fx.runA); err != nil {
		t.Fatal(err)
	}
	if prompt := readNativePrompt(t, db, fx); prompt != want {
		t.Fatalf("after release NativePrompt = %+v, want %+v", prompt, want)
	}

	// OpenAttempt does not rewrite it. Run B openGeneration is that call on the same session.
	openGeneration(t, db, fx, fx.runB, fx.hashA)
	if prompt := readNativePrompt(t, db, fx); prompt != want {
		t.Fatalf("OpenAttempt NativePrompt = %+v, want %+v", prompt, want)
	}

	// Run B SaveAttemptProgress of accepted, for run A's prompt hash, generation 1, and an empty
	// attempt state, returns nil. NativePrompt stays disposition sending, started true, run A, generation 1.
	if err := db.SaveAttemptProgress(ctx, fx.workspace, fx.runB, fx.session, 1, "accepted", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}
	if prompt := readNativePrompt(t, db, fx); prompt != want {
		t.Fatalf("other run accepted NativePrompt = %+v, want %+v", prompt, want)
	}

	// Run A's task stays RUNNING. No new outbox row is inserted.
	requireRunningWithoutOutbox(t, db, fx)
}
