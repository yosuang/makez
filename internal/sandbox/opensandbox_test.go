package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

func testRequest() core.SandboxRequest {
	return core.SandboxRequest{Key: core.OperationKey{WorkspaceID: "ws", TaskID: "task", RunID: "run", Step: "sandbox"}, RuntimeProfileID: "default", RuntimeProfileVersion: "1", ExpiresAt: time.Now().Add(time.Hour).UTC()}
}
func testProvider(t *testing.T, h http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	p, err := New(Config{BaseURL: server.URL, APIKey: "api-key", InstallationID: "test", ControlKey: strings.Repeat("x", 32), Image: "makez-runtime:0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestUnknownCreateIsNeverRepeated(t *testing.T) {
	creates := 0
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("OPEN-SANDBOX-API-KEY") != "api-key" {
			t.Error("missing auth")
		}
		if r.Method == "POST" {
			creates++
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[],"pagination":{"hasNextPage":false}}`))
	})
	req := testRequest()
	for range 2 {
		_, err := p.Ensure(context.Background(), req)
		var pe core.ProviderError
		if !errors.As(err, &pe) || pe.Code() != core.OutcomeUnknown {
			t.Fatalf("expected uncertain creation, got %v", err)
		}
	}
	if creates != 1 {
		t.Fatalf("created %d times", creates)
	}
}
func TestConcurrentEnsureAndConflictingRequest(t *testing.T) {
	var rows []resource
	creates, readinessChecks := 0, 0
	var p *Provider
	p = testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/process" {
			readinessChecks++
			if readinessChecks < 3 {
				w.WriteHeader(502)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"State": "NOT_STARTED"}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/endpoints/8099") {
			json.NewEncoder(w).Encode(map[string]string{"endpoint": strings.TrimSuffix(p.cfg.BaseURL, "/v1")})
			return
		}
		if r.URL.Path == "/v1/sandboxes/sandbox" {
			json.NewEncoder(w).Encode(rows[0])
			return
		}
		if r.Method == "POST" {
			creates++
			var body struct {
				Metadata map[string]string `json:"metadata"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			item := resource{ID: "sandbox", Metadata: body.Metadata}
			item.Status.State = "Running"
			rows = append(rows, item)
			json.NewEncoder(w).Encode(item)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"items": rows, "pagination": map[string]bool{"hasNextPage": false}})
	})
	request := testRequest()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := p.Ensure(context.Background(), request); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if readinessChecks < 3 {
		t.Fatal("runtime readiness was not checked")
	}
	if creates != 1 {
		t.Fatalf("created %d times", creates)
	}
	request.Limits.MemoryBytes = 512 << 20
	_, err := p.Ensure(context.Background(), request)
	var pe core.ProviderError
	if !errors.As(err, &pe) || pe.Code() != core.Conflict {
		t.Fatalf("expected conflict, got %v", err)
	}
}
func TestInspectRejectsUnownedRowsAndMultipleMatches(t *testing.T) {
	var rows []resource
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"items": rows, "pagination": map[string]bool{"hasNextPage": false}})
	})
	key := testRequest().Key
	owned := resource{ID: "one", Metadata: p.labels(key)}
	owned.Status.State = "Running"
	rows = []resource{{ID: "foreign"}, owned}
	observation, err := p.Inspect(context.Background(), key)
	if err != nil || observation.State != core.SandboxReady || len(observation.Matches) != 1 {
		t.Fatalf("bad ownership result: %+v %v", observation, err)
	}
	owned.ID = "two"
	rows = append(rows, owned)
	observation, err = p.Inspect(context.Background(), key)
	if err != nil || observation.State != core.SandboxUnknown || len(observation.Matches) != 2 {
		t.Fatalf("bad duplicate result: %+v %v", observation, err)
	}
}
func TestDeleteWaitsForConfirmedAbsence(t *testing.T) {
	var p *Provider
	deleted := false
	lists := 0
	key := testRequest().Key
	p = testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleted = true
			w.WriteHeader(204)
			return
		}
		lists++
		rows := []resource{}
		if !deleted || lists < 3 {
			item := resource{ID: "sandbox", Metadata: p.labels(key)}
			item.Status.State = "Stopping"
			rows = append(rows, item)
		}
		json.NewEncoder(w).Encode(map[string]any{"items": rows, "pagination": map[string]bool{"hasNextPage": false}})
	})
	if err := p.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if lists < 3 {
		t.Fatal("delete completed before absence")
	}
}
