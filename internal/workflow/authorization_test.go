package workflow

import (
	"errors"
	"testing"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func revokeTaskMachine(t *testing.T, a *Activities) {
	t.Helper()
	p, workspace, err := a.DB.AuthenticateMachine(t.Context(), "activity-test-machine-key")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.DB.RevokeMachine(t.Context(), workspace, p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRevocationStopsSecretsAndNewLaunch(t *testing.T) {
	t.Setenv("MAKEZ_WORKFLOW_TEST_AUTH", "isolated-test-value")
	a, task, provider, resolver := secretActivities(t, true)
	revokeTaskMachine(t, a)
	err := a.Launch(t.Context(), Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID})
	app, ok := errors.AsType[*temporal.ApplicationError](err)
	if !ok || app.Type() != "PRINCIPAL_REVOKED" {
		t.Fatalf("launch authorization: %v", err)
	}
	if len(provider.starts) != 0 || resolver.resolutions != 0 {
		t.Fatal("revoked actor resolved secrets or launched")
	}
}

func TestWorkerCanReconcileAcceptedLaunchAfterRevocation(t *testing.T) {
	a, task, provider, resolver := secretActivities(t, true)
	plan, err := a.plan(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	request := core.StartRequest{Key: key(task, "agent"), Command: plan.Command, SecretRefs: plan.SecretRefs, Deadline: task.Deadline}
	if _, claimed, err := a.DB.BeginOperation(t.Context(), task.Run.ID, "agent", hash(request)); err != nil || !claimed {
		t.Fatal(err)
	}
	revokeTaskMachine(t, a)
	if err = a.Launch(t.Context(), Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	if len(provider.starts) != 0 || provider.lookups != 1 || resolver.resolutions != 0 {
		t.Fatal("reconciliation performed a new privileged action")
	}
}

func TestCleanupStillRunsAfterPrincipalRevocation(t *testing.T) {
	a, task, _, _ := secretActivities(t, true)
	provider := &finalizerProvider{}
	a.Provider = provider
	revokeTaskMachine(t, a)
	if err := a.Reap(t.Context(), Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	if len(provider.deleted) != 1 || provider.deleted[0].WorkspaceID != task.WorkspaceID || provider.deleted[0].RunID != task.Run.ID {
		t.Fatal("cleanup escaped its verified run scope")
	}
	if err := a.Reap(t.Context(), Input{WorkspaceID: "foreign", TaskID: task.ID}); err == nil {
		t.Fatal("cross-workspace cleanup accepted")
	}
}

func TestRecoveryFinalizesAfterPrincipalRevocation(t *testing.T) {
	db := activityStore(t)
	task := activityTask(t, db, true)
	provider := &finalizerProvider{}
	a := &Activities{DB: db, Provider: provider}
	sandbox := core.SandboxRef{BackendID: "test-backend", ID: "existing", Key: key(task, "sandbox")}
	if err := db.UpdateRun(t.Context(), task.Run.ID, store.RunUpdate{State: new(core.RunStopping), Sandbox: &sandbox}); err != nil {
		t.Fatal(err)
	}
	prepared := store.PreparedResult{State: core.TaskFailed, Reason: "WORKFLOW_INTERRUPTED"}
	if err := db.UpdateRun(t.Context(), task.Run.ID, store.RunUpdate{State: new(core.RunFinalizing), VerifiedStopped: new(true), Result: &prepared}); err != nil {
		t.Fatal(err)
	}
	revokeTaskMachine(t, a)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(a.Finalize)
	env.ExecuteWorkflow(Recovery, Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	got, err := db.Get(t.Context(), task.WorkspaceID, task.ID)
	if err != nil || got.State != core.TaskFailed || got.Run.Cleanup != core.CleanupDone || len(provider.deleted) != 1 || provider.deleted[0] != key(task, "sandbox") {
		t.Fatal("recovery failed its scoped cleanup", err)
	}
}
