package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

// Run against the local stack: MAKEZ_SANDBOX_INTEGRATION=1 go test ./internal/sandbox -run TestOpenSandboxIntegration -v.
func TestOpenSandboxIntegration(t *testing.T) {
	if os.Getenv("MAKEZ_SANDBOX_INTEGRATION") != "1" {
		t.Skip("requires local OpenSandbox and runtime image")
	}
	base := cmp.Or(os.Getenv("OPEN_SANDBOX_URL"), "http://127.0.0.1:18080")
	apiKey := cmp.Or(os.Getenv("OPEN_SANDBOX_API_KEY"), "makez-local-sandbox-key")
	p, err := New(Config{BaseURL: base, APIKey: apiKey, InstallationID: "makez-local", ControlKey: "makez-local-control-key-at-least-32-bytes", Image: "makez-runtime:0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	sandboxKey := core.OperationKey{WorkspaceID: "integration", TaskID: fmt.Sprintf("test-%d", time.Now().UnixNano()), RunID: "one", Step: "sandbox"}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.Delete(ctx, sandboxKey); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	ref, err := p.Ensure(ctx, core.SandboxRequest{Key: sandboxKey, RuntimeProfileID: "default", RuntimeProfileVersion: "1", ExpiresAt: time.Now().Add(5 * time.Minute), Limits: core.ResourceLimits{MilliCPU: 500, MemoryBytes: 256 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("sandbox created", ref.ID)
	source := `import os, pathlib, sys, time
print('hello from sandbox', flush=True)
token = os.environ['TEST_TOKEN']
sys.stdout.write(token[:8]); sys.stdout.flush(); time.sleep(0.1)
sys.stdout.write(token[8:] + '\n'); sys.stdout.flush()
print(token, file=sys.stderr, flush=True)
pathlib.Path('/workspace/result.txt').write_text('verified result')
pathlib.Path('/workspace/leaked.txt').write_text(token)
time.sleep(0.2)
`
	if err = p.WriteFile(ctx, ref, "/workspace/run.py", strings.NewReader(source), int64(len(source))); err != nil {
		t.Fatal(err)
	}

	key := sandboxKey
	key.Step = "agent"
	req := core.StartRequest{Key: key, Command: core.CommandSpec{Argv: []string{"/usr/local/bin/python3", "/workspace/run.py"}, Workdir: "/workspace"}, Deadline: time.Now().Add(time.Minute), SecretRefs: map[string]string{"TEST_TOKEN": "secret://integration/test-token/v1"}, SecretValues: map[string]string{"TEST_TOKEN": "integration-fake-secret-token"}}
	process, err := p.Start(ctx, ref, req)
	if err != nil {
		t.Fatal(err)
	}
	fresh := req
	fresh.SecretValues = map[string]string{"TEST_TOKEN": "refreshed-token-not-used"}
	duplicate, err := p.Start(ctx, ref, fresh)
	if err != nil || duplicate.ID != process.ID {
		t.Fatalf("duplicate start: %+v %v", duplicate, err)
	}
	for {
		obs, e := p.Process(ctx, ref, key)
		if e != nil {
			t.Fatal(e)
		}
		if obs.State == core.ProcessExited {
			if obs.Exit == nil || obs.Exit.Code != 0 {
				t.Fatalf("bad exit %+v", obs)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("did not exit")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err = p.Stop(ctx, ref, key, core.StopPolicy{GracePeriod: time.Second}); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	cursor := ""
	for {
		page, e := p.Output(ctx, process, cursor, 1<<16)
		if e != nil {
			t.Fatal(e)
		}
		for _, r := range page.Records {
			output.Write(r.Data)
		}
		cursor = page.NextCursor
		if page.EOF {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("output did not finish")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !strings.Contains(output.String(), "hello from sandbox\n") || strings.Contains(output.String(), req.SecretValues["TEST_TOKEN"]) || strings.Count(output.String(), "[REDACTED]") != 2 {
		t.Fatal("output was not correctly redacted")
	}
	reader, err := p.ReadFile(ctx, ref, "/workspace/result.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err := reader.Close(); err != nil {
		t.Error(err)
	}
	if err != nil || string(data) != "verified result" {
		t.Fatalf("artifact %q %v", data, err)
	}
	_, err = p.ReadFile(ctx, ref, "/workspace/leaked.txt")
	if pe, ok := errors.AsType[core.ProviderError](err); !ok || pe.Code() != core.PermissionDenied {
		t.Fatalf("secret artifact was not rejected: %v", err)
	}
	if _, err = p.Start(ctx, ref, req); err == nil {
		t.Fatal("closed launch key restarted")
	}
	t.Log("launch, attach, exit, stop, redacted replay and protected artifacts verified")
}
