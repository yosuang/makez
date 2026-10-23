package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

var testKey = core.OperationKey{WorkspaceID: "test", TaskID: "task", RunID: "run", Step: "agent"}

func newTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	s, err := New(filepath.Join(dir, "control"), filepath.Join(dir, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRedactorSplitsAndStructuredSecrets(t *testing.T) {
	secret := "example-private-token-123"
	structured := `{"auth_mode":"chatgpt","tokens":{"access_token":"` + secret + `","account_id":"visible"},"nested":[{"clientSecret":"private-client-value"}]}`
	literals, err := secretLiterals(map[string]string{"TOKEN": structured})
	if err != nil {
		t.Fatal(err)
	}
	input := []byte("progress " + secret + " private-client-value chatgpt visible\n")
	for split := 0; split <= len(input); split++ {
		filter := redactor{values: literals}
		out := append(filter.feed(input[:split], false), filter.feed(input[split:], true)...)
		if string(out) != "progress [REDACTED] [REDACTED] chatgpt visible\n" {
			t.Fatalf("split %d: %q", split, out)
		}
	}
	filter := redactor{values: [][]byte{[]byte("abcde"), []byte("abc"), []byte("xyz")}}
	var out []byte
	for _, b := range []byte("abcde/xyz/abc!") {
		out = append(out, filter.feed([]byte{b}, false)...)
	}
	out = append(out, filter.feed(nil, true)...)
	if string(out) != "[REDACTED]/[REDACTED]/[REDACTED]!" {
		t.Fatalf("overlap: %q", out)
	}
	deep := strings.Repeat(`{"secret":`, 34) + `"value"` + strings.Repeat("}", 34)
	if _, err := secretLiterals(map[string]string{"TOKEN": deep}); err == nil {
		t.Fatal("unbounded JSON accepted")
	}
	deep = strings.Repeat("[", 10001) + `{"token":"private-value"}` + strings.Repeat("]", 10001)
	if _, err := secretLiterals(map[string]string{"TOKEN": deep}); err == nil {
		t.Fatal("decoder depth limit bypassed secret inspection")
	}
}

func TestFilesAndLostSecretContext(t *testing.T) {
	s := newTestSupervisor(t)
	for _, name := range []string{"../secret", "/etc/passwd", "a//b", "a/../../x"} {
		if _, _, err := s.parent(name, false); err == nil {
			t.Fatalf("unsafe path accepted: %s", name)
		}
	}
	if err := os.Symlink(os.TempDir(), filepath.Join(s.workspace, "outside")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read("outside/anything"); err == nil {
		t.Fatal("symlink traversed")
	}
	secret := "artifact-private-token"
	if err := os.WriteFile(filepath.Join(s.workspace, "result"), []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	id, _ := identity(testKey)
	s.secrets[id] = [][]byte{[]byte(secret)}
	if _, err := s.Read("result"); err == nil {
		t.Fatal("secret artifact accepted")
	}
	if err := s.save(id, &receipt{Key: testKey, State: "EXITED", HasSecrets: true}); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(s.control, s.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.workspace, "clean"), []byte("otherwise safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Read("clean"); err == nil {
		t.Fatal("lost inspection context did not fail closed")
	}
}

func TestHTTPAuthenticationAndValidation(t *testing.T) {
	s := newTestSupervisor(t)
	token := strings.Repeat("a", 32)
	encoded, _ := json.Marshal(map[string]any{"Key": testKey})
	for _, authenticated := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodPost, "/process", bytes.NewReader(encoded))
		if authenticated {
			r.Header.Set("X-Makez-Control-Token", token)
		}
		w := httptest.NewRecorder()
		s.Handler(token).ServeHTTP(w, r)
		if !authenticated && w.Code != 403 {
			t.Fatal("unauthenticated access accepted")
		}
		if authenticated && !strings.Contains(w.Body.String(), "NOT_STARTED") {
			t.Fatalf("protocol response: %s", w.Body.String())
		}
	}
	r := launchRequest{StartRequest: core.StartRequest{Key: testKey, Command: core.CommandSpec{Argv: []string{"true"}}, SecretRefs: map[string]string{"TOKEN": "auth@1"}}}
	if err := validateLaunch(r); err == nil {
		t.Fatal("unresolved secret accepted")
	}
	r.SecretValues = map[string]string{"TOKEN": "private-value"}
	before := digest(r.StartRequest)
	r.SecretValues["TOKEN"] = "rotated-private-value"
	if digest(r.StartRequest) != before {
		t.Fatal("ephemeral credential affected hash")
	}
	r.Command.Env = map[string]string{"TOKEN": "collision"}
	if err := validateLaunch(r); err == nil {
		t.Fatal("environment collision accepted")
	}
}

func TestOutputCursorAndReplay(t *testing.T) {
	s := newTestSupervisor(t)
	id, _ := identity(testKey)
	if err := s.save(id, &receipt{Key: testKey, State: "EXITED"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(s.control, id+".log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.appendOutput(f, "stdout", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Error(err)
	}
	page, err := s.Output(testKey, "", 16384)
	if err != nil || !page.EOF || len(page.Records) != 1 || string(page.Records[0].Data) != "hello\n" {
		t.Fatalf("output: %#v %v", page, err)
	}
	if _, err = s.Output(testKey, "1", 16384); err == nil {
		t.Fatal("non-boundary cursor accepted")
	}
	if _, err = s.Output(testKey, "999999", 16384); err == nil {
		t.Fatal("cursor beyond spool accepted")
	}
	s.outputGap(id)
	if _, err = s.Output(testKey, "", 16384); err == nil {
		t.Fatal("incomplete output accepted")
	} else if fault, ok := errors.AsType[*fault](err); !ok || fault.Code != core.OutputGap {
		t.Fatal(err)
	}
}
