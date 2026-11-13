package store

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestUpdateRunRejectsAnOversizedParserCheckpoint uses Postgres.
// It does not start a sandbox, call a model, or accept a Pi session.
// Rejecting an oversized parser checkpoint is not a claim that an external action ran once,
// and it is not exactly-once for that action.
func TestUpdateRunRejectsAnOversizedParserCheckpoint(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "parser-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "parser-checkpoint")
	ids := parserIDs{workspace: workspace, taskID: taskID, runID: runID}

	opened := readParserRow(t, db, ids)
	if len(opened.parser) != 0 || opened.run != core.RunRunning || opened.task != core.TaskRunning || opened.reason != "" || opened.outbox != 0 || opened.tasks != 1 || opened.runs != 1 || opened.version < 1 {
		t.Fatalf("opened %s", describeParser(opened))
	}

	// A parser state of exactly 65536 bytes is stored.
	// The run stays RUNNING. The task stays RUNNING. No outbox command is inserted.
	accepted := bytes.Repeat([]byte{0x01}, 65536)
	storedWant := opened
	storedWant.parser = accepted
	storedWant.version++
	stored := requireParserWrite(t, db, ids, RunUpdate{ParserState: &accepted}, storedWant, "65536-byte parser")
	if len(stored.parser) != 65536 || !bytes.Equal(stored.parser, accepted) || stored.run != core.RunRunning || stored.task != core.TaskRunning || stored.task == core.TaskSucceeded || stored.outbox != 0 {
		t.Fatalf("65536-byte parser wrote %s", describeParser(stored))
	}

	// A parser state of 65537 bytes is rejected.
	// The error text contains "parser checkpoint exceeds 64 KiB".
	// The stored parser stays the 65536-byte value. The run version does not move.
	// The task does not become SUCCEEDED. No outbox command is inserted.
	oversized := bytes.Repeat([]byte{0x02}, 65537)
	rejected := requireParserRejected(t, db, ids, RunUpdate{ParserState: &oversized}, stored, "65537-byte parser")
	if len(rejected.parser) != 65536 || !bytes.Equal(rejected.parser, accepted) || rejected.version != stored.version || rejected.run != core.RunRunning || rejected.task != core.TaskRunning || rejected.task == core.TaskSucceeded || rejected.outbox != 0 {
		t.Fatalf("65537-byte parser wrote %s", describeParser(rejected))
	}
}

type parserIDs struct {
	workspace string
	taskID    string
	runID     string
}

type parserRow struct {
	version int64
	run     core.RunState
	parser  []byte
	task    core.TaskState
	reason  string
	tasks   int
	runs    int
	outbox  int
}

func requireParserRejected(t *testing.T, db *Store, ids parserIDs, update RunUpdate, before parserRow, label string) parserRow {
	t.Helper()
	err := db.UpdateRun(t.Context(), ids.runID, update)
	got := readParserRow(t, db, ids)
	if err == nil || !strings.Contains(err.Error(), "parser checkpoint exceeds 64 KiB") || !sameParserRow(got, before) || got.task == core.TaskSucceeded || got.outbox != 0 {
		t.Fatalf("%s: UpdateRun returned %v; got %s, want parser checkpoint exceeds 64 KiB with %s", label, err, describeParser(got), describeParser(before))
	}
	return got
}

func requireParserWrite(t *testing.T, db *Store, ids parserIDs, update RunUpdate, want parserRow, label string) parserRow {
	t.Helper()
	err := db.UpdateRun(t.Context(), ids.runID, update)
	got := readParserRow(t, db, ids)
	if err != nil || !sameParserRow(got, want) || got.task != core.TaskRunning || got.task == core.TaskSucceeded || got.outbox != 0 {
		t.Fatalf("%s: UpdateRun returned %v; got %s, want %s", label, err, describeParser(got), describeParser(want))
	}
	return got
}

func readParserRow(t *testing.T, db *Store, ids parserIDs) parserRow {
	t.Helper()
	var row parserRow
	var run, task string
	err := db.pool.QueryRow(t.Context(), `
		SELECT
			r.version,
			r.state,
			r.parser_state,
			tk.state,
			tk.reason,
			(SELECT count(*) FROM makez_tasks WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_runs WHERE workspace_id=$1),
			(SELECT count(*) FROM makez_outbox WHERE workspace_id=$1 OR task_id=$2 OR run_id=$3)
		FROM makez_runs r
		JOIN makez_tasks tk ON tk.id=r.task_id
		WHERE r.workspace_id=$1 AND r.id=$3 AND tk.id=$2`,
		ids.workspace, ids.taskID, ids.runID).Scan(
		&row.version, &run, &row.parser, &task, &row.reason, &row.tasks, &row.runs, &row.outbox)
	if err != nil {
		t.Fatal(err)
	}
	row.run = core.RunState(run)
	row.task = core.TaskState(task)
	return row
}

func sameParserRow(got, want parserRow) bool {
	return got.version == want.version && got.run == want.run && got.task == want.task && got.reason == want.reason && got.tasks == want.tasks && got.runs == want.runs && got.outbox == want.outbox && bytes.Equal(got.parser, want.parser)
}

func describeParser(row parserRow) string {
	mark := ""
	if len(row.parser) > 0 {
		mark = fmt.Sprintf("%02x", row.parser[0])
	}
	return fmt.Sprintf("parser %d bytes mark %s run %s task %s reason %q outbox %d tasks %d runs %d version %d", len(row.parser), mark, row.run, row.task, row.reason, row.outbox, row.tasks, row.runs, row.version)
}
