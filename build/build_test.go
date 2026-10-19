package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModuleRootFindsGoMod(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/tmp\n\ngo 1.27.2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "nested")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	got, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err = filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("module root %s, want %s", got, want)
	}
}

func TestDefaultBinaryUsesTargetOS(t *testing.T) {
	t.Setenv("GOOS", "linux")
	if got := defaultBinary(); got != filepath.Join("bin", "makez") {
		t.Fatalf("linux binary %s", got)
	}
	t.Setenv("GOOS", "windows")
	if got := defaultBinary(); got != filepath.Join("bin", "makez.exe") {
		t.Fatalf("windows binary %s", got)
	}
}

func TestUnknownTask(t *testing.T) {
	if err := runTask("web-dev"); err == nil {
		t.Fatal("expected unknown task")
	}
}

func TestCleanRemovesBuildOutputsOnly(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	writeFile(t, filepath.Join(".local", "env"), "secret")
	writeFile(t, filepath.Join(".local", "artifacts", "report.md"), "kept")
	writeFile(t, filepath.Join(".local", "makez"), "old-binary")
	writeFile(t, filepath.Join("bin", "makez"), "binary")
	writeFile(t, "makez", "root-binary")
	writeFile(t, filepath.Join("dist", "makez.tar.gz"), "archive")
	writeFile(t, filepath.Join("apps", "web", "dist", "index.html"), "web")
	writeFile(t, filepath.Join("apps", "web", "package.json"), "{}")

	if err := clean(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(".local", "env"),
		filepath.Join(".local", "artifacts", "report.md"),
		filepath.Join("apps", "web", "package.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("clean removed %s: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(".local", "makez"),
		"bin",
		"makez",
		"dist",
		filepath.Join("apps", "web", "dist"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("clean left %s: %v", path, err)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
