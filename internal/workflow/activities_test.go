package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
	"github.com/stretchr/testify/mock"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/testsuite"
)

func activityStore(t *testing.T) *store.Store {
	t.Helper()
	databaseURL := os.Getenv("MAKEZ_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MAKEZ_TEST_DATABASE_URL to run PostgreSQL activity integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	schema := "makez_activity_test_" + hex.EncodeToString(random[:])
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	// A private schema prevents a concurrently running development dispatcher from consuming test outbox rows.
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("remove isolated test schema: %v", err)
		}
		admin.Close(ctx)
	})
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

func activityTask(t *testing.T, db *store.Store, initialize bool) store.Task {
	t.Helper()
	task, _, err := db.Submit(context.Background(), "activity-test", "task", store.SubmitInput{Prompt: "test", AgentID: "demo", AgentVersion: "1", ProfileID: "local", ProfileVersion: "1", TimeoutSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if initialize {
		task, err = db.InitializeRun(context.Background(), task.WorkspaceID, task.ID, "test-backend")
		if err != nil {
			t.Fatal(err)
		}
	}
	return task
}

type finalizerProvider struct {
	core.SandboxProvider
	deleteErr error
	stopErr   error
	deleted   []core.OperationKey
	stopped   int
}

func (p *finalizerProvider) Delete(_ context.Context, key core.OperationKey) error {
	p.deleted = append(p.deleted, key)
	return p.deleteErr
}
func (p *finalizerProvider) Stop(context.Context, core.SandboxRef, core.OperationKey, core.StopPolicy) error {
	p.stopped++
	return p.stopErr
}

type resultWorkspace string

func (w resultWorkspace) Read(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(w))), nil
}

func executeFinalize(t *testing.T, activities *Activities, task store.Task, reason string) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.Finalize)
	_, err := env.ExecuteActivity(activities.Finalize, FinishInput{Input: Input{task.WorkspaceID, task.ID}, Reason: reason})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
}

func TestFinalizePublishesStagedResultAfterSandboxDeletion(t *testing.T) {
	for _, scenario := range []string{"success", "late cancellation", "cleanup unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			db := activityStore(t)
			task := activityTask(t, db, true)
			files, err := artifact.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { files.Close() })
			records, err := files.Archive(context.Background(), task.Run.ID, resultWorkspace("durable result"), []core.ArtifactCandidate{{Path: "report.txt", Required: true}})
			if err != nil {
				t.Fatal(err)
			}
			sandbox := core.SandboxRef{BackendID: "test-backend", ID: "already-deleted", Key: key(task, "sandbox")}
			if err = db.UpdateRun(context.Background(), task.Run.ID, store.RunUpdate{State: ptr(core.RunStopping), Sandbox: &sandbox}); err != nil {
				t.Fatal(err)
			}
			record := records[0]
			prepared := store.PreparedResult{State: core.TaskSucceeded, Artifacts: []store.Artifact{{RunID: task.Run.ID, Path: "report.txt", StorageKey: record.StorageKey, SHA256: record.SHA256, Size: record.Size, MediaType: "text/plain"}}}
			if err = db.UpdateRun(context.Background(), task.Run.ID, store.RunUpdate{State: ptr(core.RunFinalizing), VerifiedStopped: ptr(true), Exit: &core.ProcessExit{Code: 0, FinishedAt: time.Now()}, Result: &prepared}); err != nil {
				t.Fatal(err)
			}
			provider := &finalizerProvider{}
			if scenario == "late cancellation" {
				if _, err = db.Cancel(context.Background(), task.WorkspaceID, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cleanup unavailable" {
				provider.deleteErr = errors.New("backend unavailable")
			}
			// No adapters or file-reading methods exist: the staged manifest must be sufficient after sandbox deletion.
			executeFinalize(t, &Activities{DB: db, Provider: provider, Artifacts: files}, task, "WORKFLOW_INTERRUPTED")
			got, err := db.Get(context.Background(), task.WorkspaceID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := core.TaskSucceeded
			if scenario == "late cancellation" {
				want = core.TaskCanceled
			}
			if got.State != want || !got.Run.VerifiedStopped {
				t.Fatalf("state=%s stopped=%v", got.State, got.Run.VerifiedStopped)
			}
			wantCleanup := core.CleanupDone
			if scenario == "cleanup unavailable" {
				wantCleanup = core.CleanupPending
			}
			if got.Run.Cleanup != wantCleanup {
				t.Fatalf("cleanup=%s, want %s", got.Run.Cleanup, wantCleanup)
			}
			if provider.stopped != 0 || len(provider.deleted) != 1 || provider.deleted[0] != key(task, "sandbox") {
				t.Fatalf("unexpected sandbox operations: %#v", provider)
			}
			published, err := db.Artifacts(context.Background(), task.WorkspaceID, task.ID)
			if err != nil || len(published) != 1 || published[0].StorageKey != record.StorageKey {
				t.Fatalf("staged artifact was lost: %#v, %v", published, err)
			}
			file, err := files.Open(published[0].StorageKey)
			if err != nil {
				t.Fatal(err)
			}
			contents, err := io.ReadAll(file)
			file.Close()
			if err != nil || string(contents) != "durable result" {
				t.Fatalf("artifact content lost: %q %v", contents, err)
			}
			if scenario == "cleanup unavailable" {
				pending, err := db.Pending(context.Background(), 20)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range pending {
					if row.TaskID == task.ID && row.Command == "START_CLEANUP" {
						found = true
					}
				}
				if !found {
					t.Fatal("pending cleanup has no durable outbox handoff")
				}
			}
		})
	}
}

