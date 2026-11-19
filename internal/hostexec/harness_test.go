package hostexec

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
)

func TestHarnessRunsAHostProcess(t *testing.T) {
	h := testHarness(t)
	ctx := t.Context()
	ref, err := h.Ensure(ctx, core.SandboxRequest{Key: core.OperationKey{WorkspaceID: "w", TaskID: "t", RunID: "r", Step: "sandbox"}})
	if err != nil {
		t.Fatal(err)
	}
	if ref.BackendID != "host-exec-test" || ref.ID != "host-exec" {
		t.Fatalf("sandbox = %+v", ref)
	}
	caps, err := h.Capabilities(ctx, "default", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range caps.Supported {
		if capability == core.NetworkAllowlist {
			t.Fatal("harness advertises a network allowlist")
		}
	}
	proc, err := h.Start(ctx, ref, core.StartRequest{
		Key: core.OperationKey{WorkspaceID: "w", TaskID: "t", RunID: "r", Step: "agent"},
		Command: core.CommandSpec{
			Argv:    []string{"/usr/local/bin/python3", "-c", "print('host-exec-marker')"},
			Workdir: "/workspace",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if proc.ID != "host-proc-1" {
		t.Fatalf("process = %+v", proc)
	}
	deadline := time.Now().Add(10 * time.Second)
	var obs core.ProcessObservation
	for time.Now().Before(deadline) {
		obs, err = h.Process(ctx, ref, proc.Key)
		if err != nil {
			t.Fatal(err)
		}
		if obs.Exit != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if obs.Exit == nil || obs.Exit.Code != 0 {
		t.Fatalf("process = %+v", obs)
	}
	page, err := h.Output(ctx, proc, "", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !page.EOF || len(page.Records) != 1 || !bytes.Contains(page.Records[0].Data, []byte("host-exec-marker")) {
		t.Fatalf("output = %+v", page)
	}
	again, err := h.Start(ctx, ref, core.StartRequest{Command: core.CommandSpec{Argv: []string{"/bin/true"}, Workdir: "/workspace"}})
	if err != nil || again.ID != proc.ID {
		t.Fatalf("second start = %+v %v", again, err)
	}
}

func TestHarnessRefusesSecretsEscapeAndCheckout(t *testing.T) {
	h := testHarness(t)
	ctx := t.Context()
	ref, err := h.Ensure(ctx, core.SandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Start(ctx, ref, core.StartRequest{
		SecretValues: map[string]string{"TOKEN": "secret"},
		Command:      core.CommandSpec{Argv: []string{"/bin/true"}, Workdir: "/workspace"},
	})
	if err == nil {
		t.Fatal("secret was accepted")
	}
	body := []byte("nope")
	if err = h.WriteFile(ctx, ref, "/workspace/../outside", bytes.NewReader(body), int64(len(body))); err == nil {
		t.Fatal("escape was accepted")
	}
	if err = h.WriteFile(ctx, ref, "/etc/passwd", bytes.NewReader(body), int64(len(body))); err == nil {
		t.Fatal("outside path was accepted")
	}
	home := t.TempDir()
	if _, err = New("/workspace", home); err == nil {
		t.Fatal("accepted /workspace")
	}
	instruction := []byte(`{"session_dir":"/workspace/pi-session","binary":"/usr/local/bin/pi"}`)
	if err = h.WriteFile(ctx, ref, piharness.InstructionPath, bytes.NewReader(instruction), int64(len(instruction))); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(h.Workspace(), "pi-instruction.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(written, []byte(`"/workspace`)) || !bytes.Contains(written, []byte(h.Workspace())) {
		t.Fatalf("instruction = %s", written)
	}
	if _, err = os.Lstat("/workspace/pi-instruction.json"); err == nil {
		t.Fatal("wrote the product checkout")
	}
}

func TestHarnessRejectsAnEmptyPath(t *testing.T) {
	h := testHarness(t)
	t.Setenv("PATH", "")
	_, err := h.Start(t.Context(), core.SandboxRef{}, core.StartRequest{
		Command: core.CommandSpec{Argv: []string{"/bin/true"}, Workdir: "/workspace"},
	})
	if err == nil {
		t.Fatal("empty PATH was accepted")
	}
}

func testHarness(t *testing.T) *Harness {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := New(root, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}
