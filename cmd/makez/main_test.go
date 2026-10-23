package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

func TestRunRejectsOccupiedPortBeforeStartingServices(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("MAKEZ_LISTEN", listener.Addr().String())
	t.Setenv("MAKEZ_DATABASE_URL", "invalid-database-url")
	t.Setenv("MAKEZ_CLI_AGENT", "")
	t.Setenv("MAKEZ_CODEX_AGENT", "")
	t.Setenv("MAKEZ_SECRETS_FILE", "")
	for _, mode := range []string{"all", "api"} {
		t.Run(mode, func(t *testing.T) {
			original := os.Args
			os.Args = []string{"makez", mode}
			t.Cleanup(func() { os.Args = original })
			if err := run(); !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("expected port conflict before database access, got %v", err)
			}
		})
	}
}

func configFile(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSecretsOptionalAndMetadataOnly(t *testing.T) {
	resolver, err := loadSecrets("")
	if err != nil || resolver.Check("workspace", nil) != nil {
		t.Fatalf("demo should work without a secret registry: %v", err)
	}
	path := configFile(t, `{"workspace":{"codex-auth@1":{"env":"MAKEZ_CONFIG_TEST_CREDENTIAL"},"file-auth@1":{"file":"/registered/credential.json","json_field":["tokens","access_token"]}}}`)
	resolver, err = loadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = resolver.Check("workspace", map[string]string{"CODEX_ACCESS_TOKEN": "codex-auth@1"}); err != nil {
		t.Fatalf("metadata registry should load without resolving credentials: %v", err)
	}
	if err = resolver.Check("other-workspace", map[string]string{"CODEX_ACCESS_TOKEN": "codex-auth@1"}); err == nil {
		t.Fatal("registry bypassed workspace scoping")
	}
}

func TestLoadSecretsRejectsInvalidConfigWithoutEchoingContents(t *testing.T) {
	const marker = "sensitive-configuration-marker"
	for _, body := range []string{
		`{"workspace":{"auth@1":{"value":"` + marker + `"}}}`,
		`{"workspace":{"auth@1":{}}}`,
		`{"workspace":{"auth@1":{"env":"TOKEN","file":"/credential"}}}`,
		`{"workspace":{"unversioned":{"env":"TOKEN"}}}`,
		`{"workspace":{"auth@1":{"file":"relative"}}}`,
		`{"workspace":{"auth@1":{"env":"TOKEN","unexpected":"` + marker + `"}}}`,
		`{} {"value":"` + marker + `"}`,
		`null`,
		`[]`,
		`{"broken":"` + marker,
		strings.Repeat(" ", maxHostConfigBytes) + `{}`,
	} {
		path := configFile(t, body)
		_, err := loadSecrets(path)
		if err == nil || strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), path) {
			t.Fatalf("configuration error must be generic: %v", err)
		}
	}
	missing := filepath.Join(t.TempDir(), marker)
	if _, err := loadSecrets(missing); err == nil || strings.Contains(err.Error(), missing) {
		t.Fatalf("explicit missing registry must fail without revealing path: %v", err)
	}
	if _, err := loadSecrets(t.TempDir()); err == nil {
		t.Fatal("directory accepted as registry")
	}
}

func TestLoadAgentsKeepsDemoAndGenericCLIAndAddsCodex(t *testing.T) {
	t.Setenv("MAKEZ_CLI_AGENT", `{"ID":"custom","Version":"1","Argv":["/opt/custom-agent"],"Env":{"MODE":"test"}}`)
	t.Setenv("MAKEZ_CODEX_AGENT", `{"AuthMode":"api_key","SecretRef":"codex-auth@1","Model":"model-id"}`)
	agents, err := loadAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"demo@1", "custom@1", "codex@1"} {
		if agents[id] == nil {
			t.Fatalf("missing adapter %s", id)
		}
	}
	plan, err := agents["codex@1"].Plan(core.AgentInput{Prompt: "make a deliverable"})
	if err != nil || plan.SecretRefs["CODEX_API_KEY"] != "codex-auth@1" || len(plan.Command.Env) != 0 {
		t.Fatalf("Codex must retain only a logical credential reference: %v", err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil || strings.Contains(string(encoded), "MAKEZ_CONFIG_TEST_CREDENTIAL") {
		t.Fatal("source environment leaked into plan")
	}
	t.Setenv("MAKEZ_CODEX_AGENT", "")
	t.Setenv("MAKEZ_CLI_AGENT", "")
	agents, err = loadAgents()
	if err != nil || len(agents) != 1 || agents["demo@1"] == nil {
		t.Fatalf("default configuration changed: %v", err)
	}
}

func TestLoadCodexRejectsInvalidAndRawCredentialConfig(t *testing.T) {
	t.Setenv("MAKEZ_CLI_AGENT", "")
	const marker = "do-not-echo-credential-content"
	for _, config := range []string{
		`{"AuthMode":"api_key","SecretRef":"auth@1","APIKey":"` + marker + `"}`,
		`{"AuthMode":"api_key","SecretRef":"auth@1","unexpected":true}`,
		`{"AuthMode":"api_key"}`,
		`{"AuthMode":"unsupported","SecretRef":"auth@1"}`,
		`{"AuthMode":"api_key","SecretRef":"auth"}`,
		`{} {}`,
		`null`,
		`{"` + marker,
		strings.Repeat(" ", maxHostConfigBytes) + `{}`,
	} {
		t.Setenv("MAKEZ_CODEX_AGENT", config)
		if _, err := loadAgents(); err == nil || strings.Contains(err.Error(), marker) {
			t.Fatalf("invalid Codex configuration should fail generically: %v", err)
		}
	}
	t.Setenv("MAKEZ_CLI_AGENT", `{"ID":"codex","Version":"1","Argv":["/different-program"]}`)
	t.Setenv("MAKEZ_CODEX_AGENT", `{"AuthMode":"api_key","SecretRef":"auth@1"}`)
	if _, err := loadAgents(); err == nil {
		t.Fatal("silently replaced a versioned adapter")
	}
}
