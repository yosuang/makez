//go:build linux

package supervisor

import (
	"errors"
	"os"
	"testing"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
)

func TestReleaseSentinelMatchesTheRunner(t *testing.T) {
	if releaseSentinel != piharness.ReleasePath {
		t.Fatalf("sentinel = %s, runner = %s", releaseSentinel, piharness.ReleasePath)
	}
}

func TestLiveAgentRejectsInputReplacement(t *testing.T) {
	s := testSupervisor(t)
	withAgentScan(t, []int{1000})
	for _, path := range []string{"/workspace/pi-instruction.json", "/workspace/makez-pi-run.py", "/workspace/nested/new.txt"} {
		err := s.Write(path, []byte("x"))
		requireConflict(t, err)
		if _, statErr := os.Lstat(s.workspace + path[len("/workspace"):]); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s was written: %v", path, statErr)
		}
	}
}

func TestLiveAgentCreatesTheReleaseFileOnce(t *testing.T) {
	s := testSupervisor(t)
	withAgentScan(t, []int{1000})
	withoutAgentChown(t)
	body := []byte("1\n")
	if err := s.Write(releaseSentinel, body); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(releaseSentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("release = %q", got)
	}
	requireConflict(t, s.Write(releaseSentinel, []byte("2\n")))
	got, err = s.Read(releaseSentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("release replaced = %q", got)
	}
}

func TestLiveAgentDoesNotReplaceAnExistingReleaseFile(t *testing.T) {
	s := testSupervisor(t)
	if err := os.WriteFile(s.workspace+"/pi-release", []byte("already\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pi-release", s.workspace+"/pi-link"); err != nil {
		t.Fatal(err)
	}
	withAgentScan(t, []int{1000})
	requireConflict(t, s.Write(releaseSentinel, []byte("1\n")))
	requireConflict(t, s.Write("/workspace/pi-link", []byte("1\n")))
	got, err := os.ReadFile(s.workspace + "/pi-release")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "already\n" {
		t.Fatalf("release = %q", got)
	}
}

func TestIdleWriteStillReplacesAFile(t *testing.T) {
	s := testSupervisor(t)
	withAgentScan(t, nil)
	withoutAgentChown(t)
	if err := s.Write("/workspace/pi-instruction.json", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("/workspace/pi-instruction.json", []byte("two")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read("/workspace/pi-instruction.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "two" {
		t.Fatalf("instruction = %q", got)
	}
}

func testSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	s, err := New(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func withAgentScan(t *testing.T, pids []int) {
	t.Helper()
	previous := listAgentProcesses
	listAgentProcesses = func() ([]int, error) { return pids, nil }
	t.Cleanup(func() { listAgentProcesses = previous })
}

func withoutAgentChown(t *testing.T) {
	t.Helper()
	previous := chownAgent
	chownAgent = func(*os.File) error { return nil }
	t.Cleanup(func() { chownAgent = previous })
}

func requireConflict(t *testing.T, err error) {
	t.Helper()
	var got *fault
	if !errors.As(err, &got) || got.Code != core.Conflict || got.Message != "input files cannot be replaced while an Agent runs" {
		t.Fatalf("error = %v", err)
	}
}
