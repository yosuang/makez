package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestAppendOutputRejectsAnOversizedBatch uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// AppendOutput records decoded output. It is not exactly-once.
// A stale epoch and a terminal run stay covered by the existing tests.
// Bot, Conversation, Turn, message sequence, and personal memory stay with Philo.
func TestAppendOutputRejectsAnOversizedBatch(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "batch-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "output-batch")
	epoch, err := db.ClaimObserver(ctx, runID)
	if err != nil || epoch < 1 {
		t.Fatalf("ClaimObserver epoch = %d err=%v", epoch, err)
	}
	ids := outputBatchIDs{workspace: workspace, taskID: taskID, runID: runID}

	opened := readOutputBatch(t, db, ids)
	if opened.cursor != "" || len(opened.parser) != 0 || len(opened.events) != 0 || opened.epoch != epoch || opened.run != core.RunRunning || opened.task != core.TaskRunning || opened.reason != "" || opened.task == core.TaskSucceeded {
		t.Fatalf("opened %s, want a live RUNNING run at epoch %d with an empty cursor", describeOutputBatch(opened), epoch)
	}

	// The cursor pad is a run of the letter c. The parser pad is a run of 0x01. Neither is a credential.
	// A parser of exactly 64<<10 bytes, a cursor of exactly 4096 bytes, and one valid event are stored.
	// The task stays RUNNING. Outbox rows that already exist stay unchanged.
	parser := bytes.Repeat([]byte{0x01}, 64<<10)
	cursor := strings.Repeat("c", 4096)
	kept := core.AgentEvent{SourceOffset: "stdout:16", Kind: "log", Payload: []byte(`{"text":"kept"}`)}
	requireValidOutputEvent(t, kept)
	if len(parser) != 64<<10 || len(cursor) != 4096 || strings.Trim(cursor, "c") != "" {
		t.Fatalf("boundary parser %d cursor %d", len(parser), len(cursor))
	}
	if err = db.AppendOutput(ctx, runID, epoch, cursor, parser, []core.AgentEvent{kept}); err != nil {
		t.Fatal(err)
	}
	stored := readOutputBatch(t, db, ids)
	if stored.cursor != cursor || !bytes.Equal(stored.parser, parser) || !oneStoredEvent(stored.events, kept) || stored.epoch != epoch || stored.version != opened.version+1 || stored.run != core.RunRunning || stored.task != core.TaskRunning || stored.reason != opened.reason || stored.outbox != opened.outbox || stored.outboxN != opened.outboxN || stored.task == core.TaskSucceeded {
		t.Fatalf("accepted batch wrote %s, want cursor %d parser %d one event %s task RUNNING outbox %d version %d", describeOutputBatch(stored), len(cursor), len(parser), kept.SourceOffset, opened.outboxN, opened.version+1)
	}

	short := "short"
	small := []byte(`{}`)
	if len(short) > 4096 || len(small) > 64<<10 {
		t.Fatal("short cursor or small parser is over the limit")
	}

	// A parser of 64<<10+1 bytes is rejected. The stored cursor and events stay.
	overParser := bytes.Repeat([]byte{0x02}, 64<<10+1)
	parserEvent := core.AgentEvent{SourceOffset: "stdout:parser", Kind: "log", Payload: []byte(`{"text":"parser"}`)}
	if len(overParser) != 64<<10+1 {
		t.Fatalf("oversized parser is %d bytes", len(overParser))
	}
	requireOversizedBatchRejected(t, db, ids, epoch, short, overParser, []core.AgentEvent{parserEvent}, stored, "65537-byte parser")

	// A cursor of 4097 bytes is rejected. The stored cursor and events stay.
	longCursor := strings.Repeat("d", 4097)
	cursorEvent := core.AgentEvent{SourceOffset: "stdout:cursor", Kind: "log", Payload: []byte(`{"text":"cursor"}`)}
	if len(longCursor) != 4097 || len(small) > 64<<10 {
		t.Fatalf("oversized cursor is %d bytes", len(longCursor))
	}
	requireOversizedBatchRejected(t, db, ids, epoch, longCursor, small, []core.AgentEvent{cursorEvent}, stored, "4097-byte cursor")

	// A batch of 1025 events is rejected. The stored cursor and events stay.
	batch := make([]core.AgentEvent, 1025)
	for i := range batch {
		batch[i] = core.AgentEvent{SourceOffset: fmt.Sprintf("batch:%04d", i), Kind: "log", Payload: []byte(`{"text":"batch"}`)}
	}
	if len(batch) != 1025 {
		t.Fatalf("batch has %d events", len(batch))
	}
	requireOversizedBatchRejected(t, db, ids, epoch, short, small, batch, stored, "1025 events")
}

func requireOversizedBatchRejected(t *testing.T, db *Store, ids outputBatchIDs, epoch int64, cursor string, parser []byte, events []core.AgentEvent, before outputBatchRow, label string) {
	t.Helper()
	if len(parser) <= 64<<10 && len(cursor) <= 4096 && len(events) <= 1024 {
		t.Fatalf("%s is within the output batch limit", label)
	}
	for _, event := range events {
		requireValidOutputEvent(t, event)
	}
	err := db.AppendOutput(t.Context(), ids.runID, epoch, cursor, parser, events)
	got := readOutputBatch(t, db, ids)
	if err == nil || !sameOutputBatch(got, before) {
		t.Fatalf("AppendOutput accepted or stored an oversized batch (%s): err=%v got %s, want the stored cursor and events unchanged", label, err, describeOutputBatch(got))
	}
	if !strings.Contains(err.Error(), "output batch exceeds limit") || got.task != core.TaskRunning || got.run != core.RunRunning || got.task == core.TaskSucceeded || !sameOutputBatch(got, before) {
		t.Fatalf("%s: AppendOutput returned %v; got %s, want output batch exceeds limit with %s", label, err, describeOutputBatch(got), describeOutputBatch(before))
	}
}

