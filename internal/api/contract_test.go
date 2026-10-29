package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/api/docs"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
	"github.com/makez-dev/makez/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRoutesAndStrictInput(t *testing.T) {
	f := apiTestServer(t)
	handler := (&Server{DB: f.db, Keys: map[string]string{f.token: f.workspace}, AllowedOrigins: []string{"https://console.example"}, Agents: map[string]core.AgentAdapter{"demo@1": agent.NewDemo()}}).Handler()
	for _, tc := range []struct {
		method, path, body, token string
		status                    int
	}{
		{"GET", "/missing", "", "", 404}, {"GET", "/v1/missing", "", "", 401}, {"GET", "/v1/missing", "", "test-machine-token-a", 404},
		{"PUT", "/v1/tasks", "", "test-machine-token-a", 405}, {"OPTIONS", "/v1/agents", "", "", 204},
		{"GET", "/v1/agents/", "", "test-machine-token-a", 404}, {"HEAD", "/v1/agents", "", "test-machine-token-a", 200},
		{"POST", "/v1/tasks", `{"prompt":"x","unknown":1}`, "test-machine-token-a", 400},
		{"POST", "/v1/tasks", `{"prompt":"x"} {}`, "test-machine-token-a", 400},
		{"POST", "/v1/tasks", `{"prompt":"` + strings.Repeat("x", 128<<10) + `"}`, "test-machine-token-a", 400},
	} {
		t.Run(tc.method+tc.path+tc.body[:min(20, len(tc.body))], func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set("Idempotency-Key", "test")
			req.Header.Set("X-Request-ID", "contract-123")
			req.Header.Set("Origin", "https://console.example")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Location") != "" {
				t.Fatal("unexpected redirect")
			}
			if rec.Header().Get("X-Request-ID") != "contract-123" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing shared headers")
			}
			if rec.Header().Get("Access-Control-Allow-Origin") != "https://console.example" {
				t.Fatal("missing browser CORS origin")
			}
			if tc.method == http.MethodOptions {
				allow := rec.Header().Get("Access-Control-Allow-Headers")
				for _, header := range []string{"Authorization", "Content-Type", "Idempotency-Key", "Last-Event-ID"} {
					if !strings.Contains(allow, header) {
						t.Fatalf("preflight missing %s", header)
					}
				}
			}
			if tc.method == "HEAD" && rec.Body.Len() != 0 {
				t.Fatal("HEAD included a body")
			}
		})
	}
}

func TestDocumentationAccessAndReadiness(t *testing.T) {
	for _, development := range []bool{false, true} {
		s := &Server{Development: development, Metrics: newTestMetrics(t), Ready: func(context.Context) error { return nil }}
		handler := s.Handler()
		for _, tc := range []struct {
			path, remote string
			status       int
		}{
			{"/swagger/doc.json", "192.0.2.1:1234", 404}, {"/readyz", "127.0.0.1:1234", 200},
		} {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.RemoteAddr = tc.remote
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("%s: %d", tc.path, rec.Code)
			}
		}
		req := httptest.NewRequest(http.MethodGet, "/swagger/doc.json", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := 404
		if development {
			want = 200
		}
		if rec.Code != want {
			t.Fatalf("development=%v status=%d", development, rec.Code)
		}
		s.Ready = func(context.Context) error { return errors.New("dependency failed") }
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != 503 {
			t.Fatal("unhealthy dependency reported ready")
		}
	}
	s := &Server{}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 404 {
		t.Fatal("unexpected metrics scrape endpoint")
	}
}

