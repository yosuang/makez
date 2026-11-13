package store

import (
	"errors"
	"testing"
)

// TestHasUnknownPromptSeesAnotherStartedOrAcceptedPrompt uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// A true result is not a claim that the prompt bytes were written, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestHasUnknownPromptSeesAnotherStartedOrAcceptedPrompt(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireRunningWithoutOutbox(t, db, fx)

	// An empty session id returns ErrInvalidInput and false.
	unknown, err := db.HasUnknownPrompt(ctx, fx.workspace, "", fx.hashA)
	if unknown || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty session HasUnknownPrompt = %v %v, want false and ErrInvalidInput", unknown, err)
	}

	// A hash that is not 64 hex characters returns ErrInvalidInput and false.
	unknown, err = db.HasUnknownPrompt(ctx, fx.workspace, fx.session, "not-a-sha256")
	if unknown || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid hash HasUnknownPrompt = %v %v, want false and ErrInvalidInput", unknown, err)
	}

	// A different prompt that is sending and not started does not count.
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	requireSendingPrompt(t, db, fx, false, 1)
	unknown, err = db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || unknown {
		t.Fatalf("unstarted sending row HasUnknownPrompt = %v %v, want false", unknown, err)
	}

	// After that row is marked started, the same hash is false and a different hash is true.
	if err = db.MarkPromptStarted(ctx, fx.workspace, fx.runA, fx.session, 1, fx.hashA); err != nil {
		t.Fatal(err)
	}
	requireSendingPrompt(t, db, fx, true, 1)
	unknown, err = db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashA)
	if err != nil || unknown {
		t.Fatalf("started prompt same hash HasUnknownPrompt = %v %v, want false", unknown, err)
	}
	unknown, err = db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || !unknown {
		t.Fatalf("started prompt different hash HasUnknownPrompt = %v %v, want true", unknown, err)
	}

	// A different prompt whose disposition is accepted counts.
	saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "")
	requireDisposition(t, db, fx, fx.hashA, "accepted")
	unknown, err = db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || !unknown {
		t.Fatalf("accepted prompt HasUnknownPrompt = %v %v, want true", unknown, err)
	}

	// A different prompt whose disposition is settled does not count.
	saveProgress(t, db, fx, fx.runA, 1, "settled", fx.hashA, "")
	requireDisposition(t, db, fx, fx.hashA, "settled")
	unknown, err = db.HasUnknownPrompt(ctx, fx.workspace, fx.session, fx.hashB)
	if err != nil || unknown {
		t.Fatalf("settled prompt HasUnknownPrompt = %v %v, want false", unknown, err)
	}

	// The task stays RUNNING. No new outbox row is inserted by these calls.
	requireRunningWithoutOutbox(t, db, fx)
}
