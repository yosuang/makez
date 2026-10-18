package sandbox

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

func TestSecretTransportPolicy(t *testing.T) {
	for _, tc := range []struct {
		target  string
		allowed bool
	}{
		{"http://localhost:18080/v1", true}, {"http://127.0.0.1:18080/v1", true}, {"http://[::1]:18080/v1", true},
		{"https://sandbox.example/v1", true}, {"http://sandbox.example/v1", false}, {"http://localhost.evil/v1", false},
		{"https://user:password@sandbox.example/v1", false}, {"ftp://localhost/v1", false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			if got := secretTransportAllowed(tc.target); got != tc.allowed {
				t.Fatalf("allowed=%v", got)
			}
		})
	}
}

func TestSecretInjectionRequiresResolvedNonConflictingMapping(t *testing.T) {
	good := core.StartRequest{SecretRefs: map[string]string{"TOKEN": "secret://workspace/token/v1"}, SecretValues: map[string]string{"TOKEN": "test-secret-value"}}
	if err := validateSecrets(good); err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]core.StartRequest{
		"unresolved":     {SecretRefs: good.SecretRefs},
		"unreferenced":   {SecretValues: good.SecretValues},
		"different keys": {SecretRefs: good.SecretRefs, SecretValues: map[string]string{"OTHER": "test-secret-value"}},
		"collision":      {SecretRefs: good.SecretRefs, SecretValues: good.SecretValues, Command: core.CommandSpec{Env: map[string]string{"TOKEN": "normal-env"}}},
		"reserved":       {SecretRefs: map[string]string{"MAKEZ_CONTROL_TOKEN": "ref"}, SecretValues: map[string]string{"MAKEZ_CONTROL_TOKEN": "test-secret-value"}},
		"marker":         {SecretRefs: good.SecretRefs, SecretValues: map[string]string{"TOKEN": "[REDACTED]"}},
		"invalid UTF-8":  {SecretRefs: good.SecretRefs, SecretValues: map[string]string{"TOKEN": string([]byte{255, 254})}},
		"nul":            {SecretRefs: good.SecretRefs, SecretValues: map[string]string{"TOKEN": "value\x00value"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateSecrets(request)
			if pe, ok := errors.AsType[core.ProviderError](err); !ok || pe.Code() != core.InvalidInput {
				t.Fatalf("expected invalid input: %v", err)
			}
			if strings.Contains(err.Error(), "test-secret-value") {
				t.Fatal("secret in error")
			}
		})
	}
	ordinary, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ordinary), "test-secret-value") || strings.Contains(string(ordinary), "SecretValues") {
		t.Fatal("default JSON serialization included secret values")
	}
}

func TestStartUsesExplicitSecretWirePayload(t *testing.T) {
	key := testRequest().Key
	var p *Provider
	var posted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/start":
			posted = true
			var wire struct{ SecretRefs, SecretValues map[string]string }
			if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
				t.Error(err)
			}
			if wire.SecretValues["TOKEN"] != "test-secret-value" || wire.SecretRefs["TOKEN"] != "secret://workspace/token/v1" {
				t.Error("incomplete ephemeral injection")
			}
			json.NewEncoder(w).Encode(map[string]any{"data": core.ProcessObservation{State: core.ProcessRunning, Ref: &core.ProcessRef{ID: "receipt", Key: key}}})
		case strings.HasSuffix(r.URL.Path, "/endpoints/8099"):
			json.NewEncoder(w).Encode(map[string]string{"endpoint": strings.TrimSuffix(p.cfg.BaseURL, "/v1")})
		default:
			json.NewEncoder(w).Encode(resource{ID: "sandbox", Metadata: p.labels(key)})
		}
	}))
	t.Cleanup(server.Close)
	var err error
	p, err = New(Config{BaseURL: server.URL, APIKey: "local", InstallationID: "test", ControlKey: strings.Repeat("x", 32), Image: "makez-runtime:0.1"})
	if err != nil {
		t.Fatal(err)
	}
	request := core.StartRequest{Key: key, SecretRefs: map[string]string{"TOKEN": "secret://workspace/token/v1"}, SecretValues: map[string]string{"TOKEN": "test-secret-value"}}
	ref := core.SandboxRef{BackendID: "opensandbox", ID: "sandbox", Key: key}
	if _, err = p.Start(t.Context(), ref, request); err != nil {
		t.Fatal(err)
	}
	if !posted {
		t.Fatal("runtime did not receive launch")
	}
	p.cfg.BaseURL = "http://sandbox.example/v1"
	if _, err = p.Start(t.Context(), ref, request); err == nil {
		t.Fatal("remote plaintext secret transport accepted")
	}
}

func TestStartRejectsPlaintextRuntimeEndpoint(t *testing.T) {
	key := testRequest().Key
	var p *Provider
	p = testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/endpoints/8099") {
			json.NewEncoder(w).Encode(map[string]string{"endpoint": "http://remote.example:8099"})
			return
		}
		json.NewEncoder(w).Encode(resource{ID: "sandbox", Metadata: p.labels(key)})
	})
	request := core.StartRequest{Key: key, SecretRefs: map[string]string{"TOKEN": "ref"}, SecretValues: map[string]string{"TOKEN": "test-secret-value"}}
	_, err := p.Start(t.Context(), core.SandboxRef{BackendID: "opensandbox", ID: "sandbox", Key: key}, request)
	if pe, ok := errors.AsType[core.ProviderError](err); !ok || pe.Code() != core.PermissionDenied {
		t.Fatalf("expected rejected endpoint, got %v", err)
	}
}

type secretErrorTransport struct{}

func (secretErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("synthetic-secret-value-from-transport")
}

func TestTransportErrorsDoNotEchoSecretValues(t *testing.T) {
	p, err := New(Config{BaseURL: "http://localhost", InstallationID: "test", ControlKey: strings.Repeat("x", 32), Image: "runtime", HTTPClient: &http.Client{Transport: secretErrorTransport{}}})
	if err != nil {
		t.Fatal(err)
	}
	err = p.request(t.Context(), "POST", "http://localhost/start", map[string]string{"SecretValues": "synthetic-secret-value-from-transport"}, nil, nil)
	if err == nil || strings.Contains(err.Error(), "synthetic-secret-value-from-transport") {
		t.Fatal("transport error was not sanitized")
	}
}
