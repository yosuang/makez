package store

import (
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestUnstartedSendingMovesToTheCurrentRun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// An unstarted sending row moves to the current run. The prompt has not reached Pi.
// Moving the row does not write the prompt bytes, and it is not exactly-once.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestUnstartedSendingMovesToTheCurrentRun(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)
	requireBothTasksRunningWithoutOutbox(t, db, fx)

	// Run A saveProgress of sending leaves disposition sending, started false, and run A
	// on the command row. MarkPromptStarted is not called.
	saveProgress(t, db, fx, fx.runA, 1, "sending", fx.hashA, "")
	held := sendingPrompt(fx.runA, false, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("unstarted sending NativePrompt = %+v, want %+v", prompt, held)
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
	// empty attempt state, returns nil. NativePrompt is disposition sending, started
	// false, run B, generation 1.
	openGeneration(t, db, fx, fx.runB, fx.hashA)
	if prompt := readNativePrompt(t, db, fx); prompt != held {
		t.Fatalf("OpenAttempt NativePrompt = %+v, want %+v", prompt, held)
	}
	if err := db.SaveAttemptProgress(ctx, fx.workspace, fx.runB, fx.session, 1, "sending", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}
	moved := sendingPrompt(fx.runB, false, 1)
	if prompt := readNativePrompt(t, db, fx); prompt != moved {
		t.Fatalf("current run sending NativePrompt = %+v, want %+v", prompt, moved)
	}

	// Both tasks stay RUNNING. No new outbox row is inserted.
	requireBothTasksRunningWithoutOutbox(t, db, fx)
}

func requireBothTasksRunningWithoutOutbox(t *testing.T, db *Store, fx leasedSession) {
	t.Helper()
	requireRunningWithoutOutbox(t, db, fx)
	var task, run string
	err := db.pool.QueryRow(t.Context(), `
		SELECT tk.state, r.state
		FROM makez_runs r
		JOIN makez_tasks tk ON tk.id = r.task_id
		WHERE r.workspace_id = $1 AND r.id = $2`, fx.workspace, fx.runB).Scan(&task, &run)
	if err != nil {
		t.Fatal(err)
	}
	if task != string(core.TaskRunning) || run != string(core.RunRunning) {
		t.Fatalf("run B task %s run %s, want RUNNING", task, run)
	}
}
