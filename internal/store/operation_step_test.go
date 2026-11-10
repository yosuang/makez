package store

import "testing"

func TestOperationStepKeepsTheFirstAgentName(t *testing.T) {
	for _, step := range []string{"sandbox", "agent", "agent:2", "agent:3"} {
		if !operationStep(step) {
			t.Fatalf("%s should be allowed", step)
		}
	}
	for _, step := range []string{"", "agent:", "agent:0", "agent:01", "agent:1000", "bash"} {
		if operationStep(step) {
			t.Fatalf("%s should be rejected", step)
		}
	}
}
