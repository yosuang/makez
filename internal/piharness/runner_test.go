package piharness

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunnerReconcilesWithoutSending(t *testing.T) {
	dir := t.TempDir()
	binary := fakePi(t, dir, fakeOptions{version: PackageVersion})
	code, result := runRunner(t, dir, binary, Instruction{Mode: "reconcile_only", RestoreCheckpoint: true}, nil)
	if code != 0 || result.Succeeded {
		t.Fatalf("exit %d result %#v", code, result)
	}
	if strings.Contains(result.Summary, "sent the prompt again") == false && !strings.Contains(result.Summary, "did not send") {
		t.Fatalf("summary = %s", result.Summary)
	}
	log := readFile(t, filepath.Join(dir, "pi.log"))
	if strings.Contains(log, "prompt") || strings.Contains(log, "bash") {
		t.Fatalf("reconcile sent a command:\n%s", log)
	}
	if !strings.Contains(log, "get_messages") {
		t.Fatalf("log = %s", log)
	}
	args := argvList(t, filepath.Join(dir, "pi.argv"))
	if !containsArg(args, "--no-tools") || containsArg(args, "--session") {
		t.Fatalf("argv = %#v", args)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.jsonl")); !os.IsNotExist(err) {
		t.Fatal("missing checkpoint was created")
	}
}

func TestRunnerRecordsSendingBeforeThePrompt(t *testing.T) {
	dir := t.TempDir()
	binary := fakePi(t, dir, fakeOptions{version: PackageVersion, settle: true})
	code, result := runRunner(t, dir, binary, Instruction{Mode: "send", SettleSeconds: 5}, nil)
	if code != 0 || !result.Succeeded {
		t.Fatalf("exit %d result %#v", code, result)
	}
	seen := readFile(t, filepath.Join(dir, "disposition-at-prompt"))
	if !strings.Contains(seen, `"disposition":"sending"`) {
		t.Fatalf("disposition at prompt = %s", seen)
	}
	argv := readFile(t, filepath.Join(dir, "pi.argv"))
	for _, forbidden := range []string{"--approve", "--api-key", "--no-session", "danger-full-access", "not-forwarded-value"} {
		if strings.Contains(argv, forbidden) {
			t.Fatalf("argv contains %s", forbidden)
		}
	}
	keys := readFile(t, filepath.Join(dir, "pi.env"))
	if strings.Contains(keys, "LEAKED") {
		t.Fatal("unlisted environment reached Pi")
	}
}

func TestRunnerRejectsAPromptWithoutCallingItSettled(t *testing.T) {
	dir := t.TempDir()
	binary := fakePi(t, dir, fakeOptions{version: PackageVersion, reject: true})
	code, result := runRunner(t, dir, binary, Instruction{Mode: "send"}, nil)
	if code != 1 || result.Succeeded {
		t.Fatalf("exit %d result %#v", code, result)
	}
	raw := readFile(t, filepath.Join(dir, "disposition.json"))
	if !strings.Contains(raw, `"disposition":"rejected"`) {
		t.Fatalf("disposition = %s", raw)
	}
}

func TestRunnerClearQueuePrecedesAbort(t *testing.T) {
	dir := t.TempDir()
	binary := fakePi(t, dir, fakeOptions{version: PackageVersion})
	instruction := instructionAt(t, dir, binary, Instruction{Mode: "send", SettleSeconds: 10})
	cmd := exec.Command("python3", filepath.Join(dir, "run.py"), instruction)
	cmd.Env = runnerEnv(dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, filepath.Join(dir, "disposition.json"), `"disposition":"accepted"`)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("runner did not exit after SIGTERM")
	}
	log := readFile(t, filepath.Join(dir, "pi.log"))
	clearAt := strings.Index(log, "clear_queue")
	abortAt := strings.Index(log, "abort")
	if clearAt < 0 || abortAt < clearAt {
		t.Fatalf("log = %s", log)
	}
	if strings.Contains(log, "\nbash\n") || strings.HasPrefix(log, "bash\n") {
		t.Fatalf("log = %s", log)
	}
}

func TestRunnerRefusesADifferentPackageVersion(t *testing.T) {
	dir := t.TempDir()
	binary := fakePi(t, dir, fakeOptions{version: "0.0.0"})
	code, result := runRunner(t, dir, binary, Instruction{Mode: "send"}, nil)
	if code != 1 || result.Succeeded {
		t.Fatalf("exit %d result %#v", code, result)
	}
	if _, err := os.Stat(filepath.Join(dir, "pi.argv")); !os.IsNotExist(err) {
		t.Fatal("version mismatch started RPC")
	}
}

