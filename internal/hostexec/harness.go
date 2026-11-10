// Package hostexec is a host process fake for the sandbox port.
//
// It is a substitute for OpenSandbox: one command runs on this machine, and
// /workspace is a private directory. It does not create a container, dial a
// sandbox API, or apply a network policy. The default makez build does not
// import it. A listening worker selects it only when that binary is built with
// the makezhostexec tag and MAKEZ_TEST_HOST_EXEC names the directory.
package hostexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/piharness"
)

// Harness implements core.SandboxProvider by running one host process.
// A second Start returns the same receipt. It is not safe as a multi-task backend.
type Harness struct {
	root   string
	home   string
	python string
	mu     sync.Mutex
	box    core.SandboxRef
	ref    core.ProcessRef
	cmd    *exec.Cmd
	stdout []byte
	stderr []byte
	group  sync.WaitGroup
	exit   *core.ProcessExit
}

// New binds /workspace to workspace and the child HOME to home.
// Both paths must already be absolute. /workspace itself is refused because
// that path is the product checkout on a developer machine.
func New(workspace, home string) (*Harness, error) {
	if err := refuseCheckout(workspace); err != nil {
		return nil, err
	}
	if err := refuseCheckout(home); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(home) {
		return nil, errors.New("host exec paths must be absolute")
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return nil, errors.New("host exec workspace is missing")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		return nil, errors.New("python3 is not on PATH")
	}
	if err = os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	return &Harness{root: workspace, home: home, python: python}, nil
}

// Open prepares dir/workspace and dir/home, then returns a harness rooted there.
func Open(dir string) (*Harness, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("host exec directory must be absolute")
	}
	if err := refuseCheckout(dir); err != nil {
		return nil, err
	}
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return nil, err
	}
	return New(workspace, filepath.Join(dir, "home"))
}

func refuseCheckout(path string) error {
	cleaned := filepath.Clean(path)
	if cleaned == "/workspace" || strings.HasPrefix(cleaned, "/workspace"+string(filepath.Separator)) {
		return errors.New("host exec refuses /workspace")
	}
	return nil
}

// Workspace returns the host directory that stands in for /workspace.
func (h *Harness) Workspace() string { return h.root }

// Python returns the host interpreter substituted for /usr/local/bin/python3.
func (h *Harness) Python() string { return h.python }

// Snapshot returns the output collected so far.
func (h *Harness) Snapshot() (stdout, stderr []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]byte(nil), h.stdout...), append([]byte(nil), h.stderr...)
}

