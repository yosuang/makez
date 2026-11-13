package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
)

// TestCancelAcceptanceDoesNotReturnSucceededOrFailed posts cancel through HTTP.
// It uses Postgres. It does not start a sandbox, call a model, or accept a Pi session.
func TestCancelAcceptanceDoesNotReturnSucceededOrFailed(t *testing.T) {
	f := apiTestServer(t)
	client := wireClient{t: t, handler: f.handler}

	queued := submitTaskHTTP(t, client, f, "cancel-before-run")
	requireCancelResponse(t, client, f, queued, "CANCELED", "CANCEL_REQUESTED", "")
	requireCancelResponse(t, client, f, queued, "CANCELED", "CANCEL_REQUESTED", "")

	running := submitTaskHTTP(t, client, f, "cancel-while-running")
	moveTaskRun(t, f, running, core.RunRunning, nil)
	requireCancelResponse(t, client, f, running, "CANCELING", "", "RUNNING")
	requireCancelResponse(t, client, f, running, "CANCELING", "", "RUNNING")

	// The run holds a prepared SUCCEEDED result. The response body must not say so.
	prepared := submitTaskHTTP(t, client, f, "cancel-holding-prepared-result")
	moveTaskRun(t, f, prepared, core.RunFinalizing, &store.PreparedResult{State: core.TaskSucceeded, Reason: "SHOULD_NOT_PUBLISH"})
	requireCancelResponse(t, client, f, prepared, "CANCELING", "", "FINALIZING")
	requireCancelResponse(t, client, f, prepared, "CANCELING", "", "FINALIZING")
}

func requireCancelResponse(t *testing.T, client wireClient, f *apiFixture, id, state, reason, run string) {
	t.Helper()
	status, body := client.call(http.MethodPost, "/v1/tasks/"+id+"/cancel", f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK {
		t.Fatalf("cancel %s: %d %s", id, status, body)
	}
	for _, word := range []string{"SUCCEEDED", "FAILED"} {
		if bytes.Contains(body, []byte(word)) {
			t.Fatalf("cancel response says %s: %s", word, body)
		}
	}
	var got struct {
		ID              string `json:"id"`
		State           string `json:"state"`
		Reason          string `json:"reason"`
		CancelRequested bool   `json:"cancel_requested"`
		Run             *struct {
			State string `json:"state"`
		} `json:"run"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.State != state || got.Reason != reason || !got.CancelRequested {
		t.Fatalf("cancel response id %s state %s reason %q requested %v, want id %s state %s reason %q requested true", got.ID, got.State, got.Reason, got.CancelRequested, id, state, reason)
	}
	if run == "" {
		if got.Run != nil {
			t.Fatalf("cancel response included a run: %+v", got.Run)
		}
		return
	}
	if got.Run == nil || got.Run.State != run {
		t.Fatalf("cancel response run = %v, want %s", got.Run, run)
	}
}

func submitTaskHTTP(t *testing.T, client wireClient, f *apiFixture, prompt string) string {
	t.Helper()
	body := mustJSON(t, taskSubmit{Prompt: prompt, Timeout: 60})
	status, resp := client.call(http.MethodPost, "/v1/tasks", f.token, body, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "cancel-http-" + prompt, "X-Makez-Workspace-ID": f.workspace,
	})
	if status != http.StatusAccepted {
		t.Fatalf("submit %s: %d %s", prompt, status, resp)
	}
	var created struct {
		ID    string          `json:"id"`
		State string          `json:"state"`
		Run   json.RawMessage `json:"run"`
	}
	if err := json.Unmarshal(resp, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.State != "QUEUED" || len(created.Run) != 0 {
		t.Fatalf("submitted task = %s", resp)
	}
	return created.ID
}

func moveTaskRun(t *testing.T, f *apiFixture, id string, state core.RunState, result *store.PreparedResult) {
	t.Helper()
	started, err := f.db.InitializeRun(t.Context(), f.workspace, id, "test")
	if err != nil || started.Run == nil || started.State != core.TaskRunning || started.Run.State != core.RunPreparing {
		t.Fatalf("initialize %s = %+v err=%v", id, started, err)
	}
	runID := started.Run.ID
	setRun(t, f, runID, core.RunStarting, nil)
	setRun(t, f, runID, core.RunRunning, nil)
	if state != core.RunRunning {
		setRun(t, f, runID, state, result)
	}
	got, err := f.db.Get(t.Context(), f.workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.TaskRunning || got.CancelRequested || got.Run == nil || got.Run.State != state {
		t.Fatalf("before cancel = state %s run %v", got.State, got.Run)
	}
	if result == nil {
		if got.Run.Result != nil {
			t.Fatalf("before cancel prepared = %+v", got.Run.Result)
		}
		return
	}
	if got.Run.Result == nil || got.Run.Result.State != result.State || got.Run.Result.Reason != result.Reason {
		t.Fatalf("before cancel prepared = %+v, want %+v", got.Run.Result, result)
	}
}

func setRun(t *testing.T, f *apiFixture, runID string, state core.RunState, result *store.PreparedResult) {
	t.Helper()
	if err := f.db.UpdateRun(t.Context(), runID, store.RunUpdate{State: &state, Result: result}); err != nil {
		t.Fatal(err)
	}
}
