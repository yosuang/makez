package store

import "testing"

// TestRejectedPromptStaysRejectedForTheSameRun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A rejected row stays rejected when the same run writes sending again.
// That is not a claim that Pi rejected the prompt bytes, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestRejectedPromptStaysRejectedForTheSameRun(t *testing.T) {
	sameRunKeepsTerminalDisposition(t, "rejected")
}

// TestReconciledPromptStaysReconciledForTheSameRun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A reconciled row stays reconciled when the same run writes sending again.
// That is not a claim that Pi reconciled the prompt bytes, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestReconciledPromptStaysReconciledForTheSameRun(t *testing.T) {
	sameRunKeepsTerminalDisposition(t, "reconciled")
}

// sameRunKeepsTerminalDisposition records sending, then disposition, for generation 1
// and the current writer. A later sending write by that same run leaves the row there.
// MarkPromptStarted is not called.
func sameRunKeepsTerminalDisposition(t *testing.T, disposition string) {
	t.Helper()
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireRunningWithoutOutbox(t, db, fx)

	// saveProgress of sending, then saveProgress of disposition, leaves one command row:
	// that disposition, run A, generation 1, started false.
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	saveProgress(t, db, fx, fx.runA, 1, disposition, fx.hashA, "")
	held := NativePrompt{Disposition: disposition, Started: false, RunID: fx.runA, Generation: 1, Found: true}
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("%s NativePrompt = %+v, want %+v", disposition, prompt, held)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("%s commands = %d, want 1", disposition, n)
	}

	// HasUnknownPrompt for the other prompt hash returns false, nil.
	unknown, err := db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || unknown {
		t.Fatalf("%s HasUnknownPrompt = %v %v, want false, nil", disposition, unknown, err)
	}

	// SaveAttemptProgress of sending again, for that same run, session, generation, and
	// prompt hash, with an empty attempt state, returns nil.
	if err = db.SaveAttemptProgress(ctx, fx.workspace, fx.runA, fx.session, 1, "sending", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}

	// NativePrompt is still that disposition, run A, generation 1, started false.
	// commandCount stays 1. HasUnknownPrompt for the other hash stays false.
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("%s same run sending NativePrompt = %+v, want %+v", disposition, prompt, held)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("%s commands = %d, want 1", disposition, n)
	}
	unknown, err = db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || unknown {
		t.Fatalf("%s after sending HasUnknownPrompt = %v %v, want false, nil", disposition, unknown, err)
	}

	// The task stays RUNNING. No new outbox row is inserted.
	requireRunningWithoutOutbox(t, db, fx)
}

func rejectedPrompt(runID string, generation int64) NativePrompt {
	return NativePrompt{Disposition: "rejected", Started: false, RunID: runID, Generation: generation, Found: true}
}