func requireValidOutputEvent(t *testing.T, event core.AgentEvent) {
	t.Helper()
	if event.SourceOffset == "" || len(event.SourceOffset) > 4096 || event.Kind == "" || len(event.Kind) > 128 || len(event.Payload) > 64<<10 || !json.Valid(event.Payload) {
		t.Fatalf("event is invalid: offset %d kind %d payload %d", len(event.SourceOffset), len(event.Kind), len(event.Payload))
	}
}

func oneStoredEvent(events []batchEvent, want core.AgentEvent) bool {
	if len(events) != 1 || events[0].offset != want.SourceOffset || events[0].index != int64(want.Index) || events[0].kind != want.Kind {
		return false
	}
	return sameJSON(events[0].payload, want.Payload)
}

type outputBatchIDs struct {
	workspace string
	taskID    string
	runID     string
}

type batchEvent struct {
	offset  string
	index   int64
	kind    string
	payload []byte
}

type outputBatchRow struct {
	cursor  string
	parser  []byte
	epoch   int64
	version int64
	run     core.RunState
	task    core.TaskState
	reason  string
	events  []batchEvent
	outboxN int
	outbox  string
}

func readOutputBatch(t *testing.T, db *Store, ids outputBatchIDs) outputBatchRow {
	t.Helper()
	var row outputBatchRow
	var run, task string
	err := db.pool.QueryRow(t.Context(), `
		SELECT r.cursor, r.parser_state, r.observer_epoch, r.version, r.state, tk.state, tk.reason
		FROM makez_runs r
		JOIN makez_tasks tk ON tk.id=r.task_id
		WHERE r.id=$1 AND r.workspace_id=$2 AND tk.id=$3`,
		ids.runID, ids.workspace, ids.taskID).Scan(&row.cursor, &row.parser, &row.epoch, &row.version, &run, &task, &row.reason)
	if err != nil {
		t.Fatal(err)
	}
	row.run = core.RunState(run)
	row.task = core.TaskState(task)
	rows, err := db.pool.Query(t.Context(), `
		SELECT source_offset, item_index, type, payload
		FROM makez_events WHERE run_id=$1 ORDER BY seq`, ids.runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var event batchEvent
		if err = rows.Scan(&event.offset, &event.index, &event.kind, &event.payload); err != nil {
			t.Fatal(err)
		}
		row.events = append(row.events, event)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	err = db.pool.QueryRow(t.Context(), `
		SELECT count(*),
			COALESCE(string_agg(
				id || '|' || workspace_id || '|' || task_id || '|' || run_id || '|' || command || '|' ||
				attempts::text || '|' || last_error || '|' || lease_token::text || '|' || COALESCE(lease_owner, '') || '|' ||
				COALESCE(delivered_at::text, '') || '|' || COALESCE(dead_at::text, '') || '|' || COALESCE(lease_until::text, '') || '|' ||
				next_attempt_at::text || '|' || created_at::text,
				E'\n' ORDER BY id), '')
		FROM makez_outbox
		WHERE workspace_id=$1 OR task_id=$2 OR run_id=$3`,
		ids.workspace, ids.taskID, ids.runID).Scan(&row.outboxN, &row.outbox)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func sameOutputBatch(got, want outputBatchRow) bool {
	return got.cursor == want.cursor && bytes.Equal(got.parser, want.parser) && sameBatchEvents(got.events, want.events) && got.epoch == want.epoch && got.version == want.version && got.run == want.run && got.task == want.task && got.reason == want.reason && got.outboxN == want.outboxN && got.outbox == want.outbox
}

func sameBatchEvents(got, want []batchEvent) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].offset != want[i].offset || got[i].index != want[i].index || got[i].kind != want[i].kind || !sameJSON(got[i].payload, want[i].payload) {
			return false
		}
	}
	return true
}

func sameJSON(left, right []byte) bool {
	var a, b bytes.Buffer
	if json.Compact(&a, left) != nil || json.Compact(&b, right) != nil {
		return false
	}
	return bytes.Equal(a.Bytes(), b.Bytes())
}

func describeOutputBatch(row outputBatchRow) string {
	mark := ""
	if len(row.parser) > 0 {
		mark = fmt.Sprintf("%02x", row.parser[0])
	}
	offsets := make([]string, 0, len(row.events))
	for _, event := range row.events {
		offsets = append(offsets, event.offset)
	}
	shown := strings.Join(offsets, ",")
	if len(shown) > 80 {
		shown = shown[:80] + "..."
	}
	return fmt.Sprintf("cursor %d parser %d mark %s events %d [%s] run %s task %s reason %q epoch %d version %d outbox %d", len(row.cursor), len(row.parser), mark, len(row.events), shown, row.run, row.task, row.reason, row.epoch, row.version, row.outboxN)
}
