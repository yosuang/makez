package agent

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

type memoryWorkspace map[string]string

func (w memoryWorkspace) Read(_ context.Context, name string) (io.ReadCloser, error) {
	data, ok := w[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

func TestCollectRejectsInvalidResults(t *testing.T) {
	tests := []struct {
		name, manifest string
		exit           core.ProcessExit
	}{
		{"missing", "", core.ProcessExit{}},
		{"invalid", "not-json", core.ProcessExit{}},
		{"missing success", `{"summary":"ok"}`, core.ProcessExit{}},
		{"failed exit", `{"succeeded":true,"summary":"ok"}`, core.ProcessExit{Code: 1}},
		{"signal", `{"succeeded":true,"summary":"ok"}`, core.ProcessExit{Signal: "SIGTERM"}},
		{"unknown field", `{"succeeded":true,"summary":"ok","ignore":true}`, core.ProcessExit{}},
		{"traversal", `{"succeeded":true,"summary":"ok","artifacts":[{"path":"../secret"}]}`, core.ProcessExit{}},
		{"duplicate", `{"succeeded":true,"summary":"ok","artifacts":[{"path":"a"},{"path":"/workspace/a"}]}`, core.ProcessExit{}},
		{"trailing", `{"succeeded":true,"summary":"ok"}{}`, core.ProcessExit{}},
		{"oversize", strings.Repeat("a", (64<<10)+1), core.ProcessExit{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := memoryWorkspace{}
			if test.manifest != "" {
				workspace[manifestPath] = test.manifest
			}
			if _, err := NewDemo().Collect(context.Background(), workspace, test.exit); err == nil {
				t.Fatal("expected invalid result error")
			}
		})
	}
}

func TestCollectIncludesManifestAndNormalizesArtifacts(t *testing.T) {
	workspace := memoryWorkspace{manifestPath: `{"succeeded":true,"summary":"done","artifacts":[{"path":"out/report.md","media_type":"text/markdown","required":true}]}`}
	result, err := NewDemo().Collect(context.Background(), workspace, core.ProcessExit{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded || len(result.Artifacts) != 2 || result.Artifacts[1].Path != "/workspace/out/report.md" || !result.Artifacts[0].Required {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestPlanCannotInjectCommand(t *testing.T) {
	a := NewDemo()
	plan, err := a.Plan(core.AgentInput{Prompt: "$(touch /tmp/oops); `echo hi`", Config: json.RawMessage(`{"delay_seconds":0}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Command.Argv) != 3 || string(plan.Files[0].Data) != "$(touch /tmp/oops); `echo hi`" {
		t.Fatal("prompt was mixed with command")
	}
	for _, config := range []string{`{"delay_seconds":301}`, `{"argv":["sh"]}`, `{"delay_seconds":1.5}`} {
		if _, err := a.Plan(core.AgentInput{Prompt: "hello", Config: json.RawMessage(config)}); err == nil {
			t.Fatalf("accepted config %s", config)
		}
	}
	config := Config{ID: "cli", Version: "v1", Argv: []string{"/usr/bin/test"}, Env: map[string]string{"MODE": "fixed"}}
	adapter, err := NewCLI(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Argv[0], config.Env["MODE"] = "/bin/sh", "changed"
	plan, err = adapter.Plan(core.AgentInput{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Command.Argv[0] != "/usr/bin/test" || plan.Command.Env["MODE"] != "fixed" {
		t.Fatal("adapter configuration is mutable")
	}
}

func TestDecodeUTF8AcrossPagesAndReplay(t *testing.T) {
	a := NewDemo()
	first, err := a.Decode(core.OutputPage{Records: []core.OutputRecord{{Offset: "0", Stream: "stdout", Data: []byte{'a', 0xe2, 0x82}}}, NextCursor: "3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	page := core.OutputPage{Records: []core.OutputRecord{{Offset: "3", Stream: "stdout", Data: []byte{0xac, 'b', 0xe2}}}, NextCursor: "6"}
	second, err := a.Decode(page, first.NextState)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := a.Decode(page, first.NextState)
	if err != nil || !reflect.DeepEqual(second, replay) {
		t.Fatal("parser replay differs")
	}
	last, err := a.Decode(core.OutputPage{EOF: true, NextCursor: "6"}, second.NextState)
	if err != nil {
		t.Fatal(err)
	}
	var output string
	for _, decoded := range []core.DecodedOutput{first, second, last} {
		for _, event := range decoded.Events {
			var log map[string]string
			if err := json.Unmarshal(event.Payload, &log); err != nil {
				t.Fatal(err)
			}
			output += log["text"]
		}
	}
	if output != "a€b�" {
		t.Fatalf("decoded %q", output)
	}
}

func TestDecodeUTF8SplitInsideFinalPage(t *testing.T) {
	output, err := NewDemo().Decode(core.OutputPage{Records: []core.OutputRecord{
		{Offset: "0", Stream: "stdout", Data: []byte{0xe2, 0x82}}, {Offset: "2", Stream: "stdout", Data: []byte{0xac}},
	}, NextCursor: "3", EOF: true}, nil)
	if err != nil || len(output.Events) != 1 || !strings.Contains(string(output.Events[0].Payload), "€") {
		t.Fatalf("bad UTF8 output: %#v, %v", output, err)
	}
}
