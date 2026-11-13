package store

import "testing"

// TestNextGenerationRecordsSendingAfterRejected uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A newer generation records sending after rejected for the same run.
// The prompt bytes were not written again, and this is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestNextGenerationRecordsSendingAfterRejected(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireRunningWithoutOutbox(t, db, fx)

	// saveProgress of sending at generation 1, then saveProgress of rejected at
	// generation 1 with attempt state lost, leaves one command row: disposition
	// rejected, run A, generation 1, started false. MarkPromptStarted is not called.
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	saveProgress(t, db, fx, fx.runA, 1, "rejected", fx.hashA, "lost")
	held := rejectedPrompt(fx.runA, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("rejected NativePrompt = %+v, want %+v", prompt, held)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("commands = %d, want 1", n)
	}

	// OpenAttempt for that same run, session, and prompt hash returns generation 2.
	// The command row is still rejected at generation 1. OpenAttempt does not rewrite it.
	// An open attempt does not advance, so the rejected write must use state lost.
	next, err := db.OpenAttempt(ctx, fx.workspace, fx.runA, fx.session, fx.hashA)
	if err != nil || next.Generation != 2 || next.Step != "agent:2" || next.State != "open" || next.Disposition != "none" || next.SessionID != fx.session || next.PromptSHA256 != fx.hashA {
		t.Fatalf("generation 2 = %+v %v", next, err)
	}
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("OpenAttempt NativePrompt = %+v, want %+v", prompt, held)
	}

	// SaveAttemptProgress of sending at generation 2, for that same run, session,
	// and prompt hash, with an empty attempt state, returns nil.
	if err = db.SaveAttemptProgress(ctx, fx.workspace, fx.runA, fx.session, 2, "sending", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}

	// NativePrompt is disposition sending, started false, run A, generation 2.
	// commandCount stays 1.
	moved := sendingPrompt(fx.runA, false, 2)
	if prompt := readNativePrompt(t, db, fx); prompt != moved {
		t.Fatalf("next generation sending NativePrompt = %+v, want %+v", prompt, moved)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("commands = %d, want 1", n)
	}

	// HasUnknownPrompt for the other prompt hash returns false, nil.
	unknown, err := db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || unknown {
		t.Fatalf("next generation HasUnknownPrompt = %v %v, want false, nil", unknown, err)
	}

	// The task stays RUNNING. No new outbox row is inserted.
	requireRunningWithoutOutbox(t, db, fx)
}
