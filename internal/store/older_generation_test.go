package store

import "testing"

// TestOlderGenerationLeavesTheNewerSendingRow uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// The same run stays the writer. It records sending at generation 1, then
// ReopenUnsentPrompt for that same prompt at generation 2. The command row is
// sending, unstarted, generation 2, while the latest attempt is still
// generation 1. SaveAttemptProgress writes sending again at generation 1 with
// an empty attempt state. NativePrompt stays sending, unstarted, generation 2,
// on that run.
// That is not a second prompt write and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestOlderGenerationLeavesTheNewerSendingRow(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireBothTasksRunningWithoutOutbox(t, db, fx)

	// saveProgress of sending at generation 1 leaves disposition sending, started false,
	// and run A on the command row. The same run stays the writer. MarkPromptStarted is not called.
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	held := sendingPrompt(fx.runA, false, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("generation 1 sending NativePrompt = %+v, want %+v", prompt, held)
	}
	writer := readWriter(t, db, fx.session)
	if !writer.current || writer.runID != fx.runA {
		t.Fatalf("writer = %+v, want run A", writer)
	}

	// ReopenUnsentPrompt for that same prompt at generation 2 writes the command generation
	// directly and does not open an attempt. The command row is sending, unstarted, generation 2.
	// The latest attempt is still generation 1.
	if err := db.ReopenUnsentPrompt(ctx, fx.workspace, fx.runA, fx.session, 2, fx.hashA); err != nil {
		t.Fatal(err)
	}
	newer := sendingPrompt(fx.runA, false, 2)
	if prompt := readNativePrompt(t, db, fx); prompt != newer {
		t.Fatalf("reopened NativePrompt = %+v, want %+v", prompt, newer)
	}
	latest, err := db.LatestAttempt(ctx, fx.workspace, fx.runA)
	if err != nil || latest.Generation != 1 || latest.Step != "agent" || latest.State != "open" || latest.Disposition != "sending" || latest.SessionID != fx.session || latest.PromptSHA256 != fx.hashA {
		t.Fatalf("latest attempt = %+v %v, want generation 1", latest, err)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("commands = %d, want 1", n)
	}

	// SaveAttemptProgress writes sending again at generation 1 with an empty attempt state.
	// NativePrompt stays sending, unstarted, generation 2, on that run.
	if err = db.SaveAttemptProgress(ctx, fx.workspace, fx.runA, fx.session, 1, "sending", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}
	if prompt := readNativePrompt(t, db, fx); prompt != newer {
		t.Fatalf("older generation NativePrompt = %+v, want %+v", prompt, newer)
	}
	latest, err = db.LatestAttempt(ctx, fx.workspace, fx.runA)
	if err != nil || latest.Generation != 1 || latest.Step != "agent" || latest.State != "open" || latest.Disposition != "sending" || latest.SessionID != fx.session || latest.PromptSHA256 != fx.hashA {
		t.Fatalf("after sending latest attempt = %+v %v, want generation 1", latest, err)
	}
	writer = readWriter(t, db, fx.session)
	if !writer.current || writer.runID != fx.runA {
		t.Fatalf("after sending writer = %+v, want run A", writer)
	}
	if n := commandCount(t, db, fx.session); n != 1 {
		t.Fatalf("commands = %d, want 1", n)
	}

	// Both tasks stay RUNNING. No new outbox row is inserted.
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}