// Close stops a still-running process group. It is safe to call more than once.
func (h *Harness) Close() {
	h.mu.Lock()
	pid := 0
	if h.cmd != nil && h.cmd.Process != nil {
		pid = h.cmd.Process.Pid
	}
	exited := h.exit != nil
	h.mu.Unlock()
	if pid != 0 && !exited {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// BackendID is the receipt name. It is not the OpenSandbox provider name.
func (h *Harness) BackendID() string { return "host-exec-test" }

// Capabilities reports the process operations this fake actually implements.
// NetworkAllowlist is absent: this fake is not a network policy.
func (h *Harness) Capabilities(context.Context, string, string) (core.SandboxCapabilities, error) {
	return core.SandboxCapabilities{Supported: []core.Capability{
		core.BackgroundProcess, core.OperationLookup, core.ReplayableOutput, core.ProcessStop,
		core.FileAccess, core.FiniteLifetime, core.SecretInjection,
	}, MaxLifetime: 2 * time.Hour}, nil
}

// Ensure records one local workspace. It does not call a sandbox service.
func (h *Harness) Ensure(_ context.Context, request core.SandboxRequest) (core.SandboxRef, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.box.ID == "" {
		h.box = core.SandboxRef{BackendID: h.BackendID(), ID: "host-exec", Key: request.Key}
	}
	return h.box, nil
}

// Inspect reports the one workspace recorded by Ensure.
func (h *Harness) Inspect(context.Context, core.OperationKey) (core.SandboxObservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.box.ID == "" {
		return core.SandboxObservation{State: core.SandboxAbsent, ObservedAt: time.Now()}, nil
	}
	return core.SandboxObservation{State: core.SandboxReady, Matches: []core.SandboxRef{h.box}, ObservedAt: time.Now()}, nil
}

// Renew is unsupported. The fake does not extend a sandbox lease.
func (h *Harness) Renew(context.Context, core.SandboxRef, time.Time) error {
	return errors.New("host exec does not renew sandboxes")
}

// Delete drops no host resources. The test owns the directory.
func (h *Harness) Delete(context.Context, core.OperationKey) error { return nil }

// Start runs the command. /usr/local/bin/python3 is rewritten to the host
// interpreter, and /workspace paths are rewritten into the private directory.
// Secrets and a copied command environment are refused. The child receives
// only PATH, HOME, and LANG.
func (h *Harness) Start(_ context.Context, sandbox core.SandboxRef, request core.StartRequest) (core.ProcessRef, error) {
	h.mu.Lock()
	if h.ref.ID != "" {
		ref := h.ref
		h.mu.Unlock()
		return ref, nil
	}
	h.mu.Unlock()
	if len(request.SecretValues) != 0 || len(request.SecretRefs) != 0 || len(request.Command.Env) != 0 {
		return core.ProcessRef{}, errors.New("host exec refuses secrets and command env")
	}
	if len(request.Command.Argv) == 0 {
		return core.ProcessRef{}, errors.New("missing argv")
	}
	argv := append([]string(nil), request.Command.Argv...)
	if _, err := os.Stat(argv[0]); err != nil {
		if argv[0] != "/usr/local/bin/python3" {
			return core.ProcessRef{}, err
		}
		argv[0] = h.python
	}
	for i, arg := range argv {
		if arg == "/workspace" || strings.HasPrefix(arg, "/workspace/") {
			host, err := workspaceHost(h.root, arg)
			if err != nil {
				return core.ProcessRef{}, err
			}
			argv[i] = host
		}
	}
	workdir, err := workspaceHost(h.root, request.Command.Workdir)
	if err != nil {
		return core.ProcessRef{}, err
	}
	if os.Getenv("PATH") == "" {
		return core.ProcessRef{}, errors.New("PATH is empty")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.home, "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return core.ProcessRef{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return core.ProcessRef{}, err
	}
	if err = cmd.Start(); err != nil {
		return core.ProcessRef{}, err
	}
	h.mu.Lock()
	h.cmd = cmd
	h.ref = core.ProcessRef{Sandbox: sandbox, ID: "host-proc-1", Key: request.Key}
	ref := h.ref
	h.mu.Unlock()
	h.group.Add(2)
	go h.consume(stdout, &h.stdout)
	go h.consume(stderr, &h.stderr)
	go h.wait(cmd)
	return ref, nil
}

func (h *Harness) consume(r io.Reader, dest *[]byte) {
	defer h.group.Done()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			h.mu.Lock()
			*dest = append(*dest, buf[:n]...)
			h.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (h *Harness) wait(cmd *exec.Cmd) {
	h.group.Wait()
	err := cmd.Wait()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = 1
		}
	}
	h.mu.Lock()
	h.exit = &core.ProcessExit{Code: code, FinishedAt: time.Now().UTC()}
	h.mu.Unlock()
}

// Process reports the one process receipt, or not-started when Start has not run.
func (h *Harness) Process(context.Context, core.SandboxRef, core.OperationKey) (core.ProcessObservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ref.ID == "" {
		return core.ProcessObservation{State: core.ProcessNotStarted, ObservedAt: time.Now()}, nil
	}
	ref := h.ref
	if h.exit != nil {
		exit := *h.exit
		return core.ProcessObservation{State: core.ProcessExited, Ref: &ref, Exit: &exit, ObservedAt: time.Now()}, nil
	}
	return core.ProcessObservation{State: core.ProcessRunning, Ref: &ref, ObservedAt: time.Now()}, nil
}

// Output returns process output at a stdout/stderr byte cursor.
func (h *Harness) Output(_ context.Context, _ core.ProcessRef, cursor string, maxBytes int) (core.OutputPage, error) {
	if maxBytes < 1 {
		return core.OutputPage{}, errors.New("output page is empty")
	}
	stdoutAt, stderrAt, err := parseCursor(cursor)
	if err != nil {
		return core.OutputPage{}, err
	}
	h.mu.Lock()
	stdout := append([]byte(nil), h.stdout...)
	stderr := append([]byte(nil), h.stderr...)
	exited := h.exit != nil
	h.mu.Unlock()
	if stdoutAt > len(stdout) || stderrAt > len(stderr) {
		return core.OutputPage{}, errors.New("output cursor is ahead of the stream")
	}
	var records []core.OutputRecord
	stdoutAt, records = takeLines("stdout", stdout, stdoutAt, exited, &maxBytes, records)
	stderrAt, records = takeLines("stderr", stderr, stderrAt, exited, &maxBytes, records)
	page := core.OutputPage{Records: records, NextCursor: fmt.Sprintf("stdout:%d,stderr:%d", stdoutAt, stderrAt)}
	page.EOF = exited && stdoutAt == len(stdout) && stderrAt == len(stderr)
	return page, nil
}

func parseCursor(cursor string) (int, int, error) {
	if cursor == "" {
		return 0, 0, nil
	}
	var stdoutAt, stderrAt int
	if _, err := fmt.Sscanf(cursor, "stdout:%d,stderr:%d", &stdoutAt, &stderrAt); err != nil || stdoutAt < 0 || stderrAt < 0 {
		return 0, 0, errors.New("invalid output cursor")
	}
	return stdoutAt, stderrAt, nil
}

func takeLines(stream string, buf []byte, from int, eof bool, budget *int, records []core.OutputRecord) (int, []core.OutputRecord) {
	for from < len(buf) && *budget > 0 {
		rest := buf[from:]
		newline := bytes.IndexByte(rest, '\n')
		var chunk []byte
		switch {
		case newline >= 0:
			chunk = rest[:newline+1]
		case eof:
			chunk = rest
		default:
			return from, records
		}
		if len(chunk) > *budget && len(records) > 0 {
			return from, records
		}
		records = append(records, core.OutputRecord{Offset: fmt.Sprintf("%s:%d", stream, from), Stream: stream, Data: append([]byte(nil), chunk...)})
		from += len(chunk)
		*budget -= len(chunk)
		if *budget < 0 {
			*budget = 0
		}
	}
	return from, records
}

// Stop signals the process group and waits through the grace period.
func (h *Harness) Stop(_ context.Context, _ core.SandboxRef, _ core.OperationKey, policy core.StopPolicy) error {
	h.mu.Lock()
	exited := h.exit != nil
	pid := 0
	if h.cmd != nil && h.cmd.Process != nil {
		pid = h.cmd.Process.Pid
	}
	h.mu.Unlock()
	if exited || pid == 0 {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	grace := policy.GracePeriod
	if grace <= 0 {
		grace = time.Second
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		done := h.exit != nil
		h.mu.Unlock()
		if done {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return nil
}

// ReadFile reads a /workspace path from the private directory.
func (h *Harness) ReadFile(_ context.Context, _ core.SandboxRef, path string) (io.ReadCloser, error) {
	host, err := workspaceHost(h.root, path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(host)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	return file, nil
}

// WriteFile writes a /workspace path. The Pi instruction is rewritten so its
// /workspace strings point at the private directory.
func (h *Harness) WriteFile(_ context.Context, _ core.SandboxRef, path string, contents io.Reader, size int64) error {
	data, err := io.ReadAll(contents)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(data)) != size {
		return errors.New("write size does not match")
	}
	if path == piharness.InstructionPath {
		data, err = rewriteWorkspace(h.root, data)
		if err != nil {
			return err
		}
	}
	host, err := workspaceHost(h.root, path)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(host), 0o700); err != nil {
		return err
	}
	return os.WriteFile(host, data, 0o600)
}

func workspaceHost(root, path string) (string, error) {
	if path != "/workspace" && !strings.HasPrefix(path, "/workspace/") {
		return "", fmt.Errorf("path is outside the workspace")
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(path, "/workspace"), "/")
	if rel == "" {
		return root, nil
	}
	if strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		return "", errors.New("path escapes the workspace")
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	back, err := filepath.Rel(root, full)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the workspace")
	}
	return full, nil
}

func rewriteWorkspace(root string, body []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	var walk func(any) (any, error)
	walk = func(node any) (any, error) {
		switch typed := node.(type) {
		case map[string]any:
			for key, item := range typed {
				next, err := walk(item)
				if err != nil {
					return nil, err
				}
				typed[key] = next
			}
			return typed, nil
		case []any:
			for i, item := range typed {
				next, err := walk(item)
				if err != nil {
					return nil, err
				}
				typed[i] = next
			}
			return typed, nil
		case string:
			if typed == "/workspace" || strings.HasPrefix(typed, "/workspace/") {
				return workspaceHost(root, typed)
			}
			return typed, nil
		default:
			return typed, nil
		}
	}
	next, err := walk(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(next)
}

var _ core.SandboxProvider = (*Harness)(nil)
