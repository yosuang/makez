// Command codex-runner executes the Codex CLI using the Makez file contract.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/makez-dev/makez/internal/agent/codexrunner"
)

func main() {
	c := codexrunner.Config{Workspace: "/workspace", Home: "/home/agent", Executable: "/usr/local/bin/codex"}
	flag.StringVar(&c.AuthMode, "auth-mode", "", "registered authentication mode")
	flag.StringVar(&c.Model, "model", "", "model override")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := codexrunner.Run(ctx, c, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
