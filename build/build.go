// Command build compiles the makez binary and removes local build outputs.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func main() {
	root, err := moduleRoot()
	if err != nil {
		exit(err)
	}
	if err = os.Chdir(root); err != nil {
		exit(err)
	}

	args := []string{os.Args[0]}
	for _, arg := range os.Args[1:] {
		if key, value, ok := strings.Cut(arg, "="); ok {
			if err = os.Setenv(key, value); err != nil {
				exit(err)
			}
			continue
		}
		args = append(args, arg)
	}
	if len(args) == 1 {
		args = append(args, defaultBinary())
	}
	for _, task := range args[1:] {
		if err = runTask(task); err != nil {
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprintf(os.Stderr, "build.go: building task `%s` failed.\n", task)
			os.Exit(1)
		}
	}
}

func exit(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		info, statErr := os.Stat(filepath.Join(dir, "go.mod"))
		if statErr == nil && !info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}

func defaultBinary() string {
	name := "makez"
	if windowsTarget() {
		name += ".exe"
	}
	return filepath.Join("bin", name)
}

func windowsTarget() bool {
	if goos := os.Getenv("GOOS"); goos != "" {
		return goos == "windows"
	}
	return runtime.GOOS == "windows"
}

func runTask(task string) error {
	switch normalizeTask(task) {
	case "bin/makez":
		return buildBinary(task)
	case "clean":
		return clean()
	default:
		return fmt.Errorf("don't know how to build task `%s`", task)
	}
}

func normalizeTask(task string) string {
	return filepath.ToSlash(strings.TrimSuffix(task, ".exe"))
}

func buildBinary(exe string) error {
	args := []string{"go", "build", "-trimpath"}
	if tags := os.Getenv("GO_BUILDTAGS"); tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "-o", exe, "./cmd/makez")
	return run(args...)
}

var cleanTargets = []string{
	"bin",
	"dist",
	"makez",
	filepath.Join(".local", "makez"),
	filepath.Join("apps", "web", "dist"),
}

func clean() error {
	printCommand(append([]string{"rm", "-rf"}, cleanTargets...))
	for _, target := range cleanTargets {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	return nil
}

func run(args ...string) error {
	exe, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	printCommand(args)
	cmd := exec.Command(exe, args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func printCommand(args []string) {
	fmt.Println(formatCommand(args))
}

func formatCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if strings.ContainsAny(arg, " \t'\"") {
			quoted[i] = strconv.Quote(arg)
		} else {
			quoted[i] = arg
		}
	}
	return strings.Join(quoted, " ")
}
