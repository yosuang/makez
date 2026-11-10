package agent

import (
	"context"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

type memWorkspace struct{ files map[string]string }

func (m memWorkspace) Read(_ context.Context, path string) (io.ReadCloser, error) {
	body, ok := m.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func TestNonZeroExitKeepsAFailureManifest(t *testing.T) {
	adapter, err := NewPi(PiConfig{Binary: "/usr/local/bin/pi"})
	if err != nil {
		t.Fatal(err)
	}
	failure := memWorkspace{files: map[string]string{
		manifestPath: `{"succeeded":false,"summary":"Pi rejected the prompt before acceptance.","artifacts":[]}`,
	}}
	result, err := adapter.Collect(t.Context(), failure, core.ProcessExit{Code: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded || result.Summary != "Pi rejected the prompt before acceptance." || len(result.Artifacts) != 0 {
		t.Fatalf("result = %+v", result)
	}
	claimed := memWorkspace{files: map[string]string{
		manifestPath: `{"succeeded":true,"summary":"must not publish","artifacts":[]}`,
	}}
	result, err = adapter.Collect(t.Context(), claimed, core.ProcessExit{Code: 1})
	if err == nil || result.Succeeded || !strings.Contains(err.Error(), "agent exited unsuccessfully") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	_, err = adapter.Collect(t.Context(), memWorkspace{}, core.ProcessExit{Code: 1})
	if err == nil || !strings.Contains(err.Error(), "agent exited unsuccessfully") {
		t.Fatal(err)
	}
	success := memWorkspace{files: map[string]string{
		manifestPath: `{"succeeded":true,"summary":"done","artifacts":[]}`,
	}}
	result, err = adapter.Collect(t.Context(), success, core.ProcessExit{})
	if err != nil || !result.Succeeded || len(result.Artifacts) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
