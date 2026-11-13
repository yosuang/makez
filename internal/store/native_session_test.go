package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// These tests use Postgres row locks. They do not start Pi and they do not show that a Pi session file can be resumed.

const executedPin = "dd30101cfc5b0609411dd80b6f93adbc14db14cf"

type leasedSession struct {
	workspace string
	session   string
	runA      string
	runB      string
	hashA     string
	hashB     string
}

func TestNativeSessionSingleWriter(t *testing.T) {
	db := leaseDB(t)
	other := leaseDB(t)
	t.Run("ConcurrentAcquireWriteRelease", func(t *testing.T) {
		for range 8 {
			fx := seedPersonalSession(t, db)
			assertOneWriter(t, db, other, fx)
		}
	})
	t.Run("SettledDoesNotClearAccepted", func(t *testing.T) {
		fx := seedPersonalSession(t, db)
		openGeneration(t, db, fx, fx.runA, fx.hashA)
		saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "")
		err := other.SaveAttemptProgress(t.Context(), fx.workspace, fx.runB, fx.session, 1, "settled", fx.hashB, "")
		if !errors.Is(err, ErrStaleWriter) {
			t.Fatalf("write without the lease = %v", err)
		}
		requireDisposition(t, db, fx, fx.hashA, "accepted")
		if err = db.ReleaseSessionWriter(t.Context(), fx.workspace, fx.session, fx.runA); err != nil {
			t.Fatal(err)
		}
		requireDisposition(t, db, fx, fx.hashA, "accepted")
		openGeneration(t, other, fx, fx.runB, fx.hashB)
		saveProgress(t, other, fx, fx.runB, 1, "settled", fx.hashB, "")
		requireDisposition(t, db, fx, fx.hashA, "accepted")
		requireDisposition(t, db, fx, fx.hashB, "settled")
		var cachedDisposition, cachedHash string
		if err = db.pool.QueryRow(t.Context(), `SELECT last_disposition, last_prompt_sha256 FROM makez_native_sessions WHERE id = $1`, fx.session).Scan(&cachedDisposition, &cachedHash); err != nil {
			t.Fatal(err)
		}
		if cachedDisposition != "settled" || cachedHash != fx.hashB {
			t.Fatalf("cache = %s %s", cachedDisposition, cachedHash)
		}
		if err = other.SaveAttemptProgress(t.Context(), fx.workspace, fx.runB, fx.session, 1, "settled", fx.hashA, ""); err != nil {
			t.Fatal(err)
		}
		requireDisposition(t, db, fx, fx.hashA, "accepted")
		if commandCount(t, db, fx.session) != 2 {
			t.Fatalf("commands = %d", commandCount(t, db, fx.session))
		}
	})
	t.Run("StaleAttempt", func(t *testing.T) {
		fx := seedPersonalSession(t, db)
		openGeneration(t, db, fx, fx.runA, fx.hashA)
		saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "")
		saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "lost")
		next, err := db.OpenAttempt(t.Context(), fx.workspace, fx.runA, fx.session, fx.hashA)
		if err != nil {
			t.Fatal(err)
		}
		if next.Generation != 2 {
			t.Fatalf("generation = %d", next.Generation)
		}
		err = db.SaveAttemptProgress(t.Context(), fx.workspace, fx.runA, fx.session, 1, "settled", fx.hashA, "closed")
		if !errors.Is(err, ErrStaleWriter) {
			t.Fatalf("stale generation = %v", err)
		}
		requireDisposition(t, db, fx, fx.hashA, "accepted")
		if got := commandGeneration(t, db, fx.session, fx.hashA); got != 1 {
			t.Fatalf("command generation = %d", got)
		}
		err = db.SaveAttemptProgress(t.Context(), fx.workspace, fx.runA, fx.session, 1, "settled", fx.hashA, "closed")
		if !errors.Is(err, ErrStaleWriter) {
			t.Fatalf("repeated stale generation = %v", err)
		}
		requireDisposition(t, db, fx, fx.hashA, "accepted")
	})
	t.Run("CrashRestart", func(t *testing.T) {
		fx := seedPersonalSession(t, db)
		openGeneration(t, db, fx, fx.runA, fx.hashA)
		saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "")
		if err := db.AcquireSessionWriter(t.Context(), fx.workspace, fx.session, fx.runA); err != nil {
			t.Fatal(err)
		}
		held := readWriter(t, db, fx.session)
		if held.runID != fx.runA || !held.current || held.fence != 1 {
			t.Fatalf("writer = %+v", held)
		}
		expireLease(t, db, fx.session)
		if readWriter(t, db, fx.session).current {
			t.Fatal("expired lease still current")
		}
		openGeneration(t, other, fx, fx.runB, fx.hashB)
		restarted := readWriter(t, db, fx.session)
		if restarted.runID != fx.runB || !restarted.current || restarted.fence != 2 {
			t.Fatalf("writer after expiry = %+v", restarted)
		}
		err := db.SaveAttemptProgress(t.Context(), fx.workspace, fx.runA, fx.session, 1, "settled", fx.hashA, "closed")
		if !errors.Is(err, ErrStaleWriter) {
			t.Fatalf("old writer = %v", err)
		}
		requireDisposition(t, db, fx, fx.hashA, "accepted")
		if err = db.ReleaseSessionWriter(t.Context(), fx.workspace, fx.session, fx.runA); err != nil {
			t.Fatal(err)
		}
		if got := readWriter(t, db, fx.session); got.runID != fx.runB || !got.current {
			t.Fatalf("release cleared the new writer: %+v", got)
		}
		checkpoint := []byte("{}\n")
		if err = db.SaveCheckpoint(t.Context(), fx.workspace, fx.session, fx.runA, 1, checkpoint); !errors.Is(err, ErrStaleWriter) {
			t.Fatalf("stale checkpoint = %v", err)
		}
		if err = other.SaveCheckpoint(t.Context(), fx.workspace, fx.session, fx.runB, 1, checkpoint); err != nil {
			t.Fatal(err)
		}
		loaded, err := db.LoadCheckpoint(t.Context(), fx.workspace, fx.session)
		if err != nil {
			t.Fatal(err)
		}
		if string(loaded) != string(checkpoint) {
			t.Fatalf("checkpoint = %q", loaded)
		}
		saveProgress(t, other, fx, fx.runB, 1, "settled", fx.hashB, "")
		requireDisposition(t, db, fx, fx.hashA, "accepted")
		requireDisposition(t, db, fx, fx.hashB, "settled")
	})
	t.Run("RepeatedObservation", func(t *testing.T) {
		fx := seedPersonalSession(t, db)
		openGeneration(t, db, fx, fx.runA, fx.hashA)
		saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "")
		saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "")
		if commandCount(t, db, fx.session) != 1 {
			t.Fatalf("commands = %d", commandCount(t, db, fx.session))
		}
		saveProgress(t, db, fx, fx.runA, 1, "settled", fx.hashA, "")
		saveProgress(t, db, fx, fx.runA, 1, "settled", fx.hashA, "")
		requireDisposition(t, db, fx, fx.hashA, "settled")
		if commandCount(t, db, fx.session) != 1 || commandGeneration(t, db, fx.session, fx.hashA) != 1 {
			t.Fatalf("commands = %d generation = %d", commandCount(t, db, fx.session), commandGeneration(t, db, fx.session, fx.hashA))
		}
		if err := db.SaveAttemptProgress(t.Context(), fx.workspace, fx.runA, fx.session, 1, "sending", fx.hashA, ""); err != nil {
			t.Fatal(err)
		}
		requireDisposition(t, db, fx, fx.hashA, "settled")
		if err := db.ReleaseSessionWriter(t.Context(), fx.workspace, fx.session, fx.runA); err != nil {
			t.Fatal(err)
		}
		openGeneration(t, other, fx, fx.runB, fx.hashA)
		saveProgress(t, other, fx, fx.runB, 1, "sending", fx.hashA, "")
		requireDisposition(t, db, fx, fx.hashA, "sending")
		if commandCount(t, db, fx.session) != 1 {
			t.Fatalf("commands = %d", commandCount(t, db, fx.session))
		}
	})
	t.Run("StaleAttemptDoesNotAdvanceCheckpoint", func(t *testing.T) {
		fx := seedPersonalSession(t, db)
		openGeneration(t, db, fx, fx.runA, fx.hashA)
		if err := db.SaveCheckpoint(t.Context(), fx.workspace, fx.session, fx.runA, 1, []byte("checkpoint-v1\n")); err != nil {
			t.Fatal(err)
		}
		saveProgress(t, db, fx, fx.runA, 1, "accepted", fx.hashA, "lost")
		next, err := db.OpenAttempt(t.Context(), fx.workspace, fx.runA, fx.session, fx.hashA)
		if err != nil {
			t.Fatal(err)
		}
		if next.Generation != 2 {
			t.Fatalf("generation = %d", next.Generation)
		}
		second := []byte("checkpoint-v2\n")
		if err = db.SaveCheckpoint(t.Context(), fx.workspace, fx.session, fx.runA, 2, second); err != nil {
			t.Fatal(err)
		}
		if err = db.SaveCheckpoint(t.Context(), fx.workspace, fx.session, fx.runA, 1, []byte("checkpoint-stale\n")); !errors.Is(err, ErrStaleWriter) {
			t.Fatalf("stale attempt checkpoint = %v", err)
		}
		loaded, err := db.LoadCheckpoint(t.Context(), fx.workspace, fx.session)
		if err != nil {
			t.Fatal(err)
		}
		if string(loaded) != string(second) {
			t.Fatalf("checkpoint = %q", loaded)
		}
		if commandCount(t, db, fx.session) != 1 {
			t.Fatalf("commands = %d", commandCount(t, db, fx.session))
		}
	})
	t.Run("WorkerSessionUsesTheSameLease", func(t *testing.T) {
		fx := seedPersonalSession(t, db)
		session, err := db.EnsureNativeSession(t.Context(), fx.workspace, SessionIdentity{
			Scope: "worker", DelegationRunID: "delegation-" + newID(), HarnessID: "pi", HarnessPin: executedPin,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = db.AcquireSessionWriter(t.Context(), fx.workspace, session.ID, fx.runA); err != nil {
			t.Fatal(err)
		}
		err = other.AcquireSessionWriter(t.Context(), fx.workspace, session.ID, fx.runB)
		if !errors.Is(err, ErrSessionBusy) {
			t.Fatalf("second worker = %v", err)
		}
		if err = db.ReleaseSessionWriter(t.Context(), fx.workspace, session.ID, fx.runA); err != nil {
			t.Fatal(err)
		}
		if err = other.AcquireSessionWriter(t.Context(), fx.workspace, session.ID, fx.runB); err != nil {
			t.Fatal(err)
		}
	})
}

func assertOneWriter(t *testing.T, left, right *Store, fx leasedSession) {
	t.Helper()
	ctx := t.Context()
	var leftErr, rightErr error
	started := make(chan struct{})
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-started
		leftErr = left.AcquireSessionWriter(ctx, fx.workspace, fx.session, fx.runA)
	}()
	go func() {
		defer group.Done()
		<-started
		rightErr = right.AcquireSessionWriter(ctx, fx.workspace, fx.session, fx.runB)
	}()
	close(started)
	group.Wait()
	if !exactlyOneBusy(leftErr, rightErr) {
		t.Fatalf("acquire = %v and %v", leftErr, rightErr)
	}
	holder, waiter := left, right
	holdRun, waitRun := fx.runA, fx.runB
	if errors.Is(leftErr, ErrSessionBusy) {
		holder, waiter = right, left
		holdRun, waitRun = fx.runB, fx.runA
	}
	if _, err := holder.OpenAttempt(ctx, fx.workspace, holdRun, fx.session, fx.hashA); err != nil {
		t.Fatal(err)
	}
	if err := holder.SaveAttemptProgress(ctx, fx.workspace, holdRun, fx.session, 1, "accepted", fx.hashA, ""); err != nil {
		t.Fatal(err)
	}
	err := waiter.SaveAttemptProgress(ctx, fx.workspace, waitRun, fx.session, 1, "settled", fx.hashB, "")
	if !errors.Is(err, ErrStaleWriter) {
		t.Fatalf("waiter write = %v", err)
	}
	requireDisposition(t, left, fx, fx.hashA, "accepted")
	if _, err = waiter.LatestAttempt(ctx, fx.workspace, waitRun); !errors.Is(err, ErrNotFound) {
		t.Fatalf("waiter attempt = %v", err)
	}
	if err = holder.ReleaseSessionWriter(ctx, fx.workspace, fx.session, holdRun); err != nil {
		t.Fatal(err)
	}
	requireDisposition(t, left, fx, fx.hashA, "accepted")
	if err = waiter.AcquireSessionWriter(ctx, fx.workspace, fx.session, waitRun); err != nil {
		t.Fatal(err)
	}
	if readWriter(t, left, fx.session).runID != waitRun {
		t.Fatal("lease did not move after release")
	}
}

