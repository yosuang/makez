package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
)

type apiFixture struct {
	databaseURL string
	db          *store.Store
	files       *artifact.Store
	handler     http.Handler
	workspace   string
	token       string
}

func TestAPIHasNoEmbeddedWebConsole(t *testing.T) {
	handler := (&Server{}).Handler()
	for route, want := range map[string]int{"/": http.StatusNotFound, "/healthz": http.StatusOK, "/v1/tasks": http.StatusUnauthorized} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, route, nil))
		if response.Code != want {
			t.Fatalf("%s: status=%d want=%d", route, response.Code, want)
		}
	}
}

func apiTestServer(t *testing.T) *apiFixture {
	t.Helper()
	databaseURL := os.Getenv("MAKEZ_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MAKEZ_TEST_DATABASE_URL for HTTP/PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
		t.Fatal(err)
	}
	schema := "makez_api_test_" + hex.EncodeToString(nonce[:])
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
		t.Fatal(err)
	}
	// Isolate Outbox rows from a concurrently running development dispatcher.
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("remove isolated test schema: %v", err)
		}
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("PostgreSQL integration fixture ready")
	files, err := artifact.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	workspace := "api-test-" + hex.EncodeToString(nonce[:])
	s := &Server{DB: db, Artifacts: files, Keys: map[string]string{"test-machine-token-a": workspace, "test-machine-token-b": workspace + "-other"}, Agents: map[string]core.AgentAdapter{"demo@1": agent.NewDemo()}}
	if err = db.RegisterMachines(ctx, s.Keys); err != nil {
		t.Fatal(err)
	}
	return &apiFixture{databaseURL: u.String(), db: db, files: files, handler: s.Handler(), workspace: workspace, token: "test-machine-token-a"}
}
func (f *apiFixture) request(t *testing.T, method, route, body, key, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, route, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	r.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, r)
	return response
}
func taskResponse(t *testing.T, r *httptest.ResponseRecorder, want int) store.Task {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status=%d want=%d body=%s", r.Code, want, r.Body.String())
	}
	var task store.Task
	if err := json.Unmarshal(r.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	return task
}
func (f *apiFixture) submit(t *testing.T, key string) store.Task {
	t.Helper()
	return taskResponse(t, f.request(t, "POST", "/v1/tasks", `{"prompt":"test HTTP task"}`, key, f.token), 202)
}

func TestAPIIdempotencyAndDefaults(t *testing.T) {
	f := apiTestServer(t)
	task := f.submit(t, "same-request")
	if task.AgentID != "demo" || task.AgentVersion != "1" || task.ProfileID != "default" || task.ProfileVersion != "1" || task.State != core.TaskQueued {
		t.Fatalf("bad defaults: %+v", task)
	}
	if remaining := time.Until(task.Deadline); remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatalf("default deadline %v", remaining)
	}
	retry := taskResponse(t, f.request(t, "POST", "/v1/tasks", `{"prompt":"test HTTP task"}`, "same-request", f.token), 200)
	if retry.ID != task.ID || !retry.Deadline.Equal(task.Deadline) {
		t.Fatalf("idempotency changed task/deadline: %+v", retry)
	}
	if got := f.request(t, "POST", "/v1/tasks", `{"prompt":"different"}`, "same-request", f.token); got.Code != 409 {
		t.Fatalf("different request status=%d body=%s", got.Code, got.Body.String())
	}
	deadline := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	body, _ := json.Marshal(map[string]any{"prompt": "explicit deadline", "deadline": deadline})
	explicit := taskResponse(t, f.request(t, "POST", "/v1/tasks", string(body), "explicit-deadline", f.token), 202)
	if !explicit.Deadline.Equal(deadline) {
		t.Fatalf("explicit deadline changed: %+v", explicit)
	}
}

