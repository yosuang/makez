package api

import (
	"testing"

	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/core"
)

func TestOmittedAgentStaysDemoUntilThePublicDefaultChanges(t *testing.T) {
	id, version := submittedAgent("", "")
	if id != "demo" || version != "1" {
		t.Fatalf("omitted = %s@%s", id, version)
	}
	id, version = submittedAgent("pi", "")
	if id != "pi" || version != "1" {
		t.Fatalf("explicit pi = %s@%s", id, version)
	}
	id, version = submittedAgent("demo", "1")
	if id != "demo" || version != "1" {
		t.Fatalf("explicit demo = %s@%s", id, version)
	}
}

func TestAgentListSortsDemoBeforePi(t *testing.T) {
	pi, err := agent.NewPi(agent.PiConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ids := adapterIdentities(map[string]core.AgentAdapter{"pi@1": pi, "demo@1": agent.NewDemo()})
	if len(ids) != 2 || ids[0].ID != "demo" || ids[1].ID != "pi" {
		t.Fatalf("order = %#v", ids)
	}
}
