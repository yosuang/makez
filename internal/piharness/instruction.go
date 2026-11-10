package piharness

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// Instruction is the per-attempt file. It is not part of the frozen plan, so a
// later attempt can read history instead of sending the same prompt again.
type Instruction struct {
	Binary            string   `json:"binary"`
	Prompt            string   `json:"prompt"`
	Mode              string   `json:"mode"`
	SessionDir        string   `json:"session_dir"`
	Checkpoint        string   `json:"checkpoint"`
	DispositionPath   string   `json:"disposition_path"`
	HistoryPath       string   `json:"history_path"`
	ResultPath        string   `json:"result_path"`
	ForwardEnv        []string `json:"forward_env"`
	Offline           bool     `json:"offline"`
	PackageVersion    string   `json:"package_version"`
	SettleSeconds     int      `json:"settle_seconds"`
	RestoreCheckpoint bool     `json:"restore_checkpoint"`
}

// BuildInstruction checks one attempt and returns canonical JSON.
func BuildInstruction(in Instruction) ([]byte, error) {
	if in.Mode != "send" && in.Mode != "reconcile_only" && in.Mode != "reconcile_then_send" {
		return nil, fmt.Errorf("unsupported runner mode")
	}
	for _, candidate := range []string{in.Binary, in.SessionDir, in.Checkpoint, in.DispositionPath, in.HistoryPath, in.ResultPath} {
		if !absolute(candidate) {
			return nil, fmt.Errorf("runner path must be absolute")
		}
	}
	if in.PackageVersion != PackageVersion {
		return nil, fmt.Errorf("runner package version does not match the pin")
	}
	if in.SettleSeconds < 1 || in.SettleSeconds > 7200 {
		return nil, fmt.Errorf("runner settle window is invalid")
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, fmt.Errorf("runner prompt is empty")
	}
	for _, name := range in.ForwardEnv {
		if !forwardable(name) {
			return nil, fmt.Errorf("runner cannot forward %q", name)
		}
	}
	if in.ForwardEnv == nil {
		in.ForwardEnv = []string{}
	}
	return json.Marshal(in)
}

func absolute(value string) bool {
	return path.IsAbs(value) && path.Clean(value) == value && !strings.ContainsRune(value, 0)
}

func forwardable(name string) bool {
	if name == "" || name == "PATH" || name == "HOME" || name == "LANG" || name == "PI_OFFLINE" || strings.HasPrefix(name, "MAKEZ_") {
		return false
	}
	for _, r := range name {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return name[0] < '0' || name[0] > '9'
}
