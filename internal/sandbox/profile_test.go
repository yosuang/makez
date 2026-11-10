package sandbox

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

func TestDefaultProfileIsNotANetworkAllowlist(t *testing.T) {
	called := false
	provider, err := New(Config{
		BaseURL: "http://127.0.0.1:9", InstallationID: "makez-local", ControlKey: strings.Repeat("k", 32), Image: "makez-runtime:0.1",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("runtime should not be called")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	caps, err := provider.Capabilities(context.Background(), "default", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range caps.Supported {
		if capability == core.NetworkAllowlist {
			t.Fatal("default@1 advertised network_allowlist")
		}
	}
	_, err = provider.Ensure(context.Background(), core.SandboxRequest{
		Key: core.OperationKey{WorkspaceID: "w", TaskID: "t", RunID: "r", Step: "sandbox"}, RuntimeProfileID: "default", RuntimeProfileVersion: "1",
		ExpiresAt: time.Now().Add(time.Hour), NetworkPolicyRef: "deny-all",
	})
	if err == nil {
		t.Fatal("deny-all was accepted")
	}
	if called {
		t.Fatal("rejecting deny-all called the runtime")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