func TestRunnerRestoresOnlyARealCheckpoint(t *testing.T) {
	dir := t.TempDir()
	binary := fakePi(t, dir, fakeOptions{version: PackageVersion})
	checkpoint := filepath.Join(dir, "checkpoint.jsonl")
	if err := os.WriteFile(checkpoint, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _ := runRunner(t, dir, binary, Instruction{Mode: "reconcile_only", RestoreCheckpoint: true}, nil)
	if code != 0 {
		t.Fatal(code)
	}
	argv := readFile(t, filepath.Join(dir, "pi.argv"))
	if !strings.Contains(argv, checkpoint) || !strings.Contains(argv, "--offline") {
		t.Fatalf("argv = %s", argv)
	}
}

type resultFile struct {
	Succeeded bool
	Summary   string
}

type fakeOptions struct {
	version string
	reject  bool
	settle  bool
}

func fakePi(t *testing.T, dir string, opts fakeOptions) string {
	t.Helper()
	path := filepath.Join(dir, "pi")
	quote := strconv.Quote
	script := "#!/usr/bin/env python3\nimport json, os, sys\n" +
		"LOG = " + quote(filepath.Join(dir, "pi.log")) + "\n" +
		"ARGV = " + quote(filepath.Join(dir, "pi.argv")) + "\n" +
		"ENV = " + quote(filepath.Join(dir, "pi.env")) + "\n" +
		"SEEN = " + quote(filepath.Join(dir, "disposition-at-prompt")) + "\n" +
		"DISP = " + quote(filepath.Join(dir, "disposition.json")) + "\n" +
		"VERSION = " + quote(opts.version) + "\n" +
		"REJECT = " + boolPython(opts.reject) + "\n" +
		"SETTLE = " + boolPython(opts.settle) + "\n" +
		`if "--version" in sys.argv:
    print(VERSION)
    raise SystemExit(0)
open(ARGV, "w").write(json.dumps(sys.argv))
open(ENV, "w").write(json.dumps(sorted(os.environ)))
for line in sys.stdin:
    cmd = json.loads(line)
    kind = cmd.get("type", "")
    open(LOG, "a").write(kind + "\n")
    if kind in ("bash", "abort_bash"):
        raise SystemExit(99)
    if kind == "prompt":
        open(SEEN, "w").write(open(DISP, encoding="utf-8").read())
        body = {"success": False, "error": "No API key found for the selected model."} if REJECT else {"success": True, "data": {"disposition": "started"}}
        print(json.dumps({"id": cmd["id"], "type": "response", "command": "prompt", **body}), flush=True)
        if SETTLE:
            print(json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "ok"}}), flush=True)
            print(json.dumps({"type": "agent_settled"}), flush=True)
    elif kind == "get_messages":
        print(json.dumps({"id": cmd["id"], "type": "response", "command": "get_messages", "success": True, "data": {"messages": []}}), flush=True)
    elif kind == "clear_queue":
        print(json.dumps({"id": cmd["id"], "type": "response", "command": "clear_queue", "success": True, "data": {"steering": [], "followUp": []}}), flush=True)
    elif kind == "abort":
        print(json.dumps({"id": cmd["id"], "type": "response", "command": "abort", "success": True}), flush=True)
        print(json.dumps({"type": "agent_settled"}), flush=True)
    else:
        raise SystemExit(98)
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func boolPython(value bool) string {
	if value {
		return "True"
	}
	return "False"
}

func runRunner(t *testing.T, dir, binary string, inst Instruction, extra []string) (int, resultFile) {
	t.Helper()
	instruction := instructionAt(t, dir, binary, inst)
	if err := os.WriteFile(filepath.Join(dir, "run.py"), []byte(runnerSource), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", filepath.Join(dir, "run.py"), instruction)
	cmd.Env = append(runnerEnv(dir), extra...)
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("%v\n%s", err, out)
	}
	if exit != nil {
		code = exit.ExitCode()
	}
	var result resultFile
	raw, readErr := os.ReadFile(filepath.Join(dir, "result.json"))
	if readErr != nil {
		t.Fatalf("no result (%v): %s", readErr, out)
	}
	if json.Unmarshal(raw, &result) != nil || result.Summary == "" {
		t.Fatalf("result %s\n%s", raw, out)
	}
	return code, result
}

func instructionAt(t *testing.T, dir, binary string, inst Instruction) string {
	t.Helper()
	if inst.SettleSeconds == 0 {
		inst.SettleSeconds = 5
	}
	inst.Binary = binary
	inst.Prompt = "hello"
	inst.SessionDir = filepath.Join(dir, "session")
	inst.Checkpoint = filepath.Join(dir, "checkpoint.jsonl")
	inst.DispositionPath = filepath.Join(dir, "disposition.json")
	inst.HistoryPath = filepath.Join(dir, "history.json")
	inst.ResultPath = filepath.Join(dir, "result.json")
	inst.PackageVersion = PackageVersion
	inst.Offline = true
	if inst.ForwardEnv == nil {
		inst.ForwardEnv = []string{}
	}
	body, err := BuildInstruction(inst)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "instruction.json")
	if err = os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "run.py"), []byte(runnerSource), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runnerEnv(dir string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"LANG=C.UTF-8",
		"LEAKED=not-forwarded-value",
	}
}

func waitFor(t *testing.T, path, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(raw), needle) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s in %s", needle, path)
}

func argvList(t *testing.T, path string) []string {
	t.Helper()
	var args []string
	if err := json.Unmarshal([]byte(readFile(t, path)), &args); err != nil {
		t.Fatal(err)
	}
	return args
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
