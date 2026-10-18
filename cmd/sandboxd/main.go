package main

import (
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/makez-dev/makez/internal/sandbox/supervisor"
)

func main() {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		slog.Error("sandboxd requires Linux root; the Agent runs as uid 1000")
		os.Exit(1)
	}
	token := os.Getenv("MAKEZ_CONTROL_TOKEN")
	os.Unsetenv("MAKEZ_CONTROL_TOKEN")
	if len(token) < 32 {
		slog.Error("a control token is required")
		os.Exit(1)
	}
	s, err := supervisor.New("/var/lib/makez", "/workspace")
	if err == nil {
		err = os.Chown("/workspace", 1000, 1000)
	}
	if err != nil {
		slog.Error("cannot initialize sandbox supervisor")
		os.Exit(1)
	}
	server := &http.Server{Addr: ":8099", Handler: s.Handler(token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	if server.ListenAndServe() != nil {
		slog.Error("sandbox supervisor stopped")
		os.Exit(1)
	}
}
