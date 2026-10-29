package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/core"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	databaseURL := os.Getenv("MAKEZ_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MAKEZ_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "makez_store_test_" + newID()
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
		t.Fatal(err)
	}
	// Isolate Outbox rows from a concurrently running development dispatcher.
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("remove isolated test schema: %v", err)
		}
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("PostgreSQL integration fixture ready")
	workspace := "test-" + newID()
	if err = s.RegisterMachines(ctx, map[string]string{"test-key-" + workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	return s, workspace
}
func input() SubmitInput {
	return SubmitInput{Prompt: "test task", AgentID: "demo", AgentVersion: "1", ProfileID: "local", ProfileVersion: "1", TimeoutSeconds: 3600}
}
func submit(t *testing.T, s *Store, w string) Task {
	t.Helper()
	task, created, err := s.Submit(testPrincipal(t.Context(), w), w, newID(), input())
	if err != nil || !created {
		t.Fatalf("submit created=%v err=%v", created, err)
	}
	return task
}
func prepareRun(t *testing.T, s *Store, w string) Task {
	t.Helper()
	task := submit(t, s, w)
	task, err := s.InitializeRun(context.Background(), w, task.ID, "test-backend")
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func finalizing(t *testing.T, s *Store, run string) {
	t.Helper()
	state := core.RunStopping
	if err := s.UpdateRun(context.Background(), run, RunUpdate{State: &state}); err != nil {
		t.Fatal(err)
	}
	state = core.RunFinalizing
	if err := s.UpdateRun(context.Background(), run, RunUpdate{State: &state}); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitIdempotencyAndOutbox(t *testing.T) {
	s, w := testStore(t)
	ctx := context.Background()
	key := newID()
	var count atomic.Int64
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for range 16 {
		wg.Go(func() {
			task, created, err := s.Submit(testPrincipal(ctx, w), w, key, input())
			if err != nil {
				t.Error(err)
				return
			}
			if created {
				count.Add(1)
			}
			ids <- task.ID
		})
	}
	wg.Wait()
	close(ids)
	if count.Load() != 1 {
		t.Fatalf("created %d tasks", count.Load())
	}
	var id string
	for candidate := range ids {
		if id != "" && candidate != id {
			t.Fatal("different IDs for same request")
		}
		id = candidate
	}
	changed := input()
	changed.Prompt = "different"
	if _, _, err := s.Submit(testPrincipal(ctx, w), w, key, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("different request: %v", err)
	}
	pending, err := s.Pending(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	matching := 0
	for _, o := range pending {
		if o.WorkspaceID == w {
			matching++
			if o.TaskID != id || o.Command != "START_TASK" {
				t.Fatalf("bad outbox: %+v", o)
			}
			if err = s.RetryOutbox(ctx, o, "temporal offline"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if matching != 1 {
		t.Fatalf("outbox commands=%d", matching)
	}
	task, err := s.Get(ctx, w, id)
	if err != nil || task.DispatchError != "temporal offline" {
		t.Fatalf("dispatch error: %+v %v", task, err)
	}
	canonical := input()
	canonical.Config = json.RawMessage(`{"b":2,"a":1}`)
	task, _, err = s.Submit(testPrincipal(ctx, w), w, "canonical", canonical)
	if err != nil {
		t.Fatal(err)
	}
	canonical.Config = json.RawMessage(`{ "a":1, "b":2 }`)
	retried, created, err := s.Submit(testPrincipal(ctx, w), w, "canonical", canonical)
	if err != nil || created || task.ID != retried.ID || !task.Deadline.Equal(retried.Deadline) {
		t.Fatalf("canonical retry: %+v %v", retried, err)
	}
}

func TestCancellationAndWorkspaceIsolation(t *testing.T) {
	s, w := testStore(t)
	ctx := context.Background()
	task := submit(t, s, w)
	if _, err := s.Get(ctx, "another-workspace", task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign task leaked: %v", err)
	}
	if _, err := s.Cancel(testPrincipal(ctx, w), "another-workspace", task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign cancel: %v", err)
	}
	if _, err := s.Events(ctx, "another-workspace", task.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign events: %v", err)
	}
	if _, err := s.Artifacts(ctx, "another-workspace", task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign artifacts: %v", err)
	}
	if _, err := s.Cancel(testPrincipal(ctx, w), w, task.ID); err != nil {
		t.Fatal(err)
	}
	task, err := s.InitializeRun(ctx, w, task.ID, "test-backend")
	if err != nil || task.State != core.TaskCanceled || task.Run != nil {
		t.Fatalf("queued cancellation must prevent run: %+v %v", task, err)
	}
	for _, cancelFirst := range []bool{true, false} {
		task = prepareRun(t, s, w)
		finalizing(t, s, task.Run.ID)
		if cancelFirst {
			if _, err = s.Cancel(testPrincipal(ctx, w), w, task.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err = s.Publish(ctx, w, task.ID, core.TaskSucceeded, "", true, core.CleanupPending, nil); err != nil {
			t.Fatal(err)
		}
		if !cancelFirst {
			if _, err = s.Cancel(testPrincipal(ctx, w), w, task.ID); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Get(ctx, w, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := core.TaskSucceeded
		if cancelFirst {
			want = core.TaskCanceled
		}
		if got.State != want || core.TaskState(got.Run.State) != want {
			t.Fatalf("cancelFirst=%v got=%s", cancelFirst, got.State)
		}
		state := core.RunRunning
		if err = s.UpdateRun(ctx, task.Run.ID, RunUpdate{State: &state}); !errors.Is(err, ErrConflict) {
			t.Fatalf("terminal changed: %v", err)
		}
	}
}

func TestCancelPublishRace(t *testing.T) {
	s, w := testStore(t)
	ctx := context.Background()
	for range 12 {
		task := prepareRun(t, s, w)
		finalizing(t, s, task.Run.ID)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var canceled Task
		wg.Go(func() {
			<-start
			var err error
			canceled, err = s.Cancel(testPrincipal(ctx, w), w, task.ID)
			if err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			<-start
			if err := s.Publish(ctx, w, task.ID, core.TaskSucceeded, "", true, core.CleanupDone, nil); err != nil {
				t.Error(err)
			}
		})
		close(start)
		wg.Wait()
		got, err := s.Get(ctx, w, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if canceled.State == core.TaskCanceling && got.State != core.TaskCanceled {
			t.Fatalf("accepted cancellation lost: %+v", got)
		}
		if canceled.State == core.TaskSucceeded && got.State != core.TaskSucceeded {
			t.Fatalf("published outcome changed: %+v", got)
		}
	}
}

func TestObserverFencingAndReplay(t *testing.T) {
	s, w := testStore(t)
	ctx := context.Background()
	task := prepareRun(t, s, w)
	epoch, err := s.ClaimObserver(ctx, task.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	events := []core.AgentEvent{{SourceOffset: "stdout:0", Kind: "log", Payload: json.RawMessage(`{"text":"hello"}`)}}
	for range 2 {
		if err = s.AppendOutput(ctx, task.Run.ID, epoch, "10", []byte("checkpoint"), events); err != nil {
			t.Fatal(err)
		}
	}
	next, err := s.ClaimObserver(ctx, task.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AppendOutput(ctx, task.Run.ID, epoch, "0", nil, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("old epoch accepted: %v", err)
	}
	if err = s.AppendOutput(ctx, task.Run.ID, next, "20", []byte("new checkpoint"), nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Events(ctx, w, task.ID, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("events=%+v err=%v", got, err)
	}
	task, err = s.Get(ctx, w, task.ID)
	if err != nil || task.Run.Cursor != "20" || string(task.Run.ParserState) != "new checkpoint" {
		t.Fatalf("checkpoint=%+v %v", task.Run, err)
	}
}

func TestOperationClaimAndArchiveCheckpoint(t *testing.T) {
	s, w := testStore(t)
	ctx := context.Background()
	task := prepareRun(t, s, w)
	phase, claimed, err := s.BeginOperation(ctx, task.Run.ID, "sandbox", "hash")
	if err != nil || !claimed || phase != "DISPATCHING" {
		t.Fatalf("claim: %s %v %v", phase, claimed, err)
	}
	phase, claimed, err = s.BeginOperation(ctx, task.Run.ID, "sandbox", "hash")
	if err != nil || claimed || phase != "DISPATCHING" {
		t.Fatalf("retry repeated mutation: %s %v %v", phase, claimed, err)
	}
	if _, _, err = s.BeginOperation(ctx, task.Run.ID, "sandbox", "different"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed operation: %v", err)
	}
	if err = s.ConfirmOperation(ctx, task.Run.ID, "sandbox"); err != nil {
		t.Fatal(err)
	}
	finalizing(t, s, task.Run.ID)
	artifact := Artifact{Path: "result.txt", StorageKey: "private-storage-key", SHA256: strings.Repeat("a", 64), MediaType: "text/plain", Size: 4}
	result := &PreparedResult{State: core.TaskSucceeded, Artifacts: []Artifact{artifact}}
	if err = s.UpdateRun(ctx, task.Run.ID, RunUpdate{Result: result}); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Get(ctx, w, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Run.Result == nil || recovered.Run.Result.Artifacts[0].StorageKey != artifact.StorageKey {
		t.Fatalf("archive reference not recovered: %+v", recovered.Run.Result)
	}
	if err = s.Publish(ctx, w, task.ID, recovered.Run.Result.State, "", true, core.CleanupPending, recovered.Run.Result.Artifacts); err != nil {
		t.Fatal(err)
	}
	published, err := s.Artifacts(ctx, w, task.ID)
	if err != nil || len(published) != 1 || published[0].ID == "" {
		t.Fatalf("published: %+v %v", published, err)
	}
	if _, err = s.Artifact(ctx, "another-workspace", published[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign artifact leaked: %v", err)
	}
	if err = s.Publish(ctx, w, task.ID, core.TaskFailed, "late", false, core.CleanupPending, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, w, task.ID)
	if err != nil || got.State != core.TaskSucceeded {
		t.Fatalf("immutable outcome: %+v %v", got, err)
	}
	if err = s.SetCleanup(ctx, task.Run.ID, core.CleanupDone); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCleanup(ctx, task.Run.ID, core.CleanupPending); !errors.Is(err, ErrConflict) {
		t.Fatalf("cleanup regressed: %v", err)
	}
}

func TestDeadlineAndUnconfirmedStop(t *testing.T) {
	s, w := testStore(t)
	ctx := context.Background()
	in := input()
	in.TimeoutSeconds = 0
	in.Deadline = time.Now().Add(time.Minute)
	task, _, err := s.Submit(testPrincipal(ctx, w), w, newID(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE makez_tasks SET deadline=$2 WHERE id=$1", task.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	task, err = s.InitializeRun(ctx, w, task.ID, "test-backend")
	if err != nil || task.State != core.TaskFailed || task.Run != nil || task.Reason != "DEADLINE_EXCEEDED" {
		t.Fatalf("expired task: %+v %v", task, err)
	}
	task = prepareRun(t, s, w)
	finalizing(t, s, task.Run.ID)
	if _, err = s.Cancel(testPrincipal(ctx, w), w, task.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(ctx, w, task.ID, core.TaskCanceled, "", false, core.CleanupPending, nil); err != nil {
		t.Fatal(err)
	}
	task, err = s.Get(ctx, w, task.ID)
	if err != nil || task.State != core.TaskFailed || task.Reason != "STOP_UNCONFIRMED" {
		t.Fatalf("unconfirmed stop: %+v %v", task, err)
	}
}

func TestFinalizationGraceUsesExecutionFinishTime(t *testing.T) {
	s, w := testStore(t)
	ctx := context.Background()
	task := prepareRun(t, s, w)
	deadline := time.Now().Add(-time.Minute)
	if _, err := s.pool.Exec(ctx, "UPDATE makez_tasks SET deadline=$2 WHERE id=$1", task.ID, deadline); err != nil {
		t.Fatal(err)
	}
	finalizing(t, s, task.Run.ID)
	if err := s.UpdateRun(ctx, task.Run.ID, RunUpdate{Exit: &core.ProcessExit{Code: 0, FinishedAt: deadline.Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, w, task.ID, core.TaskSucceeded, "", true, core.CleanupDone, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, w, task.ID)
	if err != nil || got.State != core.TaskSucceeded {
		t.Fatalf("timely exit should survive finalization grace: %+v %v", got, err)
	}
}

func TestExpiredExplicitDeadlineRetry(t *testing.T) {
	s, w := testStore(t)
	ctx := context.Background()
	in := input()
	in.TimeoutSeconds = 0
	in.Deadline = time.Now().Add(time.Minute)
	task, _, err := s.Submit(testPrincipal(ctx, w), w, "deadline-retry", in)
	if err != nil {
		t.Fatal(err)
	}
	// Aging the request in PostgreSQL avoids waiting for a real deadline in the test.
	expired := in
	expired.Deadline = time.Now().Add(-time.Minute)
	expired.Config = json.RawMessage(`{}`)
	expired.Deadline = expired.Deadline.UTC()
	request, _ := json.Marshal(expired)
	digest := sha256.Sum256(request)
	if _, err = s.pool.Exec(ctx, "UPDATE makez_tasks SET request_hash=$2,deadline=$3 WHERE id=$1", task.ID, hex.EncodeToString(digest[:]), expired.Deadline); err != nil {
		t.Fatal(err)
	}
	retried, created, err := s.Submit(testPrincipal(ctx, w), w, "deadline-retry", expired)
	if err != nil || created || retried.ID != task.ID {
		t.Fatalf("expired duplicate rejected: %+v %v", retried, err)
	}
	if _, _, err = s.Submit(testPrincipal(ctx, w), w, "new-expired", expired); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("new expired task accepted: %v", err)
	}
}

func TestFreezePlanChoosesOneImmutablePlanAndHidesItFromTaskJSON(t *testing.T) {
	s, workspace := testStore(t)
	task := prepareRun(t, s, workspace)
	var wg sync.WaitGroup
	plans := make(chan core.AgentPlan, 8)
	for i := range 8 {
		wg.Go(func() {
			candidate := core.AgentPlan{
				Command:    core.CommandSpec{Argv: []string{"/agent", fmt.Sprint(i)}},
				SecretRefs: map[string]string{"API_TOKEN": "auth@" + fmt.Sprint(i)},
				Files:      []core.InputFile{{Path: "/workspace/input.txt", Data: []byte("private-plan-file")}},
			}
			frozen, err := s.FreezePlan(t.Context(), task.Run.ID, candidate)
			if err != nil {
				t.Error(err)
				return
			}
			plans <- frozen
		})
	}
	wg.Wait()
	close(plans)
	var winner []byte
	for plan := range plans {
		encoded, _ := json.Marshal(plan)
		if winner == nil {
			winner = encoded
		}
		if !bytes.Equal(winner, encoded) {
			t.Fatal("concurrent callers received different plans")
		}
	}
	stored, err := s.Get(t.Context(), workspace, task.ID)
	if err != nil || stored.Run.Plan == nil {
		t.Fatalf("plan not persisted: %v", err)
	}
	public, err := json.Marshal(stored)
	if err != nil || bytes.Contains(public, []byte("SecretRefs")) || bytes.Contains(public, []byte("private-plan-file")) || bytes.Contains(public, []byte("agent_plan")) {
		t.Fatal("private execution plan appeared in Task JSON")
	}
	if _, err := s.FreezePlan(t.Context(), task.Run.ID, core.AgentPlan{Command: core.CommandSpec{Argv: []string{"/agent"}, Env: map[string]string{"API_TOKEN": "must-not-be-persisted"}}, SecretRefs: map[string]string{"API_TOKEN": "auth@1"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("accepted colliding plain environment and secret binding: %v", err)
	}
}

func testPrincipal(ctx context.Context, workspace string) context.Context {
	return WithPrincipal(ctx, Principal{Kind: "machine", ID: "machine-" + hex.EncodeToString(tokenDigest("test-key-"+workspace))})
}
