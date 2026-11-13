package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestAppendOutputRejectsAnInvalidEvent uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// AppendOutput records decoded output. It is not exactly-once.
// The batch-size check, a stale epoch, and a terminal run stay covered by the existing tests.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestAppendOutputRejectsAnInvalidEvent(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "invalid-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "invalid-event")
	epoch, err := db.ClaimObserver(ctx, runID)
	if err != nil || epoch < 1 {
		t.Fatalf("ClaimObserver epoch = %d err=%v", epoch, err)
	}
	ids := outputBatchIDs{workspace: workspace, taskID: taskID, runID: runID}

	opened := readOutputBatch(t, db, ids)
	if opened.cursor != "" || len(opened.parser) != 0 || len(opened.events) != 0 || opened.epoch != epoch || opened.run != core.RunRunning || opened.task != core.TaskRunning || opened.reason != "" || opened.task == core.TaskSucceeded {
		t.Fatalf("opened %s, want a live RUNNING run at epoch %d with an empty cursor", describeOutputBatch(opened), epoch)
	}

	// One valid event and its cursor are stored. The task stays RUNNING.
	// Outbox rows that already exist stay unchanged.
	cursor := "stdout:16"
	parser := []byte(`{}`)
	kept := core.AgentEvent{SourceOffset: cursor, Kind: "log", Payload: []byte(`{"text":"kept"}`)}
	requireValidOutputEvent(t, kept)
	if err = db.AppendOutput(ctx, runID, epoch, cursor, parser, []core.AgentEvent{kept}); err != nil {
		t.Fatal(err)
	}
	stored := readOutputBatch(t, db, ids)
	if stored.cursor != cursor || !bytes.Equal(stored.parser, parser) || !oneStoredEvent(stored.events, kept) || stored.epoch != epoch || stored.version != opened.version+1 || stored.run != core.RunRunning || stored.task != core.TaskRunning || stored.reason != opened.reason || stored.outbox != opened.outbox || stored.outboxN != opened.outboxN || stored.task == core.TaskSucceeded {
		t.Fatalf("accepted event wrote %s, want cursor %q one event %s task RUNNING outbox %d version %d", describeOutputBatch(stored), cursor, kept.SourceOffset, opened.outboxN, opened.version+1)
	}

	// The offset pad is a run of the letter o. The kind pad is a run of the letter k.
	// The payload pad is a JSON string of the letter x. None of these is a credential.
	// Each call sends one event, with a cursor and parser inside the batch limit.
	longOffset := strings.Repeat("o", 4097)
	longKind := strings.Repeat("k", 129)
	longPayload := []byte(`"` + strings.Repeat("x", 64<<10-1) + `"`)
	broken := []byte("{")
	if len(longOffset) != 4097 || len(longKind) != 129 || len(longPayload) != 64<<10+1 || !json.Valid(longPayload) || json.Valid(broken) || !bytes.Equal(broken, []byte("{")) {
		t.Fatalf("invalid sizes offset %d kind %d payload %d", len(longOffset), len(longKind), len(longPayload))
	}

	// An empty SourceOffset is rejected. The stored cursor and events stay.
	requireInvalidEventRejected(t, db, ids, epoch, stored, core.AgentEvent{SourceOffset: "", Kind: "log", Payload: []byte(`{"text":"empty-offset"}`)}, "empty source offset")

	// A SourceOffset of 4097 bytes is rejected. The stored cursor and events stay.
	requireInvalidEventRejected(t, db, ids, epoch, stored, core.AgentEvent{SourceOffset: longOffset, Kind: "log", Payload: []byte(`{"text":"long-offset"}`)}, "4097-byte source offset")

	// An empty Kind is rejected. The stored cursor and events stay.
	requireInvalidEventRejected(t, db, ids, epoch, stored, core.AgentEvent{SourceOffset: "stdout:kind", Kind: "", Payload: []byte(`{"text":"empty-kind"}`)}, "empty kind")

	// A Kind of 129 bytes is rejected. The stored cursor and events stay.
	requireInvalidEventRejected(t, db, ids, epoch, stored, core.AgentEvent{SourceOffset: "stdout:long-kind", Kind: longKind, Payload: []byte(`{"text":"long-kind"}`)}, "129-byte kind")

	// A Payload of 64<<10+1 bytes is rejected. The stored cursor and events stay.
	requireInvalidEventRejected(t, db, ids, epoch, stored, core.AgentEvent{SourceOffset: "stdout:payload", Kind: "log", Payload: longPayload}, "65537-byte payload")

	// A Payload of []byte("{") is rejected. The stored cursor and events stay.
	requireInvalidEventRejected(t, db, ids, epoch, stored, core.AgentEvent{SourceOffset: "stdout:json", Kind: "log", Payload: []byte("{")}, "payload []byte(\"{\")")
}

func requireInvalidEventRejected(t *testing.T, db *Store, ids outputBatchIDs, epoch int64, before outputBatchRow, event core.AgentEvent, label string) {
	t.Helper()
	if event.SourceOffset != "" && len(event.SourceOffset) <= 4096 && event.Kind != "" && len(event.Kind) <= 128 && len(event.Payload) <= 64<<10 && json.Valid(event.Payload) {
		t.Fatalf("%s is a valid output event", label)
	}
	// The cursor and parser stay inside the batch limit, so this call does not trip that check.
	cursor := "short"
	parser := []byte(`{}`)
	if len(cursor) > 4096 || len(parser) > 64<<10 {
		t.Fatal("rejection cursor or parser exceeds the output batch limit")
	}
	err := db.AppendOutput(t.Context(), ids.runID, epoch, cursor, parser, []core.AgentEvent{event})
	got := readOutputBatch(t, db, ids)
	if err == nil || !sameOutputBatch(got, before) {
		t.Fatalf("AppendOutput accepted or stored an invalid event (%s): err=%v got %s, want the stored cursor and events unchanged", label, err, describeOutputBatch(got))
	}
	if !strings.Contains(err.Error(), "invalid event") || got.task != core.TaskRunning || got.run != core.RunRunning || got.task == core.TaskSucceeded || !sameOutputBatch(got, before) {
		t.Fatalf("%s: AppendOutput returned %v; got %s, want invalid event with %s", label, err, describeOutputBatch(got), describeOutputBatch(before))
	}
}
