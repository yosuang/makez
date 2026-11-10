// Package piharness is Makez's policy for a Pi process.
// Pi is not the execution authority and is not a security boundary.
package piharness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	// AdapterID is the registered agent id. The harness pin is not this version.
	AdapterID = "pi"
	// AdapterVersion is the Makez adapter contract, not the Pi package version.
	AdapterVersion = "1"
	// Contract is the execution-config spelling proposed for Philo.
	Contract = "2026-11-10.1"
	// PackageVersion is the probed npm / CLI version.
	PackageVersion = "1.5.1"
	// Pin is the git commit of official Pi v1.5.1. v1.5.0 is not this pin.
	Pin = "dd30101cfc5b0609411dd80b6f93adbc14db14cf"
	// DefaultBinary is the path inside the runtime image, not on the worker host.
	DefaultBinary = "/usr/local/bin/pi"
	// InstructionPath is the per-attempt runner instruction inside the sandbox.
	InstructionPath = "/workspace/pi-instruction.json"
	// ScriptPath is the embedded runner written at Prepare.
	ScriptPath = "/workspace/makez-pi-run.py"
	// DispositionPath is the fsynced recovery ledger for the current process.
	DispositionPath = "/workspace/pi-disposition.json"
	// HistoryPath is the get_messages snapshot from a reconcile read.
	HistoryPath = "/workspace/pi-history.json"
	// ResultPath is the runner's result manifest, same location as demo.
	ResultPath = "/workspace/result.json"
	// SessionDir is Pi's session directory. It is not --no-session.
	SessionDir = "/workspace/pi-session"
	// CheckpointPath is the only file copied into the native-session row.
	CheckpointPath = "/workspace/pi-session/checkpoint.jsonl"
	// MaxAttempts bounds process generations of one run.
	MaxAttempts = 3
	// MaxCheckpoint is the durable copy of one Pi session file.
	MaxCheckpoint = 1 << 20
)

// Disposition is how far the last native prompt got. It is not a task state.
type Disposition string

// Recovery dispositions. Same-command resend is refused after sending or accepted.
const (
	DispositionNone       Disposition = "none"
	DispositionSending    Disposition = "sending"
	DispositionAccepted   Disposition = "accepted"
	DispositionSettled    Disposition = "settled"
	DispositionRejected   Disposition = "rejected"
	DispositionReconciled Disposition = "reconciled"
)

// History is what a read of native messages established.
type History int

// History values are the result of one get_messages read, or the fact that it was not read.
const (
	HistoryUnread History = iota
	HistoryHasCommand
	HistoryAbsent
)

// Action is the next native step. A read is not a prompt.
type Action struct {
	ReadHistory bool
	SendPrompt  bool
}

// Decide applies the recovery rule. A command that might already have been
// accepted is not sent again. A different prompt may be sent only after a read.
func Decide(prev Disposition, sameCommand bool, history History) Action {
	switch prev {
	case "", DispositionNone, DispositionSettled, DispositionRejected:
		return Action{SendPrompt: true}
	case DispositionReconciled:
		if sameCommand {
			return Action{}
		}
		return Action{SendPrompt: true}
	default:
		if history == HistoryUnread {
			return Action{ReadHistory: true}
		}
		if sameCommand {
			return Action{}
		}
		return Action{SendPrompt: true}
	}
}

// Mode maps an action onto the runner instruction.
func Mode(action Action) string {
	if action.ReadHistory && action.SendPrompt {
		return "reconcile_then_send"
	}
	if action.SendPrompt {
		return "send"
	}
	return "reconcile_only"
}

// Execution is the caller-supplied correlation for one logical native session.
// It does not create a conversation, turn, or message sequence.
type Execution struct {
	Contract        string `json:"contract"`
	Scope           string `json:"scope"`
	BotID           string `json:"bot_id,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
	DelegationRunID string `json:"delegation_run_id,omitempty"`
	HarnessPin      string `json:"harness_pin"`
}

type executionEnvelope struct {
	Execution Execution `json:"execution"`
}

// ParseExecution reads the versioned execution object. Unknown fields are rejected.
func ParseExecution(config json.RawMessage) (Execution, error) {
	if len(config) == 0 || string(config) == "null" {
		return Execution{}, fmt.Errorf("pi execution config is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(config))
	decoder.DisallowUnknownFields()
	var envelope executionEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return Execution{}, fmt.Errorf("pi execution config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Execution{}, fmt.Errorf("pi execution config: trailing JSON")
	}
	exec := envelope.Execution
	if exec.Contract != Contract {
		return Execution{}, fmt.Errorf("unsupported execution contract")
	}
	if exec.HarnessPin != Pin {
		return Execution{}, fmt.Errorf("harness pin does not match the registered Pi build")
	}
	switch exec.Scope {
	case "personal":
		if !opaque(exec.BotID) || !opaque(exec.ConversationID) || exec.DelegationRunID != "" {
			return Execution{}, fmt.Errorf("personal scope requires bot_id and conversation_id")
		}
	case "worker":
		if !opaque(exec.DelegationRunID) || exec.BotID != "" || exec.ConversationID != "" {
			return Execution{}, fmt.Errorf("worker scope requires delegation_run_id")
		}
	default:
		return Execution{}, fmt.Errorf("execution scope must be personal or worker")
	}
	return exec, nil
}

func opaque(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return false
	}
	return strings.TrimSpace(value) == value
}

// PromptHash identifies one prompt byte string. It is not an idempotency key.
func PromptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// SameCommand reports whether this prompt is the one left in an unknown state.
func SameCommand(previousHash, prompt string) bool {
	return previousHash != "" && previousHash == PromptHash(prompt)
}
