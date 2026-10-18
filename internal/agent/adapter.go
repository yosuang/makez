// Package agent adapts native Agent commands without implementing their loops.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/core"
)

const manifestPath = "/workspace/result.json"

// Config is trusted host configuration. Task input cannot replace Argv or Env.
type Config struct {
	ID      string
	Version string
	Argv    []string
	Env     map[string]string // Non-secret values; resolve secret references in the execution layer.
}

type cli struct {
	config Config
	demo   bool
}

func NewCLI(config Config) (core.AgentAdapter, error) {
	if config.ID == "" || config.Version == "" || len(config.Argv) == 0 || !path.IsAbs(config.Argv[0]) {
		return nil, fmt.Errorf("CLI ID, version and absolute executable path are required")
	}
	for _, value := range config.Argv {
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("CLI argv contains NUL")
		}
	}
	for key, value := range config.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid CLI environment")
		}
	}
	config.Argv = append([]string(nil), config.Argv...)
	env := make(map[string]string, len(config.Env))
	for key, value := range config.Env {
		env[key] = value
	}
	config.Env = env
	return &cli{config: config}, nil
}

// NewDemo returns a deterministic fixture, not an LLM-backed Agent.
func NewDemo() core.AgentAdapter {
	return &cli{config: Config{ID: "demo", Version: "1", Argv: []string{"/usr/local/bin/python3", "-u", "/workspace/demo.py"}}, demo: true}
}

func (a *cli) Identity() core.AdapterIdentity {
	return core.AdapterIdentity{ID: a.config.ID, Version: a.config.Version}
}
func (a *cli) Requirements() []core.Capability {
	return []core.Capability{core.BackgroundProcess, core.OperationLookup, core.ReplayableOutput, core.ProcessStop, core.FileAccess, core.FiniteLifetime}
}

func (a *cli) Plan(input core.AgentInput) (core.AgentPlan, error) {
	if strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 256<<10 || !utf8.ValidString(input.Prompt) {
		return core.AgentPlan{}, fmt.Errorf("prompt must be valid UTF-8 with 1..262144 bytes")
	}
	if len(input.Assets) != 0 {
		return core.AgentPlan{}, fmt.Errorf("input assets are not supported in this version")
	}
	env := make(map[string]string, len(a.config.Env))
	for key, value := range a.config.Env {
		env[key] = value
	}
	plan := core.AgentPlan{
		Command:           core.CommandSpec{Argv: append([]string(nil), a.config.Argv...), Workdir: "/workspace", Env: env},
		Files:             []core.InputFile{{Path: "/workspace/input.txt", Data: []byte(input.Prompt)}},
		Stop:              core.StopPolicy{Signal: "SIGTERM", GracePeriod: 15 * time.Second},
		RequiredArtifacts: []string{manifestPath},
	}
	if a.demo {
		config := struct {
			DelaySeconds int `json:"delay_seconds"`
		}{DelaySeconds: 3}
		if len(input.Config) > 0 && string(input.Config) != "null" {
			if err := decodeStrict(input.Config, &config); err != nil {
				return core.AgentPlan{}, fmt.Errorf("demo config: %w", err)
			}
		}
		if config.DelaySeconds < 0 || config.DelaySeconds > 300 {
			return core.AgentPlan{}, fmt.Errorf("demo delay_seconds must be between 0 and 300")
		}
		data, _ := json.Marshal(config)
		plan.Files = append(plan.Files, core.InputFile{Path: "/workspace/demo.py", Data: []byte(demoScript)}, core.InputFile{Path: "/workspace/demo-config.json", Data: data})
	} else if len(input.Config) > 0 && string(input.Config) != "null" && string(input.Config) != "{}" {
		return core.AgentPlan{}, fmt.Errorf("CLI task config is not supported; configure argv on the host")
	}
	return plan, nil
}

type decoderState struct {
	Pending map[string][]byte `json:"pending,omitempty"`
}

func (a *cli) Decode(page core.OutputPage, previousState []byte) (core.DecodedOutput, error) {
	state := decoderState{Pending: map[string][]byte{}}
	if len(previousState) > 0 {
		if len(previousState) > 256 {
			return core.DecodedOutput{}, fmt.Errorf("invalid parser checkpoint")
		}
		if err := decodeStrict(previousState, &state); err != nil {
			return core.DecodedOutput{}, err
		}
		if len(state.Pending) > 2 {
			return core.DecodedOutput{}, fmt.Errorf("invalid parser checkpoint")
		}
		for stream, data := range state.Pending {
			if (stream != "stdout" && stream != "stderr") || len(data) > 3 {
				return core.DecodedOutput{}, fmt.Errorf("invalid parser checkpoint")
			}
		}
	}
	if state.Pending == nil {
		state.Pending = map[string][]byte{}
	}
	result := core.DecodedOutput{}
	total := 0
	for _, record := range page.Records {
		if record.Stream != "stdout" && record.Stream != "stderr" {
			return result, fmt.Errorf("invalid output stream")
		}
		total += len(record.Data)
		if total > 1<<20 {
			return result, fmt.Errorf("output page exceeds parser limit")
		}
		data := append(append([]byte(nil), state.Pending[record.Stream]...), record.Data...)
		end := len(data)
		for i := max(0, len(data)-3); i < len(data); i++ {
			if utf8.RuneStart(data[i]) && !utf8.FullRune(data[i:]) {
				end = i
				break
			}
		}
		state.Pending[record.Stream] = append([]byte(nil), data[end:]...)
		if end > 0 {
			payload, _ := json.Marshal(map[string]string{"stream": record.Stream, "text": strings.ToValidUTF8(string(data[:end]), "\uFFFD")})
			result.Events = append(result.Events, core.AgentEvent{SourceOffset: record.Offset, Kind: "log", Payload: payload})
		}
	}
	if page.EOF {
		for index, stream := range []string{"stdout", "stderr"} {
			if len(state.Pending[stream]) != 0 {
				payload, _ := json.Marshal(map[string]string{"stream": stream, "text": strings.ToValidUTF8(string(state.Pending[stream]), "\uFFFD")})
				result.Events = append(result.Events, core.AgentEvent{SourceOffset: "eof:" + page.NextCursor, Index: uint32(index), Kind: "log", Payload: payload})
				delete(state.Pending, stream)
			}
		}
	}
	result.NextState, _ = json.Marshal(state)
	return result, nil
}