func exactlyOneBusy(left, right error) bool {
	leftBusy := errors.Is(left, ErrSessionBusy)
	rightBusy := errors.Is(right, ErrSessionBusy)
	if leftBusy == rightBusy {
		return false
	}
	return (left == nil || leftBusy) && (right == nil || rightBusy)
}

var (
	leaseMigrateOnce sync.Once
	leaseMigrateErr  error
)

func leaseDB(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("MAKEZ_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MAKEZ_TEST_DATABASE_URL is not set")
	}
	db, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	leaseMigrateOnce.Do(func() {
		leaseMigrateErr = db.Migrate(t.Context())
	})
	if leaseMigrateErr != nil {
		t.Fatal(leaseMigrateErr)
	}
	return db
}

func seedPersonalSession(t *testing.T, db *Store) leasedSession {
	t.Helper()
	ctx := t.Context()
	fx := leasedSession{
		workspace: newID(),
		runA:      newID(),
		runB:      newID(),
		hashA:     promptDigest("alpha"),
		hashB:     promptDigest("beta"),
	}
	taskA, taskB := newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, fx.workspace, "lease-"+fx.workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, fx.workspace, taskA, fx.runA, "alpha")
	insertRun(t, db, fx.workspace, taskB, fx.runB, "beta")
	session, err := db.EnsureNativeSession(ctx, fx.workspace, SessionIdentity{
		Scope: "personal", BotID: "bot-" + fx.workspace, ConversationID: "conversation-" + fx.workspace,
		HarnessID: "pi", HarnessPin: executedPin,
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.session = session.ID
	return fx
}

func insertRun(t *testing.T, db *Store, workspace, taskID, runID, prompt string) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO makez_tasks (
			id, workspace_id, idempotency_key, request_hash, prompt, agent_id, agent_version,
			profile_id, profile_version, state, deadline
		) VALUES ($1, $2, $3, $4, $5, 'pi', '1', 'default', '1', 'RUNNING', $6)`,
		taskID, workspace, "key-"+taskID, promptDigest(prompt), prompt, time.Now().Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO makez_runs (id, task_id, state, backend_id, workspace_id)
		VALUES ($1, $2, 'RUNNING', 'test', $3)`, runID, taskID, workspace); err != nil {
		t.Fatal(err)
	}
}

