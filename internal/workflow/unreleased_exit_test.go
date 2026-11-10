package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/piharness"
	"github.com/makez-dev/makez/internal/store"
	"go.temporal.io/sdk/testsuite"
)

// TestUnreleasedRunnerExitRelaunches runs the runner, then the task workflow.
// A child that dies before the release file must not exit 0. That exit closed
// the sending row, and this workflow did not call Launch again. Launch tests
// that call Launch themselves do not cover that loop.
func TestUnreleasedRunnerExitRelaunches(t *testing.T) {
	code := runnerExitBeforeRelease(t)
	if code != 75 {
		t.Fatalf("runner exit = %d", code)
	}
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
	gate := &sendingGate{t: t, db: db, fx: fx, remember: true, lostCode: code}
	replay := &unreleasedReplay{inner: newPiActivities(t, db, gate)}
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(Task)
	env.RegisterActivity(replay)
	env.ExecuteWorkflow(Task, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err = env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if replay.launches != 2 {
		t.Fatalf("launches = %d", replay.launches)
	}
	if len(gate.modes) != 2 || gate.modes[0] != "send" || gate.modes[1] != "send" {
		t.Fatalf("modes = %v", gate.modes)
	}
	if len(gate.releases) != 2 || gate.polls != 2 {
		t.Fatalf("releases = %v polls = %d", gate.releases, gate.polls)
	}
	for key := range gate.files {
		if strings.HasSuffix(key, piharness.DispositionPath) {
			t.Fatalf("disposition was written: %s", key)
		}
	}
}

type unreleasedReplay struct {
	inner    *Activities
	launches int
}

func (r *unreleasedReplay) Initialize(context.Context, Input) (Initialized, error) {
	return Initialized{Deadline: time.Now().Add(time.Hour)}, nil
}

func (r *unreleasedReplay) Prepare(context.Context, Input) error { return nil }

func (r *unreleasedReplay) Launch(ctx context.Context, in Input) error {
	r.launches++
	return r.inner.Launch(ctx, in)
}

func (r *unreleasedReplay) Observe(ctx context.Context, in Input) error {
	return r.inner.Observe(ctx, in)
}

func (r *unreleasedReplay) Finalize(context.Context, FinishInput) error { return nil }

func runnerExitBeforeRelease(t *testing.T) int {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "pi")
	script := "#!/usr/bin/env python3\nimport sys\nif \"--version\" in sys.argv:\n    print(" + strconv.Quote(piharness.PackageVersion) + ")\n    raise SystemExit(0)\nraise SystemExit(7)\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.py"), piharness.RunnerScript(), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := piharness.BuildInstruction(piharness.Instruction{
		Binary:          binary,
		Prompt:          "hello",
		Mode:            "send",
		SessionDir:      filepath.Join(dir, "session"),
		Checkpoint:      filepath.Join(dir, "checkpoint.jsonl"),
		DispositionPath: filepath.Join(dir, "disposition.json"),
		HistoryPath:     filepath.Join(dir, "history.json"),
		ResultPath:      filepath.Join(dir, "result.json"),
		ReleasePath:     filepath.Join(dir, "release"),
		ForwardEnv:      []string{},
		Offline:         true,
		PackageVersion:  piharness.PackageVersion,
		SettleSeconds:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	instruction := filepath.Join(dir, "instruction.json")
	if err = os.WriteFile(instruction, body, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", filepath.Join(dir, "run.py"), instruction)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("%v\n%s", err, out)
	}
	if exit != nil {
		code = exit.ExitCode()
	}
	if _, statErr := os.Stat(filepath.Join(dir, "disposition.json")); !os.IsNotExist(statErr) {
		t.Fatalf("disposition file: %v\n%s", statErr, out)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "release")); !os.IsNotExist(statErr) {
		t.Fatalf("release file: %v\n%s", statErr, out)
	}
	return code
}
