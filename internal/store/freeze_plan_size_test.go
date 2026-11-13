package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestFreezePlanRejectsAnOversizedPlan uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Freezing a plan keeps the first stored plan. It is not exactly-once for an external action.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestFreezePlanRejectsAnOversizedPlan(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "freeze-size-" + newID()
	token := "freeze-size-token-" + newID()
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
	fx := runInitFixture{db: db, ctx: ctx, workspace: workspace}

	// SecretRefs names a versioned reference. That name is not a key in Command.Env.
	// The padded env value is a run of the letter x. It is not a credential.
	oversized, oversizedBytes := planOfSize(t, 1<<20+1)
	accepted, acceptedBytes := planOfSize(t, 1<<20)

	rejected := openPreparingRun(t, fx)
	requireNoFrozenPlan(t, fx, rejected)
	rejectedOutbox := requireOneStartTask(t, fx.db, rejected)
	requireOversizedPlanRejected(t, fx, rejected, oversized, oversizedBytes, rejectedOutbox)

	kept := openPreparingRun(t, fx)
	requireNoFrozenPlan(t, fx, kept)
	keptOutbox := requireOneStartTask(t, fx.db, kept)
	requireExactPlanFrozen(t, fx, kept, accepted, acceptedBytes, keptOutbox)
}

// planOfSize pads one env value so json.Marshal of the plan is exactly size bytes.
func planOfSize(t *testing.T, size int) (core.AgentPlan, []byte) {
	t.Helper()
	plan := core.AgentPlan{
		SecretRefs: map[string]string{"OPENAI_API_KEY": "openai@1"},
		Command: core.CommandSpec{
			Argv: []string{"run"},
			Env:  map[string]string{"PAD": ""},
		},
	}
	if len(plan.Command.Argv) == 0 {
		t.Fatal("argv is empty")
	}
	for name := range plan.SecretRefs {
		if _, exists := plan.Command.Env[name]; exists {
			t.Fatalf("SecretRefs name %s is present in Command.Env", name)
		}
	}
	bare, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	width := size - len(bare)
	if width < 1 {
		t.Fatalf("plan skeleton is %d bytes, want room to pad to %d", len(bare), size)
	}
	plan.Command.Env["PAD"] = strings.Repeat("x", width)
	if strings.Trim(plan.Command.Env["PAD"], "x") != "" {
		t.Fatal("env pad is not a run of x")
	}
	for name := range plan.SecretRefs {
		if _, exists := plan.Command.Env[name]; exists {
			t.Fatalf("SecretRefs name %s is present in Command.Env", name)
		}
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != size {
		t.Fatalf("json.Marshal plan = %d bytes, want %d", len(encoded), size)
	}
	return plan, encoded
}

type freezeOutbox struct {
	count    int
	commands string
}

func requireOneStartTask(t *testing.T, db *Store, run transitionRun) freezeOutbox {
	t.Helper()
	got := readFreezeOutbox(t, db, run)
	if got.count != 1 || got.commands != "START_TASK" {
		t.Fatalf("outbox before FreezePlan = %+v, want one START_TASK row", got)
	}
	return got
}

func readFreezeOutbox(t *testing.T, db *Store, run transitionRun) freezeOutbox {
	t.Helper()
	var got freezeOutbox
	err := db.pool.QueryRow(t.Context(), `
		SELECT count(*), COALESCE(string_agg(command, ',' ORDER BY command), '')
		FROM makez_outbox
		WHERE task_id = $1 OR run_id = $2`, run.taskID, run.runID).Scan(&got.count, &got.commands)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func requireSameOutbox(t *testing.T, db *Store, run transitionRun, before freezeOutbox) {
	t.Helper()
	got := readFreezeOutbox(t, db, run)
	if got != before {
		t.Fatalf("outbox = %+v, want %+v with no inserted row", got, before)
	}
}

func requireOversizedPlanRejected(t *testing.T, fx runInitFixture, before transitionRun, offer core.AgentPlan, encoded []byte, outbox freezeOutbox) {
	t.Helper()
	if len(encoded) != 1<<20+1 {
		t.Fatalf("oversized encoding is %d bytes", len(encoded))
	}
	got, err := fx.db.FreezePlan(fx.ctx, before.runID, offer)
	row := readFrozenPlan(t, fx.db, before.runID, nil)
	if err == nil || row.plan != nil || !row.same {
		t.Fatalf("FreezePlan accepted or stored the oversized plan: err=%v argv=%v state=%s version=%d stored=%v", err, got.Command.Argv, row.state, row.version, row.plan != nil)
	}
	if !errors.Is(err, ErrInvalidInput) || !reflect.DeepEqual(got, core.AgentPlan{}) || row.state != core.RunPreparing || row.version != before.version || row.plan != nil || !row.same {
		t.Fatalf("FreezePlan of %d bytes returned %v empty=%v; row %s version %d stored %v, want ErrInvalidInput, PREPARING version %d, and null agent_plan", len(encoded), err, reflect.DeepEqual(got, core.AgentPlan{}), row.state, row.version, row.plan != nil, before.version)
	}
	requireTaskRunning(t, fx.db, before.taskID)
	requireSameOutbox(t, fx.db, before, outbox)
}

func requireExactPlanFrozen(t *testing.T, fx runInitFixture, before transitionRun, offer core.AgentPlan, encoded []byte, outbox freezeOutbox) {
	t.Helper()
	if len(encoded) != 1<<20 {
		t.Fatalf("accepted encoding is %d bytes", len(encoded))
	}
	got, err := fx.db.FreezePlan(fx.ctx, before.runID, offer)
	row := readFrozenPlan(t, fx.db, before.runID, &offer)
	same := storedPlanEquals(t, fx.db, before.runID, encoded)
	returned, marshalErr := json.Marshal(got)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err != nil || !bytes.Equal(returned, encoded) || !reflect.DeepEqual(got, offer) || !same || !row.same || row.plan == nil || !reflect.DeepEqual(*row.plan, offer) || row.state != core.RunPreparing || row.version != before.version+1 {
		t.Fatalf("FreezePlan of %d bytes returned err=%v bytes=%v plan=%v stored=%v row-same=%v state=%s version=%d, want the plan, those stored bytes, PREPARING version %d", len(encoded), err, bytes.Equal(returned, encoded), reflect.DeepEqual(got, offer), same, row.same, row.state, row.version, before.version+1)
	}
	requireTaskRunning(t, fx.db, before.taskID)
	requireSameOutbox(t, fx.db, before, outbox)
}

// storedPlanEquals reports whether agent_plan is the JSON document in encoded.
// The column is jsonb, so equality is the stored document against those bytes.
func storedPlanEquals(t *testing.T, db *Store, runID string, encoded []byte) bool {
	t.Helper()
	var same bool
	err := db.pool.QueryRow(t.Context(), `SELECT agent_plan = $2::jsonb FROM makez_runs WHERE id=$1`, runID, string(encoded)).Scan(&same)
	if err != nil {
		t.Fatal(err)
	}
	return same
}
