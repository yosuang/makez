package store

import "testing"

// TestAcceptedPromptStaysWithTheFirstRun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// An accepted prompt stays with the first run when another run writes settled.
// That is not a second prompt write and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestAcceptedPromptStaysWithTheFirstRun(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireBothTasksRunningWithoutOutbox(t, db, fx)

	// Run A saveProgress of accepted leaves disposition accepted, started false, and run A
	// on the command row.
	saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "")
	want := acceptedPrompt(fx.runA, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != want {
		t.Fatalf("accepted NativePrompt = %+v, want %+v", prompt, want)
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

	// Run B SaveAttemptProgress of settled, for run A's prompt hash, generation 1, and an empty
	// attempt state, returns nil. NativePrompt stays disposition accepted, started false, run A,
	// generation 1. Run B does not become the command row's run.
	if err := db.SaveAttemptProgress(ctx, fx.workspace, fx.runB, fx.session, 1, "settled", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}
	if prompt := readNativePrompt(t, db, fx); prompt != want {
		t.Fatalf("other run settled NativePrompt = %+v, want %+v", prompt, want)
	}

	// Both tasks stay RUNNING. No new outbox row is inserted.
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}

func acceptedPrompt(runID string, generation int64) NativePrompt {
	return NativePrompt{Disposition: "accepted", Started: false, RunID: runID, Generation: generation, Found: true}
}
