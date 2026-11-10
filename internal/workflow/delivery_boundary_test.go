package workflow

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
	"github.com/makez-dev/makez/internal/store"
)

// TestDeliveryBoundaryInjection fails the launch at the commit, release file,
// pre-prompt flush, and ack boundaries. The sandbox is fake. This does not start Pi.
func TestDeliveryBoundaryInjection(t *testing.T) {
	url := testingDB(t)
	ctx := t.Context()
	db := openTestDB(t, url)
	t.Run("InactiveSuccessorSendsUnreleasedPrompt", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		gate.releaseBlocks = 1
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); !errors.Is(err, errReleaseBlocked) {
			t.Fatalf("first launch = %v", err)
		}
		failRun(t, url, fx.runA)
		session := boundarySession(t, db, fx)
		expireWriter(t, url, session.ID)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		if len(gate.releases) != 1 || gate.releases[0] != "sb-"+fx.runB {
			t.Fatalf("releases = %v", gate.releases)
		}
		prompt := requirePrompt(t, db, fx)
		if !prompt.Started || prompt.RunID != fx.runB {
			t.Fatalf("prompt = %+v", prompt)
		}
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		if len(gate.releases) != 1 {
			t.Fatalf("prompt released again: %v", gate.releases)
		}
	})
	t.Run("ActiveSuccessorDoesNotSend", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		gate.releaseBlocks = 1
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); !errors.Is(err, errReleaseBlocked) {
			t.Fatalf("first launch = %v", err)
		}
		session := boundarySession(t, db, fx)
		expireWriter(t, url, session.ID)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		if len(gate.releases) != 0 || gate.modes[len(gate.modes)-1] != "reconcile_only" {
			t.Fatalf("modes %v releases %v", gate.modes, gate.releases)
		}
		prompt := requirePrompt(t, db, fx)
		if !prompt.Started || prompt.RunID != fx.runA {
			t.Fatalf("prompt = %+v", prompt)
		}
	})
	t.Run("EmptyReleaseFileIsNotSentAgain", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		gate.files["sb-"+fx.runA+"\x00"+piharness.ReleasePath] = []byte{}
		failRun(t, url, fx.runA)
		session := boundarySession(t, db, fx)
		expireWriter(t, url, session.ID)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		if len(gate.releases) != 1 || gate.releases[0] != "sb-"+fx.runA || gate.modes[len(gate.modes)-1] != "reconcile_only" {
			t.Fatalf("modes %v releases %v", gate.modes, gate.releases)
		}
	})
	t.Run("DeadAfterReleaseBeforeFlushIsSentOnce", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		exit := core.ProcessExit{Code: 1}
		if err := db.UpdateRun(ctx, fx.runA, store.RunUpdate{Exit: &exit}); err != nil {
			t.Fatal(err)
		}
		failRun(t, url, fx.runA)
		session := boundarySession(t, db, fx)
		expireWriter(t, url, session.ID)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		if len(gate.releases) != 2 || gate.releases[1] != "sb-"+fx.runB {
			t.Fatalf("releases = %v", gate.releases)
		}
		prompt := requirePrompt(t, db, fx)
		if !prompt.Started || prompt.RunID != fx.runB {
			t.Fatalf("prompt = %+v", prompt)
		}
	})
	t.Run("ReleaseFileIsNotSentAgain", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		failRun(t, url, fx.runA)
		session := boundarySession(t, db, fx)
		expireWriter(t, url, session.ID)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		if len(gate.releases) != 1 || gate.releases[0] != "sb-"+fx.runA || gate.modes[len(gate.modes)-1] != "reconcile_only" {
			t.Fatalf("modes %v releases %v", gate.modes, gate.releases)
		}
	})
	t.Run("SendingFlushIsNotSentAgain", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]string{"disposition": "sending", "prompt_sha256": piharness.PromptHash(fx.prompt)})
		if err != nil {
			t.Fatal(err)
		}
		gate.files["sb-"+fx.runA+"\x00"+piharness.DispositionPath] = body
		failRun(t, url, fx.runA)
		session := boundarySession(t, db, fx)
		expireWriter(t, url, session.ID)
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		if gate.modes[len(gate.modes)-1] != "reconcile_only" || len(gate.releases) != 1 {
			t.Fatalf("modes %v releases %v", gate.modes, gate.releases)
		}
	})
	t.Run("UnreadableSandboxIsNotSent", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		gate.unreadable = "sb-" + fx.runA
		failRun(t, url, fx.runA)
		session := boundarySession(t, db, fx)
		expireWriter(t, url, session.ID)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		prompt := requirePrompt(t, db, fx)
		if prompt.Disposition != string(piharness.DispositionSending) || prompt.RunID != fx.runA || gate.modes[len(gate.modes)-1] != "reconcile_only" {
			t.Fatalf("modes %v prompt %+v", gate.modes, prompt)
		}
	})
	t.Run("OwnerResumeWritesOneRelease", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		gate.releaseBlocks = 1
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); !errors.Is(err, errReleaseBlocked) {
			t.Fatalf("first launch = %v", err)
		}
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		if len(gate.modes) != 1 || gate.modes[0] != "send" || len(gate.releases) != 1 || gate.releases[0] != "sb-"+fx.runA {
			t.Fatalf("modes %v releases %v", gate.modes, gate.releases)
		}
	})
	t.Run("DeadProcessBeforeFlushRelaunches", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		gate.releaseBlocks = 1
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); !errors.Is(err, errReleaseBlocked) {
			t.Fatalf("first launch = %v", err)
		}
		exit := core.ProcessExit{Code: 1}
		if err := db.UpdateRun(ctx, fx.runA, store.RunUpdate{Exit: &exit}); err != nil {
			t.Fatal(err)
		}
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		prompt := requirePrompt(t, db, fx)
		if prompt.Generation != 2 || !prompt.Started || len(gate.releases) != 1 {
			t.Fatalf("prompt %+v releases %v modes %v", prompt, gate.releases, gate.modes)
		}
	})
	t.Run("StaleOwnerDoesNotRelease", func(t *testing.T) {
		fx, gate, activities := boundaryLaunch(t, db, url)
		gate.releaseBlocks = 1
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); !errors.Is(err, errReleaseBlocked) {
			t.Fatalf("first launch = %v", err)
		}
		failRun(t, url, fx.runA)
		session := boundarySession(t, db, fx)
		expireWriter(t, url, session.ID)
		if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		err := activities.launchPi(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA})
		if !errors.Is(err, store.ErrSessionBusy) {
			t.Fatalf("stale owner = %v releases %v", err, gate.releases)
		}
		expireWriter(t, url, session.ID)
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		if len(gate.releases) != 1 || gate.releases[0] != "sb-"+fx.runB {
			t.Fatalf("releases = %v", gate.releases)
		}
	})
}

func testingDB(t *testing.T) string {
	t.Helper()
	url := os.Getenv("MAKEZ_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MAKEZ_TEST_DATABASE_URL is not set")
	}
	return url
}

func openTestDB(t *testing.T, url string) *store.Store {
	t.Helper()
	db, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}

func boundaryLaunch(t *testing.T, db *store.Store, url string) (launchFixture, *sendingGate, *Activities) {
	t.Helper()
	fx := seedPiLaunch(t, db, url)
	gate := &sendingGate{t: t, db: db, fx: fx, remember: true, files: map[string][]byte{}}
	return fx, gate, newPiActivities(t, db, gate)
}

func boundarySession(t *testing.T, db *store.Store, fx launchFixture) store.NativeSession {
	t.Helper()
	session, err := db.EnsureNativeSession(t.Context(), fx.workspace, fx.identity())
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func failRun(t *testing.T, url, runID string) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx) //nolint:errcheck // The update is checked before close.
	tag, err := conn.Exec(ctx, `UPDATE makez_runs SET state = 'FAILED' WHERE id = $1`, runID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("failed %d rows", tag.RowsAffected())
	}
}