func TestAPIAuthIsolationAndQueuedCancel(t *testing.T) {
	f := apiTestServer(t)
	task := f.submit(t, "isolation")
	for _, token := range []string{"", "invalid"} {
		if got := f.request(t, "GET", "/v1/tasks", "", "", token); got.Code != 401 {
			t.Fatalf("bad token status=%d", got.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	r.Header.Set("Authorization", f.token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, r)
	if response.Code != 401 {
		t.Fatalf("Bearer prefix required, got %d", response.Code)
	}
	for _, route := range []string{"/v1/tasks/" + task.ID, "/v1/tasks/" + task.ID + "/artifacts", "/v1/tasks/" + task.ID + "/events"} {
		if got := f.request(t, "GET", route, "", "", "test-machine-token-b"); got.Code != 404 {
			t.Fatalf("foreign route %s status=%d", route, got.Code)
		}
	}
	if got := f.request(t, "POST", "/v1/tasks/"+task.ID+"/cancel", "", "", "test-machine-token-b"); got.Code != 404 {
		t.Fatalf("foreign cancel status=%d", got.Code)
	}
	if got := f.request(t, "GET", "/v1/tasks", "", "", "test-machine-token-b"); got.Code != 200 || strings.TrimSpace(got.Body.String()) != "[]" {
		t.Fatalf("foreign list: %d %s", got.Code, got.Body.String())
	}
	canceled := taskResponse(t, f.request(t, "POST", "/v1/tasks/"+task.ID+"/cancel", "", "", f.token), 200)
	if canceled.State != core.TaskCanceled || canceled.Run != nil {
		t.Fatalf("queued cancel: %+v", canceled)
	}
	again := taskResponse(t, f.request(t, "POST", "/v1/tasks/"+task.ID+"/cancel", "", "", f.token), 200)
	if again.State != canceled.State {
		t.Fatal("cancel not idempotent")
	}
}

func TestAPIValidation(t *testing.T) {
	f := apiTestServer(t)
	cases := []struct{ name, body, key string }{
		{"missing key", `{"prompt":"x"}`, ""},
		{"malformed", `{"prompt":`, "malformed"},
		{"extra JSON", `{"prompt":"x"} {}`, "extra"},
		{"unknown field", `{"prompt":"x","command":"rm"}`, "field"},
		{"blank prompt", `{"prompt":"  "}`, "blank"},
		{"unknown agent", `{"prompt":"x","agent_id":"other"}`, "agent"},
		{"agent version", `{"prompt":"x","agent_version":"2"}`, "version"},
		{"profile", `{"prompt":"x","profile_id":"privileged"}`, "profile"},
		{"profile version", `{"prompt":"x","profile_version":"2"}`, "profileversion"},
		{"delay config", `{"prompt":"x","config":{"delay_seconds":301}}`, "delay"},
		{"config command", `{"prompt":"x","config":{"command":"something"}}`, "config"},
		{"timeout", `{"prompt":"x","timeout_seconds":1}`, "timeout"},
		{"negative timeout", `{"prompt":"x","timeout_seconds":-5}`, "negative"},
		{"both deadline timeout", fmt.Sprintf(`{"prompt":"x","deadline":%q,"timeout_seconds":100}`, time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339)), "both"},
		{"large prompt", `{"prompt":"` + strings.Repeat("a", 65537) + `"}`, "large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := f.request(t, "POST", "/v1/tasks", tc.body, tc.key, f.token)
			if got.Code != 400 {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
	if got := f.request(t, "POST", "/v1/tasks", `{"prompt":"null config","config":null}`, "null", f.token); got.Code != 202 && got.Code != 400 {
		t.Fatalf("null config returned server error %d: %s", got.Code, got.Body.String())
	}
}

type stringReader string

func (s stringReader) Read(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(s))), nil
}

func TestAPIArtifactAndEventReplay(t *testing.T) {
	f := apiTestServer(t)
	ctx := context.Background()
	task := f.submit(t, "events")
	task, err := f.db.InitializeRun(ctx, f.workspace, task.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := f.db.ClaimObserver(ctx, task.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.AppendOutput(ctx, task.Run.ID, epoch, "5", nil, []core.AgentEvent{{SourceOffset: "0", Kind: "log", Payload: json.RawMessage(`{"text":"hello\nworld"}`)}}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []core.RunState{core.RunStopping, core.RunFinalizing} {
		if err = f.db.UpdateRun(ctx, task.Run.ID, store.RunUpdate{State: &state}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := f.files.Archive(ctx, task.Run.ID, stringReader("result content"), []core.ArtifactCandidate{{Path: "result.txt", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	var artifacts []store.Artifact
	for _, a := range records {
		artifacts = append(artifacts, store.Artifact{Path: strings.TrimPrefix(a.LogicalPath, "/workspace/"), StorageKey: a.StorageKey, SHA256: a.SHA256, Size: a.Size, MediaType: a.MediaType})
	}
	if err = f.db.Publish(ctx, f.workspace, task.ID, core.TaskSucceeded, "", true, core.CleanupDone, artifacts); err != nil {
		t.Fatal(err)
	}
	list := f.request(t, "GET", "/v1/tasks/"+task.ID+"/artifacts", "", "", f.token)
	if list.Code != 200 || bytes.Contains(list.Body.Bytes(), []byte("storage_key")) {
		t.Fatalf("artifact list: %d %s", list.Code, list.Body.String())
	}
	var published []store.Artifact
	if err = json.Unmarshal(list.Body.Bytes(), &published); err != nil || len(published) != 1 {
		t.Fatalf("artifact decode %+v %v", published, err)
	}
	route := "/v1/artifacts/" + published[0].ID + "/content"
	download := f.request(t, "GET", route, "", "", f.token)
	if download.Code != 200 || download.Body.String() != "result content" || !strings.HasPrefix(download.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("download: %d %s", download.Code, download.Body.String())
	}
	if got := f.request(t, "GET", route, "", "", "test-machine-token-b"); got.Code != 404 {
		t.Fatalf("foreign download=%d", got.Code)
	}
	events := f.request(t, "GET", "/v1/tasks/"+task.ID+"/events", "", "", f.token)
	if events.Code != 200 || events.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(events.Body.String(), "event: log\ndata: {") || !strings.Contains(events.Body.String(), "event: status\n") || !strings.Contains(events.Body.String(), "\\nworld") {
		t.Fatalf("SSE framing: %d %s", events.Code, events.Body.String())
	}
	saved, err := f.db.Events(ctx, f.workspace, task.ID, 0)
	if err != nil || len(saved) != 1 {
		t.Fatalf("event lookup %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Last-Event-ID", fmt.Sprint(saved[0].Seq))
	replay := httptest.NewRecorder()
	f.handler.ServeHTTP(replay, req)
	if strings.Contains(replay.Body.String(), "event: log") || !strings.Contains(replay.Body.String(), "event: status") {
		t.Fatalf("replay duplicated event: %s", replay.Body.String())
	}
	if got := f.request(t, "GET", "/v1/tasks/"+task.ID+"/events?after=-1", "", "", f.token); got.Code != 400 {
		t.Fatalf("negative cursor status=%d", got.Code)
	}
}
