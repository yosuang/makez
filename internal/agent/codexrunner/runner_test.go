package codexrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("MAKEZ_RUNNER_FIXTURE"); mode != "" {
		if err := fixture(mode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(10)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fixture(mode string) error {
	home := os.Getenv("CODEX_HOME")
	if home == "" || !strings.HasPrefix(filepath.Base(home), ".makez-codex-") {
		return fmt.Errorf("missing isolated home")
	}
	if !slices.Contains(os.Args, `shell_environment_policy.inherit="none"`) || os.Args[len(os.Args)-1] != "-" {
		return fmt.Errorf("incorrect invocation")
	}
	if strings.Contains(strings.Join(os.Args, " "), "fixture-credential") {
		return fmt.Errorf("credential appeared in argv")
	}
	if os.Getenv("CODEX_AUTH_JSON") != "" || os.Getenv("OPENAI_API_KEY") != "" {
		return fmt.Errorf("inherited unrelated credential")
	}
	if mode == "api" {
		if os.Getenv("CODEX_API_KEY") != "fixture-credential" {
			return fmt.Errorf("missing API credential")
		}
	} else {
		path := filepath.Join(home, "auth.json")
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(data, []byte("fixture-access")) || bytes.Contains(data, []byte("fixture-refresh")) {
			return fmt.Errorf("invalid session cache")
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			return fmt.Errorf("unsafe session permissions")
		}
		if os.Getenv("CODEX_API_KEY") != "" || os.Getenv("CODEX_ACCESS_TOKEN") != "" {
			return fmt.Errorf("session inherited token environment")
		}
	}
	if err := os.WriteFile("observed-home.txt", []byte(home), 0600); err != nil {
		return err
	}
	if mode == "cancel" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		if err := os.WriteFile("ready", nil, 0600); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	}
	if mode == "symlink" {
		return os.Symlink("input.txt", ".codex-result.tmp")
	}
	return os.WriteFile(".codex-result.tmp", []byte(`{"succeeded":true,"summary":"fixture","artifacts":[]}`), 0600)
}

func runnerConfig(t *testing.T, mode string) Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("literal $(prompt)"), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEZ_RUNNER_FIXTURE", mode)
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	if mode == "api" {
		t.Setenv("CODEX_API_KEY", "fixture-credential")
	} else {
		t.Setenv("CODEX_AUTH_JSON", `{"auth_mode":"chatgpt","tokens":{"access_token":"fixture-access","refresh_token":"fixture-refresh"}}`)
	}
	authMode := "chatgpt_session"
	if mode == "api" {
		authMode = "api_key"
	}
	return Config{AuthMode: authMode, Workspace: dir, Home: dir, Executable: executable}
}

func assertRemovedHome(t *testing.T, dir string) {
	t.Helper()
	home, err := os.ReadFile(filepath.Join(dir, "observed-home.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(string(home)); !os.IsNotExist(err) {
		t.Fatal("temporary credentials were not removed")
	}
}

func TestRunCredentialsAndManifest(t *testing.T) {
	for _, mode := range []string{"api", "session"} {
		t.Run(mode, func(t *testing.T) {
			c := runnerConfig(t, mode)
			var output bytes.Buffer
			if code := Run(t.Context(), c, &output, &output); code != 0 {
				t.Fatalf("exit=%d output=%s", code, output.String())
			}
			data, err := os.ReadFile(filepath.Join(c.Workspace, "result.json"))
			if err != nil || !json.Valid(data) {
				t.Fatal("result manifest not published")
			}
			assertRemovedHome(t, c.Home)
		})
	}
}

func TestRunRejectsSymlinkAndMissingCredential(t *testing.T) {
	c := runnerConfig(t, "symlink")
	var output bytes.Buffer
	if code := Run(t.Context(), c, &output, &output); code != 74 {
		t.Fatalf("symlink accepted: %d", code)
	}
	assertRemovedHome(t, c.Home)
	if _, err := os.Stat(filepath.Join(c.Workspace, "result.json")); !os.IsNotExist(err) {
		t.Fatal("invalid manifest published")
	}
	output.Reset()
	if code := Run(t.Context(), c, &output, &output); code != 77 || !strings.Contains(output.String(), "AUTH_MISSING") {
		t.Fatalf("missing auth: %d %s", code, output.String())
	}
}

func TestRunCancellationRemovesCache(t *testing.T) {
	c := runnerConfig(t, "cancel")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var output bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- Run(ctx, c, &output, &output) }()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(c.Workspace, "ready")); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("fixture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
		assertRemovedHome(t, c.Home)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled runner did not stop")
	}
}
