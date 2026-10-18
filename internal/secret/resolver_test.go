package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func privateFile(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func resolver(t *testing.T, source Source) *Resolver {
	t.Helper()
	r, err := New(Registry{"workspace-a": {"credential@1": source}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestWorkspaceAndReferenceAuthorization(t *testing.T) {
	r := resolver(t, Source{Env: "MAKEZ_SECRET_TEST_NOT_PRESENT"})
	valid := map[string]string{"CODEX_ACCESS_TOKEN": "credential@1"}
	if err := r.Check("workspace-a", valid); err != nil {
		t.Fatalf("Check must not read sources: %v", err)
	}
	if err := r.Check("workspace-b", valid); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign workspace: %v", err)
	}
	if _, err := r.Resolve(t.Context(), "workspace-b", valid); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign resolution: %v", err)
	}
	for _, ref := range []string{"credential", "credential@2", "/tmp/credential@1", "../../credential@1", "env:AWS_SECRET_ACCESS_KEY", "workspace-a/credential@1"} {
		if err := r.Check("workspace-a", map[string]string{"CODEX_ACCESS_TOKEN": ref}); err == nil {
			t.Fatalf("accepted unauthorized reference %q", ref)
		}
	}
	if _, err := r.Resolve(t.Context(), "unused-workspace", nil); err != nil {
		t.Fatalf("empty references should need no registration: %v", err)
	}
}

func TestRejectsProcessControlBindings(t *testing.T) {
	r := resolver(t, Source{Env: "MAKEZ_SECRET_TEST_AUTH"})
	for _, name := range []string{"PATH", "HOME", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "NODE_OPTIONS", "PYTHONPATH", "BASH_ENV", "MAKEZ_DATABASE_URL", "INVALID=NAME", "TOKEN\x00"} {
		if err := r.Check("workspace-a", map[string]string{name: "credential@1"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted unsafe destination %q: %v", name, err)
		}
	}
	for _, name := range []string{"CODEX_ACCESS_TOKEN", "CODEX_API_KEY", "OPENAI_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if err := r.Check("workspace-a", map[string]string{name: "credential@1"}); err != nil {
			t.Fatalf("valid auth destination %q: %v", name, err)
		}
	}
}

func TestNewRejectsInvalidSources(t *testing.T) {
	for _, source := range []Source{{}, {Env: "TOKEN", File: "/tmp/credential"}, {Env: "BAD=NAME"}, {Env: "TOKEN", JSONField: []string{"token"}}, {File: "relative"}, {File: "/tmp/../credential"}, {File: "/tmp/credential", JSONField: []string{""}}} {
		if _, err := New(Registry{"workspace-a": {"credential@1": source}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid source accepted: %v", err)
		}
	}
	if _, err := New(Registry{"workspace-a": {"unversioned": {Env: "TOKEN"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unversioned registration: %v", err)
	}
}

func TestEnvironmentRotationAndImmutableRegistry(t *testing.T) {
	const variable = "MAKEZ_SECRET_TEST_ROTATION"
	t.Setenv(variable, "first-secret-value")
	registry := Registry{"workspace-a": {"credential@1": {Env: variable}}}
	r, err := New(registry)
	if err != nil {
		t.Fatal(err)
	}
	registry["workspace-a"]["credential@1"] = Source{Env: "DO_NOT_READ_THIS"}
	refs := map[string]string{"CODEX_API_KEY": "credential@1"}
	first, err := r.Resolve(t.Context(), "workspace-a", refs)
	if err != nil || first["CODEX_API_KEY"] != "first-secret-value" {
		t.Fatalf("first resolution failed: %v", err)
	}
	t.Setenv(variable, "second-secret-value")
	second, err := r.Resolve(t.Context(), "workspace-a", refs)
	if err != nil || second["CODEX_API_KEY"] != "second-secret-value" {
		t.Fatalf("rotation not reread: %v", err)
	}
}

func TestFileJSONExtractionRotationAndRawValue(t *testing.T) {
	file := privateFile(t, `{"tokens":{"access_token":"token-one"}}`)
	fields := []string{"tokens", "access_token"}
	r := resolver(t, Source{File: file, JSONField: fields})
	fields[0] = "mutated"
	refs := map[string]string{"CODEX_ACCESS_TOKEN": "credential@1"}
	first, err := r.Resolve(t.Context(), "workspace-a", refs)
	if err != nil || first["CODEX_ACCESS_TOKEN"] != "token-one" {
		t.Fatalf("nested extraction failed: %v", err)
	}
	if err = os.WriteFile(file, []byte(`{"tokens":{"access_token":"token-two"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := r.Resolve(t.Context(), "workspace-a", refs)
	if err != nil || second["CODEX_ACCESS_TOKEN"] != "token-two" {
		t.Fatalf("file rotation failed: %v", err)
	}
	raw := resolver(t, Source{File: privateFile(t, "raw-secret\n")})
	value, err := raw.Resolve(t.Context(), "workspace-a", refs)
	if err != nil || value["CODEX_ACCESS_TOKEN"] != "raw-secret\n" {
		t.Fatalf("raw file content changed: %v", err)
	}
}

func TestFileRestrictionsAndSanitizedErrors(t *testing.T) {
	const secretText = "never-include-this-secret"
	refs := map[string]string{"CODEX_API_KEY": "credential@1"}
	for _, content := range []string{`{"tokens": "` + secretText, `{"tokens":{"access_token":123}}`, `{"tokens":{}}`, `{"tokens":{"access_token":null}}`, `{"tokens":{"access_token":""}}`, `{"tokens":{"access_token":"a\u0000b"}}`, strings.Repeat("x", maxFileBytes+1)} {
		file := privateFile(t, content)
		r := resolver(t, Source{File: file, JSONField: []string{"tokens", "access_token"}})
		_, err := r.Resolve(t.Context(), "workspace-a", refs)
		if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), secretText) || strings.Contains(err.Error(), file) {
			t.Fatalf("error must be unavailable without source data: %v", err)
		}
	}
	file := privateFile(t, secretText)
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver(t, Source{File: file}).Resolve(t.Context(), "workspace-a", refs); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("world-readable credential accepted: %v", err)
	}
	private := privateFile(t, secretText)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver(t, Source{File: link}).Resolve(t.Context(), "workspace-a", refs); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("symlink accepted: %v", err)
	}
	if _, err := resolver(t, Source{File: t.TempDir()}).Resolve(t.Context(), "workspace-a", refs); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("directory accepted: %v", err)
	}
}

func TestResolutionBoundsAndCancellation(t *testing.T) {
	const variable = "MAKEZ_SECRET_TEST_LIMITS"
	t.Setenv(variable, strings.Repeat("x", maxValueBytes+1))
	r := resolver(t, Source{Env: variable})
	refs := map[string]string{"TOKEN": "credential@1"}
	if _, err := r.Resolve(t.Context(), "workspace-a", refs); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("oversized value accepted: %v", err)
	}
	t.Setenv(variable, strings.Repeat("x", maxValueBytes))
	if _, err := r.Resolve(t.Context(), "workspace-a", map[string]string{"TOKEN_ONE": "credential@1", "TOKEN_TWO": "credential@1", "TOKEN_THREE": "credential@1"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("oversized total accepted: %v", err)
	}
	for i := range maxReferences + 1 {
		refs[fmt.Sprintf("TOKEN_%d", i)] = "credential@1"
	}
	if err := r.Check("workspace-a", refs); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many references accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Resolve(ctx, "workspace-a", map[string]string{"TOKEN": "credential@1"}); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	for _, value := range []string{"", " \n"} {
		t.Setenv(variable, value)
		if _, err := r.Resolve(t.Context(), "workspace-a", map[string]string{"TOKEN": "credential@1"}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("empty value accepted: %v", err)
		}
	}
}
