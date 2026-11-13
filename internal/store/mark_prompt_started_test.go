package store

import (
	"errors"
	"testing"
)

// TestMarkPromptStartedRequiresTheCurrentWriter uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Marking a prompt started records the store flag. It is not a claim that the
// prompt bytes were written, and it is not exactly-once for that action.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestMarkPromptStartedRequiresTheCurrentWriter(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	requireSendingPrompt(t, db, fx, false, 1)

	// After the writer lease is released, MarkPromptStarted returns ErrStaleWriter.
	// started stays false. The task stays RUNNING. No outbox command is inserted.
	if err := db.ReleaseSessionWriter(ctx, fx.workspace, fx.session, fx.runA); err != nil {
		t.Fatal(err)
	}
	err := db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashA)
	prompt := readNativePrompt(t, db, fx)
	unstarted := sendingPrompt(fx.runA, false, 1)
	if !errors.Is(err, ErrStaleWriter) || prompt != unstarted {
		t.Fatalf("released writer MarkPromptStarted = %v prompt %+v, want ErrStaleWriter with %+v", err, prompt, unstarted)
	}
	requireRunningWithoutOutbox(t, db, fx)

	// After the same run acquires the lease again, MarkPromptStarted returns nil and started is true.
	// A second call returns nil and started stays true.
	if err = db.AcquireSessionWriter(ctx, fx.workspace, fx.session, fx.runA); err != nil {
		t.Fatal(err)
	}
	if err = db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashA); err != nil {
		t.Fatal(err)
	}
	requireSendingPrompt(t, db, fx, true, 1)
	if err = db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashA); err != nil {
		t.Fatal(err)
	}
	requireSendingPrompt(t, db, fx, true, 1)

	// A different generation returns ErrConflict. started stays true and the stored generation stays 1.
	err = db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 2, fx.hashA)
	prompt = readNativePrompt(t, db, fx)
	started := sendingPrompt(fx.runA, true, 1)
	if !errors.Is(err, ErrConflict) || prompt != started {
		t.Fatalf("generation 2 MarkPromptStarted = %v prompt %+v, want ErrConflict with %+v", err, prompt, started)
	}

	// After the lease is released again, ReopenUnsentPrompt returns ErrStaleWriter. started stays true.
	if err = db.ReleaseSessionWriter(ctx, fx.workspace, fx.session, fx.runA); err != nil {
		t.Fatal(err)
	}
	err = db.ReopenUnsentPrompt(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashA)
	prompt = readNativePrompt(t, db, fx)
	if !errors.Is(err, ErrStaleWriter) || prompt != started {
		t.Fatalf("released writer ReopenUnsentPrompt = %v prompt %+v, want ErrStaleWriter with %+v", err, prompt, started)
	}

	// After the lease is acquired again, ReopenUnsentPrompt returns nil and started is false.
	// The task stays RUNNING. No outbox command is inserted.
	if err = db.AcquireSessionWriter(ctx, fx.workspace, fx.session, fx.runA); err != nil {
		t.Fatal(err)
	}
	if err = db.ReopenUnsentPrompt(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashA); err != nil {
		t.Fatal(err)
	}
	requireSendingPrompt(t, db, fx, false, 1)
	requireRunningWithoutOutbox(t, db, fx)
}

func sendingPrompt(runID string, started bool, generation int64) NativePrompt {
	return NativePrompt{Disposition: "sending", Started: started, RunID: runID, Generation: generation, Found: true}
}

func requireSendingPrompt(t *testing.T, db *Store, fx leasedSession, started bool, generation int64) {
	t.Helper()
	prompt := readNativePrompt(t, db, fx)
	want := sendingPrompt(fx.runA, started, generation)
	if prompt != want {
		t.Fatalf("prompt = %+v, want %+v", prompt, want)
	}
}

func readNativePrompt(t *testing.T, db *Store, fx leasedSession) NativePrompt {
	t.Helper()
	prompt, err := db.NativePrompt(t.Context(), fx.workspace, fx.session, fx.hashA)
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}