func TestFinalizeCannotSucceedWhenStopIsUnconfirmed(t *testing.T) {
	db := activityStore(t)
	task := activityTask(t, db, true)
	sandbox := core.SandboxRef{BackendID: "test-backend", ID: "uncertain", Key: key(task, "sandbox")}
	if err := db.UpdateRun(context.Background(), task.Run.ID, store.RunUpdate{Sandbox: &sandbox}); err != nil {
		t.Fatal(err)
	}
	provider := &finalizerProvider{stopErr: errors.New("stop response lost"), deleteErr: errors.New("delete unavailable")}
	executeFinalize(t, &Activities{DB: db, Provider: provider}, task, "")
	got, err := db.Get(context.Background(), task.WorkspaceID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.TaskFailed || got.Reason != "STOP_UNCONFIRMED" || got.Run.VerifiedStopped || got.Run.Cleanup != core.CleanupPending {
		t.Fatalf("unsafe finalization: %#v", got)
	}
}

func TestFinalizeFailedInitializationWithoutRun(t *testing.T) {
	db := activityStore(t)
	task := activityTask(t, db, false)
	executeFinalize(t, &Activities{DB: db}, task, "EXECUTION_FAILED")
	got, err := db.Get(context.Background(), task.WorkspaceID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.TaskFailed || got.Reason != "INITIALIZE_FAILED" || got.Run != nil {
		t.Fatalf("initialization failure stuck: %#v", got)
	}
}

func TestUnknownCreateRemainsPendingBeforeResourceDeadline(t *testing.T) {
	db := activityStore(t)
	task := activityTask(t, db, true)
	if _, claimed, err := db.BeginOperation(context.Background(), task.Run.ID, "sandbox", "create-request"); err != nil || !claimed {
		t.Fatalf("prepare uncertain create: claimed=%v err=%v", claimed, err)
	}
	// A create response can be lost before the resource becomes visible to Delete.
	provider := &finalizerProvider{}
	activities := &Activities{DB: db, Provider: provider}
	executeFinalize(t, activities, task, "SANDBOX_OUTCOME_UNKNOWN")
	got, err := db.Get(context.Background(), task.WorkspaceID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.TaskFailed || !got.Run.VerifiedStopped || got.Run.Cleanup != core.CleanupPending {
		t.Fatalf("invisible uncertain create was prematurely considered clean: state=%s cleanup=%s stopped=%v", got.State, got.Run.Cleanup, got.Run.VerifiedStopped)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.Reap)
	if _, err = env.ExecuteActivity(activities.Reap, Input{task.WorkspaceID, task.ID}); err == nil {
		t.Fatal("cleanup claimed completion before a late create could be ruled out")
	}
	got, err = db.Get(context.Background(), task.WorkspaceID, task.ID)
	if err != nil || got.Run.Cleanup != core.CleanupPending {
		t.Fatalf("cleanup state changed prematurely: %#v %v", got.Run, err)
	}
}

func TestStartOnlyAllowsFailedRecoveryWorkflowsToRunAgain(t *testing.T) {
	for _, test := range []struct {
		id     string
		policy enums.WorkflowIdReusePolicy
	}{
		{ID("workspace", "task"), enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE},
		{CleanupID("workspace", "task"), enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY},
		{"makez/recover/workspace/task", enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY},
	} {
		t.Run(test.id, func(t *testing.T) {
			remote := new(mocks.Client)
			remote.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(options client.StartWorkflowOptions) bool {
				return options.ID == test.id && options.WorkflowIDReusePolicy == test.policy && options.WorkflowExecutionErrorWhenAlreadyStarted
			}), mock.Anything, testInput).Return(nil, nil).Once()
			if err := start(context.Background(), remote, test.id, Task, testInput); err != nil {
				t.Fatal(err)
			}
			remote.AssertExpectations(t)
		})
	}
}

func TestReconcileResubmitsCleanupForTerminalPendingTask(t *testing.T) {
	db := activityStore(t)
	task := activityTask(t, db, true)
	for _, state := range []core.RunState{core.RunStopping, core.RunFinalizing} {
		if err := db.UpdateRun(context.Background(), task.Run.ID, store.RunUpdate{State: &state}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Publish(context.Background(), task.WorkspaceID, task.ID, core.TaskSucceeded, "", true, core.CleanupPending, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Pending(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err = db.Delivered(context.Background(), row.ID); err != nil {
			t.Fatal(err)
		}
	}
	remote := new(mocks.Client)
	remote.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(options client.StartWorkflowOptions) bool {
		return options.ID == CleanupID(task.WorkspaceID, task.ID) && options.WorkflowIDReusePolicy == enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY
	}), mock.Anything, Input{task.WorkspaceID, task.ID}).Return(nil, nil).Once()
	reconcile(context.Background(), db, remote, store.RecoveryCursor{})
	remote.AssertExpectations(t)
}
