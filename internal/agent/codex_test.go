package agent

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

func codexForTest(t *testing.T) core.AgentAdapter {
	t.Helper()
	a, err := NewCodex(CodexConfig{AuthMode: "api_key", SecretRef: "codex-key@1"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCodexPlanUsesReferencesAndFixedCommand(t *testing.T) {
	for _, mode := range []string{"api_key", "codex_access_token", "chatgpt_session"} {
		t.Run(mode, func(t *testing.T) {
			a, err := NewCodex(CodexConfig{AuthMode: mode, SecretRef: "codex-key@1", Model: "test-model"})
			if err != nil {
				t.Fatal(err)
			}
			prompt := "$(touch /tmp/injected); `echo unexpected`"
			plan, err := a.Plan(core.AgentInput{Prompt: prompt})
			if err != nil {
				t.Fatal(err)
			}
			wantVariable := "CODEX_API_KEY"
			switch mode {
			case "codex_access_token":
				wantVariable = "CODEX_ACCESS_TOKEN"
			case "chatgpt_session":
				wantVariable = "CODEX_AUTH_JSON"
			}
			if !reflect.DeepEqual(plan.SecretRefs, map[string]string{wantVariable: "codex-key@1"}) || len(plan.Command.Env) != 0 {
				t.Fatalf("credential contract leaked into command: %#v", plan)
			}
			if a.Identity() != (core.AdapterIdentity{ID: "codex", Version: "1"}) || !slices.Contains(a.Requirements(), core.SecretInjection) {
				t.Fatal("missing adapter identity or secret requirement")
			}
			if slices.Contains(plan.Command.Argv, prompt) || !slices.Equal(plan.Command.Argv, []string{"/usr/local/bin/codex-runner", "--auth-mode", mode, "--model", "test-model"}) {
				t.Fatalf("unexpected command: %v", plan.Command.Argv)
			}
			if string(plan.Files[0].Data) != prompt || !json.Valid(plan.Files[1].Data) {
				t.Fatal("input or schema malformed")
			}
			plan.SecretRefs[wantVariable] = "changed@2"
			again, err := a.Plan(core.AgentInput{Prompt: "again"})
			if err != nil || again.SecretRefs[wantVariable] != "codex-key@1" {
				t.Fatal("caller mutated adapter registration")
			}
		})
	}
}

func TestCodexRejectsUnversionedSecretsAndTaskOverrides(t *testing.T) {
	for _, config := range []CodexConfig{
		{AuthMode: "chatgpt", SecretRef: "key@1"},
		{AuthMode: "api_key", SecretRef: "key"},
		{AuthMode: "api_key", SecretRef: "key@"},
		{AuthMode: "api_key", SecretRef: "key@1", Model: "model; touch /tmp/no"},
		{AuthMode: "api_key", SecretRef: "key@1", Model: "--dangerously-bypass-approvals-and-sandbox"},
	} {
		if _, err := NewCodex(config); err == nil {
			t.Fatalf("accepted invalid config: %#v", config)
		}
	}
	if _, err := codexForTest(t).Plan(core.AgentInput{Prompt: "test", Config: json.RawMessage(`{"model":"another","secret":"raw-value"}`)}); err == nil {
		t.Fatal("task overrode registered agent configuration")
	}
}

func TestCodexCollectRequiresRealDeliverableManifest(t *testing.T) {
	a := codexForTest(t)
	valid := `{"succeeded":true,"summary":"created report","artifacts":[{"path":"deliverables/report.md","media_type":"text/markdown","required":true}]}`
	result, err := a.Collect(t.Context(), memoryWorkspace{manifestPath: valid}, core.ProcessExit{})
	if err != nil || len(result.Artifacts) != 2 || result.Artifacts[1].Path != "/workspace/deliverables/report.md" {
		t.Fatalf("invalid result: %#v %v", result, err)
	}
	for _, manifest := range []string{
		`{"succeeded":true,"summary":"only described it","artifacts":[]}`,
		`{"succeeded":true,"summary":"secret","artifacts":[{"path":"input.txt","required":true}]}`,
		`{"succeeded":true,"summary":"optional","artifacts":[{"path":"deliverables/missing","required":false}]}`,
	} {
		if _, err = a.Collect(t.Context(), memoryWorkspace{manifestPath: manifest}, core.ProcessExit{}); err == nil {
			t.Fatalf("accepted invalid manifest: %s", manifest)
		}
	}
}

func TestCodexDecodeReplaysJSONLAndBoundsEscapedLogs(t *testing.T) {
	a := codexForTest(t)
	first, err := a.Decode(core.OutputPage{Records: []core.OutputRecord{{Offset: "0", Stream: "stdout", Data: []byte(`{"type":"turn.`)}}}, nil)
	if err != nil || len(first.Events) != 0 {
		t.Fatalf("partial JSON emitted: %#v %v", first, err)
	}
	page := core.OutputPage{Records: []core.OutputRecord{{Offset: "20", Stream: "stdout", Data: []byte("completed\",\"usage\":{\"input_tokens\":12}}\n{\"type\":\"turn.started\"}\n")}, {Offset: "80", Stream: "stderr", Data: []byte(strings.Repeat("\x00", 64<<10))}}, EOF: true, NextCursor: "end"}
	decoded, err := a.Decode(page, first.NextState)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := a.Decode(page, first.NextState)
	if err != nil || !reflect.DeepEqual(decoded, replay) {
		t.Fatal("Codex parser is not deterministic")
	}
	if decoded.Events[0].Kind != "usage" || decoded.Events[1].Kind != "status" {
		t.Fatalf("lost structured events: %#v", decoded.Events[:2])
	}
	if len(decoded.NextState) > 64<<10 {
		t.Fatal("parser checkpoint exceeds database limit")
	}
	for _, event := range decoded.Events {
		if len(event.Payload) > 64<<10 || !json.Valid(event.Payload) {
			t.Fatal("event payload exceeds database contract")
		}
	}
}

func TestCodexDecodeKeepsUTF8AtFragmentBoundary(t *testing.T) {
	a := codexForTest(t)
	data := append([]byte(strings.Repeat("a", codexFragmentBytes-2)), 0xe4, 0xbd)
	first, err := a.Decode(core.OutputPage{Records: []core.OutputRecord{{Offset: "0", Stream: "stderr", Data: data}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Decode(core.OutputPage{Records: []core.OutputRecord{{Offset: "next", Stream: "stderr", Data: []byte{0xa0, '\n'}}}, EOF: true}, first.NextState)
	if err != nil || len(second.Events) != 1 || !strings.Contains(string(second.Events[0].Payload), "你") {
		t.Fatalf("split UTF8 was damaged: %#v %v", second, err)
	}
}
