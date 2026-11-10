package agent

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
)

// PiConfig is host-owned. A task cannot change the binary, the pin, or which
// secret is injected. An empty secret means the process starts offline.
type PiConfig struct {
	Binary    string `json:"binary"`
	SecretRef string `json:"secret_ref"` // Optional name@version. The value is resolved by the worker, never here.
	SecretEnv string `json:"secret_env"` // Destination name inside the sandbox. Required when SecretRef is set.
}

type piAdapter struct {
	binary    string
	secretRef string
	secretEnv string
	base      *cli
}

// NewPi registers the pinned Pi adapter. It does not grant Pi a sandbox,
// a tool-approval policy, or the worker environment.
func NewPi(config PiConfig) (core.AgentAdapter, error) {
	binary := config.Binary
	if binary == "" {
		binary = piharness.DefaultBinary
	}
	if !path.IsAbs(binary) || strings.ContainsRune(binary, 0) {
		return nil, fmt.Errorf("pi binary must be an absolute path")
	}
	if config.SecretRef == "" && config.SecretEnv != "" {
		return nil, fmt.Errorf("pi secret env requires a versioned secret ref")
	}
	if config.SecretRef != "" {
		name, version, ok := strings.Cut(config.SecretRef, "@")
		if !ok || name == "" || version == "" || strings.Contains(version, "@") || strings.ContainsAny(config.SecretRef, " \t\r\n\x00") {
			return nil, fmt.Errorf("pi requires a versioned SecretRef (name@version)")
		}
		if !forwardName(config.SecretEnv) {
			return nil, fmt.Errorf("pi secret env name is reserved or invalid")
		}
	}
	return &piAdapter{
		binary:    binary,
		secretRef: config.SecretRef,
		secretEnv: config.SecretEnv,
		base:      &cli{config: Config{ID: piharness.AdapterID, Version: piharness.AdapterVersion, Argv: []string{"/usr/local/bin/python3", "-u", piharness.ScriptPath}}},
	}, nil
}

func forwardName(name string) bool {
	if name == "" || name == "PATH" || name == "HOME" || name == "LANG" || name == "PI_OFFLINE" || strings.HasPrefix(name, "MAKEZ_") {
		return false
	}
	if name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for _, r := range name {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func (a *piAdapter) Identity() core.AdapterIdentity {
	return core.AdapterIdentity{ID: piharness.AdapterID, Version: piharness.AdapterVersion}
}

func (a *piAdapter) Requirements() []core.Capability {
	required := []core.Capability{core.BackgroundProcess, core.OperationLookup, core.ReplayableOutput, core.ProcessStop, core.FileAccess, core.FiniteLifetime}
	if a.secretRef != "" {
		required = append(required, core.SecretInjection)
	}
	return required
}

// PiLaunchSpec returns the host-owned process settings for one attempt.
// Offline is true only when no secret is configured. The returned env list
// names a destination; it never contains a secret value.
func (a *piAdapter) PiLaunchSpec() (string, []string, bool) {
	if a.secretRef == "" {
		return a.binary, nil, true
	}
	return a.binary, []string{a.secretEnv}, false
}

func (a *piAdapter) Plan(input core.AgentInput) (core.AgentPlan, error) {
	if strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 256<<10 || !utf8.ValidString(input.Prompt) {
		return core.AgentPlan{}, fmt.Errorf("prompt must be valid UTF-8 with 1..262144 bytes")
	}
	if len(input.Assets) != 0 {
		return core.AgentPlan{}, fmt.Errorf("input assets are not supported in this version")
	}
	if _, err := piharness.ParseExecution(input.Config); err != nil {
		return core.AgentPlan{}, err
	}
	// The runner's only argument is the instruction file. Omitting it exits before Pi starts.
	argv := append(append([]string(nil), a.base.config.Argv...), piharness.InstructionPath)
	plan := core.AgentPlan{
		Command:           core.CommandSpec{Argv: argv, Workdir: "/workspace", Env: map[string]string{}},
		Files:             []core.InputFile{{Path: piharness.ScriptPath, Data: piharness.RunnerScript()}},
		Stop:              core.StopPolicy{Signal: "SIGTERM", GracePeriod: 15 * time.Second},
		RequiredArtifacts: []string{piharness.ResultPath},
	}
	if a.secretRef != "" {
		plan.SecretRefs = map[string]string{a.secretEnv: a.secretRef}
	}
	return plan, nil
}

func (a *piAdapter) Decode(page core.OutputPage, previous []byte) (core.DecodedOutput, error) {
	return a.base.Decode(page, previous)
}

func (a *piAdapter) Collect(ctx context.Context, workspace core.WorkspaceReader, exit core.ProcessExit) (core.AgentResult, error) {
	return a.base.Collect(ctx, workspace, exit)
}
