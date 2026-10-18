package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/secret"
	"github.com/makez-dev/makez/internal/store"
	"go.temporal.io/sdk/temporal"
)

type secretAgent struct{ core.AgentAdapter }

func (a secretAgent) Plan(input core.AgentInput) (core.AgentPlan, error) {
	plan, err := a.AgentAdapter.Plan(input)
	plan.SecretRefs = map[string]string{"CODEX_ACCESS_TOKEN": "agent-auth@1"}
	return plan, err
}

type secretProviderSpy struct {
	core.SandboxProvider
	ensures                int
	starts                 []core.StartRequest
	lookups                int
	withoutSecretInjection bool
}

func (p *secretProviderSpy) BackendID() string { return "test-backend" }
func (p *secretProviderSpy) Capabilities(context.Context, string, string) (core.SandboxCapabilities, error) {
	caps := agent.NewDemo().Requirements()
	if !p.withoutSecretInjection {
		caps = append(caps, core.SecretInjection)
	}
	return core.SandboxCapabilities{Supported: caps}, nil
}
func (p *secretProviderSpy) Ensure(_ context.Context, request core.SandboxRequest) (core.SandboxRef, error) {
	p.ensures++
	return core.SandboxRef{BackendID: p.BackendID(), ID: "sandbox", Key: request.Key}, nil
}
func (p *secretProviderSpy) WriteFile(context.Context, core.SandboxRef, string, io.Reader, int64) error {
	return nil
}
func (p *secretProviderSpy) Start(_ context.Context, sandbox core.SandboxRef, request core.StartRequest) (core.ProcessRef, error) {
	request.SecretValues = maps.Clone(request.SecretValues)
	p.starts = append(p.starts, request)
	return core.ProcessRef{Sandbox: sandbox, ID: "receipt", Key: request.Key}, nil
}
func (p *secretProviderSpy) Process(_ context.Context, sandbox core.SandboxRef, key core.OperationKey) (core.ProcessObservation, error) {
	p.lookups++
	return core.ProcessObservation{State: core.ProcessRunning, Ref: &core.ProcessRef{Sandbox: sandbox, ID: "receipt", Key: key}}, nil
}

type secretResolverSpy struct {
	core.SecretResolver
	resolutions int
}

func (r *secretResolverSpy) Resolve(ctx context.Context, workspace string, refs map[string]string) (map[string]string, error) {
	r.resolutions++
	return r.SecretResolver.Resolve(ctx, workspace, refs)
}

func secretActivities(t *testing.T, readySandbox bool) (*Activities, store.Task, *secretProviderSpy, *secretResolverSpy) {
	t.Helper()
	db := activityStore(t)
	task := activityTask(t, db, true)
	provider := &secretProviderSpy{}
	resolver, err := secret.New(secret.Registry{task.WorkspaceID: {"agent-auth@1": {Env: "MAKEZ_WORKFLOW_TEST_AUTH"}}})
	if err != nil {
		t.Fatal(err)
	}
	spy := &secretResolverSpy{SecretResolver: resolver}
	a := &Activities{DB: db, Provider: provider, Agents: map[string]core.AgentAdapter{"demo@1": secretAgent{agent.NewDemo()}}, Secrets: spy}
	if readySandbox {
		sandbox := &core.SandboxRef{BackendID: provider.BackendID(), ID: "sandbox", Key: key(task, "sandbox")}
		if err = db.UpdateRun(t.Context(), task.Run.ID, store.RunUpdate{Sandbox: sandbox, State: new(core.RunStarting)}); err != nil {
			t.Fatal(err)
		}
		task, err = db.Get(t.Context(), task.WorkspaceID, task.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return a, task, provider, spy
}

func TestPrepareRejectsUnavailableSecretsBeforeCreatingSandbox(t *testing.T) {
	for _, scenario := range []string{"missing resolver", "unregistered reference", "missing credential"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("MAKEZ_WORKFLOW_TEST_AUTH", "")
			a, task, provider, _ := secretActivities(t, false)
			if scenario == "missing resolver" {
				a.Secrets = nil
			} else if scenario == "unregistered reference" {
				r, err := secret.New(nil)
				if err != nil {
					t.Fatal(err)
				}
				a.Secrets = r
			}
			err := a.Prepare(t.Context(), Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID})
			app, ok := errors.AsType[*temporal.ApplicationError](err)
			if !ok || (app.Type() != "AUTH_NOT_CONFIGURED" && app.Type() != "AUTH_UNAVAILABLE") {
				t.Fatalf("expected safe authentication failure: %v", err)
			}
			if provider.ensures != 0 {
				t.Fatal("created a sandbox without usable credentials")
			}
			phase, err := a.DB.OperationPhase(t.Context(), task.Run.ID, "sandbox")
			if err != nil || phase != "" {
				t.Fatalf("claimed sandbox operation before checking credentials: phase=%q err=%v", phase, err)
			}
		})
	}
}

