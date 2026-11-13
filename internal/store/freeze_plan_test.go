package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestFreezePlanKeepsTheFirstPlan uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Keeping the first plan is not exactly-once for an external action.
func TestFreezePlanKeepsTheFirstPlan(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace := "freeze-" + newID()
	token := "freeze-token-" + newID()
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
	first := core.AgentPlan{
		SecretRefs: map[string]string{"OPENAI_API_KEY": "openai@1"},
		Command: core.CommandSpec{
			Argv: []string{"first"},
			Env:  map[string]string{"LOG_LEVEL": "info"},
		},
	}
	run := openPreparingRun(t, fx)
	requireNoFrozenPlan(t, fx, run)

	empty := first
	empty.Command.Argv = []string{}
	requireFreezeRejected(t, fx, run, empty, ErrInvalidInput)

	// The env value marks the name collision. It is not a secret value.
	overlap := first
	overlap.Command.Env = map[string]string{"OPENAI_API_KEY": "name-only"}
	requireFreezeRejected(t, fx, run, overlap, ErrInvalidInput)

	kept := requireFreezeKept(t, fx, run, first, first, run.version+1)
	second := first
	second.Command.Argv = []string{"second"}
	if reflect.DeepEqual(second, first) {
		t.Fatal("second argv matches the stored plan")
	}
	// A later call returns the stored plan and leaves the version unchanged.
	requireFreezeKept(t, fx, kept, second, first, kept.version)

	running := openPreparingRun(t, fx)
	running = requireRunCommitted(t, fx, running, core.RunStarting)
	running = requireRunCommitted(t, fx, running, core.RunRunning)
	requireFreezeRejected(t, fx, running, first, ErrConflict)
}

func requireNoFrozenPlan(t *testing.T, fx runInitFixture, run transitionRun) {
	t.Helper()
	row := readFrozenPlan(t, fx.db, run.runID, nil)
	if row.state != core.RunPreparing || row.version != run.version || row.plan != nil || !row.same {
		t.Fatalf("opened plan = %s version %d plan %v, want %s version %d with no plan", row.state, row.version, planArgv(row.plan), core.RunPreparing, run.version)
	}
}

func requireFreezeRejected(t *testing.T, fx runInitFixture, before transitionRun, offer core.AgentPlan, wantErr error) {
	t.Helper()
	got, err := fx.db.FreezePlan(fx.ctx, before.runID, offer)
	row := readFrozenPlan(t, fx.db, before.runID, nil)
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, core.AgentPlan{}) || row.state != before.state || row.version != before.version || row.plan != nil || !row.same {
		t.Fatalf("FreezePlan %s argv %v returned %v plan %+v; row %s version %d stored argv %v, want %v with row %s version %d and no plan", before.state, offer.Command.Argv, err, got, row.state, row.version, planArgv(row.plan), wantErr, before.state, before.version)
	}
	requireTaskRunning(t, fx.db, before.taskID)
}

// requireFreezeKept checks that FreezePlan returns want and leaves that JSON stored.
// wantVersion is the run version after the call.
func requireFreezeKept(t *testing.T, fx runInitFixture, before transitionRun, offer, want core.AgentPlan, wantVersion int64) transitionRun {
	t.Helper()
	got, err := fx.db.FreezePlan(fx.ctx, before.runID, offer)
	row := readFrozenPlan(t, fx.db, before.runID, &want)
	if err != nil || !reflect.DeepEqual(got, want) || secretRefInEnv(got) || row.state != before.state || row.version != wantVersion || !row.same || row.plan == nil || !reflect.DeepEqual(*row.plan, want) {
		t.Fatalf("FreezePlan offer argv %v returned %v argv %v; row %s version %d stored argv %v matches %v, want argv %v version %d", offer.Command.Argv, err, got.Command.Argv, row.state, row.version, planArgv(row.plan), row.same, want.Command.Argv, wantVersion)
	}
	requireTaskRunning(t, fx.db, before.taskID)
	return transitionRun{taskID: before.taskID, runID: before.runID, state: row.state, version: row.version}
}

func secretRefInEnv(plan core.AgentPlan) bool {
	for name := range plan.SecretRefs {
		if _, ok := plan.Command.Env[name]; ok {
			return true
		}
	}
	return false
}

type frozenPlanRow struct {
	state   core.RunState
	version int64
	plan    *core.AgentPlan
	same    bool
}

func readFrozenPlan(t *testing.T, db *Store, id string, want *core.AgentPlan) frozenPlanRow {
	t.Helper()
	var encoded any
	if want != nil {
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		encoded = string(raw)
	}
	var state string
	var version int64
	var raw []byte
	var same bool
	err := db.pool.QueryRow(t.Context(), `
		SELECT state, version, agent_plan,
		       CASE WHEN $2::jsonb IS NULL THEN agent_plan IS NULL ELSE agent_plan = $2::jsonb END
		FROM makez_runs WHERE id=$1`, id, encoded).Scan(&state, &version, &raw, &same)
	if err != nil {
		t.Fatal(err)
	}
	row := frozenPlanRow{state: core.RunState(state), version: version, same: same}
	if len(raw) == 0 {
		return row
	}
	var plan core.AgentPlan
	if err = json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	row.plan = &plan
	return row
}

func planArgv(plan *core.AgentPlan) []string {
	if plan == nil {
		return nil
	}
	return plan.Command.Argv
}
