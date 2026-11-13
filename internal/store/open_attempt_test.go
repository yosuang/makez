package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestOpenAttemptStopsAfterThreeGenerations uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Stopping at three generations is not a claim that an external action ran once,
// and it is not exactly-once for that action.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestOpenAttemptStopsAfterThreeGenerations(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	fx := seedPersonalSession(t, db)
	openGeneration(t, db, fx, fx.runA, fx.hashA)

	// While generation 1 is open, the same session and prompt hash return that attempt.
	// The attempt count stays 1.
	again, err := db.OpenAttempt(ctx, fx.workspace, fx.runA, fx.session, fx.hashA)
	if err != nil || again.Generation != 1 || again.Step != "agent" || again.State != "open" || again.Disposition != "none" || again.SessionID != fx.session || again.PromptSHA256 != fx.hashA {
		t.Fatalf("same prompt = %+v %v", again, err)
	}
	requireAttemptCount(t, db, fx, 1, 1, "open", fx.hashA)

	// An open attempt with a different session returns ErrConflict and leaves generation 1.
	other, err := db.EnsureNativeSession(ctx, fx.workspace, SessionIdentity{
		Scope: "personal", BotID: "other-bot-" + fx.workspace, ConversationID: "other-conversation-" + fx.workspace,
		HarnessID: "pi", HarnessPin: executedPin,
	})
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := db.OpenAttempt(ctx, fx.workspace, fx.runA, other.ID, fx.hashA)
	if !errors.Is(err, ErrConflict) || conflict != (Attempt{}) {
		t.Fatalf("different session = %+v %v, want ErrConflict", conflict, err)
	}
	requireAttemptCount(t, db, fx, 1, 1, "open", fx.hashA)

	// A different prompt hash returns ErrConflict. Generation stays 1. The attempt count stays 1.
	conflict, err = db.OpenAttempt(ctx, fx.workspace, fx.runA, fx.session, fx.hashB)
	if !errors.Is(err, ErrConflict) || conflict != (Attempt{}) {
		t.Fatalf("different prompt = %+v %v, want ErrConflict", conflict, err)
	}
	requireAttemptCount(t, db, fx, 1, 1, "open", fx.hashA)

	// A lost generation 1 opens generation 2. A lost generation 2 opens generation 3.
	saveProgress(t, db, fx, fx.runA, 1, "none", fx.hashA, "lost")
	second, err := db.OpenAttempt(ctx, fx.workspace, fx.runA, fx.session, fx.hashA)
	if err != nil || second.Generation != 2 || second.Step != "agent:2" || second.State != "open" || second.Disposition != "none" || second.SessionID != fx.session || second.PromptSHA256 != fx.hashA {
		t.Fatalf("generation 2 = %+v %v", second, err)
	}
	requireAttemptCount(t, db, fx, 2, 2, "open", fx.hashA)

	saveProgress(t, db, fx, fx.runA, 2, "none", fx.hashA, "lost")
	third, err := db.OpenAttempt(ctx, fx.workspace, fx.runA, fx.session, fx.hashA)
	if err != nil || third.Generation != 3 || third.Step != "agent:3" || third.State != "open" || third.Disposition != "none" || third.SessionID != fx.session || third.PromptSHA256 != fx.hashA {
		t.Fatalf("generation 3 = %+v %v", third, err)
	}
	requireAttemptCount(t, db, fx, 3, 3, "open", fx.hashA)

	// After generation 3 is lost, OpenAttempt returns ErrAttemptsExhausted.
	// The attempt count stays 3. There is no generation 4.
	// The task stays RUNNING. No outbox command is inserted.
	saveProgress(t, db, fx, fx.runA, 3, "none", fx.hashA, "lost")
	exhausted, err := db.OpenAttempt(ctx, fx.workspace, fx.runA, fx.session, fx.hashA)
	if err == nil || !errors.Is(err, ErrAttemptsExhausted) || !strings.Contains(err.Error(), "attempt limit reached") || exhausted != (Attempt{}) {
		t.Fatalf("generation 3 lost: OpenAttempt = %+v %v, want ErrAttemptsExhausted (attempt limit reached)", exhausted, err)
	}
	requireAttemptCount(t, db, fx, 3, 3, "lost", fx.hashA)
	requireNoGenerationFour(t, db, fx)
	requireRunningWithoutOutbox(t, db, fx)
}

type attemptRow struct {
	generation int64
	step       string
	state      string
	sessionID  string
	prompt     string
}

func requireAttemptCount(t *testing.T, db *Store, fx leasedSession, count int, generation int64, state, hash string) {
	t.Helper()
	rows := readAttempts(t, db, fx)
	if len(rows) != count {
		t.Fatalf("attempts = %d, want %d", len(rows), count)
	}
	for i, row := range rows {
		if row.generation != int64(i+1) || row.generation >= 4 {
			t.Fatalf("attempts = %+v", rows)
		}
	}
	latest := rows[len(rows)-1]
	if latest.generation != generation || latest.state != state || latest.prompt != hash || latest.sessionID != fx.session {
		t.Fatalf("generation %d = %+v, want %d %s %s", generation, latest, generation, state, hash)
	}
}

func requireNoGenerationFour(t *testing.T, db *Store, fx leasedSession) {
	t.Helper()
	var n int
	err := db.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM makez_attempts
		WHERE workspace_id = $1 AND run_id = $2 AND generation >= 4`, fx.workspace, fx.runA).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("generation 4 rows = %d", n)
	}
	rows := readAttempts(t, db, fx)
	if len(rows) != 3 || rows[0].generation != 1 || rows[0].state != "lost" || rows[1].generation != 2 || rows[1].state != "lost" || rows[2].generation != 3 || rows[2].state != "lost" {
		t.Fatalf("attempts = %+v, want generations 1, 2, and 3 lost", rows)
	}
}

func requireRunningWithoutOutbox(t *testing.T, db *Store, fx leasedSession) {
	t.Helper()
	var task, run string
	var outbox int
	err := db.pool.QueryRow(t.Context(), `
		SELECT tk.state, r.state,
			(SELECT count(*) FROM makez_outbox WHERE workspace_id = $1 OR task_id = tk.id OR run_id = r.id)
		FROM makez_runs r
		JOIN makez_tasks tk ON tk.id = r.task_id
		WHERE r.workspace_id = $1 AND r.id = $2`, fx.workspace, fx.runA).Scan(&task, &run, &outbox)
	if err != nil {
		t.Fatal(err)
	}
	if task != string(core.TaskRunning) || run != string(core.RunRunning) || outbox != 0 {
		t.Fatalf("task %s run %s outbox %d, want task RUNNING and no outbox command", task, run, outbox)
	}
}

func readAttempts(t *testing.T, db *Store, fx leasedSession) []attemptRow {
	t.Helper()
	rows, err := db.pool.Query(t.Context(), `
		SELECT generation, step, state, session_id, prompt_sha256
		FROM makez_attempts
		WHERE workspace_id = $1 AND run_id = $2
		ORDER BY generation`, fx.workspace, fx.runA)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []attemptRow
	for rows.Next() {
		var row attemptRow
		if err = rows.Scan(&row.generation, &row.step, &row.state, &row.sessionID, &row.prompt); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}
