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
	// ReleasePath is written after the process exists and the command row is marked started.
	// The runner must not send the prompt before this file exists.
	ReleasePath = "/workspace/pi-release"
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

// Evidence is what this process can prove about one prompt.
// Started and a live process are not Pi acceptance. Official Pi RPC has no idempotency key.
type Evidence struct {
	Command         Disposition
	Started         bool
	Owner           bool
	RunActive       bool
	ProcessLive     bool
	ProcessKnown    bool
	FileKnown       bool
	ReleaseWritten  bool
	FileDisposition Disposition
}

// Recovery is the next local action. It does not mark a task successful.
type Recovery struct {
	Class   string
	Send    bool
	Release bool
}

// Delivery classes. They are local proof, not a Pi checkpoint result.
const (
	ClassNotSent   = "not_sent"
	ClassHandedOff = "handed_off"
	ClassAccepted  = "accepted"
	ClassUnknown   = "unknown"
)

// Recover chooses one action from local evidence.
// Send is allowed only when this process can prove the prompt bytes were not written.
// Release completes that one write on a live process. A possible handoff is not replayed.
func Recover(ev Evidence) Recovery {
	switch ev.Command {
	case "", DispositionNone, DispositionRejected:
		return Recovery{Class: ClassNotSent, Send: true}
	case DispositionAccepted:
		return Recovery{Class: ClassAccepted}
	case DispositionSettled:
		// A settled row is a finished local observation. A later send is a new turn.
		return Recovery{Class: ClassAccepted, Send: true}
	case DispositionReconciled:
		return Recovery{Class: ClassAccepted}
	case DispositionSending:
		if !ev.Started {
			return Recovery{Class: ClassNotSent, Send: true}
		}
		if !ev.FileKnown {
			return Recovery{Class: ClassUnknown}
		}
		switch ev.FileDisposition {
		case DispositionAccepted, DispositionSettled:
			return Recovery{Class: ClassAccepted}
		case DispositionSending:
			// The runner fsyncs this before the prompt pipe write, so the bytes may already have followed.
			return Recovery{Class: ClassHandedOff}
		case DispositionRejected:
			return Recovery{Class: ClassNotSent, Send: true}
		default:
			return recoverBeforePipe(ev)
		}
	default:
		return Recovery{Class: ClassUnknown}
	}
}

// recoverBeforePipe runs when the pre-prompt disposition file is absent.
// The runner writes that file before any prompt byte, and only after the release file.
func recoverBeforePipe(ev Evidence) Recovery {
	if !ev.ProcessKnown {
		if ev.ReleaseWritten || ev.RunActive {
			return Recovery{Class: ClassHandedOff}
		}
		return Recovery{Class: ClassNotSent, Send: true}
	}
	if ev.ProcessLive && ev.ReleaseWritten {
		return Recovery{Class: ClassHandedOff}
	}
	if ev.ProcessLive && !ev.ReleaseWritten && ev.RunActive && ev.Owner {
		return Recovery{Class: ClassNotSent, Release: true}
	}
	if ev.ProcessLive && !ev.ReleaseWritten && ev.RunActive {
		return Recovery{Class: ClassNotSent}
	}
	return Recovery{Class: ClassNotSent, Send: true}
}

// Deliver decides one prompt when the sandbox files were not read.
// A started row without those files is treated as a possible handoff, not as acceptance.
func Deliver(prev Disposition, sameCommand bool, history History, processStarted bool) Action {
	ev := Evidence{
		Command: prev, Started: processStarted, Owner: sameCommand, RunActive: true,
		ProcessLive: processStarted, ProcessKnown: true, FileKnown: true, ReleaseWritten: processStarted,
	}
	if Recover(ev).Send {
		return Action{SendPrompt: true}
	}
	return Decide(prev, sameCommand, history)
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
