package store

import "testing"

// TestRejectedPromptStaysRejectedForTheSameRun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A rejected row stays rejected when the same run writes sending again.
// That is not a claim that Pi rejected the prompt bytes, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestRejectedPromptStaysRejectedForTheSameRun(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireRunningWithoutOutbox(t, db, fx)

	// saveProgress of sending, then saveProgress of rejected, leaves one command row:
	// disposition rejected, run A, generation 1.
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	saveProgress(t, db, fx, fx.runA, 1, "rejected", fx.hashA, "")
	held := rejectedPrompt(fx.runA, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("rejected NativePrompt = %+v, want %+v", prompt, held)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("commands = %d, want 1", n)
	}

	// HasUnknownPrompt for the other prompt hash returns false, nil.
	unknown, err := db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || unknown {
		t.Fatalf("rejected prompt HasUnknownPrompt = %v %v, want false, nil", unknown, err)
	}

	// SaveAttemptProgress of sending again, for that same run, session, generation, and
	// prompt hash, with an empty attempt state, returns nil.
	if err = db.SaveAttemptProgress(ctx, fx.workspace, fx.runA, fx.session, 1, "sending", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}

	// NativePrompt is still disposition rejected, run A, generation 1.
	// commandCount stays 1. HasUnknownPrompt for the other hash stays false.
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("same run sending NativePrompt = %+v, want %+v", prompt, held)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("commands = %d, want 1", n)
	}
	unknown, err = db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || unknown {
		t.Fatalf("after sending HasUnknownPrompt = %v %v, want false, nil", unknown, err)
	}

	// The task stays RUNNING. No new outbox row is inserted.
	requireRunningWithoutOutbox(t, db, fx)
}

func rejectedPrompt(runID string, generation int64) NativePrompt {
	return NativePrompt{Disposition: "rejected", Started: false, RunID: runID, Generation: generation, Found: true}
}
