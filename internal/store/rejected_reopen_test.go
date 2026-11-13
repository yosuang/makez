package store

import "testing"

// TestRejectedPromptMovesToTheNextRun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A rejected row moves to the next run when that run records sending.
// The prompt bytes were not written again, and this is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestRejectedPromptMovesToTheNextRun(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireBothTasksRunningWithoutOutbox(t, db, fx)

	// Run A saveProgress of sending, then saveProgress of rejected, leaves one command row:
	// disposition rejected, run A, generation 1, started false. MarkPromptStarted is not called.
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	saveProgress(t, db, fx, fx.runA, 1, "rejected", fx.hashA, "")
	held := rejectedPrompt(fx.runA, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("rejected NativePrompt = %+v, want %+v", prompt, held)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("commands = %d, want 1", n)
	}

	// Run A ReleaseSessionWriter. The command row stays as it was.
	if err := db.ReleaseSessionWriter(ctx, fx.workspace, fx.session, fx.runA); err != nil {
		t.Fatal(err)
	}
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("after release NativePrompt = %+v, want %+v", prompt, held)
	}

	// Run B openGeneration on the same session does not rewrite the command row.
	// SaveAttemptProgress of sending, for that same prompt hash, generation 1, and an
	// empty attempt state, returns nil.
	openGeneration(t, db, fx, fx.runB, fx.hashA)
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("OpenAttempt NativePrompt = %+v, want %+v", prompt, held)
	}
	if err := db.SaveAttemptProgress(ctx, fx.workspace, fx.runB, fx.session, 1, "sending", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}

	// NativePrompt is disposition sending, started false, run B, generation 1.
	// commandCount stays 1.
	moved := sendingPrompt(fx.runB, false, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != moved {
		t.Fatalf("next run sending NativePrompt = %+v, want %+v", prompt, moved)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("commands = %d, want 1", n)
	}

	// HasUnknownPrompt for the other prompt hash returns false, nil.
	unknown, err := db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || unknown {
		t.Fatalf("next run HasUnknownPrompt = %v %v, want false, nil", unknown, err)
	}

	// Both tasks stay RUNNING. No new outbox row is inserted.
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}
