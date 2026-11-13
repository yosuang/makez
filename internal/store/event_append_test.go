package store

import (
	"errors"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

// TestSameRunEpochAndCursorDoesNotAppendTwice uses Postgres. It does not start a sandbox, call a model, or accept a Pi session.
func TestSameRunEpochAndCursorDoesNotAppendTwice(t *testing.T) {
	db := leaseDB(t)
	ctx := t.Context()
	workspace, taskID, runID := newID(), newID(), newID()
	if _, err := db.pool.Exec(ctx, `INSERT INTO makez_workspaces (id, slug) VALUES ($1, $2)`, workspace, "event-"+workspace); err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, workspace, taskID, runID, "event-append")

	epoch, err := db.ClaimObserver(ctx, runID)
	if err != nil || epoch != 1 {
		t.Fatalf("epoch = %d err=%v", epoch, err)
	}
	const cursor = "stdout:16"
	parser := []byte(`{}`)
	first := core.AgentEvent{SourceOffset: cursor, Kind: "log", Payload: []byte(`{"text":"first"}`)}
	if err = db.AppendOutput(ctx, runID, epoch, cursor, parser, []core.AgentEvent{first}); err != nil {
		t.Fatal(err)
	}
	rows := storedEvents(t, db, runID)
	if len(rows) != 1 || rows[0].offset != cursor || rows[0].index != 0 || rows[0].generation != 1 || rows[0].text != "first" {
		t.Fatalf("first append = %+v", rows)
	}
	seq := rows[0].seq
	if got, gotEpoch := outputCursor(t, db, runID); got != cursor || gotEpoch != epoch {
		t.Fatalf("cursor after first append = %q epoch %d", got, gotEpoch)
	}
	if published := publishedEvents(t, db, workspace, taskID); len(published) != 1 || published[0].Seq != seq {
		t.Fatalf("published after first append = %+v", published)
	}

	replay := first
	replay.Payload = []byte(`{"text":"replay"}`)
	if err = db.AppendOutput(ctx, runID, epoch, cursor, parser, []core.AgentEvent{replay}); err != nil {
		t.Fatal(err)
	}
	rows = storedEvents(t, db, runID)
	if len(rows) != 1 || rows[0].seq != seq || rows[0].offset != cursor || rows[0].index != 0 || rows[0].generation != 1 || rows[0].text != "first" {
		t.Fatalf("replay = %+v", rows)
	}
	if got, gotEpoch := outputCursor(t, db, runID); got != cursor || gotEpoch != epoch {
		t.Fatalf("cursor after replay = %q epoch %d", got, gotEpoch)
	}
	if published := publishedEvents(t, db, workspace, taskID); len(published) != 1 || published[0].Seq != seq {
		t.Fatalf("published after replay = %+v", published)
	}

	const next = "stdout:32"
	if err = db.AppendOutput(ctx, runID, epoch, next, parser, []core.AgentEvent{{
		SourceOffset: next, Kind: "log", Payload: []byte(`{"text":"next"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	rows = storedEvents(t, db, runID)
	if len(rows) != 2 || rows[0].seq != seq || rows[0].text != "first" || rows[1].offset != next || rows[1].generation != 1 || rows[1].text != "next" || rows[1].seq == seq {
		t.Fatalf("next cursor = %+v", rows)
	}
	nextSeq := rows[1].seq

	stale := core.AgentEvent{SourceOffset: "stdout:48", Kind: "log", Payload: []byte(`{"text":"stale"}`)}
	if err = db.AppendOutput(ctx, runID, epoch-1, stale.SourceOffset, parser, []core.AgentEvent{stale}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale epoch = %v", err)
	}
	rows = storedEvents(t, db, runID)
	if len(rows) != 2 || rows[0].seq != seq || rows[0].text != "first" || rows[1].seq != nextSeq || rows[1].text != "next" {
		t.Fatalf("after stale epoch = %+v", rows)
	}
	if got, gotEpoch := outputCursor(t, db, runID); got != next || gotEpoch != epoch {
		t.Fatalf("cursor after stale epoch = %q epoch %d", got, gotEpoch)
	}
	published := publishedEvents(t, db, workspace, taskID)
	if len(published) != 2 || published[0].Seq != seq || published[1].Seq != nextSeq {
		t.Fatalf("published after stale epoch = %+v", published)
	}
}

type storedEvent struct {
	seq        int64
	offset     string
	index      int64
	generation int64
	text       string
}

func storedEvents(t *testing.T, db *Store, runID string) []storedEvent {
	t.Helper()
	rows, err := db.pool.Query(t.Context(), `
		SELECT seq, source_offset, item_index, attempt_generation, payload->>'text'
		FROM makez_events WHERE run_id=$1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []storedEvent
	for rows.Next() {
		var row storedEvent
		if err = rows.Scan(&row.seq, &row.offset, &row.index, &row.generation, &row.text); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func outputCursor(t *testing.T, db *Store, runID string) (string, int64) {
	t.Helper()
	var cursor string
	var epoch int64
	if err := db.pool.QueryRow(t.Context(), `SELECT cursor, observer_epoch FROM makez_runs WHERE id=$1`, runID).Scan(&cursor, &epoch); err != nil {
		t.Fatal(err)
	}
	return cursor, epoch
}

func publishedEvents(t *testing.T, db *Store, workspace, taskID string) []Event {
	t.Helper()
	events, err := db.Events(t.Context(), workspace, taskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}
