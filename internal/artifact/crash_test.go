package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

const crashContent = "durable crash fixture"

type crashReader struct{ sent bool }

func (r *crashReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "partial"), nil
	}
	os.Exit(91)
	return 0, io.EOF
}

type crashWorkspace struct{}

func (crashWorkspace) Read(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(&crashReader{}), nil
}

// The child exits without Go cleanup, modeling process loss at the file boundary.
func TestArtifactCrashHelper(t *testing.T) {
	phase := os.Getenv("MAKEZ_ARTIFACT_CRASH_PHASE")
	if phase == "" {
		t.Skip("subprocess-only crash fixture")
	}
	files, err := New(os.Getenv("MAKEZ_ARTIFACT_CRASH_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	var reader core.WorkspaceReader = memoryWorkspace{"/workspace/result": crashContent}
	if phase == "before" {
		reader = crashWorkspace{}
	}
	if _, err = files.Archive(t.Context(), "run", reader, []core.ArtifactCandidate{{Path: "result", Required: true}}); err != nil {
		t.Fatal(err)
	}
	os.Exit(91)
}

func TestProcessCrashBeforeAndAfterArtifactPublication(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(t.Context(), binary, "-test.run=^TestArtifactCrashHelper$")
			child.Env = append(os.Environ(), "MAKEZ_ARTIFACT_CRASH_PHASE="+phase, "MAKEZ_ARTIFACT_CRASH_DIR="+dir)
			output, err := child.CombinedOutput()
			exit, ok := errors.AsType[*exec.ExitError](err)
			if !ok || exit.ExitCode() != 91 {
				t.Fatalf("child did not reach crash boundary: %v %s", err, output)
			}
			archive, err := New(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close() //nolint:errcheck // Test archive handle.
			digest := sha256.Sum256([]byte(crashContent))
			key := "run/" + hex.EncodeToString(digest[:])
			published := 0
			if err = archive.WalkPublished(t.Context(), func(string) error { published++; return nil }); err != nil {
				t.Fatal(err)
			}
			if phase == "before" {
				if published != 0 {
					t.Fatal("partial object became available")
				}
				if f, err := archive.Open(key); err == nil {
					_ = f.Close()
					t.Fatal("partial object downloadable")
				}
				entries, err := os.ReadDir(filepath.Join(dir, "run"))
				if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".tmp-") {
					t.Fatal("crash fixture did not leave its private temporary file", err)
				}
			} else if published != 1 {
				t.Fatal("published file lost across process exit")
			}
			if _, err = archive.Archive(t.Context(), "run", memoryWorkspace{"/workspace/result": crashContent}, []core.ArtifactCandidate{{Path: "result", Required: true}}); err != nil {
				t.Fatal("safe publication retry", err)
			}
			f, err := archive.Open(key)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil || string(body) != crashContent {
				t.Fatal("retry returned incorrect bytes", err)
			}
		})
	}
}
