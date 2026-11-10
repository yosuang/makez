package main

import (
	"testing"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
)

func TestLoadAgentsKeepsDemoAndRegistersPi(t *testing.T) {
	t.Setenv("MAKEZ_CLI_AGENT", "")
	t.Setenv("MAKEZ_CODEX_AGENT", "")
	t.Setenv("MAKEZ_PI_AGENT", "")
	agents, err := loadAgents()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agents["demo@1"]; !ok {
		t.Fatal("demo was removed")
	}
	pi, ok := agents["pi@1"]
	if !ok {
		t.Fatal("pi was not registered")
	}
	plan, err := pi.Plan(core.AgentInput{Prompt: "hello", Config: []byte(`{"execution":{"contract":"` + piharness.Contract + `","scope":"worker","delegation_run_id":"run-1","harness_pin":"` + piharness.Pin + `"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Command.Env) != 0 || len(plan.SecretRefs) != 0 {
		t.Fatalf("default pi plan carries env=%#v secrets=%#v", plan.Command.Env, plan.SecretRefs)
	}
	for _, capability := range pi.Requirements() {
		if capability == core.NetworkAllowlist {
			t.Fatal("pi requires a network allowlist")
		}
	}
}
