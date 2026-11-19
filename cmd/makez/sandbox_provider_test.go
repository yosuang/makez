//go:build !makezhostexec

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultBuildIgnoresHostExecEnv(t *testing.T) {
	t.Setenv("MAKEZ_TEST_HOST_EXEC", t.TempDir())
	t.Setenv("MAKEZ_SANDBOX_URL", "http://127.0.0.1:9")
	t.Setenv("MAKEZ_SANDBOX_API_KEY", "")
	t.Setenv("MAKEZ_CONTROL_KEY", strings.Repeat("k", 32))
	t.Setenv("MAKEZ_INSTALLATION_ID", "makez-local")
	t.Setenv("MAKEZ_RUNTIME_IMAGE", "makez-runtime:0.1")
	provider, err := sandboxProvider()
	if err != nil {
		t.Fatal(err)
	}
	if provider.BackendID() != "opensandbox" {
		t.Fatalf("backend = %s", provider.BackendID())
	}
}

func TestDefaultBinaryOmitsHostExec(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "makez")
	tagged := filepath.Join(dir, "makez-hostexec")
	build(t, plain, nil)
	build(t, tagged, []string{"-tags", "makezhostexec"})
	plainBytes, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	taggedBytes, err := os.ReadFile(tagged)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"MAKEZ_TEST_HOST_EXEC", "host-exec-test"} {
		if bytes.Contains(plainBytes, []byte(needle)) {
			t.Fatalf("default binary contains %s", needle)
		}
		if !bytes.Contains(taggedBytes, []byte(needle)) {
			t.Fatalf("tagged binary does not contain %s", needle)
		}
	}
}

func build(t *testing.T, output string, extra []string) {
	t.Helper()
	root := moduleRoot(t)
	args := append([]string{"build", "-buildvcs=false"}, extra...)
	args = append(args, "-o", output, "./cmd/makez")
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = withoutGoflagsTags(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func withoutGoflagsTags(env []string) []string {
	next := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, "GOFLAGS=") {
			continue
		}
		next = append(next, entry)
	}
	return append(next, "GOFLAGS=")
}
