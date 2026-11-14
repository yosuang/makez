package workflow

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
	"go.temporal.io/sdk/temporal"
)

// liveWriteGate models the supervisor lock. A running agent rejects every write
// except the first create of the release file. This is not an OpenSandbox.
type liveWriteGate struct {
	*sendingGate
	rejectRelease bool
	order         []string
}

type liveConflictError struct{}

func (liveConflictError) Error() string          { return "input files cannot be replaced while an Agent runs" }
func (liveConflictError) Code() core.FailureCode { return core.Conflict }

func (g *liveWriteGate) Start(ctx context.Context, sandbox core.SandboxRef, request core.StartRequest) (core.ProcessRef, error) {
	ref, err := g.sendingGate.Start(ctx, sandbox, request)
	if err != nil {
		return ref, err
	}
	g.order = append(g.order, "start")
	return ref, nil
}

func (g *liveWriteGate) WriteFile(ctx context.Context, sandbox core.SandboxRef, path string, contents io.Reader, size int64) error {
	g.t.Helper()
	if g.started && path != piharness.ReleasePath {
		return liveConflictError{}
	}
	if path == piharness.ReleasePath {
		if !g.started || g.rejectRelease {
			return liveConflictError{}
		}
		prompt := requirePrompt(g.t, g.db, g.fx)
		if !prompt.Started {
			g.t.Fatal("release file was written before the prompt was marked started")
		}
		g.order = append(g.order, "release")
	}
	if path == piharness.InstructionPath {
		g.order = append(g.order, "instruction")
	}
	return g.sendingGate.WriteFile(ctx, sandbox, path, contents, size)
}

func TestReleaseCreateFollowsTheRunningProcess(t *testing.T) {
	url := testingDB(t)
	ctx := t.Context()
	db := openTestDB(t, url)
	fx := seedPiLaunch(t, db, url)
	gate := &liveWriteGate{sendingGate: &sendingGate{t: t, db: db, fx: fx, remember: true, files: map[string][]byte{}}}
	activities := newPiActivities(t, db, gate.sendingGate)
	activities.Provider = gate
	if err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(gate.order, ",") != "instruction,start,release" {
		t.Fatalf("order = %v", gate.order)
	}
	if len(gate.releases) != 1 || gate.releases[0] != "sb-"+fx.runA {
		t.Fatalf("releases = %v", gate.releases)
	}
}

func TestRejectedReleaseStaysConflict(t *testing.T) {
	url := testingDB(t)
	ctx := t.Context()
	db := openTestDB(t, url)
	fx := seedPiLaunch(t, db, url)
	gate := &liveWriteGate{sendingGate: &sendingGate{t: t, db: db, fx: fx, remember: true, files: map[string][]byte{}}, rejectRelease: true}
	activities := newPiActivities(t, db, gate.sendingGate)
	activities.Provider = gate
	err := activities.Launch(ctx, Input{WorkspaceID: fx.workspace, TaskID: fx.taskA})
	var app *temporal.ApplicationError
	// The task reason is this application-error type. It stays CONFLICT, not AGENT_FAILED.
	if !errors.As(err, &app) || app.Type() != string(core.Conflict) || !app.NonRetryable() {
		t.Fatalf("launch = %v", err)
	}
	cause := errors.Unwrap(err)
	if cause == nil || cause.Error() != "input files cannot be replaced while an Agent runs" {
		t.Fatalf("cause = %v", cause)
	}
	if strings.Join(gate.order, ",") != "instruction,start" {
		t.Fatalf("order = %v", gate.order)
	}
}
