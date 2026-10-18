package api

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/secret"
)

type authenticatedTestAgent struct{ core.AgentAdapter }

func (a authenticatedTestAgent) Plan(input core.AgentInput) (core.AgentPlan, error) {
	plan, err := a.AgentAdapter.Plan(input)
	plan.SecretRefs = map[string]string{"CODEX_ACCESS_TOKEN": "agent-auth@1"}
	return plan, err
}

type admissionSecretSpy struct {
	core.SecretResolver
	resolutions int
}

func (r *admissionSecretSpy) Resolve(ctx context.Context, workspace string, refs map[string]string) (map[string]string, error) {
	r.resolutions++
	return r.SecretResolver.Resolve(ctx, workspace, refs)
}

func TestAPISecretReferencesAreWorkspaceScopedWithoutReadingCredentials(t *testing.T) {
	const value = "test-only-credential-not-request-data"
	t.Setenv("MAKEZ_API_TEST_AUTH", value)
	f := apiTestServer(t)
	resolver, err := secret.New(secret.Registry{f.workspace: {"agent-auth@1": {Env: "MAKEZ_API_TEST_AUTH"}}})
	if err != nil {
		t.Fatal(err)
	}
	spy := &admissionSecretSpy{SecretResolver: resolver}
	server := &Server{DB: f.db, Artifacts: f.files, Keys: map[string]string{"token-a": f.workspace, "token-b": f.workspace + "-other"}, Agents: map[string]core.AgentAdapter{"demo@1": authenticatedTestAgent{agent.NewDemo()}}, Secrets: spy}
	f.handler = server.Handler()
	response := f.request(t, "POST", "/v1/tasks", `{"prompt":"authenticated Agent request"}`, "auth-task", "token-a")
	task := taskResponse(t, response, 202)
	if spy.resolutions != 0 {
		t.Fatal("API resolved a credential instead of authorizing its reference")
	}
	if bytes.Contains(response.Body.Bytes(), []byte(value)) {
		t.Fatal("credential leaked in task response")
	}
	stored, err := f.db.Get(t.Context(), f.workspace, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := f.db.Pending(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []any{stored, outbox} {
		encoded, err := json.Marshal(data)
		if err != nil || bytes.Contains(encoded, []byte(value)) {
			t.Fatal("credential entered stored request/outbox data")
		}
	}
	denied := f.request(t, "POST", "/v1/tasks", `{"prompt":"another workspace"}`, "foreign-auth", "token-b")
	if denied.Code != 403 || bytes.Contains(denied.Body.Bytes(), []byte(value)) {
		t.Fatalf("foreign workspace should be denied safely, status=%d", denied.Code)
	}
	for _, body := range []string{`{"prompt":"raw credential","secret_values":{"CODEX_ACCESS_TOKEN":"` + value + `"}}`, `{"prompt":"override binding","secret_refs":{"CODEX_ACCESS_TOKEN":"other@1"}}`} {
		got := f.request(t, "POST", "/v1/tasks", body, "injected-credentials", "token-a")
		if got.Code != 400 || bytes.Contains(got.Body.Bytes(), []byte(value)) {
			t.Fatalf("task must not accept raw credentials or override registered bindings, status=%d", got.Code)
		}
	}
	// Admission remains read-only when a registered credential rotates or disappears.
	t.Setenv("MAKEZ_API_TEST_AUTH", "")
	taskResponse(t, f.request(t, "POST", "/v1/tasks", `{"prompt":"defer availability check to execution"}`, "missing-at-admission", "token-a"), 202)
	if spy.resolutions != 0 {
		t.Fatal("admission tried to read a revoked credential")
	}
	server.Secrets = nil
	f.handler = server.Handler()
	if got := f.request(t, "POST", "/v1/tasks", `{"prompt":"no resolver"}`, "missing-resolver", "token-a"); got.Code != 403 {
		t.Fatalf("missing authentication registry should be denied, status=%d", got.Code)
	}
}
