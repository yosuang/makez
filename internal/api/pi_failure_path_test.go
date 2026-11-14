package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/hostexec"
	"github.com/makez-dev/makez/internal/piharness"
	"github.com/makez-dev/makez/internal/store"
	"github.com/makez-dev/makez/internal/workflow"
	"go.temporal.io/sdk/testsuite"
)

// TestNoKeyPiFailureThroughMakez submits a pi@1 task and runs the pinned Pi
// binary with no key. The sandbox side is a host process double: /workspace on
// this machine is the git checkout, and this test does not create an OpenSandbox.
func TestNoKeyPiFailureThroughMakez(t *testing.T) {
	f := apiTestServer(t)
	piBin := pinnedPiBinary(t)
	versionHome := t.TempDir()
	version := piVersion(t, piBin, versionHome)
	if version != piharness.PackageVersion {
		t.Fatalf("pi --version = %q", version)
	}
	adapter, err := agent.NewPi(agent.PiConfig{Binary: piBin})
	if err != nil {
		t.Fatal(err)
	}
	viewerToken := "synthetic-viewer-token-1"
	if err = f.db.RegisterMachines(t.Context(), map[string]string{viewerToken: f.workspace}); err != nil {
		t.Fatal(err)
	}
	if err = setMachineRole(t, f.databaseURL, viewerToken, "Viewer"); err != nil {
		t.Fatal(err)
	}
	betaToken := "test-machine-token-b"
	betaWorkspace := f.workspace + "-other"
	keys := map[string]string{f.token: f.workspace, betaToken: betaWorkspace, viewerToken: f.workspace}
	files, err := artifact.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	server := &Server{DB: f.db, Artifacts: files, Keys: keys, Agents: map[string]core.AgentAdapter{
		"demo@1": agent.NewDemo(),
		"pi@1":   adapter,
	}}
	client := wireClient{t: t, handler: server.Handler()}
	host := newHostExec(t)
	t.Cleanup(func() {
		host.Close()
		if t.Failed() {
			out, errOut := host.Snapshot()
			t.Logf("runner stdout:\n%s", out)
			t.Logf("runner stderr:\n%s", errOut)
		}
	})
	assertRepoUntouched(t)
	activities := &workflow.Activities{
		DB: f.db, Provider: host, Artifacts: files,
		Agents: map[string]core.AgentAdapter{"demo@1": agent.NewDemo(), "pi@1": adapter},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.SetTestTimeout(3 * time.Minute)
	env.RegisterActivity(activities.Initialize)
	env.RegisterActivity(activities.Prepare)
	env.RegisterActivity(activities.Launch)
	env.RegisterActivity(activities.Observe)
	env.RegisterActivity(activities.Finalize)

	var g1, g2, g3, g4, g5 strings.Builder
	config := fmt.Sprintf(`{"execution":{"contract":%q,"scope":"personal","bot_id":"bot-wire","conversation_id":"conv-wire","harness_pin":%q}}`, piharness.Contract, piharness.Pin)
	omitBody := mustJSON(t, taskSubmit{Prompt: "omitted agent stays demo", Timeout: 180})
	status, body := client.call(http.MethodPost, "/v1/tasks", f.token, omitBody, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-omit-agent", "X-Makez-Workspace-ID": f.workspace,
	})
	if status != http.StatusAccepted {
		t.Fatalf("omit agent: %d %s", status, body)
	}
	var omitted struct {
		ID           string          `json:"id"`
		AgentID      string          `json:"agent_id"`
		AgentVersion string          `json:"agent_version"`
		Run          json.RawMessage `json:"run"`
	}
	if err = json.Unmarshal(body, &omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.AgentID != "demo" || omitted.AgentVersion != "1" || len(omitted.Run) != 0 {
		t.Fatalf("omitted agent = %s", body)
	}
	fmt.Fprintf(&g1, "HTTP create, agent omitted\n%s\n", client.format(http.MethodPost, "/v1/tasks", true, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-omit-agent", "X-Makez-Workspace-ID": f.workspace,
	}, omitBody, status, body))

	ownerBody := mustJSON(t, map[string]any{"prompt": "x", "owner_id": "not-a-field", "timeout_seconds": 180})
	status, body = client.call(http.MethodPost, "/v1/tasks", f.token, ownerBody, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-owner-top", "X-Makez-Workspace-ID": f.workspace,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("top-level owner: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP rejected top-level owner_id\n%s\n", client.format(http.MethodPost, "/v1/tasks", true, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-owner-top", "X-Makez-Workspace-ID": f.workspace,
	}, ownerBody, status, body))
	nested := mustJSON(t, taskSubmit{Prompt: "x", Config: json.RawMessage(`{"execution":{"owner_id":"not-a-field"}}`), AgentID: "pi", AgentVersion: "1", Timeout: 180})
	status, body = client.call(http.MethodPost, "/v1/tasks", f.token, nested, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-owner-nested", "X-Makez-Workspace-ID": f.workspace,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("nested owner: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP rejected owner_id inside execution config\n%s\n", client.format(http.MethodPost, "/v1/tasks", true, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-owner-nested", "X-Makez-Workspace-ID": f.workspace,
	}, nested, status, body))
	retention := mustJSON(t, taskSubmit{Prompt: "x", Config: json.RawMessage(`{"retention":"1h"}`), AgentID: "pi", AgentVersion: "1", Timeout: 180})
	status, body = client.call(http.MethodPost, "/v1/tasks", f.token, retention, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-retention", "X-Makez-Workspace-ID": f.workspace,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("retention: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP rejected retention\n%s\n", client.format(http.MethodPost, "/v1/tasks", true, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-retention", "X-Makez-Workspace-ID": f.workspace,
	}, retention, status, body))

	status, body = client.call(http.MethodGet, "/v1/tasks/missing-task", "", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("anon: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP no Authorization\n%s\n", client.format(http.MethodGet, "/v1/tasks/missing-task", false, nil, "", status, body))
	status, body = client.call(http.MethodGet, "/v1/tasks/missing-task", "synthetic-unknown-token", "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP unregistered bearer\n%s\n", client.format(http.MethodGet, "/v1/tasks/missing-task", true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, body))
	status, body = client.call(http.MethodGet, "/v1/tasks/missing-task", f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusNotFound {
		t.Fatalf("missing: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP task id absent in the caller's workspace\n%s\n", client.format(http.MethodGet, "/v1/tasks/missing-task", true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, body))

	piBody := mustJSON(t, taskSubmit{Prompt: "makez-no-key-failure", Config: json.RawMessage(config), AgentID: "pi", AgentVersion: "1", Timeout: 180})
	piHeaders := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "wire-no-key-1", "X-Makez-Workspace-ID": f.workspace}
	status, body = client.call(http.MethodPost, "/v1/tasks", f.token, piBody, piHeaders)
	if status != http.StatusAccepted {
		t.Fatalf("submit: %d %s", status, body)
	}
	var created struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		AgentID     string `json:"agent_id"`
		State       string `json:"state"`
	}
	if err = json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.AgentID != "pi" || created.WorkspaceID != f.workspace || created.State != "QUEUED" || created.ID == "" {
		t.Fatalf("created = %s", body)
	}
	fmt.Fprintf(&g1, "HTTP create pi@1\n%s\n", client.format(http.MethodPost, "/v1/tasks", true, piHeaders, piBody, status, body))
	status, replay := client.call(http.MethodPost, "/v1/tasks", f.token, piBody, piHeaders)
	if status != http.StatusOK || !bytes.Contains(replay, []byte(created.ID)) {
		t.Fatalf("replay: %d %s", status, replay)
	}
	fmt.Fprintf(&g1, "HTTP same Idempotency-Key and same body\n%s\n", client.format(http.MethodPost, "/v1/tasks", true, piHeaders, piBody, status, replay))
	conflictBody := mustJSON(t, taskSubmit{Prompt: "different-prompt", Config: json.RawMessage(config), AgentID: "pi", AgentVersion: "1", Timeout: 180})
	status, conflict := client.call(http.MethodPost, "/v1/tasks", f.token, conflictBody, piHeaders)
	if status != http.StatusConflict {
		t.Fatalf("conflict: %d %s", status, conflict)
	}
	fmt.Fprintf(&g1, "HTTP same Idempotency-Key and different body\n%s\n", client.format(http.MethodPost, "/v1/tasks", true, piHeaders, conflictBody, status, conflict))

	status, body = client.call(http.MethodGet, "/v1/tasks/"+created.ID, betaToken, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusForbidden {
		t.Fatalf("cross workspace header: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP other machine, workspace header of the owning workspace\n%s\n", client.format(http.MethodGet, "/v1/tasks/"+created.ID, true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, body))
	status, body = client.call(http.MethodGet, "/v1/tasks/"+created.ID, betaToken, "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("other workspace: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP other machine, no workspace header, so its own workspace is used\n%s\n", client.format(http.MethodGet, "/v1/tasks/"+created.ID, true, nil, "", status, body))
	status, body = client.call(http.MethodPost, "/v1/tasks", viewerToken, piBody, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-viewer", "X-Makez-Workspace-ID": f.workspace,
	})
	if status != http.StatusForbidden {
		t.Fatalf("viewer write: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP viewer machine cannot submit\n%s\n", client.format(http.MethodPost, "/v1/tasks", true, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "wire-viewer", "X-Makez-Workspace-ID": f.workspace,
	}, piBody, status, body))
	status, body = client.call(http.MethodGet, "/v1/tasks/"+created.ID, viewerToken, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK {
		t.Fatalf("viewer read: %d %s", status, body)
	}
	fmt.Fprintf(&g2, "HTTP same viewer can read\n%s\n", client.format(http.MethodGet, "/v1/tasks/"+created.ID, true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, body))

	in := workflow.Input{WorkspaceID: f.workspace, TaskID: created.ID}
	runActivity(t, env, activities.Initialize, in)
	status, body = client.call(http.MethodGet, "/v1/tasks/"+created.ID, f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK {
		t.Fatalf("after initialize: %d %s", status, body)
	}
	var associated struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Run   struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
			State  string `json:"state"`
		} `json:"run"`
	}
	if err = json.Unmarshal(body, &associated); err != nil {
		t.Fatal(err)
	}
	if associated.State != "RUNNING" || associated.Run.ID == "" || associated.Run.TaskID != created.ID || associated.Run.State != "PREPARING" {
		t.Fatalf("association = %s", body)
	}
	fmt.Fprintf(&g1, "service Initialize, then HTTP get. backend_id host-exec-test is the process double, not OpenSandbox.\n%s\n", client.format(http.MethodGet, "/v1/tasks/"+created.ID, true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, body))

	runActivity(t, env, activities.Prepare, in)
	runActivity(t, env, activities.Launch, in)
	status, body = client.call(http.MethodGet, "/v1/tasks/"+created.ID, f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"backend_id":"host-exec-test"`)) || !bytes.Contains(body, []byte(`"ID":"host-proc-1"`)) {
		t.Fatalf("after launch: %d %s", status, body)
	}
	fmt.Fprintf(&g1, "service Prepare and Launch, then HTTP get. process.id host-proc-1 is a receipt, not an OS pid.\n%s\n", client.format(http.MethodGet, "/v1/tasks/"+created.ID, true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, body))

	runActivity(t, env, activities.Observe, in)
	task, err := f.db.Get(t.Context(), f.workspace, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	drainRemaining(t, f.db, adapter, host, task)
	task, err = f.db.Get(t.Context(), f.workspace, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.db.Events(t.Context(), f.workspace, created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("no source events")
	}
	page, err := host.Output(t.Context(), *task.Run.Process, "", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := adapter.Decode(page, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.AppendOutput(t.Context(), task.Run.ID, task.Run.ObserverEpoch, page.NextCursor, decoded.NextState, decoded.Events); err != nil {
		t.Fatal(err)
	}
	after, err := f.db.Events(t.Context(), f.workspace, created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || !sameSeqs(before, after) {
		t.Fatalf("duplicate delivery changed events %v -> %v", seqsOf(before), seqsOf(after))
	}
	rest, err := f.db.Events(t.Context(), f.workspace, created.ID, before[0].Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) > 1 && len(rest) != len(before)-1 {
		t.Fatalf("continuation = %d events, full = %d", len(rest), len(before))
	}
	tail, err := f.db.Events(t.Context(), f.workspace, created.ID, before[len(before)-1].Seq)
	if err != nil || len(tail) != 0 {
		t.Fatalf("after last seq: %d %v", len(tail), err)
	}
	storeJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&g3, "durable store Events(after=0) re-encoded with encoding/json. Columns are seq, run_id, type, payload, created_at.\n%s\n\n", storeJSON)
	fmt.Fprintf(&g3, "durable store duplicate AppendOutput of the same decoded page\nbefore_seqs: %v\nafter_seqs: %v\nerror: nil\n\n", seqsOf(before), seqsOf(after))
	fmt.Fprintf(&g3, "durable store Events(after=%d) is the continuation after the first log seq\ncount: %d\nseqs: %v\n\n", before[0].Seq, len(rest), seqsOf(rest))
	fmt.Fprintf(&g3, "durable store Events(after=%d) is past the last log seq\ncount: 0\n\n", before[len(before)-1].Seq)

	runActivity(t, env, activities.Finalize, workflow.FinishInput{Input: in})
	status, terminalBody := client.call(http.MethodGet, "/v1/tasks/"+created.ID, f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK {
		t.Fatalf("terminal get: %d %s", status, terminalBody)
	}
	var terminal struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
		Run    struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
			State  string `json:"state"`
			Cursor string `json:"cursor"`
			Exit   struct {
				Code   int    `json:"code"`
				Signal string `json:"signal"`
			} `json:"exit"`
		} `json:"run"`
	}
	if err = json.Unmarshal(terminalBody, &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.State != "FAILED" || terminal.Reason != "AGENT_FAILED" || terminal.Run.State != "FAILED" || terminal.Run.Exit.Code != 1 || terminal.Run.Exit.Signal != "" || terminal.Run.TaskID != created.ID {
		t.Fatalf("terminal = %s", terminalBody)
	}
	fmt.Fprintf(&g4, "HTTP authoritative task record after Finalize\n%s\n", client.format(http.MethodGet, "/v1/tasks/"+created.ID, true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, terminalBody))
	fmt.Fprintf(&g4, "run.cursor is the process-output cursor %q. It is not the log replay cursor and not a Philo cursor.\n\n", terminal.Run.Cursor)

	status, stream := client.call(http.MethodGet, "/v1/tasks/"+created.ID+"/events?after=0", f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK {
		t.Fatalf("sse: %d %s", status, stream)
	}
	if !bytes.Contains(stream, []byte("event: status\n")) || bytes.Contains(stream, []byte("event: status\nid:")) {
		t.Fatalf("status frame = %s", stream)
	}
	if !bytes.Contains(stream, []byte(`"state":"FAILED"`)) || !bytes.Contains(stream, []byte(`"reason":"AGENT_FAILED"`)) {
		t.Fatalf("status payload = %s", stream)
	}
	ids := logIDs(stream)
	if len(ids) != len(before) {
		t.Fatalf("sse ids %v store seqs %v", ids, seqsOf(before))
	}
	fmt.Fprintf(&g3, "HTTP SSE after=0. Raw response body.\n%s\n", client.format(http.MethodGet, "/v1/tasks/"+created.ID+"/events?after=0", true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, stream))
	fmt.Fprintf(&g4, "HTTP SSE status notification from the same response. Log frames are separate and carry id. This frame does not.\n")
	for _, frame := range strings.Split(string(stream), "\n\n") {
		if strings.HasPrefix(frame, "event: status\n") {
			fmt.Fprintf(&g4, "%s\n\n", frame)
		}
	}

	if len(before) >= 2 {
		mid := before[0].Seq
		status, reconnect := client.call(http.MethodGet, "/v1/tasks/"+created.ID+"/events", f.token, "", map[string]string{
			"X-Makez-Workspace-ID": f.workspace, "Last-Event-ID": fmt.Sprintf("%d", mid),
		})
		if status != http.StatusOK {
			t.Fatalf("reconnect: %d %s", status, reconnect)
		}
		got := logIDs(reconnect)
		if len(got) != len(before)-1 || got[0] == fmt.Sprintf("%d", mid) {
			t.Fatalf("reconnect ids %v", got)
		}
		fmt.Fprintf(&g3, "HTTP SSE reconnect, Last-Event-ID %d. The first log id is not repeated.\n%s\n", mid, client.format(http.MethodGet, "/v1/tasks/"+created.ID+"/events", true, map[string]string{
			"X-Makez-Workspace-ID": f.workspace, "Last-Event-ID": fmt.Sprintf("%d", mid),
		}, "", status, reconnect))
	} else {
		fmt.Fprintf(&g3, "NOT RUN: a log cursor strictly between two events. The failure produced %d log event(s), so Last-Event-ID of a middle event had no later log to return.\n\n", len(before))
	}
	last := before[len(before)-1].Seq
	status, headerReplay := client.call(http.MethodGet, "/v1/tasks/"+created.ID+"/events", f.token, "", map[string]string{
		"X-Makez-Workspace-ID": f.workspace, "Last-Event-ID": fmt.Sprintf("%d", last),
	})
	if status != http.StatusOK || len(logIDs(headerReplay)) != 0 || !bytes.Contains(headerReplay, []byte("event: status\n")) {
		t.Fatalf("last-event-id: %d ids=%v body=%s", status, logIDs(headerReplay), headerReplay)
	}
	fmt.Fprintf(&g3, "HTTP SSE reconnect with Last-Event-ID %d, the last stored log. No log frame is repeated.\n%s\n", last, client.format(http.MethodGet, "/v1/tasks/"+created.ID+"/events", true, map[string]string{
		"X-Makez-Workspace-ID": f.workspace, "Last-Event-ID": fmt.Sprintf("%d", last),
	}, "", status, headerReplay))
	status, again := client.call(http.MethodGet, "/v1/tasks/"+created.ID+"/events?after="+fmt.Sprintf("%d", last), f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK || len(logIDs(again)) != 0 || !bytes.Contains(again, []byte("event: status\n")) {
		t.Fatalf("after last: %d ids=%v body=%s", status, logIDs(again), again)
	}
	fmt.Fprintf(&g3, "HTTP SSE after the last log seq. Status remains. No log id is repeated.\n%s\n", client.format(http.MethodGet, "/v1/tasks/"+created.ID+"/events?after="+fmt.Sprintf("%d", last), true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, again))

	restarted := (&Server{DB: f.db, Artifacts: files, Keys: keys, Agents: server.Agents}).Handler()
	restartClient := wireClient{t: t, handler: restarted}
	status, restartedGet := restartClient.call(http.MethodGet, "/v1/tasks/"+created.ID, f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	status2, restartedSSE := restartClient.call(http.MethodGet, "/v1/tasks/"+created.ID+"/events?after=0", f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK || status2 != http.StatusOK || strings.Join(logIDs(restartedSSE), ",") != strings.Join(ids, ",") {
		t.Fatalf("restart get=%d sse=%d ids=%v", status, status2, logIDs(restartedSSE))
	}
	fmt.Fprintf(&g3, "HTTP handler constructed again on the same database. This is not a Pi process restart.\n%s\n%s\n", restartClient.format(http.MethodGet, "/v1/tasks/"+created.ID, true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, restartedGet), restartClient.format(http.MethodGet, "/v1/tasks/"+created.ID+"/events?after=0", true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status2, restartedSSE))

	if err = f.db.Publish(t.Context(), f.workspace, created.ID, core.TaskSucceeded, "SHOULD_NOT_REPLACE", true, core.CleanupDone, nil); err != nil {
		t.Fatal(err)
	}
	status, still := client.call(http.MethodGet, "/v1/tasks/"+created.ID, f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK || !bytes.Equal(still, terminalBody) {
		t.Fatalf("republish changed the task\n%s\n%s", terminalBody, still)
	}
	fmt.Fprintf(&g4, "durable store second Publish(state=SUCCEEDED, reason=SHOULD_NOT_REPLACE) returned nil. The following HTTP get is unchanged.\n%s\n", client.format(http.MethodGet, "/v1/tasks/"+created.ID, true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, still))
	fresh, err := f.db.Get(t.Context(), f.workspace, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = f.db.AppendOutput(t.Context(), fresh.Run.ID, fresh.Run.ObserverEpoch, "after-terminal", []byte(`{}`), []core.AgentEvent{{
		SourceOffset: "after-terminal", Kind: "log", Payload: []byte(`{"stream":"stdout","text":"not-written"}`),
	}})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("append after terminal: %v", err)
	}
	finalEvents, err := f.db.Events(t.Context(), f.workspace, created.ID, 0)
	if err != nil || len(finalEvents) != len(before) || bytes.Contains(mustJSONBytes(t, finalEvents), []byte("not-written")) {
		t.Fatalf("events after rejected append: %v", err)
	}
	fmt.Fprintf(&g3, "durable store AppendOutput after the terminal run returned %v. Event seqs stayed %v. The new text was not stored.\n\n", store.ErrConflict, seqsOf(finalEvents))
	fmt.Fprintf(&g4, "contract mapping, not a Philo record and not executed here: FAILED with reason AGENT_FAILED is the Makez input to Philo message.error / status error. Dedup tuple is workspace_id, task_id, run_id, state, reason. Values from this task: workspace_id=%s task_id=%s run_id=%s state=%s reason=%s. No Philo snapshot was written.\n\n", f.workspace, created.ID, terminal.Run.ID, terminal.State, terminal.Reason)

	status, artifacts := client.call(http.MethodGet, "/v1/tasks/"+created.ID+"/artifacts", f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK || strings.TrimSpace(string(artifacts)) != "[]" {
		t.Fatalf("artifacts: %d %s", status, artifacts)
	}
	stdout, stderr := host.Snapshot()
	if !bytes.Contains(stdout, []byte("No API key found for the selected model")) {
		t.Fatalf("stdout missing rejection:\n%s\nstderr:\n%s", stdout, stderr)
	}
	assertNoModelSuccess(t, before)
	resultRaw, err := os.ReadFile(filepath.Join(host.Workspace(), "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(resultRaw, []byte(`"succeeded":true`)) || !bytes.Contains(resultRaw, []byte(`"succeeded":false`)) {
		t.Fatalf("result = %s", resultRaw)
	}
	dispositionRaw, err := os.ReadFile(filepath.Join(host.Workspace(), "pi-disposition.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dispositionRaw, []byte(`"disposition":"rejected"`)) {
		t.Fatalf("disposition = %s", dispositionRaw)
	}
	instructionRaw, err := os.ReadFile(filepath.Join(host.Workspace(), "pi-instruction.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(instructionRaw, []byte(`"offline":true`)) || bytes.Contains(instructionRaw, []byte(`"restore_checkpoint":true`)) {
		t.Fatalf("instruction = %s", instructionRaw)
	}
	session, err := f.db.EnsureNativeSession(t.Context(), f.workspace, store.SessionIdentity{
		Scope: "personal", BotID: "bot-wire", ConversationID: "conv-wire", HarnessID: piharness.AdapterID, HarnessPin: piharness.Pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := f.db.LoadCheckpoint(t.Context(), f.workspace, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint) != 0 || session.CheckpointBytes != 0 {
		t.Fatalf("checkpoint bytes=%d session=%d", len(checkpoint), session.CheckpointBytes)
	}
	prompt, err := f.db.NativePrompt(t.Context(), f.workspace, session.ID, piharness.PromptHash("makez-no-key-failure"))
	if err != nil || prompt.Disposition != string(piharness.DispositionRejected) {
		t.Fatalf("prompt = %+v err=%v", prompt, err)
	}
	assertRepoUntouched(t)
	for _, raw := range [][]byte{stdout, stderr, resultRaw, dispositionRaw, instructionRaw, stream, terminalBody} {
		rejectSecrets(t, raw, f.token, betaToken, viewerToken)
	}

	fmt.Fprintf(&g5, "native CLI, before the task. Env keys were PATH, HOME, LANG. HOME was an empty temp directory. No API key was passed.\n$ %s --version\n%s\n\n", piBin, version)
	fmt.Fprintf(&g5, "process stdout read by the host-exec double and decoded into log events. python argv0 was rewritten from /usr/local/bin/python3 to %s because that image path is absent on this host. Product argv is unchanged.\n%s\n\n", host.Python(), stdout)
	if len(stderr) != 0 {
		fmt.Fprintf(&g5, "process stderr\n%s\n\n", stderr)
	}
	fmt.Fprintf(&g5, "workspace file result.json, not a public HTTP field\n%s\n\nworkspace file pi-disposition.json, not a public HTTP field\n%s\n\nworkspace file pi-instruction.json after the test double mapped /workspace into a temp directory\n%s\n\n", resultRaw, dispositionRaw, instructionRaw)
	fmt.Fprintf(&g5, "session directory listing under the temp workspace\n%s\ncheckpoint.jsonl copied into the native session: no. stored_bytes: %d\n\n", sessionListing(filepath.Join(host.Workspace(), "pi-session")), len(checkpoint))
	fmt.Fprintf(&g5, "HTTP log events that carry the rejection\n%s\n\nHTTP artifacts\n%s\n\nHTTP terminal task\n%s\n", storeJSON, artifacts, terminalBody)

	writeWireFiles(t, map[string]string{
		"makez-philo-01-submit-and-run.txt":           header(f.workspace, betaWorkspace) + notRunGroup1() + g1.String(),
		"makez-philo-02-auth-forbidden-not-found.txt": header(f.workspace, betaWorkspace) + notRunGroup2() + g2.String(),
		"makez-philo-03-replay-cursor.txt":            header(f.workspace, betaWorkspace) + notRunGroup3() + g3.String(),
		"makez-philo-04-terminal-status.txt":          header(f.workspace, betaWorkspace) + notRunGroup4() + g4.String(),
		"makez-philo-05-pi-no-key-failure.txt":        header(f.workspace, betaWorkspace) + notRunGroup5() + g5.String(),
	})
}

func notRunGroup1() string {
	return "NOT RUN: the Temporal worker did not poll the outbox. Initialize, Prepare, Launch, Observe, and Finalize were the activity functions that worker would call, executed in the SDK activity test environment.\n\n"
}

func notRunGroup2() string {
	return "NOT RUN: owner-to-workspace mapping. No owner field was added. The rejected requests above are the implemented behavior.\nNOT RUN: human Google OAuth. This capture used synthetic machine principals only.\n\n"
}

func notRunGroup3() string {
	return "NOT RUN: Pi process restart. A second Pi process was not started, and this does not show crash recovery.\nNOT RUN: Philo business cursor, outbox, and projection. Those were not generated.\n\n"
}

func notRunGroup4() string {
	return "NOT RUN: a Philo message.error or status snapshot. The mapping sentence is the contract applied to the Makez fields, not an emitted Philo record.\n\n"
}

func notRunGroup5() string {
	return "NOT RUN: a successful model turn, a paid call, a provider login, and a later turn that resumes a checkpoint.\nNOT RUN: OpenSandbox. No sandbox credential was used and no container was created.\n\n"
}

func header(workspace, other string) string {
	commit := os.Getenv("MAKEZ_WIRE_COMMIT")
	if commit == "" {
		commit = "PENDING"
	}
	recorded := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format(time.RFC3339)
	return fmt.Sprintf(`makez_commit: %s
recorded_at: %s
repository: github.com/yosuang/makez
branch: cursor/pi-execution-foundation-da71
database: isolated schema on the local Postgres from MAKEZ_TEST_DATABASE_URL. Not production.
identities: synthetic workspace %s and the other machine workspace %s. Authorization values are Bearer <redacted>.
unit_mocks: none of the blocks below are unit mocks. The host-exec provider is a process double, and each block is labeled HTTP, service, durable store, native CLI, or workspace file.
repro: from the makez module, with GOTOOLCHAIN=local, MAKEZ_TEST_DATABASE_URL set to the postgres:17.11 URL in .github/workflows/lint.yml, MAKEZ_PI_BIN set to the pinned @earendil-works/pi-coding-agent 1.5.1 binary, MAKEZ_WIRE_COMMIT set to this commit, and MAKEZ_WIRE_CAPTURE set to the destination directory, run: go test ./internal/api -run TestNoKeyPiFailureThroughMakez -count=1 -timeout 5m

`, commit, recorded, workspace, other)
}

func writeWireFiles(t *testing.T, files map[string]string) {
	t.Helper()
	dir := os.Getenv("MAKEZ_WIRE_CAPTURE")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if strings.Contains(body, "synthetic-viewer-token-1") || strings.Contains(body, "test-machine-token-") {
			t.Fatalf("capture %s contains a raw token", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type taskSubmit struct {
	Prompt       string          `json:"prompt"`
	Config       json.RawMessage `json:"config,omitempty"`
	AgentID      string          `json:"agent_id,omitempty"`
	AgentVersion string          `json:"agent_version,omitempty"`
	Timeout      int             `json:"timeout_seconds,omitempty"`
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type wireClient struct {
	t       *testing.T
	handler http.Handler
}

func (c wireClient) call(method, target, token, body string, header map[string]string) (int, []byte) {
	c.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range header {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (c wireClient) format(method, target string, auth bool, header map[string]string, body string, status int, resp []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", method, target)
	if auth {
		b.WriteString("Authorization: Bearer <redacted>\n")
	}
	for key, value := range header {
		fmt.Fprintf(&b, "%s: %s\n", key, value)
	}
	if body != "" {
		fmt.Fprintf(&b, "\n%s\n", body)
	}
	fmt.Fprintf(&b, "\nHTTP %d\n%s\n", status, resp)
	return b.String()
}

func runActivity(t *testing.T, env *testsuite.TestActivityEnvironment, fn any, args ...any) {
	t.Helper()
	if _, err := env.ExecuteActivity(fn, args...); err != nil {
		t.Fatal(err)
	}
}

func drainRemaining(t *testing.T, db *store.Store, adapter core.AgentAdapter, host *hostexec.Harness, task store.Task) {
	t.Helper()
	if task.Run == nil || task.Run.Process == nil {
		t.Fatal("run has no process")
	}
	cursor := task.Run.Cursor
	parser := append([]byte(nil), task.Run.ParserState...)
	for range 20 {
		page, err := host.Output(t.Context(), *task.Run.Process, cursor, 64<<10)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := adapter.Decode(page, parser)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.AppendOutput(t.Context(), task.Run.ID, task.Run.ObserverEpoch, page.NextCursor, decoded.NextState, decoded.Events); err != nil {
			t.Fatal(err)
		}
		cursor = page.NextCursor
		parser = append([]byte(nil), decoded.NextState...)
		if page.EOF {
			return
		}
	}
	t.Fatal("output did not reach EOF")
}

func sameSeqs(a, b []store.Event) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Seq != b[i].Seq || a[i].RunID != b[i].RunID || a[i].Type != b[i].Type || !bytes.Equal(a[i].Payload, b[i].Payload) {
			return false
		}
	}
	return true
}

func seqsOf(events []store.Event) []int64 {
	out := make([]int64, len(events))
	for i := range events {
		out[i] = events[i].Seq
	}
	return out
}

func logIDs(body []byte) []string {
	var ids []string
	for frame := range strings.SplitSeq(string(body), "\n\n") {
		line, rest, ok := strings.Cut(frame, "\n")
		if !ok {
			line = frame
		}
		id, found := strings.CutPrefix(line, "id: ")
		if found && !strings.Contains(rest, "\nid: ") {
			ids = append(ids, id)
		}
	}
	return ids
}

func assertNoModelSuccess(t *testing.T, events []store.Event) {
	t.Helper()
	sawRejection := false
	for _, event := range events {
		if event.Type != "log" {
			t.Fatalf("event type %s", event.Type)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		for key := range payload {
			if key == "source_offset" || key == "attempt_generation" || key == "owner_id" {
				t.Fatalf("public payload field %s", key)
			}
		}
		var text struct {
			Stream string `json:"stream"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(event.Payload, &text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text.Text, "agent_end") || strings.Contains(text.Text, "agent_settled") || strings.Contains(text.Text, "text_delta") {
			t.Fatalf("model event in log: %s", text.Text)
		}
		for line := range strings.SplitSeq(text.Text, "\n") {
			if !strings.Contains(line, `"kind":"pi"`) {
				continue
			}
			var wrapped struct {
				Kind   string `json:"kind"`
				Record struct {
					Type    string `json:"type"`
					Success *bool  `json:"success"`
					Error   string `json:"error"`
				} `json:"record"`
			}
			if err := json.Unmarshal([]byte(line), &wrapped); err != nil {
				continue
			}
			if wrapped.Record.Type == "agent_end" || wrapped.Record.Type == "agent_settled" {
				t.Fatalf("record %s", line)
			}
			if wrapped.Record.Success != nil && !*wrapped.Record.Success && strings.Contains(wrapped.Record.Error, "No API key found for the selected model") {
				sawRejection = true
			}
		}
	}
	if !sawRejection {
		t.Fatal("log events did not contain the no-key rejection")
	}
}

func rejectSecrets(t *testing.T, raw []byte, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(raw, []byte(secret)) {
			t.Fatal("evidence contains a credential")
		}
	}
}

func assertRepoUntouched(t *testing.T) {
	t.Helper()
	for _, name := range []string{"makez-pi-run.py", "pi-instruction.json", "pi-release", "pi-disposition.json", "result.json", "pi-history.json", "pi-session"} {
		if _, err := os.Lstat(filepath.Join("/workspace", name)); err == nil {
			t.Fatalf("repository gained %s", name)
		}
	}
}

func pinnedPiBinary(t *testing.T) string {
	t.Helper()
	candidate := os.Getenv("MAKEZ_PI_BIN")
	if candidate == "" {
		candidate = "/tmp/pi-probe-7a73/pkg/node_modules/.bin/pi"
	}
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		t.Skip("pinned Pi 1.5.1 binary is absent; set MAKEZ_PI_BIN")
	}
	return candidate
}

func piVersion(t *testing.T, bin, home string) string {
	t.Helper()
	cmd := exec.Command(bin, "--version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C.UTF-8"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func setMachineRole(t *testing.T, databaseURL, token, role string) error {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	digest := sha256.Sum256([]byte(token))
	id := "machine-" + hex.EncodeToString(digest[:])
	tag, err := conn.Exec(t.Context(), `UPDATE makez_machines SET role=$2 WHERE id=$1`, id, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("machine %s was not updated", id)
	}
	return nil
}

func sessionListing(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "(directory absent)"
		}
		return err.Error()
	}
	if len(entries) == 0 {
		return "(empty)"
	}
	var b strings.Builder
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			fmt.Fprintf(&b, "%s\n", entry.Name())
			continue
		}
		fmt.Fprintf(&b, "%s %d\n", entry.Name(), info.Size())
	}
	return b.String()
}

func newHostExec(t *testing.T) *hostexec.Harness {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	harness, err := hostexec.New(root, home)
	if err != nil {
		t.Fatal(err)
	}
	return harness
}
