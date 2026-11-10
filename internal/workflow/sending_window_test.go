package workflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
	"github.com/makez-dev/makez/internal/store"
)

// errStoppedBeforeStart is a definite failure before Provider.Start. The process does not exist.
var errStoppedBeforeStart = errors.New("stopped before the process started")
var errReleaseBlocked = errors.New("stopped before the release file")

// TestSendingRowPrecedesTheRunnerFlush is the crash after the process exists.
// The disposition file is missing, so an accept is unknown. The other task does not send.
// This does not start Pi and does not prove checkpoint recovery.
func TestSendingRowPrecedesTheRunnerFlush(t *testing.T) {
	url := os.Getenv("MAKEZ_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MAKEZ_TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fx := seedPiLaunch(t, db, url)
	adapter, err := agent.NewPi(agent.PiConfig{Binary: "/usr/local/bin/pi"})
	if err != nil {
		t.Fatal(err)
	}
	gate := &sendingGate{t: t, db: db, fx: fx}
	activities := &Activities{DB: db, Provider: gate, Agents: map[string]core.AgentAdapter{piharness.AdapterID + "@" + piharness.AdapterVersion: adapter}}
	if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
		t.Fatal(err)
	}
	if !gate.started {
		t.Fatal("process was not started")
	}
	if !gate.missingDisposition {
		t.Fatal("launch did not observe a missing disposition file")
	}
	session, err := db.EnsureNativeSession(ctx, fx.workspace, fx.identity())
	if err != nil {
		t.Fatal(err)
	}
	requireSending(t, db, fx.workspace, session.ID, fx.prompt)
	expireWriter(t, url, session.ID)
	if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
		t.Fatal(err)
	}
	if len(gate.modes) != 2 || gate.modes[0] != "send" || gate.modes[1] != "reconcile_only" {
		t.Fatalf("instruction modes = %v", gate.modes)
	}
	second, err := db.Get(ctx, fx.workspace, fx.taskB)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := activities.openPiAttempt(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Mode != "reconcile_only" {
		t.Fatalf("mode = %s", opened.Mode)
	}
	if piharness.Decide(piharness.DispositionSending, true, piharness.HistoryUnread).SendPrompt {
		t.Fatal("a sending prompt was sendable")
	}
	for _, next := range []string{"settled", "rejected", "reconciled", "none", "sending"} {
		err = db.SaveAttemptProgress(ctx, fx.workspace, fx.runB, session.ID, opened.Attempt.Generation, next, piharness.PromptHash(fx.prompt), "")
		if err != nil {
			t.Fatalf("save %s: %v", next, err)
		}
		requireSending(t, db, fx.workspace, session.ID, fx.prompt)
	}
}

