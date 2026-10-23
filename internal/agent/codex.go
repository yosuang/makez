package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/makez-dev/makez/internal/core"
)

// CodexConfig is host-owned configuration, never supplied by a Task.
type CodexConfig struct {
	Model     string
	AuthMode  string // api_key, codex_access_token, or explicit trusted-local chatgpt_session.
	SecretRef string // Logical name@version; resolved by the Worker, never by this adapter.
}

type codex struct {
	config CodexConfig
	base   *cli
}

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// NewCodex validates configuration for the Codex CLI adapter.
func NewCodex(config CodexConfig) (core.AgentAdapter, error) {
	if config.AuthMode != "codex_access_token" && config.AuthMode != "api_key" && config.AuthMode != "chatgpt_session" {
		return nil, fmt.Errorf("codex auth mode must be codex_access_token, api_key, or chatgpt_session")
	}
	name, version, ok := strings.Cut(config.SecretRef, "@")
	if !ok || name == "" || version == "" || len(config.SecretRef) > 256 || strings.ContainsAny(config.SecretRef, " \t\r\n\x00") || strings.Contains(version, "@") {
		return nil, fmt.Errorf("codex requires a versioned SecretRef (name@version)")
	}
	if config.Model != "" && !modelName.MatchString(config.Model) {
		return nil, fmt.Errorf("invalid Codex model name")
	}
	argv := []string{"/usr/local/bin/codex-runner", "--auth-mode", config.AuthMode}
	if config.Model != "" {
		argv = append(argv, "--model", config.Model)
	}
	return &codex{config: config, base: &cli{config: Config{ID: "codex", Version: "1", Argv: argv}}}, nil
}

func (a *codex) Identity() core.AdapterIdentity { return a.base.Identity() }
func (a *codex) Requirements() []core.Capability {
	return append(a.base.Requirements(), core.SecretInjection)
}
func (a *codex) Plan(input core.AgentInput) (core.AgentPlan, error) {
	plan, err := a.base.Plan(input)
	if err != nil {
		return plan, err
	}
	variable := "CODEX_ACCESS_TOKEN"
	switch a.config.AuthMode {
	case "api_key":
		variable = "CODEX_API_KEY"
	case "chatgpt_session":
		variable = "CODEX_AUTH_JSON"
	}
	plan.SecretRefs = map[string]string{variable: a.config.SecretRef}
	plan.Files = append(plan.Files, core.InputFile{Path: "/workspace/result-schema.json", Data: []byte(codexResultSchema)})
	return plan, nil
}

func (a *codex) Collect(ctx context.Context, workspace core.WorkspaceReader, exit core.ProcessExit) (core.AgentResult, error) {
	if exit.Code == 77 {
		return core.AgentResult{}, fmt.Errorf("codex credential was not injected; check the registered SecretRef")
	}
	result, err := a.base.Collect(ctx, workspace, exit)
	if err != nil {
		return result, err
	}
	if result.Succeeded && len(result.Artifacts) < 2 {
		return core.AgentResult{}, fmt.Errorf("codex reported success without a deliverable")
	}
	for _, candidate := range result.Artifacts[1:] {
		if !strings.HasPrefix(candidate.Path, "/workspace/deliverables/") || !candidate.Required {
			return core.AgentResult{}, fmt.Errorf("codex deliverables must be required files under /workspace/deliverables")
		}
	}
	return result, nil
}

// Eight KiB fragments bound both parser checkpoints and JSON-escaped event payloads.
const codexFragmentBytes = 8 << 10

func (a *codex) Decode(page core.OutputPage, previous []byte) (core.DecodedOutput, error) {
	state := decoderState{Pending: map[string][]byte{}}
	if len(previous) > 24<<10 {
		return core.DecodedOutput{}, fmt.Errorf("codex parser checkpoint too large")
	}
	if len(previous) != 0 {
		if err := decodeStrict(previous, &state); err != nil {
			return core.DecodedOutput{}, err
		}
	}
	if state.Pending == nil {
		state.Pending = map[string][]byte{}
	}
	for stream, pending := range state.Pending {
		if (stream != "stdout" && stream != "stderr") || len(pending) >= codexFragmentBytes {
			return core.DecodedOutput{}, fmt.Errorf("invalid Codex parser checkpoint")
		}
	}
	result := core.DecodedOutput{}
	emit := func(stream, offset string, index uint32, data []byte) error {
		if len(result.Events) >= 1024 {
			return fmt.Errorf("codex output contains too many events in one page")
		}
		var event struct {
			Type string `json:"type"`
		}
		kind := "log"
		payload := json.RawMessage(nil)
		if stream == "stdout" && json.Unmarshal(data, &event) == nil && event.Type != "" {
			switch event.Type {
			case "thread.started", "turn.started", "turn.failed", "error", "makez.error":
				kind = "status"
			case "turn.completed":
				kind = "usage"
			}
			payload = bytes.Clone(bytes.TrimSpace(data))
		} else {
			payload, _ = json.Marshal(map[string]string{"stream": stream, "text": strings.ToValidUTF8(string(data), "\uFFFD")})
		}
		result.Events = append(result.Events, core.AgentEvent{SourceOffset: offset, Index: index, Kind: kind, Payload: payload})
		return nil
	}
	total := 0
	for _, record := range page.Records {
		if record.Stream != "stdout" && record.Stream != "stderr" {
			return result, fmt.Errorf("invalid output stream")
		}
		total += len(record.Data)
		if total > 1<<20 {
			return result, fmt.Errorf("codex output page exceeds parser limit")
		}
		pending := append(state.Pending[record.Stream], record.Data...)
		var index uint32
		for len(pending) > 0 {
			length := bytes.IndexByte(pending, '\n') + 1
			if length == 0 || length > codexFragmentBytes {
				if len(pending) < codexFragmentBytes {
					break
				}
				length = codexFragmentBytes
				if len(pending) > length {
					for length > 0 && !utf8.RuneStart(pending[length]) {
						length--
					}
				} else {
					for i := max(0, length-3); i < length; i++ {
						if utf8.RuneStart(pending[i]) && !utf8.FullRune(pending[i:length]) {
							length = i
							break
						}
					}
				}
				if length == 0 {
					length = codexFragmentBytes
				}
			}
			if err := emit(record.Stream, record.Offset, index, pending[:length]); err != nil {
				return result, err
			}
			pending = pending[length:]
			index++
		}
		state.Pending[record.Stream] = bytes.Clone(pending)
	}
	if page.EOF {
		for index, stream := range []string{"stdout", "stderr"} {
			if len(state.Pending[stream]) > 0 {
				if err := emit(stream, "eof:"+page.NextCursor, uint32(index), state.Pending[stream]); err != nil {
					return result, err
				}
				delete(state.Pending, stream)
			}
		}
	}
	result.NextState, _ = json.Marshal(state)
	return result, nil
}

const codexResultSchema = `{
  "type": "object",
  "properties": {
    "succeeded": {"type": "boolean"},
    "summary": {"type": "string"},
    "artifacts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string"},
          "media_type": {"type": "string"},
          "required": {"type": "boolean"}
        },
        "required": ["path", "media_type", "required"],
        "additionalProperties": false
      }
    }
  },
  "required": ["succeeded", "summary", "artifacts"],
  "additionalProperties": false
}`
