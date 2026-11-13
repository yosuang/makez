package store

import (
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestPublishDoesNotWriteSucceededWhenUnverifiedOrAfterCancel uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
func TestPublishDoesNotWriteSucceededWhenUnverifiedOrAfterCancel(t *testing.T) {
	fx := cancelWorkspace(t)

	unverified := advanceCancelTask(t, fx, "publish-unverified", core.RunFinalizing, nil)
	requirePublishNotSucceeded(t, fx, unverified.ID, false, core.TaskFailed, "STOP_UNCONFIRMED")

	open := advanceCancelTask(t, fx, "publish-after-cancel", core.RunFinalizing, nil)
	requireCancelAccepted(t, fx, open.ID)
	requirePublishNotSucceeded(t, fx, open.ID, true, core.TaskCanceled, "CANCEL_REQUESTED")
}

func requireCancelAccepted(t *testing.T, fx cancelFixture, id string) {
	t.Helper()
	accepted, err := fx.db.Cancel(fx.ctx, fx.workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := fx.db.Get(fx.ctx, fx.workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	row := readCancelRow(t, fx.db, id)
	if accepted.State == core.TaskSucceeded || stored.State == core.TaskSucceeded || row.task == string(core.TaskSucceeded) {
		t.Fatalf("cancel acceptance wrote task SUCCEEDED")
	}
	if !accepted.CancelRequested || !stored.CancelRequested || !row.cancel || accepted.State != core.TaskCanceling || stored.State != core.TaskCanceling || row.task != string(core.TaskCanceling) {
		t.Fatalf("cancel was not accepted: return %s stored %s row %s", accepted.State, stored.State, row.task)
	}
	if stored.Run == nil || stored.Run.State != core.RunFinalizing || row.run != string(core.RunFinalizing) {
		t.Fatalf("run is not FINALIZING before publish: stored %v row %s", stored.Run, row.run)
	}
}

func requirePublishNotSucceeded(t *testing.T, fx cancelFixture, id string, verified bool, want core.TaskState, reason string) {
	t.Helper()
	err := fx.db.Publish(fx.ctx, fx.workspace, id, core.TaskSucceeded, "SHOULD_NOT_PUBLISH", verified, core.CleanupNotRequired, nil)
	row := readCancelRow(t, fx.db, id)
	if row.task == string(core.TaskSucceeded) {
		t.Fatalf("publish wrote task SUCCEEDED")
	}
	stored, getErr := fx.db.Get(fx.ctx, fx.workspace, id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.State == core.TaskSucceeded {
		t.Fatalf("publish wrote task SUCCEEDED")
	}
	if err != nil {
		t.Fatal(err)
	}
	if row.task != string(stored.State) || row.reason != stored.Reason {
		t.Fatalf("stored task %s %q does not match row %s %q", stored.State, stored.Reason, row.task, row.reason)
	}
	if stored.State != want || stored.Reason != reason {
		t.Fatalf("task = %s reason %q, want %s reason %q", stored.State, stored.Reason, want, reason)
	}
}
