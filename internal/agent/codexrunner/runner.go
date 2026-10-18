// Package codexrunner translates the platform's file contract to Codex CLI.
package codexrunner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const instructions = `Complete the user's task in this isolated makez workspace.
Create actual deliverable files under /workspace/deliverables, then run relevant checks.
Put all files needed to use the result in deliverables. Do not inspect credentials or authentication
caches. Do not modify input.txt, result-schema.json, result.json, or .codex-result.tmp.
Your final answer must follow the supplied JSON schema. Set succeeded=true only when the requested
deliverables exist and checks passed. List paths relative to /workspace, starting with deliverables/,
set required=true for every listed file, and use appropriate media types.
If the task cannot be completed, report succeeded=false and an actionable summary.

User task:
`

type Config struct {
	AuthMode   string
	Model      string
	Workspace  string
	Home       string
	Executable string
}

func arguments(c Config) []string {
	store := "ephemeral"
	if c.AuthMode == "chatgpt_session" {
		store = "file"
	}
	args := []string{"exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--sandbox", "danger-full-access", "--color", "never",
		"-c", `approval_policy="never"`, "-c", `cli_auth_credentials_store="` + store + `"`, "-c", `history.persistence="none"`,
		"-c", `shell_environment_policy.inherit="none"`, "-c", `shell_environment_policy.ignore_default_excludes=false`,
		"-c", `shell_environment_policy.filters={CODEX_ACCESS_TOKEN="exclude",CODEX_API_KEY="exclude",CODEX_AUTH_JSON="exclude"}`,
		"-c", `shell_environment_policy.set={PATH="/usr/local/bin:/usr/bin:/bin",HOME="/home/agent",LANG="C.UTF-8"}`,
		"--output-schema", filepath.Join(c.Workspace, "result-schema.json"), "--output-last-message", filepath.Join(c.Workspace, ".codex-result.tmp")}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	return append(args, "-")
}

func sessionCache(raw string) ([]byte, error) {
	var auth struct {
		Tokens struct {
			Access  string  `json:"access_token"`
			ID      string  `json:"id_token"`
			Account *string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh json.RawMessage `json:"last_refresh"`
	}
	if json.Unmarshal([]byte(raw), &auth) != nil || auth.Tokens.Access == "" {
		return nil, errors.New("invalid session")
	}
	data := map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]any{"access_token": auth.Tokens.Access, "id_token": auth.Tokens.ID, "account_id": auth.Tokens.Account, "refresh_token": ""}}
	// A local test must never rotate the host login's shared refresh token.
	if len(auth.LastRefresh) > 0 {
		data["last_refresh"] = auth.LastRefresh
	}
	return json.Marshal(data)
}

// Run returns the CLI exit code and never includes credentials in diagnostics.
func Run(ctx context.Context, c Config, stdout, stderr io.Writer) int {
	fail := func(code string, status int) int {
		_ = json.NewEncoder(stdout).Encode(map[string]string{"type": "makez.error", "code": code})
		return status
	}
	variable := map[string]string{"api_key": "CODEX_API_KEY", "codex_access_token": "CODEX_ACCESS_TOKEN", "chatgpt_session": "CODEX_AUTH_JSON"}[c.AuthMode]
	if variable == "" {
		return fail("AUTH_INVALID", 77)
	}
	credential := os.Getenv(variable)
	_ = os.Unsetenv(variable)
	if credential == "" {
		return fail("AUTH_MISSING", 77)
	}
	home, err := os.MkdirTemp(c.Home, ".makez-codex-")
	if err != nil {
		return fail("CODEX_RUNTIME_ERROR", 74)
	}
	defer os.RemoveAll(home)
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_API_KEY", "CODEX_AUTH_JSON", "CODEX_HOME", "RUST_LOG":
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "CODEX_HOME="+home, "RUST_LOG=error")
	if c.AuthMode == "chatgpt_session" {
		data, e := sessionCache(credential)
		if e != nil {
			return fail("AUTH_INVALID", 77)
		}
		err = os.WriteFile(filepath.Join(home, "auth.json"), data, 0600)
		clear(data)
		if err != nil {
			return fail("CODEX_RUNTIME_ERROR", 74)
		}
	} else {
		env = append(env, variable+"="+credential)
	}
	prompt, err := os.ReadFile(filepath.Join(c.Workspace, "input.txt"))
	if err != nil || os.MkdirAll(filepath.Join(c.Workspace, "deliverables"), 0755) != nil {
		return fail("CODEX_RUNTIME_ERROR", 74)
	}
	cmd := exec.CommandContext(ctx, c.Executable, arguments(c)...)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Workspace, env, strings.NewReader(instructions+string(prompt)), stdout, stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err = cmd.Run(); err != nil {
		code := 1
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() > 0 {
			code = exit.ExitCode()
		}
		return fail("CODEX_EXEC_FAILED", code)
	}
	result := filepath.Join(c.Workspace, ".codex-result.tmp")
	f, err := os.OpenFile(result, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail("CODEX_RUNTIME_ERROR", 74)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return fail("INVALID_RESULT", 74)
	}
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 || !json.Valid(data) {
		return fail("INVALID_RESULT", 74)
	}
	if err = os.Rename(result, filepath.Join(c.Workspace, "result.json")); err != nil {
		return fail("CODEX_RUNTIME_ERROR", 74)
	}
	return 0
}