func TestSwaggerMatchesWireNamesAndRoutes(t *testing.T) {
	var spec struct {
		Swagger     string
		Paths       map[string]map[string]any
		Definitions map[string]json.RawMessage
	}
	if err := json.Unmarshal([]byte(docs.SwaggerInfo.ReadDoc()), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Swagger != "2.0" || len(spec.Paths) != 16 {
		t.Fatalf("unexpected spec version/paths: %s %d", spec.Swagger, len(spec.Paths))
	}
	for _, route := range []struct{ path, method string }{{"/agents", "get"}, {"/tasks", "get"}, {"/tasks", "post"}, {"/tasks/{id}", "get"}, {"/tasks/{id}/cancel", "post"}, {"/tasks/{id}/events", "get"}, {"/tasks/{id}/artifacts", "get"}, {"/artifacts/{id}/content", "get"}, {"/auth/google/start", "get"}, {"/auth/google/callback", "get"}, {"/auth/refresh", "post"}, {"/auth/logout", "post"}, {"/auth/logout-all", "post"}, {"/me", "get"}, {"/workspaces", "get"}, {"/workspaces/{workspaceID}/members", "get"}, {"/workspaces/{workspaceID}/members/{userID}", "patch"}, {"/workspaces/{workspaceID}/members/{userID}", "delete"}} {
		if spec.Paths[route.path][route.method] == nil {
			t.Fatalf("missing %s %s", route.method, route.path)
		}
	}
	task := store.Task{ID: "id", Config: json.RawMessage(`{"x":1}`), Run: &store.Run{Sandbox: &core.SandboxRef{ID: "sandbox"}, Process: &core.ProcessRef{ID: "process"}, Exit: &core.ProcessExit{FinishedAt: time.Now()}, Plan: &core.AgentPlan{}, ParserState: []byte("private")}}
	old, _ := json.Marshal(task)
	current, _ := json.Marshal(taskResponseDTO(task))
	if string(old) != string(current) {
		t.Fatalf("wire shape changed:\n%s\n%s", old, current)
	}
	samples := map[string]any{"api.TaskResponse": taskResponseDTO(task), "api.RunResponse": taskResponseDTO(task).Run, "api.ArtifactResponse": artifactResponse(store.Artifact{ID: "id", StorageKey: "hidden"}), "api.ErrorResponse": ErrorResponse{Error: "failure"}, "core.AdapterIdentity": core.AdapterIdentity{ID: "demo", Version: "1"}, "core.SandboxRef": core.SandboxRef{}, "core.ProcessRef": core.ProcessRef{}, "core.OperationKey": core.OperationKey{}, "core.ProcessExit": core.ProcessExit{}}
	for name, sample := range samples {
		var definition struct{ Properties map[string]any }
		if err := json.Unmarshal(spec.Definitions[name], &definition); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(sample)
		var fields map[string]any
		_ = json.Unmarshal(encoded, &fields)
		for key := range fields {
			if _, ok := definition.Properties[key]; !ok {
				t.Errorf("%s wire field %s missing in Swagger", name, key)
			}
		}
	}
}

func TestSwaggerUIAssetsStayLocalAndDevelopmentOnly(t *testing.T) {
	for _, tc := range []struct {
		name, remote                string
		development, operationsOnly bool
		status                      int
	}{
		{"development", "127.0.0.1:1234", true, false, 200},
		{"ipv6", "[::1]:1234", true, false, 200},
		{"production", "127.0.0.1:1234", false, false, 404},
		{"remote", "192.0.2.1:1234", true, false, 404},
		{"worker", "127.0.0.1:1234", true, true, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := (&Server{Development: tc.development, OperationsOnly: tc.operationsOnly}).Handler()
			for _, path := range []string{"index.html", "swagger-ui.css", "swagger-ui-bundle.js", "doc.json"} {
				req := httptest.NewRequest(http.MethodGet, "/swagger/"+path, nil)
				req.RemoteAddr = tc.remote
				req.Header.Set("X-Forwarded-For", "127.0.0.1")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != tc.status {
					t.Fatalf("%s: status=%d", path, rec.Code)
				}
				if rec.Code == 200 && rec.Body.Len() == 0 {
					t.Fatalf("empty %s", path)
				}
				if path == "index.html" && rec.Code == 200 {
					for _, expected := range []string{`src="/swagger/swagger-ui-bundle.js"`, `href="/swagger/swagger-ui.css"`, `url: "/swagger/doc.json"`, "validatorUrl: null", "queryConfigEnabled: false", "persistAuthorization: false"} {
						if !strings.Contains(rec.Body.String(), expected) {
							t.Fatalf("missing %s", expected)
						}
					}
				}
			}
		})
	}
	handler := (&Server{Development: true}).Handler()
	for _, path := range []string{"swagger-initializer.js", "../go.mod", "swagger-ui-bundle.js.map"} {
		req := httptest.NewRequest(http.MethodGet, "/swagger/"+path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("exposed %s", path)
		}
	}
}

func TestRequestIDRejectsUnboundedOrUnsafeValues(t *testing.T) {
	for _, v := range []string{"", strings.Repeat("a", 65), "unsafe\nvalue", "authorization:secret"} {
		if got := requestID(v); got == v || len(got) > 64 || got == "" {
			t.Fatalf("bad request ID for %q", v)
		}
	}
}