// TestPromptIsSentOnceUntilTheProcessStarts distinguishes the two crash points.
// Before the process exists, the lease holder or the next writer sends the prompt once.
// After the process exists, a missing disposition is an unknown accept and is not sent again.
// This does not start Pi and does not prove checkpoint recovery.
func TestPromptIsSentOnceUntilTheProcessStarts(t *testing.T) {
	url := os.Getenv("MAKEZ_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MAKEZ_TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Run("LeaseHolderBeforeStart", func(t *testing.T) {
		fx := seedPiLaunch(t, db, url)
		gate := &sendingGate{t: t, db: db, fx: fx, blockWrites: 1}
		activities := newPiActivities(t, db, gate)
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); !errors.Is(err, errStoppedBeforeStart) {
			t.Fatalf("first launch = %v", err)
		}
		if gate.started {
			t.Fatal("process started before the instruction was written")
		}
		prompt := requirePrompt(t, db, fx)
		if prompt.Disposition != string(piharness.DispositionSending) || prompt.Started {
			t.Fatalf("prompt = %+v", prompt)
		}
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		if countSends(gate.modes) != 1 {
			t.Fatalf("modes = %v", gate.modes)
		}
		prompt = requirePrompt(t, db, fx)
		if !prompt.Started {
			t.Fatal("process start was not recorded")
		}
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		if countSends(gate.modes) != 1 {
			t.Fatalf("prompt was sent again: %v", gate.modes)
		}
	})
	t.Run("NextWriterBeforeStart", func(t *testing.T) {
		fx := seedPiLaunch(t, db, url)
		gate := &sendingGate{t: t, db: db, fx: fx, blockWrites: 1}
		activities := newPiActivities(t, db, gate)
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); !errors.Is(err, errStoppedBeforeStart) {
			t.Fatalf("first launch = %v", err)
		}
		prompt := requirePrompt(t, db, fx)
		if prompt.Disposition != string(piharness.DispositionSending) || prompt.Started {
			t.Fatalf("prompt = %+v", prompt)
		}
		session, err := db.EnsureNativeSession(ctx, fx.workspace, fx.identity())
		if err != nil {
			t.Fatal(err)
		}
		expireWriter(t, url, session.ID)
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		if countSends(gate.modes) != 1 || gate.modes[0] != "send" {
			t.Fatalf("modes = %v", gate.modes)
		}
		prompt = requirePrompt(t, db, fx)
		if !prompt.Started || prompt.RunID != fx.runB {
			t.Fatalf("prompt = %+v", prompt)
		}
		expireWriter(t, url, session.ID)
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		if countSends(gate.modes) != 1 {
			t.Fatalf("prompt was sent again: %v", gate.modes)
		}
	})
	t.Run("AfterStart", func(t *testing.T) {
		fx := seedPiLaunch(t, db, url)
		gate := &sendingGate{t: t, db: db, fx: fx}
		activities := newPiActivities(t, db, gate)
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
			t.Fatal(err)
		}
		if !gate.started || !gate.missingDisposition {
			t.Fatal("started process did not outlive a missing disposition file")
		}
		prompt := requirePrompt(t, db, fx)
		if prompt.Disposition != string(piharness.DispositionSending) || !prompt.Started {
			t.Fatalf("prompt = %+v", prompt)
		}
		session, err := db.EnsureNativeSession(ctx, fx.workspace, fx.identity())
		if err != nil {
			t.Fatal(err)
		}
		expireWriter(t, url, session.ID)
		if err = activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskB}); err != nil {
			t.Fatal(err)
		}
		if countSends(gate.modes) != 1 || len(gate.modes) != 2 || gate.modes[1] != "reconcile_only" {
			t.Fatalf("modes = %v", gate.modes)
		}
		for _, next := range []string{"settled", "rejected", "reconciled", "none", "sending"} {
			err = db.SaveAttemptProgress(ctx, fx.workspace, fx.runB, session.ID, 1, next, piharness.PromptHash(fx.prompt), "")
			if err != nil {
				t.Fatalf("save %s: %v", next, err)
			}
			prompt = requirePrompt(t, db, fx)
			if prompt.Disposition != string(piharness.DispositionSending) || !prompt.Started {
				t.Fatalf("after %s prompt = %+v", next, prompt)
			}
		}
	})
}

func newPiActivities(t *testing.T, db *store.Store, gate *sendingGate) *Activities {
	t.Helper()
	adapter, err := agent.NewPi(agent.PiConfig{Binary: "/usr/local/bin/pi"})
	if err != nil {
		t.Fatal(err)
	}
	return &Activities{DB: db, Provider: gate, Agents: map[string]core.AgentAdapter{piharness.AdapterID + "@" + piharness.AdapterVersion: adapter}}
}

