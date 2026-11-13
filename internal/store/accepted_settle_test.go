package store

import "testing"

// TestAcceptedPromptCanSettle uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// The same run stays the writer. An accepted prompt settles when that
// run writes settled.
// That is not a second prompt write and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestAcceptedPromptCanSettle(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireBothTasksRunningWithoutOutbox(t, db, fx)

	// Run A saveProgress of accepted leaves disposition accepted, started false, and run A
	// on the command row. The same run stays the writer.
	saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "")
	want := acceptedPrompt(fx.runA, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != want {
		t.Fatalf("accepted NativePrompt = %+v, want %+v", prompt, want)
	}
	writer := readWriter(t, db, fx.session)
	if !writer.current || writer.runID != fx.runA {
		t.Fatalf("writer = %+v, want run A", writer)
	}

	// SaveAttemptProgress of settled, for that same prompt, run, and generation, with an empty
	// attempt state, returns nil. NativePrompt becomes settled on that run and generation.
	if err := db.SaveAttemptProgress(ctx, fx.workspace, fx.runA, fx.session, 1, "settled", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}
	settled := NativePrompt{Disposition: "settled", Started: false, RunID: fx.runA, Generation: 1, Found: true}
	if prompt := readNativePrompt(t, db, fx); prompt != settled {
		t.Fatalf("same run settled NativePrompt = %+v, want %+v", prompt, settled)
	}
	writer = readWriter(t, db, fx.session)
	if !writer.current || writer.runID != fx.runA {
		t.Fatalf("after settled writer = %+v, want run A", writer)
	}

	// Both tasks stay RUNNING. No new outbox row is inserted.
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}
