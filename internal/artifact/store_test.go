package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/makez-dev/makez/internal/core"
)

type memoryWorkspace map[string]string

func (w memoryWorkspace) Read(_ context.Context, name string) (io.ReadCloser, error) {
	data, ok := w[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

func TestArchiveIsDurableAndRetrySafe(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	input := memoryWorkspace{"/workspace/report.md": "hello 世界\n"}
	candidates := []core.ArtifactCandidate{{Path: "report.md", MediaType: "text/markdown", Required: true}, {Path: "optional.txt"}}
	first, err := store.Archive(context.Background(), "run-1", input, candidates)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Archive(context.Background(), "run-1", input, candidates)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("non-idempotent archive: %v", err)
	}
	digest := sha256.Sum256([]byte(input["/workspace/report.md"]))
	if len(first) != 1 || first[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("invalid records: %#v", first)
	}
	delete(input, "/workspace/report.md")
	f, err := store.Open(first[0].StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	if err := f.Close(); err != nil {
		t.Error(err)
	}
	if err != nil || string(data) != "hello 世界\n" {
		t.Fatalf("stored content missing: %v", err)
	}
	files, err := os.ReadDir(filepath.Join(dir, "run-1"))
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected temp files: %v, %v", files, err)
	}
}

func TestRejectsUnsafePathsAndSymlinkEscape(t *testing.T) {
	for _, name := range []string{"", "../secret", "/etc/passwd", "/workspace/../secret", "a/../../b", "a//b", "a\\b", "./x", "/workspace/"} {
		if _, err := ValidatePath(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	_, err = store.Archive(context.Background(), "escape", memoryWorkspace{"/workspace/a": "data"}, []core.ArtifactCandidate{{Path: "a", Required: true}})
	if err == nil {
		t.Fatal("followed escaping symlink")
	}
	if _, err := store.Open("../secret"); err == nil {
		t.Fatal("accepted unsafe storage key")
	}
}

func TestArchiveFailureDoesNotPublishPartialFile(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, err = store.Archive(context.Background(), "run-1", memoryWorkspace{}, []core.ArtifactCandidate{{Path: "missing", Required: true}})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected missing required artifact: %v", err)
	}
	_, err = store.Archive(context.Background(), "run-1", memoryWorkspace{"/workspace/large": strings.Repeat("x", MaxFileBytes+1)}, []core.ArtifactCandidate{{Path: "large", Required: true}})
	if err == nil {
		t.Fatal("accepted oversized artifact")
	}
	files, err := os.ReadDir(filepath.Join(dir, "run-1"))
	if err != nil || len(files) != 0 {
		t.Fatalf("partial files leaked: %v, %v", files, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.Archive(ctx, "run-1", memoryWorkspace{"/workspace/a": "data"}, []core.ArtifactCandidate{{Path: "a"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
}
