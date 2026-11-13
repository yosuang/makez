package store

import (
	"errors"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestPublishWithoutARun uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Rejecting success without a run is not a claim that an external action already ran once.
// This is not exactly-once for an external action and not Pi acceptance.
func TestPublishWithoutARun(t *testing.T) {
	fx := cancelWorkspace(t)
	artifact := acceptedArtifact()
	requirePublishableArtifact(t, artifact)

	// Each task is queued and has no run.
	for _, state := range []core.TaskState{core.TaskSucceeded, core.TaskCanceled, core.TaskRunning} {
		task := submitCancelTask(t, fx, "publish-no-run-"+string(state))
		requireNoRunPublishRejected(t, fx, task, state, artifact)
	}
	for _, verified := range []bool{false, true} {
		requireNoRunFailureStored(t, fx, verified, artifact)
	}
	requireNoRunWorkspace(t, fx)
}

func requireNoRunPublishRejected(t *testing.T, fx cancelFixture, task Task, state core.TaskState, artifact Artifact) {
	t.Helper()
	before := readNoRunCounts(t, fx.db, task.ID)
	requireQueuedWithoutRun(t, task, before)
	err := fx.db.Publish(fx.ctx, fx.workspace, task.ID, state, "SHOULD_NOT_PUBLISH", true, core.CleanupPending, []Artifact{artifact})
	after := readNoRunCounts(t, fx.db, task.ID)
	stored, getErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	kept := after == before && stored.State == core.TaskQueued && stored.Reason == "" && stored.Run == nil && !stored.CancelRequested
	if !errors.Is(err, ErrConflict) || !kept || after.runs != 0 || after.artifacts != 0 || after.task == string(core.TaskSucceeded) || stored.State == core.TaskSucceeded {
		t.Fatalf("publish %s = %v task %s stored %s reason %q runs %d artifacts %d START_CLEANUP %d version %d, want ErrConflict and task %s reason %q with 0 runs and 0 artifacts", state, err, after.task, stored.State, after.reason, after.runs, after.artifacts, after.cleanups, after.version, before.task, before.reason)
	}
}

func requireNoRunFailureStored(t *testing.T, fx cancelFixture, verified bool, artifact Artifact) {
	t.Helper()
	prompt := "publish-no-run-failed-unverified"
	if verified {
		prompt = "publish-no-run-failed-verified"
	}
	task := submitCancelTask(t, fx, prompt)
	before := readNoRunCounts(t, fx.db, task.ID)
	requireQueuedWithoutRun(t, task, before)
	const reason = "CALLER_FAILED"
	publish := func() error {
		return fx.db.Publish(fx.ctx, fx.workspace, task.ID, core.TaskFailed, reason, verified, core.CleanupPending, []Artifact{artifact})
	}
	err := publish()
	after := readNoRunCounts(t, fx.db, task.ID)
	stored, getErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if err != nil || after.tasks != 1 || after.runs != 0 || after.artifacts != 0 || after.outbox != 1 || after.cleanups != 0 || after.commands != "START_TASK" || after.task != string(core.TaskFailed) || after.reason != reason || after.cancel || after.version != before.version+1 || stored.State != core.TaskFailed || stored.Reason != reason || stored.Run != nil || stored.CancelRequested || stored.State == core.TaskSucceeded || after.task == string(core.TaskSucceeded) {
		t.Fatalf("publish FAILED verified %v = %v task %s stored %s reason %q runs %d artifacts %d START_CLEANUP %d version %d, want FAILED reason %q, 0 runs, 0 artifacts, 0 START_CLEANUP, version %d", verified, err, after.task, stored.State, after.reason, after.runs, after.artifacts, after.cleanups, after.version, reason, before.version+1)
	}
	replayErr := publish()
	replay := readNoRunCounts(t, fx.db, task.ID)
	replayed, getErr := fx.db.Get(fx.ctx, fx.workspace, task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if replayErr != nil || replay != after || replay.version != after.version || replayed.State != core.TaskFailed || replayed.Reason != reason || replayed.Run != nil || replay.runs != 0 || replay.artifacts != 0 || replay.task == string(core.TaskSucceeded) || replayed.State == core.TaskSucceeded {
		t.Fatalf("second publish verified %v = %v task %s stored %s reason %q version %d runs %d artifacts %d, want nil and task %s reason %q version %d with 0 runs and 0 artifacts", verified, replayErr, replay.task, replayed.State, replay.reason, replay.version, replay.runs, replay.artifacts, after.task, after.reason, after.version)
	}
}

func requireQueuedWithoutRun(t *testing.T, task Task, row noRunCounts) {
	t.Helper()
	if task.State != core.TaskQueued || task.Reason != "" || task.Run != nil || task.CancelRequested || row.tasks != 1 || row.runs != 0 || row.artifacts != 0 || row.outbox != 1 || row.cleanups != 0 || row.commands != "START_TASK" || row.task != string(core.TaskQueued) || row.reason != "" || row.cancel || row.version != 1 {
		t.Fatalf("queued task state %s reason %q run %v row %+v, want QUEUED, 0 runs, 0 artifacts, 1 START_TASK, version 1", task.State, task.Reason, task.Run != nil, row)
	}
}

func requireNoRunWorkspace(t *testing.T, fx cancelFixture) {
	t.Helper()
	var succeeded, failed, queued, other, runs, artifacts, outbox, starts, cleanups int
	err := fx.db.pool.QueryRow(t.Context(), `
		SELECT
			(SELECT count(*) FROM makez_tasks WHERE workspace_id=$1 AND state='SUCCEEDED'),
			(SELECT count(*) FROM makez_tasks WHERE workspace_id=$1 AND state='FAILED'),
			(SELECT count(*) FROM makez_tasks WHERE workspace_id=$1 AND state='QUEUED'),
			(SELECT count(*) FROM makez_tasks WHERE workspace_id=$1 AND state NOT IN ('QUEUED','FAILED')),
			(SELECT count(*) FROM makez_runs r JOIN makez_tasks t ON t.id=r.task_id WHERE t.workspace_id=$1),
			(SELECT count(*) FROM makez_artifacts WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_outbox o JOIN makez_tasks t ON t.id=o.task_id WHERE t.workspace_id=$1),
			(SELECT count(*) FROM makez_outbox o JOIN makez_tasks t ON t.id=o.task_id WHERE t.workspace_id=$1 AND o.command='START_TASK'),
			(SELECT count(*) FROM makez_outbox o JOIN makez_tasks t ON t.id=o.task_id WHERE t.workspace_id=$1 AND o.command='START_CLEANUP')`,
		fx.workspace).Scan(&succeeded, &failed, &queued, &other, &runs, &artifacts, &outbox, &starts, &cleanups)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded != 0 || failed != 2 || queued != 3 || other != 0 || runs != 0 || artifacts != 0 || outbox != 5 || starts != 5 || cleanups != 0 {
		t.Fatalf("workspace SUCCEEDED %d FAILED %d QUEUED %d other %d runs %d artifacts %d outbox %d START_TASK %d START_CLEANUP %d, want 0 SUCCEEDED, 2 FAILED, 3 QUEUED, 0 runs, 0 artifacts, 0 START_CLEANUP", succeeded, failed, queued, other, runs, artifacts, outbox, starts, cleanups)
	}
}

type noRunCounts struct {
	tasks     int
	runs      int
	artifacts int
	outbox    int
	cleanups  int
	commands  string
	task      string
	reason    string
	cancel    bool
	version   int64
}

func readNoRunCounts(t *testing.T, db *Store, id string) noRunCounts {
	t.Helper()
	var row noRunCounts
	err := db.pool.QueryRow(t.Context(), `
		SELECT
			(SELECT count(*) FROM makez_tasks WHERE id=$1),
			(SELECT count(*) FROM makez_runs WHERE task_id=$1),
			(SELECT count(*) FROM makez_artifacts WHERE workspace_id=t.workspace_id),
			(SELECT count(*) FROM makez_outbox WHERE task_id=$1),
			(SELECT count(*) FROM makez_outbox WHERE task_id=$1 AND command='START_CLEANUP'),
			(SELECT COALESCE(string_agg(command, ',' ORDER BY command), '') FROM makez_outbox WHERE task_id=$1),
			t.state, t.reason, t.cancel_requested, t.version
		FROM makez_tasks t
		WHERE t.id=$1`, id).Scan(
		&row.tasks, &row.runs, &row.artifacts, &row.outbox, &row.cleanups, &row.commands,
		&row.task, &row.reason, &row.cancel, &row.version,
	)
	if err != nil {
		t.Fatal(err)
	}
	return row
}
