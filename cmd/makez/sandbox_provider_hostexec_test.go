//go:build makezhostexec

package main

import (
	"strings"
	"testing"
)

func TestTaggedBuildSelectsHostExec(t *testing.T) {
	t.Setenv("MAKEZ_SANDBOX_URL", "http://127.0.0.1:9")
	t.Setenv("MAKEZ_SANDBOX_API_KEY", "")
	t.Setenv("MAKEZ_CONTROL_KEY", strings.Repeat("k", 32))
	t.Setenv("MAKEZ_INSTALLATION_ID", "makez-local")
	t.Setenv("MAKEZ_RUNTIME_IMAGE", "makez-runtime:0.1")
	t.Setenv("MAKEZ_TEST_HOST_EXEC", "")
	provider, err := sandboxProvider()
	if err != nil {
		t.Fatal(err)
	}
	if provider.BackendID() != "opensandbox" {
		t.Fatalf("unset harness backend = %s", provider.BackendID())
	}
	t.Setenv("MAKEZ_TEST_HOST_EXEC", t.TempDir())
	provider, err = sandboxProvider()
	if err != nil {
		t.Fatal(err)
	}
	if provider.BackendID() != "host-exec-test" {
		t.Fatalf("harness backend = %s", provider.BackendID())
	}
	t.Cleanup(func() {
		closer, ok := provider.(interface{ Close() })
		if ok {
			closer.Close()
		}
	})
}
