package piharness

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecideDoesNotResendACommandThatMightHaveBeenAccepted(t *testing.T) {
	cases := []struct {
		name    string
		prev    Disposition
		same    bool
		history History
		want    Action
	}{
		{"fresh", DispositionNone, true, HistoryUnread, Action{SendPrompt: true}},
		{"rejected", DispositionRejected, true, HistoryUnread, Action{SendPrompt: true}},
		{"settled new work", DispositionSettled, true, HistoryUnread, Action{SendPrompt: true}},
		{"sending unread", DispositionSending, true, HistoryUnread, Action{ReadHistory: true}},
		{"accepted unread", DispositionAccepted, true, HistoryUnread, Action{ReadHistory: true}},
		{"accepted and present", DispositionAccepted, true, HistoryHasCommand, Action{}},
		{"accepted but absent", DispositionAccepted, true, HistoryAbsent, Action{}},
		{"sending absent still blocked", DispositionSending, true, HistoryAbsent, Action{}},
		{"different prompt waits for a read", DispositionAccepted, false, HistoryUnread, Action{ReadHistory: true}},
		{"different prompt after a read", DispositionAccepted, false, HistoryAbsent, Action{SendPrompt: true}},
		{"reconciled same command", DispositionReconciled, true, HistoryHasCommand, Action{}},
		{"reconciled different command", DispositionReconciled, false, HistoryUnread, Action{SendPrompt: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.prev, tc.same, tc.history)
			if got != tc.want {
				t.Fatalf("Decide(%s same=%v history=%v)=%+v, want %+v", tc.prev, tc.same, tc.history, got, tc.want)
			}
		})
	}
}

func TestMode(t *testing.T) {
	if Mode(Action{SendPrompt: true}) != "send" {
		t.Fatal("send")
	}
	if Mode(Action{ReadHistory: true}) != "reconcile_only" {
		t.Fatal("read without send")
	}
	if Mode(Action{ReadHistory: true, SendPrompt: true}) != "reconcile_then_send" {
		t.Fatal("read then send")
	}
	if Mode(Action{}) != "reconcile_only" {
		t.Fatal("blocked")
	}
}

func TestPersonalAndWorkerSessionsStayApart(t *testing.T) {
	personal := validExecution(t, Execution{Contract: Contract, Scope: "personal", BotID: "bot-1", ConversationID: "conv-1", HarnessPin: Pin})
	worker := validExecution(t, Execution{Contract: Contract, Scope: "worker", DelegationRunID: "run-1", HarnessPin: Pin})
	if personal.BotID == "" || worker.DelegationRunID == "" {
		t.Fatal("identities")
	}
	if personal.Scope == worker.Scope {
		t.Fatal("scopes alias")
	}
	raw, _ := json.Marshal(map[string]any{"execution": map[string]any{
		"contract": Contract, "scope": "personal", "bot_id": "bot-1", "conversation_id": "conv-1",
		"harness_pin": strings.Repeat("a", 40),
	}})
	if _, err := ParseExecution(raw); err == nil {
		t.Fatal("a different pin must not resume this build")
	}
}

func TestParseExecutionRejectsBusinessFieldsMakezDoesNotOwn(t *testing.T) {
	base := map[string]any{
		"contract": Contract, "scope": "personal", "bot_id": "bot-1", "conversation_id": "conv-1", "harness_pin": Pin,
	}
	for _, extra := range []string{"owner_id", "turn_id", "message_seq", "source_offset", "retention_seconds"} {
		body := map[string]any{}
		for k, v := range base {
			body[k] = v
		}
		body[extra] = "x"
		raw, _ := json.Marshal(map[string]any{"execution": body})
		if _, err := ParseExecution(raw); err == nil {
			t.Fatalf("accepted %s", extra)
		}
	}
	if _, err := ParseExecution([]byte(`{}`)); err == nil {
		t.Fatal("empty config")
	}
	mixed, _ := json.Marshal(map[string]any{"execution": map[string]any{
		"contract": Contract, "scope": "worker", "delegation_run_id": "run-1", "bot_id": "bot-1", "harness_pin": Pin,
	}})
	if _, err := ParseExecution(mixed); err == nil {
		t.Fatal("worker session must not carry a personal conversation")
	}
}

func TestPromptHashIsTheByteString(t *testing.T) {
	if PromptHash("Hello") == PromptHash("Hello ") {
		t.Fatal("trailing space aliased")
	}
	if !SameCommand(PromptHash("Hello"), "Hello") || SameCommand(PromptHash("Hello"), "Hello ") {
		t.Fatal("same command")
	}
	if SameCommand("", "Hello") {
		t.Fatal("missing hash is not a match")
	}
}

func validExecution(t *testing.T, exec Execution) Execution {
	t.Helper()
	raw, err := json.Marshal(map[string]Execution{"execution": exec})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseExecution(raw)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
