package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/piharness"
	"github.com/makez-dev/makez/internal/store"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/protobuf/types/known/durationpb"
)

// TestNoKeyPiFailureThroughListeningMakez is the listen path from contract
// section 6.9 with the sandbox port replaced. makez api and makez worker are
// real processes. The worker binary is built with -tags makezhostexec and
// MAKEZ_TEST_HOST_EXEC, so its sandbox provider is the host process fake.
// That fake is a substitute for OpenSandbox. The default build does not
// contain it. The child is pinned Pi 1.5.1 with no key.
func TestNoKeyPiFailureThroughListeningMakez(t *testing.T) {
	piBin := pinnedPiBinary(t)
	versionHome := t.TempDir()
	version := piVersion(t, piBin, versionHome)
	if version != piharness.PackageVersion {
		t.Fatalf("pi --version = %q", version)
	}
	temporalAddr := os.Getenv("MAKEZ_TEMPORAL_ADDRESS")
	if temporalAddr == "" {
		temporalAddr = "127.0.0.1:17233"
	}
	probe, err := net.DialTimeout("tcp", temporalAddr, time.Second)
	if err != nil {
		t.Skip("temporal is not listening at " + temporalAddr)
	}
	_ = probe.Close()
	if _, err = exec.LookPath("docker"); err != nil {
		t.Fatal("docker is required to prove no container was created")
	}
	beforeContainers := containerIDs(t)
	f := apiTestServer(t)
	namespace := "listen" + strings.TrimPrefix(f.workspace, "api-test")
	registerNamespace(t, temporalAddr, namespace)

	rootOut, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "makez")
	buildCmd := exec.Command("go", "build", "-buildvcs=false", "-tags", "makezhostexec", "-o", bin, "./cmd/makez")
	buildCmd.Dir = strings.TrimSpace(string(rootOut))
	buildCmd.Env = withoutTags(os.Environ())
	if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("build tagged makez: %v\n%s", buildErr, out)
	}

	harnessDir := t.TempDir()
	t.Cleanup(func() { killCwdUnder(harnessDir) })
	artifactDir := t.TempDir()
	apiAddr := freeLoopback(t)
	workerAddr := freeLoopback(t)
	keys, err := json.Marshal(map[string]string{f.token: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	piConfig, err := json.Marshal(map[string]string{"binary": piBin})
	if err != nil {
		t.Fatal(err)
	}
	common := map[string]string{
		"MAKEZ_DATABASE_URL":       f.databaseURL,
		"MAKEZ_TEMPORAL_ADDRESS":   temporalAddr,
		"MAKEZ_TEMPORAL_NAMESPACE": namespace,
		"MAKEZ_PI_AGENT":           string(piConfig),
		"MAKEZ_ARTIFACT_DIR":       artifactDir,
	}
	workerEnv := listenEnv(t, common)
	workerEnv = append(workerEnv, "MAKEZ_TEST_HOST_EXEC="+harnessDir, "MAKEZ_OPERATIONS_LISTEN="+workerAddr)
	apiEnv := listenEnv(t, common)
	apiEnv = append(apiEnv, "MAKEZ_API_KEYS="+string(keys), "MAKEZ_LISTEN="+apiAddr)
	for _, entry := range workerEnv {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "MAKEZ_SANDBOX_API_KEY", "MAKEZ_SANDBOX_URL", "MAKEZ_CONTROL_KEY", "MAKEZ_SECRETS_FILE", "MAKEZ_API_KEYS":
			t.Fatalf("worker env contains %s", name)
		}
	}
	worker := startMakez(t, bin, "worker", workerEnv)
	assertWorkerEnv(t, worker.cmd.Process.Pid)
	httpClient := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}
	waitReady(t, httpClient, "http://"+workerAddr+"/readyz", worker, f)
	apiProc := startMakez(t, bin, "api", apiEnv)
	base := "http://" + apiAddr
	waitReady(t, httpClient, base+"/healthz", apiProc, f)
	waitReady(t, httpClient, base+"/readyz", apiProc, f)

	var exchange strings.Builder
	record := func(format string, args ...any) {
		fmt.Fprintf(&exchange, format, args...)
	}
	call := func(method, target, body string, header map[string]string) (int, []byte) {
		t.Helper()
		status, resp := liveCall(t, httpClient, method, base+target, f.token, body, header)
		fmt.Fprintf(&exchange, "%s\n", formatLive(method, base+target, true, header, body, status, resp))
		return status, resp
	}
	record("health\n%s\n", formatLive(http.MethodGet, base+"/healthz", false, nil, "", http.StatusOK, readReady(t, httpClient, base+"/healthz")))
	record("ready, API process\n%s\n", formatLive(http.MethodGet, base+"/readyz", false, nil, "", http.StatusOK, readReady(t, httpClient, base+"/readyz")))
	record("ready, worker process\n%s\n", formatLive(http.MethodGet, "http://"+workerAddr+"/readyz", false, nil, "", http.StatusOK, readReady(t, httpClient, "http://"+workerAddr+"/readyz")))

	ownerBody := `{"prompt":"no owner","timeout_seconds":180,"owner_id":"not-a-field"}`
	status, body := call(http.MethodPost, "/v1/tasks", ownerBody, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "listen-owner-1", "X-Makez-Workspace-ID": f.workspace,
	})
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte(`"error":"invalid request JSON"`)) {
		t.Fatalf("owner: %d %s", status, body)
	}
	retentionBody := `{"prompt":"no retention","timeout_seconds":180,"config":{"retention":"1h"}}`
	status, body = call(http.MethodPost, "/v1/tasks", retentionBody, map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "listen-retention-1", "X-Makez-Workspace-ID": f.workspace,
	})
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte(`"error":"invalid agent configuration"`)) {
		t.Fatalf("retention: %d %s", status, body)
	}

	config := fmt.Sprintf(`{"execution":{"contract":%q,"scope":"personal","bot_id":"bot-listen","conversation_id":"conv-listen","harness_pin":%q}}`, piharness.Contract, piharness.Pin)
	piBody := mustJSON(t, taskSubmit{Prompt: "makez-no-key-failure", Config: json.RawMessage(config), AgentID: "pi", AgentVersion: "1", Timeout: 180})
	piHeaders := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "listen-no-key-1", "X-Makez-Workspace-ID": f.workspace}
	since := time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	status, body = call(http.MethodPost, "/v1/tasks", piBody, piHeaders)
	if status != http.StatusAccepted {
		t.Fatalf("submit: %d %s\nworker:\n%s\napi:\n%s", status, body, worker.redacted(f), apiProc.redacted(f))
	}
	var created struct {
		ID      string `json:"id"`
		AgentID string `json:"agent_id"`
		State   string `json:"state"`
	}
	if err = json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.AgentID != "pi" || created.State != "QUEUED" || created.ID == "" {
		t.Fatalf("created = %s", body)
	}
	status, replay := call(http.MethodPost, "/v1/tasks", piBody, piHeaders)
	if status != http.StatusOK || !bytes.Contains(replay, []byte(created.ID)) {
		t.Fatalf("replay: %d %s", status, replay)
	}
	conflictBody := mustJSON(t, taskSubmit{Prompt: "different-prompt", Config: json.RawMessage(config), AgentID: "pi", AgentVersion: "1", Timeout: 180})
	status, conflict := call(http.MethodPost, "/v1/tasks", conflictBody, piHeaders)
	if status != http.StatusConflict {
		t.Fatalf("conflict: %d %s", status, conflict)
	}

	var terminalBody []byte
	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, terminalBody = liveCall(t, httpClient, http.MethodGet, base+"/v1/tasks/"+created.ID, f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
		if status == http.StatusOK && bytes.Contains(terminalBody, []byte(`"state":"FAILED"`)) {
			if bytes.Contains(terminalBody, []byte(`"reason":"AGENT_FAILED"`)) && bytes.Contains(terminalBody, []byte(`"cleanup_state":"DONE"`)) {
				break
			}
			if !bytes.Contains(terminalBody, []byte(`"reason":"AGENT_FAILED"`)) {
				t.Fatalf("terminal\n%s\nworker:\n%s\napi:\n%s", terminalBody, worker.redacted(f), apiProc.redacted(f))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out\n%d %s\nworker:\n%s\napi:\n%s", status, terminalBody, worker.redacted(f), apiProc.redacted(f))
		}
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Fprintf(&exchange, "authoritative GET\n%s\n", formatLive(http.MethodGet, base+"/v1/tasks/"+created.ID, true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, terminalBody))
	var terminal struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
		Run    struct {
			ID      string `json:"id"`
			State   string `json:"state"`
			Backend string `json:"backend_id"`
			Cleanup string `json:"cleanup_state"`
			Cursor  string `json:"cursor"`
			Exit    *struct {
				Code   int    `json:"Code"`
				Signal string `json:"Signal"`
			} `json:"exit"`
			Sandbox *struct {
				ID string `json:"ID"`
			} `json:"sandbox"`
			Process *struct {
				ID string `json:"ID"`
			} `json:"process"`
		} `json:"run"`
	}
	if err = json.Unmarshal(terminalBody, &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.State != "FAILED" || terminal.Reason != "AGENT_FAILED" || terminal.Run.State != "FAILED" || terminal.Run.Backend != "host-exec-test" || terminal.Run.Cleanup != "DONE" || terminal.Run.Exit == nil || terminal.Run.Exit.Code != 1 || terminal.Run.Exit.Signal != "" || terminal.Run.Sandbox == nil || terminal.Run.Sandbox.ID != "host-exec" || terminal.Run.Process == nil || terminal.Run.Process.ID != "host-proc-1" || bytes.Contains(terminalBody, []byte("opensandbox")) {
		t.Fatalf("terminal = %s", terminalBody)
	}

	status, stream := liveCall(t, httpClient, http.MethodGet, base+"/v1/tasks/"+created.ID+"/events?after=0", f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK {
		t.Fatalf("sse: %d %s", status, stream)
	}
	fmt.Fprintf(&exchange, "SSE after=0\n%s\n", formatLive(http.MethodGet, base+"/v1/tasks/"+created.ID+"/events?after=0", true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, stream))
	if !bytes.Contains(stream, []byte("event: log\n")) || !bytes.Contains(stream, []byte("No API key found for the selected model")) {
		t.Fatalf("sse missing rejection: %s", stream)
	}
	if !bytes.Contains(stream, []byte("event: status\n")) || bytes.Contains(stream, []byte("event: status\nid:")) {
		t.Fatalf("status frame = %s", stream)
	}
	logs := logEventsFromSSE(t, stream)
	assertNoModelSuccess(t, logs)
	if len(logs) == 0 {
		t.Fatal("no log events")
	}
	last := logs[len(logs)-1].Seq
	status, again := liveCall(t, httpClient, http.MethodGet, base+"/v1/tasks/"+created.ID+"/events?after="+fmt.Sprintf("%d", last), f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK || len(logIDs(again)) != 0 || !bytes.Contains(again, []byte("event: status\n")) {
		t.Fatalf("after last: %d %s", status, again)
	}
	fmt.Fprintf(&exchange, "SSE after the last log seq\n%s\n", formatLive(http.MethodGet, base+"/v1/tasks/"+created.ID+"/events?after="+fmt.Sprintf("%d", last), true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, again))
	resumeHeaders := map[string]string{"X-Makez-Workspace-ID": f.workspace, "Last-Event-ID": fmt.Sprintf("%d", last)}
	status, resumed := liveCall(t, httpClient, http.MethodGet, base+"/v1/tasks/"+created.ID+"/events", f.token, "", resumeHeaders)
	if status != http.StatusOK || len(logIDs(resumed)) != 0 || !bytes.Contains(resumed, []byte("event: status\n")) {
		t.Fatalf("Last-Event-ID: %d %s", status, resumed)
	}
	fmt.Fprintf(&exchange, "SSE Last-Event-ID %d\n%s\n", last, formatLive(http.MethodGet, base+"/v1/tasks/"+created.ID+"/events", true, resumeHeaders, "", status, resumed))

	status, artifacts := liveCall(t, httpClient, http.MethodGet, base+"/v1/tasks/"+created.ID+"/artifacts", f.token, "", map[string]string{"X-Makez-Workspace-ID": f.workspace})
	if status != http.StatusOK || strings.TrimSpace(string(artifacts)) != "[]" {
		t.Fatalf("artifacts: %d %s", status, artifacts)
	}
	fmt.Fprintf(&exchange, "artifacts\n%s\n", formatLive(http.MethodGet, base+"/v1/tasks/"+created.ID+"/artifacts", true, map[string]string{"X-Makez-Workspace-ID": f.workspace}, "", status, artifacts))

	workspace := filepath.Join(harnessDir, "workspace")
	resultRaw := mustRead(t, filepath.Join(workspace, "result.json"))
	dispositionRaw := mustRead(t, filepath.Join(workspace, "pi-disposition.json"))
	instructionRaw := mustRead(t, filepath.Join(workspace, "pi-instruction.json"))
	if bytes.Contains(resultRaw, []byte(`"succeeded":true`)) || !bytes.Contains(resultRaw, []byte(`"succeeded":false`)) {
		t.Fatalf("result = %s", resultRaw)
	}
	if !bytes.Contains(dispositionRaw, []byte(`"disposition":"rejected"`)) {
		t.Fatalf("disposition = %s", dispositionRaw)
	}
	if !bytes.Contains(instructionRaw, []byte(`"offline":true`)) || bytes.Contains(instructionRaw, []byte(`"restore_checkpoint":true`)) {
		t.Fatalf("instruction = %s", instructionRaw)
	}
	assertRepoUntouched(t)
	session, err := f.db.EnsureNativeSession(t.Context(), f.workspace, store.SessionIdentity{
		Scope: "personal", BotID: "bot-listen", ConversationID: "conv-listen", HarnessID: piharness.AdapterID, HarnessPin: piharness.Pin,
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
	stored, err := f.db.Events(t.Context(), f.workspace, created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertNoModelSuccess(t, stored)

	until := time.Now().Add(time.Second).UTC().Format(time.RFC3339)
	eventsCmd := exec.Command("docker", "events", "--since", since, "--until", until, "--filter", "type=container", "--format", "{{.Action}}")
	eventsOut, eventsErr := eventsCmd.CombinedOutput()
	if eventsErr != nil {
		t.Fatalf("docker events: %v\n%s", eventsErr, eventsOut)
	}
	for _, action := range strings.Fields(string(eventsOut)) {
		if action == "create" {
			t.Fatalf("a container was created:\n%s", eventsOut)
		}
	}
	afterContainers := containerIDs(t)
	if strings.Join(beforeContainers, "\n") != strings.Join(afterContainers, "\n") {
		t.Fatalf("container list changed\nbefore:\n%s\nafter:\n%s", strings.Join(beforeContainers, "\n"), strings.Join(afterContainers, "\n"))
	}

	commit := strings.TrimSpace(commandOutput(t, "git", "rev-parse", "HEAD"))
	statusText := strings.TrimSpace(commandOutput(t, "git", "status", "--porcelain"))
	if statusText == "" {
		statusText = "clean"
	}
	pkgVersion := packageVersion(piBin)
	header := fmt.Sprintf(`makez_commit: %s
git_status: %s
recorded_at: %s
repository: github.com/yosuang/makez
listeners: makez api %s, makez worker %s. Both bound to 127.0.0.1. The binary was produced by: go build -tags makezhostexec -o makez ./cmd/makez
sandbox: the worker used the host process fake selected by MAKEZ_TEST_HOST_EXEC. That fake is a substitute for OpenSandbox. MAKEZ_SANDBOX_URL, MAKEZ_SANDBOX_API_KEY, and MAKEZ_CONTROL_KEY were unset. No sandbox credential was read. backend_id host-exec-test is the fake, not a created sandbox.
database: isolated schema on local Postgres from MAKEZ_TEST_DATABASE_URL. Not production.
temporal: %s namespace %s queue makez-v1. The namespace was created for this run.
pi: %s --version printed %s. That command does not print the git SHA. package.json version: %s. Pin required by the task config: %s.
identities: synthetic workspace %s. Authorization values are Bearer <redacted>.
unit_mocks: none of the blocks below are unit mocks or direct activity calls. The worker process polled Temporal. The API process dispatched the outbox.

NOT RUN: OpenSandbox. No container was created. docker events reported no container create. The container id list did not change.
NOT RUN: a model API call, a paid model, a provider login, and human OAuth.
NOT RUN: an omitted-agent task. This fake holds one process, and the capture submits one executing task.
NOT RUN: a second Pi process, owner binding, retention, and source_offset.
NOT RUN: a Philo snapshot. The HTTP records below are the Makez side.

`, commit, oneLine(statusText), time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format(time.RFC3339), apiAddr, workerAddr, temporalAddr, namespace, piBin, version, pkgVersion, piharness.Pin, f.workspace)
	bodyText := header + exchange.String() + fmt.Sprintf("workspace file result.json\n%s\n\nworkspace file pi-disposition.json\n%s\n\nworkspace file pi-instruction.json after the fake mapped /workspace into a private directory\n%s\n\nsession directory\n%s\ncheckpoint stored_bytes: %d\n\ndocker container ids unchanged: %d\n", resultRaw, dispositionRaw, instructionRaw, sessionListing(filepath.Join(workspace, "pi-session")), len(checkpoint), len(afterContainers))
	for _, secret := range secretValues(f.databaseURL, f.token) {
		if secret != "" && strings.Contains(bodyText, secret) {
			t.Fatal("capture contains a credential")
		}
	}
	dir := os.Getenv("MAKEZ_WIRE_CAPTURE")
	if dir == "" {
		return
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "makez-listen-no-key.txt"), []byte(bodyText), 0o644); err != nil {
		t.Fatal(err)
	}
}

func registerNamespace(t *testing.T, addr, name string) {
	t.Helper()
	ns, err := client.NewNamespaceClient(client.Options{HostPort: addr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ns.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err = ns.Register(ctx, &workflowservice.RegisterNamespaceRequest{
		Namespace:                        name,
		WorkflowExecutionRetentionPeriod: durationpb.New(24 * time.Hour),
	})
	if err != nil && !strings.Contains(err.Error(), "Namespace already exists") {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err = ns.Describe(ctx, name)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func listenEnv(t *testing.T, values map[string]string) []string {
	t.Helper()
	path := listenPath(t)
	env := []string{
		"PATH=" + path,
		"HOME=" + t.TempDir(),
		"LANG=C.UTF-8",
		"TMPDIR=" + t.TempDir(),
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch name {
		case "MAKEZ_SANDBOX_API_KEY", "MAKEZ_SANDBOX_URL", "MAKEZ_CONTROL_KEY", "MAKEZ_SECRETS_FILE":
			t.Fatalf("refusing %s", name)
		}
		env = append(env, name+"="+values[name])
	}
	return env
}

func listenPath(t *testing.T) string {
	t.Helper()
	dirs := []string{"/usr/bin", "/bin"}
	for _, name := range []string{"python3", "node"} {
		found, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append([]string{filepath.Dir(found)}, dirs...)
	}
	return strings.Join(dirs, ":")
}

type makezProc struct {
	cmd    *exec.Cmd
	log    string
	exited chan struct{}
}

func startMakez(t *testing.T, bin, mode string, env []string) *makezProc {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), mode+".log")
	file, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, mode)
	cmd.Env = env
	cmd.Stdout = file
	cmd.Stderr = file
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	proc := &makezProc{cmd: cmd, log: logPath, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		_ = file.Close()
		close(proc.exited)
	}()
	t.Cleanup(func() { proc.stop() })
	return proc
}

func (p *makezProc) stop() {
	if p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.exited
	}
}

func (p *makezProc) redacted(f *apiFixture) string {
	raw, err := os.ReadFile(p.log)
	if err != nil {
		return err.Error()
	}
	text := string(raw)
	for _, secret := range secretValues(f.databaseURL, f.token) {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "<redacted>")
		}
	}
	return text
}

func assertWorkerEnv(t *testing.T, pid int) {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, entry := range bytes.Split(raw, []byte{0}) {
		name, _, _ := bytes.Cut(entry, []byte("="))
		key := string(name)
		found[key] = true
		switch key {
		case "MAKEZ_SANDBOX_API_KEY", "MAKEZ_SANDBOX_URL", "MAKEZ_CONTROL_KEY", "MAKEZ_SECRETS_FILE", "MAKEZ_API_KEYS":
			t.Fatalf("worker process contains %s", key)
		}
	}
	if !found["MAKEZ_TEST_HOST_EXEC"] {
		t.Fatal("worker process is missing the test harness")
	}
}

func waitReady(t *testing.T, client *http.Client, target string, proc *makezProc, f *apiFixture) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		select {
		case <-proc.exited:
			t.Fatalf("%s exited\n%s", target, proc.redacted(f))
		default:
		}
		resp, err := client.Get(target)
		if err == nil && resp.StatusCode == http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s not ready: %v\n%s", target, last, proc.redacted(f))
}

func killCwdUnder(root string) {
	root = filepath.Clean(root)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || pid == os.Getpid() {
			continue
		}
		cwd, readErr := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if readErr != nil {
			continue
		}
		cwd = filepath.Clean(cwd)
		if cwd != root && !strings.HasPrefix(cwd, root+string(filepath.Separator)) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func readReady(t *testing.T, client *http.Client, target string) []byte {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %s", target, resp.StatusCode, body)
	}
	return body
}

func liveCall(t *testing.T, client *http.Client, method, target, token, body string, header map[string]string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range header {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, respBody
}

func formatLive(method, target string, auth bool, header map[string]string, body string, status int, resp []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", method, target)
	if auth {
		b.WriteString("Authorization: Bearer <redacted>\n")
	}
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "%s: %s\n", key, header[key])
	}
	if body != "" {
		fmt.Fprintf(&b, "\n%s\n", body)
	}
	fmt.Fprintf(&b, "\nHTTP %d\n%s\n", status, resp)
	return b.String()
}

func logEventsFromSSE(t *testing.T, body []byte) []store.Event {
	t.Helper()
	var events []store.Event
	for frame := range strings.SplitSeq(string(body), "\n\n") {
		if !strings.Contains(frame, "event: log\n") {
			continue
		}
		_, data, ok := strings.Cut(frame, "data: ")
		if !ok {
			t.Fatalf("log frame: %s", frame)
		}
		var event store.Event
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func freeLoopback(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func containerIDs(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--no-trunc").Output()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			ids = append(ids, line)
		}
	}
	sort.Strings(ids)
	return ids
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func commandOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return string(out)
}

func packageVersion(piBin string) string {
	pkg := filepath.Join(filepath.Dir(filepath.Dir(piBin)), "@earendil-works", "pi-coding-agent", "package.json")
	data, err := os.ReadFile(pkg)
	if err != nil {
		return "not found beside the binary"
	}
	var body struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &body) != nil || body.Version == "" {
		return "unreadable"
	}
	return body.Version
}

func secretValues(rawURL, token string) []string {
	values := []string{token}
	parsed, err := url.Parse(rawURL)
	if err == nil {
		if password, ok := parsed.User.Password(); ok {
			values = append(values, password)
		}
	}
	return values
}

func withoutTags(env []string) []string {
	next := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, "GOFLAGS=") {
			continue
		}
		next = append(next, entry)
	}
	return append(next, "GOFLAGS=")
}

func oneLine(value string) string {
	return strings.ReplaceAll(value, "\n", " | ")
}
