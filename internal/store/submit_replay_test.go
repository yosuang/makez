package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestSameSubmitDoesNotEnqueueTwice uses Postgres. It does not start a sandbox, call a model, or accept a Pi session.
func TestSameSubmitDoesNotEnqueueTwice(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "submit-" + newID()
	token := "submit-token-" + newID()
	if err := db.RegisterMachines(ctx, map[string]string{token: workspace}); err != nil {
		t.Fatal(err)
	}
	principal, bound, err := db.AuthenticateMachine(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if bound != workspace || principal.Kind != "machine" || principal.ID == "" {
		t.Fatalf("principal = %+v workspace %s", principal, bound)
	}
	ctx = WithPrincipal(ctx, principal)
	key := "submit-key-" + newID()
	input := SubmitInput{
		Prompt:         "same-request",
		Config:         json.RawMessage(`{"b":1,"a":2}`),
		AgentID:        "demo",
		AgentVersion:   "1",
		ProfileID:      "default",
		ProfileVersion: "1",
		TimeoutSeconds: 60,
	}

	first, created, err := db.Submit(ctx, workspace, key, input)
	if err != nil || !created || first.ID == "" || first.WorkspaceID != workspace || first.Prompt != input.Prompt || first.State != core.TaskQueued {
		t.Fatalf("first submit = %+v created=%v err=%v", first, created, err)
	}
	if n := submitTaskCount(t, db, workspace); n != 1 {
		t.Fatalf("tasks after create = %d", n)
	}
	if n := startCommandCount(t, db, workspace); n != 1 {
		t.Fatalf("START_TASK after create = %d", n)
	}
	commandID := startCommandID(t, db, first.ID)
	hash := submitRequestHash(t, db, first.ID)

	replay, created, err := db.Submit(ctx, workspace, key, input)
	if err != nil || created || replay.ID != first.ID || replay.Prompt != first.Prompt || replay.WorkspaceID != workspace {
		t.Fatalf("replay = %+v created=%v err=%v", replay, created, err)
	}
	if n := submitTaskCount(t, db, workspace); n != 1 {
		t.Fatalf("tasks after replay = %d", n)
	}
	if n := startCommandCount(t, db, workspace); n != 1 {
		t.Fatalf("START_TASK after replay = %d", n)
	}
	if got := startCommandID(t, db, first.ID); got != commandID {
		t.Fatalf("START_TASK id = %s, want %s", got, commandID)
	}
	if got := submitRequestHash(t, db, first.ID); got != hash {
		t.Fatalf("request hash after replay = %s", got)
	}

	sameHash := input
	sameHash.Config = json.RawMessage(`{"a":2,"b":1}`)
	canonical, created, err := db.Submit(ctx, workspace, key, sameHash)
	if err != nil || created || canonical.ID != first.ID || canonical.Prompt != first.Prompt {
		t.Fatalf("canonical replay = %+v created=%v err=%v", canonical, created, err)
	}
	if n := startCommandCount(t, db, workspace); n != 1 {
		t.Fatalf("START_TASK after canonical replay = %d", n)
	}

	other := input
	other.Prompt = "different-request"
	conflict, created, err := db.Submit(ctx, workspace, key, other)
	if !errors.Is(err, ErrConflict) || created || conflict.ID != "" {
		t.Fatalf("different hash = %+v created=%v err=%v", conflict, created, err)
	}
	if n := submitTaskCount(t, db, workspace); n != 1 {
		t.Fatalf("tasks after different hash = %d", n)
	}
	if n := startCommandCount(t, db, workspace); n != 1 {
		t.Fatalf("START_TASK after different hash = %d", n)
	}
	if got := startCommandID(t, db, first.ID); got != commandID {
		t.Fatalf("START_TASK id after conflict = %s, want %s", got, commandID)
	}
	stored, err := db.Get(ctx, workspace, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != first.ID || stored.Prompt != input.Prompt {
		t.Fatalf("stored task = %+v", stored)
	}
	if got := submitRequestHash(t, db, first.ID); got != hash {
		t.Fatalf("request hash after conflict = %s", got)
	}

	again, created, err := db.Submit(ctx, workspace, key, input)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("replay after conflict = %+v created=%v err=%v", again, created, err)
	}
	if n := submitTaskCount(t, db, workspace); n != 1 {
		t.Fatalf("tasks after final replay = %d", n)
	}
	if n := startCommandCount(t, db, workspace); n != 1 {
		t.Fatalf("START_TASK after final replay = %d", n)
	}
}

func submitTaskCount(t *testing.T, db *Store, workspace string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_tasks WHERE workspace_id=$1`, workspace).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func startCommandCount(t *testing.T, db *Store, workspace string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_outbox WHERE workspace_id=$1 AND command='START_TASK'`, workspace).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func startCommandID(t *testing.T, db *Store, taskID string) string {
	t.Helper()
	var id string
	if err := db.pool.QueryRow(t.Context(), `SELECT id FROM makez_outbox WHERE task_id=$1 AND command='START_TASK'`, taskID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func submitRequestHash(t *testing.T, db *Store, taskID string) string {
	t.Helper()
	var hash string
	if err := db.pool.QueryRow(t.Context(), `SELECT request_hash FROM makez_tasks WHERE id=$1`, taskID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}