func TestLaunchMissingCredentialDoesNotClaimOperation(t *testing.T) {
	t.Setenv("MAKEZ_WORKFLOW_TEST_AUTH", "")
	a, task, provider, _ := secretActivities(t, true)
	if err := a.Launch(t.Context(), Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID}); err == nil {
		t.Fatal("launched without credentials")
	}
	phase, err := a.DB.OperationPhase(t.Context(), task.Run.ID, "agent")
	if err != nil || phase != "" || len(provider.starts) != 0 || provider.lookups != 0 {
		t.Fatalf("authentication failure must leave launch unclaimed: phase=%q starts=%d lookups=%d err=%v", phase, len(provider.starts), provider.lookups, err)
	}
}

func TestLaunchTransportsSecretsWithoutPersistenceAndDoesNotRestart(t *testing.T) {
	const value = "test-only-access-token-not-for-persistence"
	t.Setenv("MAKEZ_WORKFLOW_TEST_AUTH", value)
	a, task, provider, resolver := secretActivities(t, true)
	input := Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID}
	if err := a.Launch(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	if len(provider.starts) != 1 || resolver.resolutions != 1 {
		t.Fatalf("start/resolution count: %d/%d", len(provider.starts), resolver.resolutions)
	}
	request := provider.starts[0]
	if request.SecretValues["CODEX_ACCESS_TOKEN"] != value || request.SecretRefs["CODEX_ACCESS_TOKEN"] != "agent-auth@1" {
		t.Fatal("provider did not receive separate reference and ephemeral credential")
	}
	if request.Command.Env["CODEX_ACCESS_TOKEN"] != "" {
		t.Fatal("credential entered persisted command environment")
	}
	stored, err := a.DB.Get(t.Context(), task.WorkspaceID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := a.DB.Pending(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, public := range []any{request, stored, outbox, input} {
		encoded, err := json.Marshal(public)
		if err != nil || bytes.Contains(encoded, []byte(value)) {
			t.Fatal("credential appeared in serializable task/workflow data")
		}
	}
	t.Setenv("MAKEZ_WORKFLOW_TEST_AUTH", "")
	if err := a.Launch(t.Context(), input); err != nil {
		t.Fatalf("known launch should survive credential revocation: %v", err)
	}
	if len(provider.starts) != 1 || resolver.resolutions != 1 {
		t.Fatal("retry resolved credentials or restarted an existing process")
	}
}

func TestDispatchingLaunchAttachesAfterCredentialRevocation(t *testing.T) {
	t.Setenv("MAKEZ_WORKFLOW_TEST_AUTH", "")
	a, task, provider, resolver := secretActivities(t, true)
	plan, err := a.plan(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	request := core.StartRequest{Key: key(task, "agent"), Command: plan.Command, SecretRefs: plan.SecretRefs, Deadline: task.Deadline}
	phase, claimed, err := a.DB.BeginOperation(t.Context(), task.Run.ID, "agent", hash(request))
	if err != nil || !claimed || phase != "DISPATCHING" {
		t.Fatalf("stage uncertain launch: %v", err)
	}
	if err = a.Launch(t.Context(), Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID}); err != nil {
		t.Fatalf("must recover by receipt despite revoked credentials: %v", err)
	}
	if len(provider.starts) != 0 || provider.lookups != 1 || resolver.resolutions != 0 {
		t.Fatalf("recovery should only inspect existing process: starts=%d lookups=%d resolutions=%d", len(provider.starts), provider.lookups, resolver.resolutions)
	}
	stored, err := a.DB.Get(t.Context(), task.WorkspaceID, task.ID)
	if err != nil || stored.Run.Process == nil || stored.Run.Process.ID != "receipt" || stored.Run.State != core.RunRunning {
		t.Fatalf("recovered receipt not persisted: %v", err)
	}
}

func TestLaunchHashIncludesReferencesAndExcludesSecretValues(t *testing.T) {
	request := core.StartRequest{Key: core.OperationKey{WorkspaceID: "workspace", TaskID: "task", RunID: "run", Step: "agent"}, Command: core.CommandSpec{Argv: []string{"/agent"}}, SecretRefs: map[string]string{"API_TOKEN": "auth@1"}, SecretValues: map[string]string{"API_TOKEN": "secret-value-one"}, Deadline: time.Unix(1000, 0)}
	original := hash(request)
	encoded, err := json.Marshal(request)
	if err != nil || bytes.Contains(encoded, []byte("secret-value-one")) || !bytes.Contains(encoded, []byte("auth@1")) {
		t.Fatal("serialized launch must contain references only")
	}
	request.SecretValues["API_TOKEN"] = "secret-value-two"
	if hash(request) != original {
		t.Fatal("credential rotation changed logical launch identity")
	}
	request.SecretRefs["API_TOKEN"] = "auth@2"
	if hash(request) == original {
		t.Fatal("versioned reference change did not change launch identity")
	}
}

func TestPrepareRequiresSecretInjectionEvenWhenAdapterOmitsRequirement(t *testing.T) {
	t.Setenv("MAKEZ_WORKFLOW_TEST_AUTH", "available-test-credential")
	a, task, provider, resolver := secretActivities(t, false)
	provider.withoutSecretInjection = true
	err := a.Prepare(t.Context(), Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID})
	app, ok := errors.AsType[*temporal.ApplicationError](err)
	if !ok || app.Type() != "UNSUPPORTED_CAPABILITY" {
		t.Fatalf("missing injection capability must fail closed: %v", err)
	}
	if provider.ensures != 0 || resolver.resolutions != 0 {
		t.Fatal("resolved credentials or created sandbox before checking injection capability")
	}
}

type rotatedSecretAgent struct{ core.AgentAdapter }

func (a rotatedSecretAgent) Plan(input core.AgentInput) (core.AgentPlan, error) {
	plan, err := a.AgentAdapter.Plan(input)
	plan.Command.Argv = []string{"/changed-agent", "--different-model"}
	plan.SecretRefs = map[string]string{"CODEX_ACCESS_TOKEN": "agent-auth@2"}
	plan.Stop = core.StopPolicy{Signal: "SIGINT", GracePeriod: time.Second}
	return plan, err
}

func TestDispatchingRecoveryUsesFrozenPlanAfterConfigurationRotation(t *testing.T) {
	t.Setenv("MAKEZ_WORKFLOW_TEST_AUTH", "original-test-credential")
	a, task, provider, resolver := secretActivities(t, false)
	input := Input{WorkspaceID: task.WorkspaceID, TaskID: task.ID}
	if err := a.Prepare(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	task, err := a.DB.Get(t.Context(), task.WorkspaceID, task.ID)
	if err != nil || task.Run.Plan == nil {
		t.Fatalf("Prepare did not freeze its plan: %v", err)
	}
	original := *task.Run.Plan
	request := core.StartRequest{Key: key(task, "agent"), Command: original.Command, SecretRefs: original.SecretRefs, Deadline: task.Deadline}
	if _, claimed, err := a.DB.BeginOperation(t.Context(), task.Run.ID, "agent", hash(request)); err != nil || !claimed {
		t.Fatalf("stage uncertain launch: %v", err)
	}
	// A restarted Worker now registers the same adapter ID with another binding.
	a.Agents["demo@1"] = rotatedSecretAgent{agent.NewDemo()}
	t.Setenv("MAKEZ_WORKFLOW_TEST_AUTH", "")
	resolutionsBefore := resolver.resolutions
	if err = a.Launch(t.Context(), input); err != nil {
		t.Fatalf("configuration rotation broke receipt recovery: %v", err)
	}
	if len(provider.starts) != 0 || provider.lookups != 1 || resolver.resolutions != resolutionsBefore {
		t.Fatal("recovery resolved changed credentials or restarted the process")
	}
	recovered, err := a.DB.Get(t.Context(), task.WorkspaceID, task.ID)
	if err != nil || recovered.Run.Process == nil || recovered.Run.State != core.RunRunning {
		t.Fatalf("existing process was not attached: %v", err)
	}
	frozen, err := a.plan(t.Context(), recovered)
	if err != nil || !reflect.DeepEqual(frozen, original) {
		t.Fatalf("launch/finalization plan drifted after restart: %v", err)
	}
	phase, err := a.DB.OperationPhase(t.Context(), task.Run.ID, "agent")
	if err != nil || phase != "CONFIRMED" {
		t.Fatalf("recovered operation not confirmed: %s %v", phase, err)
	}
}