func requirePrompt(t *testing.T, db *store.Store, fx launchFixture) store.NativePrompt {
	t.Helper()
	session, err := db.EnsureNativeSession(t.Context(), fx.workspace, fx.identity())
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.NativePrompt(t.Context(), fx.workspace, session.ID, piharness.PromptHash(fx.prompt))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func countSends(modes []string) int {
	count := 0
	for _, mode := range modes {
		if mode == "send" || mode == "reconcile_then_send" {
			count++
		}
	}
	return count
}

type launchFixture struct {
	workspace    string
	taskA        string
	taskB        string
	runA         string
	runB         string
	bot          string
	conversation string
	prompt       string
}

func (f launchFixture) identity() store.SessionIdentity {
	return store.SessionIdentity{Scope: "personal", BotID: f.bot, ConversationID: f.conversation, HarnessID: piharness.AdapterID, HarnessPin: piharness.Pin}
}

func (f launchFixture) config() string {
	return fmt.Sprintf(`{"execution":{"contract":%q,"scope":"personal","bot_id":%q,"conversation_id":%q,"harness_pin":%q}}`, piharness.Contract, f.bot, f.conversation, piharness.Pin)
}

func seedPiLaunch(t *testing.T, db *store.Store, url string) launchFixture {
	t.Helper()
	ctx := t.Context()
	fx := launchFixture{
		workspace:    "ws-" + randomHex(t, 8),
		taskA:        "task-a-" + randomHex(t, 8),
		taskB:        "task-b-" + randomHex(t, 8),
		runA:         "run-a-" + randomHex(t, 8),
		runB:         "run-b-" + randomHex(t, 8),
		bot:          "bot-" + randomHex(t, 8),
		conversation: "conv-" + randomHex(t, 8),
		prompt:       "flush-window",
	}
	user := testUUID(t)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx) //nolint:errcheck // The inserts are checked before close.
	if _, err = conn.Exec(ctx, `INSERT INTO makez_users (id, display_name) VALUES ($1, 'writer')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO makez_workspaces (id, slug, bootstrap_user_id) VALUES ($1, $2, $3)`, fx.workspace, "lease-"+fx.workspace, user); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `INSERT INTO makez_memberships (workspace_id, user_id, role) VALUES ($1, $2, 'Admin')`, fx.workspace, user); err != nil {
		t.Fatal(err)
	}
	insertPiTask(t, conn, fx.workspace, fx.taskA, fx.runA, user, fx.prompt, fx.config())
	insertPiTask(t, conn, fx.workspace, fx.taskB, fx.runB, user, fx.prompt, fx.config())
	for _, item := range []struct{ run, task string }{{fx.runA, fx.taskA}, {fx.runB, fx.taskB}} {
		sandbox := core.SandboxRef{BackendID: "test", ID: "sb-" + item.run, Key: core.OperationKey{WorkspaceID: fx.workspace, TaskID: item.task, RunID: item.run, Step: "sandbox"}}
		if err = db.UpdateRun(ctx, item.run, store.RunUpdate{Sandbox: &sandbox}); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

func insertPiTask(t *testing.T, conn *pgx.Conn, workspace, taskID, runID, user, prompt, config string) {
	t.Helper()
	ctx := t.Context()
	sum := sha256.Sum256([]byte(prompt + taskID))
	if _, err := conn.Exec(ctx, `
		INSERT INTO makez_tasks (
			id, workspace_id, idempotency_key, request_hash, prompt, config, agent_id, agent_version,
			profile_id, profile_version, state, deadline, submitted_by_user_id
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'pi', '1', 'default', '1', 'RUNNING', $7, $8)`,
		taskID, workspace, "key-"+taskID, hex.EncodeToString(sum[:]), prompt, config, time.Now().Add(time.Hour), user,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO makez_runs (id, task_id, state, backend_id, workspace_id)
		VALUES ($1, $2, 'STARTING', 'test', $3)`, runID, taskID, workspace); err != nil {
		t.Fatal(err)
	}
}

func requireSending(t *testing.T, db *store.Store, workspace, sessionID, prompt string) {
	t.Helper()
	got, err := db.PromptDisposition(t.Context(), workspace, sessionID, piharness.PromptHash(prompt))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(piharness.DispositionSending) {
		t.Fatalf("disposition = %q", got)
	}
}

func expireWriter(t *testing.T, url, sessionID string) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx) //nolint:errcheck // The update is checked before close.
	tag, err := conn.Exec(ctx, `UPDATE makez_native_sessions SET writer_until = now() - interval '1 second' WHERE id = $1`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expired %d rows", tag.RowsAffected())
	}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

func testUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal(err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

type sendingGate struct {
	t                  *testing.T
	db                 *store.Store
	fx                 launchFixture
	started            bool
	missingDisposition bool
	modes              []string
	blockWrites        int
	releaseBlocks      int
	remember           bool
	unreadable         string
	files              map[string][]byte
	releases           []string
	lostCode           int
	polls              int
}

func (g *sendingGate) BackendID() string { return "test" }
func (g *sendingGate) Capabilities(context.Context, string, string) (core.SandboxCapabilities, error) {
	return core.SandboxCapabilities{}, nil
}
func (g *sendingGate) Ensure(context.Context, core.SandboxRequest) (core.SandboxRef, error) {
	return core.SandboxRef{}, nil
}
func (g *sendingGate) Inspect(context.Context, core.OperationKey) (core.SandboxObservation, error) {
	return core.SandboxObservation{}, nil
}
func (g *sendingGate) Renew(context.Context, core.SandboxRef, time.Time) error { return nil }
func (g *sendingGate) Delete(context.Context, core.OperationKey) error         { return nil }
func (g *sendingGate) Start(ctx context.Context, sandbox core.SandboxRef, request core.StartRequest) (core.ProcessRef, error) {
	g.t.Helper()
	session, err := g.db.EnsureNativeSession(ctx, g.fx.workspace, g.fx.identity())
	if err != nil {
		g.t.Fatalf("session: %v", err)
	}
	got, err := g.db.PromptDisposition(ctx, g.fx.workspace, session.ID, piharness.PromptHash(g.fx.prompt))
	if err != nil {
		g.t.Fatalf("disposition: %v", err)
	}
	if got != string(piharness.DispositionSending) {
		g.t.Fatalf("process started while the command row was %q", got)
	}
	g.started = true
	return core.ProcessRef{Sandbox: sandbox, ID: "proc-" + request.Key.Step, Key: request.Key}, nil
}
func (g *sendingGate) Process(context.Context, core.SandboxRef, core.OperationKey) (core.ProcessObservation, error) {
	if g.lostCode == 0 {
		return core.ProcessObservation{}, nil
	}
	g.polls++
	code := 0
	if g.polls == 1 {
		code = g.lostCode
	}
	return core.ProcessObservation{State: core.ProcessExited, Exit: &core.ProcessExit{Code: code}}, nil
}
func (g *sendingGate) Output(context.Context, core.ProcessRef, string, int) (core.OutputPage, error) {
	return core.OutputPage{}, nil
}
func (g *sendingGate) Stop(context.Context, core.SandboxRef, core.OperationKey, core.StopPolicy) error {
	return nil
}
func (g *sendingGate) ReadFile(_ context.Context, sandbox core.SandboxRef, path string) (io.ReadCloser, error) {
	if g.unreadable != "" && sandbox.ID == g.unreadable {
		return nil, errors.New("sandbox unreadable")
	}
	if g.remember {
		data, ok := g.files[sandbox.ID+"\x00"+path]
		if !ok {
			if path == piharness.DispositionPath {
				g.missingDisposition = true
			}
			return nil, fs.ErrNotExist
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	if path == piharness.DispositionPath {
		g.missingDisposition = true
	}
	return nil, fs.ErrNotExist
}
func (g *sendingGate) WriteFile(_ context.Context, sandbox core.SandboxRef, path string, contents io.Reader, _ int64) error {
	data, err := io.ReadAll(contents)
	if err != nil {
		return err
	}
	if path == piharness.InstructionPath && g.blockWrites > 0 {
		g.blockWrites--
		return errStoppedBeforeStart
	}
	if path == piharness.ReleasePath && g.releaseBlocks > 0 {
		g.releaseBlocks--
		return errReleaseBlocked
	}
	if g.remember {
		if g.files == nil {
			g.files = map[string][]byte{}
		}
		g.files[sandbox.ID+"\x00"+path] = append([]byte(nil), data...)
		if path == piharness.ReleasePath {
			g.releases = append(g.releases, sandbox.ID)
		}
	}
	if path != piharness.InstructionPath {
		return nil
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err = json.Unmarshal(data, &body); err != nil {
		return err
	}
	g.modes = append(g.modes, body.Mode)
	return nil
}