func openGeneration(t *testing.T, db *Store, fx leasedSession, runID, hash string) {
	t.Helper()
	attempt, err := db.OpenAttempt(t.Context(), fx.workspace, runID, fx.session, hash)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Generation != 1 || attempt.State != "open" {
		t.Fatalf("attempt = %+v", attempt)
	}
}

func saveProgress(t *testing.T, db *Store, fx leasedSession, runID string, generation int64, disposition, hash, state string) {
	t.Helper()
	if err := db.SaveAttemptProgress(t.Context(), fx.workspace, runID, fx.session, generation, disposition, hash, state); err != nil {
		t.Fatal(err)
	}
}

func requireDisposition(t *testing.T, db *Store, fx leasedSession, hash, want string) {
	t.Helper()
	got, err := db.PromptDisposition(t.Context(), fx.workspace, fx.session, hash)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("disposition = %q, want %q", got, want)
	}
}

func commandCount(t *testing.T, db *Store, sessionID string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_native_commands WHERE session_id = $1`, sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func commandGeneration(t *testing.T, db *Store, sessionID, hash string) int64 {
	t.Helper()
	var n int64
	if err := db.pool.QueryRow(t.Context(), `SELECT generation FROM makez_native_commands WHERE session_id = $1 AND prompt_sha256 = $2`, sessionID, hash).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type writerState struct {
	runID   string
	fence   int64
	current bool
}

func readWriter(t *testing.T, db *Store, sessionID string) writerState {
	t.Helper()
	var runID *string
	var state writerState
	if err := db.pool.QueryRow(t.Context(), `SELECT writer_run_id, writer_fence, (writer_until IS NOT NULL AND writer_until > now()) FROM makez_native_sessions WHERE id = $1`, sessionID).Scan(&runID, &state.fence, &state.current); err != nil {
		t.Fatal(err)
	}
	if runID != nil {
		state.runID = *runID
	}
	return state
}

func expireLease(t *testing.T, db *Store, sessionID string) {
	t.Helper()
	tag, err := db.pool.Exec(t.Context(), `UPDATE makez_native_sessions SET writer_until = now() - interval '1 second' WHERE id = $1`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expired %d rows", tag.RowsAffected())
	}
}

func promptDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
