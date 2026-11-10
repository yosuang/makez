package agent

import (
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
)

func TestPiPlanStaysOutsideTheToolBoundary(t *testing.T) {
	adapter, err := NewPi(PiConfig{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := adapter.Plan(core.AgentInput{Prompt: "hello", Config: personalConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(plan.Command.Argv, " ") != "/usr/local/bin/python3 -u "+piharness.ScriptPath+" "+piharness.InstructionPath {
		t.Fatalf("argv = %#v", plan.Command.Argv)
	}
	if len(plan.Command.Env) != 0 || len(plan.SecretRefs) != 0 {
		t.Fatalf("plan leaked env=%#v secrets=%#v", plan.Command.Env, plan.SecretRefs)
	}
	script := string(plan.Files[0].Data)
	for _, required := range []string{"--no-tools", "--session-dir", "--no-extensions"} {
		if !strings.Contains(script, required) {
			t.Fatalf("script missing %s", required)
		}
	}
	for _, forbidden := range []string{"--approve", "--api-key", "--no-session", "danger-full-access"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script contains %s", forbidden)
		}
	}
	for _, capability := range adapter.Requirements() {
		if capability == core.NetworkAllowlist {
			t.Fatal("pi must not require a network allowlist")
		}
	}
	binary, forward, offline := adapter.(interface {
		PiLaunchSpec() (string, []string, bool)
	}).PiLaunchSpec()
	if binary != piharness.DefaultBinary || len(forward) != 0 || !offline {
		t.Fatalf("launch spec = %s %v offline=%v", binary, forward, offline)
	}
}

func TestPiPlanRejectsAnUnscopedTask(t *testing.T) {
	adapter, err := NewPi(PiConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adapter.Plan(core.AgentInput{Prompt: "hello", Config: []byte(`{}`)}); err == nil {
		t.Fatal("empty config")
	}
	if _, err = adapter.Plan(core.AgentInput{Prompt: "hello"}); err == nil {
		t.Fatal("missing config")
	}
}

func TestPiSecretIsANamedReferenceOnly(t *testing.T) {
	adapter, err := NewPi(PiConfig{SecretRef: "openai@1", SecretEnv: "OPENAI_API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := adapter.Plan(core.AgentInput{Prompt: "hello", Config: personalConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if plan.SecretRefs["OPENAI_API_KEY"] != "openai@1" || len(plan.Command.Env) != 0 {
		t.Fatalf("secret mapping = %#v env=%#v", plan.SecretRefs, plan.Command.Env)
	}
	if strings.Contains(string(plan.Files[0].Data), "sk-") {
		t.Fatal("plan file contains a secret-shaped value")
	}
	_, forward, offline := adapter.(interface {
		PiLaunchSpec() (string, []string, bool)
	}).PiLaunchSpec()
	if offline || len(forward) != 1 || forward[0] != "OPENAI_API_KEY" {
		t.Fatalf("forward=%v offline=%v", forward, offline)
	}
	if _, err = NewPi(PiConfig{SecretRef: "openai@1", SecretEnv: "PATH"}); err == nil {
		t.Fatal("PATH is not a secret destination")
	}
	if _, err = NewPi(PiConfig{SecretEnv: "OPENAI_API_KEY"}); err == nil {
		t.Fatal("env without a ref")
	}
}

func personalConfig() []byte {
	return []byte(`{"execution":{"contract":"` + piharness.Contract + `","scope":"personal","bot_id":"bot-1","conversation_id":"conv-1","harness_pin":"` + piharness.Pin + `"}}`)
}