func TestSSEShutdownThroughAPI(t *testing.T) {
	f := apiTestServer(t)
	task := f.submit(t, "shutdown")
	shutdown, stop := context.WithCancel(t.Context())
	defer stop()
	s := &Server{DB: f.db, Keys: map[string]string{f.token: f.workspace}, Shutdown: shutdown, Metrics: newTestMetrics(t)}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v1/tasks/"+task.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	stop()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(response.Body); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not close SSE")
	}
}

func TestRequestLoggingDoesNotExposePayloadOrCredentials(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	server := &Server{Metrics: newTestMetrics(t)}
	e := echo.New()
	e.HTTPErrorHandler = server.handleError
	e.Use(server.observe)
	e.GET("/panic/:id", func(_ *echo.Context) error { panic("private-panic-marker") })
	req := httptest.NewRequest(http.MethodGet, "/panic/private-task-id?token=private-query-marker", strings.NewReader("private-prompt-marker"))
	req.Header.Set("Authorization", "Bearer private-secret-marker")
	req.Header.Set("X-Request-ID", "safe-id")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"error":"internal error"`) {
		t.Fatal("panic was not handled")
	}
	for _, marker := range []string{"private-panic-marker", "private-task-id", "private-query-marker", "private-prompt-marker", "private-secret-marker"} {
		if strings.Contains(output.String(), marker) {
			t.Fatalf("log leaked %s", marker)
		}
	}
	if !strings.Contains(output.String(), `"request_id":"safe-id"`) || !strings.Contains(output.String(), `"route":"/panic/:id"`) {
		t.Fatal("missing safe correlation fields")
	}
}

func TestReadinessUsesOneBudgetAndRejectsShutdown(t *testing.T) {
	s := &Server{Ready: func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Error("readiness has no bounded shared budget")
		}
		return nil
	}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 200 {
		t.Fatal("healthy dependency rejected")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	s.Ready = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx)
	rec = httptest.NewRecorder()
	started := time.Now()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 503 || time.Since(started) > time.Second {
		t.Fatal("readiness ignored caller cancellation")
	}
	shutdown, stop := context.WithCancel(t.Context())
	stop()
	s.Shutdown = shutdown
	s.Ready = func(context.Context) error { t.Error("dependency called during shutdown"); return nil }
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 503 {
		t.Fatal("shutdown reported ready")
	}
}

func newTestMetrics(t *testing.T) *telemetry.Metrics {
	t.Helper()
	metrics, err := telemetry.New(noop.NewMeterProvider().Meter("api-test"))
	if err != nil {
		t.Fatal(err)
	}
	return metrics
}

func TestHTTPMetricIncludesEntireStreamLifetime(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	m, err := telemetry.New(provider.Meter("http-test"))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Metrics: m}
	release := make(chan struct{})
	started := make(chan time.Time, 1)
	e := echo.New()
	e.Use(server.observe)
	e.GET("/stream/:id", func(c *echo.Context) error {
		started <- time.Now()
		c.Response().Header().Set("Content-Type", "text/event-stream")
		c.Response().WriteHeader(200)
		if err := http.NewResponseController(c.Response()).Flush(); err != nil {
			return err
		}
		<-release
		return nil
	})
	httpServer := httptest.NewServer(e)
	defer httpServer.Close()
	closeStream := sync.OnceFunc(func() { close(release) })
	defer closeStream()
	response, err := httpServer.Client().Get(httpServer.URL + "/stream/private-id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &before); err != nil {
		t.Fatal(err)
	}
	for _, scope := range before.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "makez_http_request_seconds" {
				t.Fatal("HTTP metric recorded before stream ended")
			}
		}
	}
	elapsed := time.Since(<-started)
	closeStream()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	var after metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &after); err != nil {
		t.Fatal(err)
	}
	for _, scope := range after.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "makez_http_request_seconds" {
				continue
			}
			h := m.Data.(metricdata.Histogram[float64])
			if len(h.DataPoints) != 1 || h.DataPoints[0].Count != 1 || h.DataPoints[0].Sum < elapsed.Seconds() {
				t.Fatal("stream lifetime was not included", h)
			}
			attrs := h.DataPoints[0].Attributes
			route, _ := attrs.Value("route")
			method, _ := attrs.Value("method")
			status, _ := attrs.Value("status")
			if attrs.Len() != 3 || route.AsString() != "/stream/:id" || method.AsString() != "GET" || status.AsString() != "200" {
				t.Fatal("incorrect HTTP attributes", attrs)
			}
			return
		}
	}
	t.Fatal("missing HTTP metric after stream closure")
}