func (a *cli) Collect(ctx context.Context, workspace core.WorkspaceReader, exit core.ProcessExit) (core.AgentResult, error) {
	if exit.Code != 0 || exit.Signal != "" {
		return core.AgentResult{}, fmt.Errorf("agent exited unsuccessfully: code=%d signal=%s", exit.Code, exit.Signal)
	}
	file, err := workspace.Read(ctx, manifestPath)
	if err != nil {
		return core.AgentResult{}, fmt.Errorf("read result manifest: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	closeErr := file.Close()
	if readErr != nil {
		return core.AgentResult{}, readErr
	}
	if closeErr != nil {
		return core.AgentResult{}, closeErr
	}
	if len(data) > 64<<10 {
		return core.AgentResult{}, fmt.Errorf("result manifest exceeds 64 KiB")
	}
	if !utf8.Valid(data) {
		return core.AgentResult{}, fmt.Errorf("result manifest is not valid UTF-8")
	}
	var manifest struct {
		Succeeded *bool  `json:"succeeded"`
		Summary   string `json:"summary"`
		Artifacts []struct {
			Path      string `json:"path"`
			MediaType string `json:"media_type"`
			Required  bool   `json:"required"`
		} `json:"artifacts"`
	}
	if err := decodeStrict(data, &manifest); err != nil {
		return core.AgentResult{}, fmt.Errorf("invalid result manifest: %w", err)
	}
	if manifest.Succeeded == nil || strings.TrimSpace(manifest.Summary) == "" || len(manifest.Summary) > 16<<10 || len(manifest.Artifacts) > artifact.MaxFiles-1 {
		return core.AgentResult{}, fmt.Errorf("result manifest requires succeeded, summary (1..16384 bytes), and at most %d artifacts", artifact.MaxFiles-1)
	}
	result := core.AgentResult{Succeeded: *manifest.Succeeded, Summary: manifest.Summary, Artifacts: []core.ArtifactCandidate{{Path: manifestPath, MediaType: "application/json", Required: true}}}
	seen := map[string]bool{manifestPath: true}
	for _, candidate := range manifest.Artifacts {
		name, err := artifact.ValidatePath(candidate.Path)
		if err != nil {
			return core.AgentResult{}, err
		}
		if seen[name] {
			return core.AgentResult{}, fmt.Errorf("duplicate artifact %q", name)
		}
		seen[name] = true
		media := candidate.MediaType
		if media == "" {
			media = "application/octet-stream"
		}
		if len(media) > 128 {
			return core.AgentResult{}, fmt.Errorf("invalid artifact media type")
		}
		if _, _, err := mime.ParseMediaType(media); err != nil {
			return core.AgentResult{}, fmt.Errorf("invalid artifact media type: %w", err)
		}
		result.Artifacts = append(result.Artifacts, core.ArtifactCandidate{Path: name, MediaType: media, Required: candidate.Required})
	}
	return result, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

const demoScript = `import json, pathlib, time
workspace = pathlib.Path('/workspace')
prompt = (workspace / 'input.txt').read_text(encoding='utf-8')
delay = json.loads((workspace / 'demo-config.json').read_text())['delay_seconds']
print('Demo Agent: infrastructure fixture; no LLM is called.', flush=True)
for step in range(3):
    print(f'progress {step + 1}/3', flush=True)
    time.sleep(delay / 3)
report = '# makez demo result\n\nThis deterministic fixture validates sandbox execution and artifact delivery. No LLM was called.\n\n## Submitted request\n\n' + prompt + '\n'
(workspace / 'report.md').write_text(report, encoding='utf-8')
result = {'succeeded': True, 'summary': 'Demo completed: execution, logs and artifacts are available. No LLM was called.', 'artifacts': [{'path': 'report.md', 'media_type': 'text/markdown; charset=utf-8', 'required': True}]}
temp = workspace / 'result.json.tmp'
temp.write_text(json.dumps(result), encoding='utf-8')
temp.replace(workspace / 'result.json')
print('Artifacts ready: report.md, result.json', flush=True)
`
