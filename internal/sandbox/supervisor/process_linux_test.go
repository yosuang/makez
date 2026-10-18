package supervisor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

func linuxSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	if os.Getenv("MAKEZ_SUPERVISOR_TEST") != "1" || os.Geteuid() != 0 {
		t.Skip("requires an isolated Linux test container with MAKEZ_SUPERVISOR_TEST=1")
	}
	s := newTestSupervisor(t)
	// Go's temporary test parent is private by default; uid 1000 must traverse it.
	if err := os.Chmod(filepath.Dir(filepath.Dir(s.workspace)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(s.workspace, agentUID, agentUID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.Stop(testKey, "", 0); err != nil {
			t.Error(err)
		}
	})
	return s
}

func launch(command string) launchRequest {
	return launchRequest{StartRequest: core.StartRequest{Key: testKey, Command: core.CommandSpec{Argv: []string{"/bin/sh", "-c", command}, Workdir: "/workspace"}, Deadline: time.Now().Add(30 * time.Second)}}
}

func waitOutput(t *testing.T, s *Supervisor) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	cursor := ""
	var data []byte
	for time.Now().Before(deadline) {
		page, err := s.Output(testKey, cursor, 16384)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Records {
			data = append(data, record.Data...)
		}
		cursor = page.NextCursor
		if page.EOF {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("output did not drain")
	return nil
}

func TestLinuxConcurrentStartSecretsAndStop(t *testing.T) {
	s := linuxSupervisor(t)
	r := launch(`printf 1 >> count; printf '%s\n' "$TOKEN"; printf '%s\n' "$TOKEN" >&2; printf '%s' "$TOKEN" > leaked; awk '/NoNewPrivs:/{print $2}' /proc/self/status > nnp; sleep 0.1`)
	r.SecretRefs = map[string]string{"TOKEN": "test-token@1"}
	r.SecretValues = map[string]string{"TOKEN": "fake-private-token-123"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := s.Start(r); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	rotated := r
	rotated.SecretValues = map[string]string{"TOKEN": "rotated-not-used"}
	if _, err := s.Start(rotated); err != nil {
		t.Fatal(err)
	}
	out := waitOutput(t, s)
	if bytes.Contains(out, []byte(r.SecretValues["TOKEN"])) || bytes.Count(out, marker) != 2 {
		t.Fatalf("redaction failed: %q", out)
	}
	for name, want := range map[string]string{"count": "1", "nnp": "1\n"} {
		got, err := s.Read(name)
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	if _, err := s.Read("leaked"); err == nil {
		t.Fatal("secret artifact allowed")
	}
	id, _ := identity(testKey)
	data, err := os.ReadFile(filepath.Join(s.control, id+".json"))
	if err != nil || bytes.Contains(data, []byte(r.SecretValues["TOKEN"])) {
		t.Fatal("secret persisted in receipt")
	}
	if _, err := s.Stop(testKey, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(r); err == nil {
		t.Fatal("closed key restarted")
	}
}

func TestLinuxTombstoneAndEscapedChild(t *testing.T) {
	t.Run("tombstone", func(t *testing.T) {
		s := linuxSupervisor(t)
		if _, err := s.Stop(testKey, "", 0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Start(launch("touch unexpected")); err == nil {
			t.Fatal("late launch accepted")
		}
	})
	t.Run("escaped-session", func(t *testing.T) {
		s := linuxSupervisor(t)
		if _, err := s.Start(launch(`setsid /bin/sh -c 'sleep 30' & sleep 30`)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		if _, err := s.Stop(testKey, "", 0); err != nil {
			t.Fatal(err)
		}
		pids, err := agentProcesses()
		if err != nil || len(pids) != 0 {
			t.Fatalf("descendant survived: %v %v", pids, err)
		}
		_ = waitOutput(t, s)
	})
}

func TestLinuxDeadlineAndAtomicFiles(t *testing.T) {
	s := linuxSupervisor(t)
	if err := s.Write("nested/input.txt", []byte("input")); err != nil {
		t.Fatal(err)
	}
	r := launch("sleep 30")
	r.Deadline = time.Now().Add(200 * time.Millisecond)
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("nested/input.txt", []byte("overwrite")); err == nil {
		t.Fatal("input replaced during execution")
	}
	_ = waitOutput(t, s)
	obs, err := s.Process(testKey)
	if err != nil || obs.State != core.ProcessStopped {
		t.Fatalf("deadline not enforced: %#v %v", obs, err)
	}
	data, err := s.Read("nested/input.txt")
	if err != nil || string(data) != "input" {
		t.Fatalf("input modified: %q %v", data, err)
	}
}

func TestLinuxConcurrentStreamsDrainCompletely(t *testing.T) {
	s := linuxSupervisor(t)
	// Python here is workload fixture code; the supervisor itself is a Go binary.
	r := launch(`python3 -c 'import sys; [(sys.stdout.write("o"*9000), sys.stdout.flush(), sys.stderr.write("e"*9000), sys.stderr.flush()) for _ in range(4)]'`)
	if _, err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	out := waitOutput(t, s)
	if bytes.Count(out, []byte("o")) != 36000 || bytes.Count(out, []byte("e")) != 36000 {
		t.Fatalf("output truncated: %d bytes", len(out))
	}
	id, _ := identity(testKey)
	spool, err := os.ReadFile(filepath.Join(s.control, id+".log"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(spool)), "\n") {
		var record core.OutputRecord
		if json.Unmarshal([]byte(line), &record) != nil || seen[record.Offset] {
			t.Fatal("unstable output offset")
		}
		seen[record.Offset] = true
	}
}
