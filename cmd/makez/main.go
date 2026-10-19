package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/api"
	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/sandbox"
	"github.com/makez-dev/makez/internal/secret"
	"github.com/makez-dev/makez/internal/store"
	orchestration "github.com/makez-dev/makez/internal/workflow"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func main() {
	if err := run(); err != nil {
		slog.Error("makez stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	mode := "all"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode != "all" && mode != "api" && mode != "worker" {
		return fmt.Errorf("usage: makez [all|api|worker]")
	}
	agents, err := loadAgents()
	if err != nil {
		return err
	}
	secrets, err := loadSecrets(os.Getenv("MAKEZ_SECRETS_FILE"))
	if err != nil {
		return err
	}
	var listener net.Listener
	if mode != "worker" {
		address := env("MAKEZ_LISTEN", "127.0.0.1:8088")
		listener, err = net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("cannot listen on %s; stop the existing process or set MAKEZ_LISTEN: %w", address, err)
		}
		defer listener.Close()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, env("MAKEZ_DATABASE_URL", "postgres://makez:makez-local@127.0.0.1:55432/makez?sslmode=disable"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		return err
	}
	files, err := artifact.New(env("MAKEZ_ARTIFACT_DIR", ".local/artifacts"))
	if err != nil {
		return err
	}
	defer files.Close()
	tc, err := client.NewLazyClient(client.Options{HostPort: env("MAKEZ_TEMPORAL_ADDRESS", "127.0.0.1:17233"), Namespace: env("MAKEZ_TEMPORAL_NAMESPACE", "default")})
	if err != nil {
		return err
	}
	defer tc.Close()
	if mode != "api" {
		p, e := sandbox.New(sandbox.Config{BaseURL: env("MAKEZ_SANDBOX_URL", "http://127.0.0.1:18080"), APIKey: os.Getenv("MAKEZ_SANDBOX_API_KEY"), InstallationID: env("MAKEZ_INSTALLATION_ID", "makez-local"), ControlKey: os.Getenv("MAKEZ_CONTROL_KEY"), Image: env("MAKEZ_RUNTIME_IMAGE", "makez-runtime:0.1")})
		if e != nil {
			return e
		}
		acts := &orchestration.Activities{DB: db, Provider: p, Agents: agents, Artifacts: files, Secrets: secrets}
		w := worker.New(tc, orchestration.Queue, worker.Options{WorkerStopTimeout: 10 * time.Second})
		w.RegisterWorkflow(orchestration.Task)
		w.RegisterWorkflow(orchestration.Cleanup)
		w.RegisterWorkflow(orchestration.Recovery)
		w.RegisterActivity(acts)
		if err = w.Start(); err != nil {
			return err
		}
		defer w.Stop()
	}
	if mode == "worker" {
		<-ctx.Done()
		return nil
	}
	var keys map[string]string
	if err = json.Unmarshal([]byte(os.Getenv("MAKEZ_API_KEYS")), &keys); err != nil || len(keys) == 0 {
		return errors.New("MAKEZ_API_KEYS must be a JSON object mapping API key to workspace ID")
	}
	for k, ws := range keys {
		if len(k) < 16 || ws == "" {
			return errors.New("API keys must contain at least 16 characters and a workspace ID")
		}
	}
	go orchestration.Dispatch(ctx, db, tc)
	handler := (&api.Server{DB: db, Artifacts: files, Keys: keys, Agents: agents, Secrets: secrets}).Handler()
	srv := &http.Server{Addr: listener.Addr().String(), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() {
		slog.Info("makez API ready", "address", srv.Addr, "mode", mode)
		done <- srv.Serve(listener)
	}()
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

const maxHostConfigBytes = 64 << 10

func decodeConfig(data []byte, target any) error {
	if len(data) > maxHostConfigBytes || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return errors.New("invalid configuration")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return errors.New("invalid configuration")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid configuration")
	}
	return nil
}

func loadAgents() (map[string]core.AgentAdapter, error) {
	agents := map[string]core.AgentAdapter{"demo@1": agent.NewDemo()}
	if raw := os.Getenv("MAKEZ_CLI_AGENT"); raw != "" {
		var config agent.Config
		if len(raw) > maxHostConfigBytes || json.Unmarshal([]byte(raw), &config) != nil {
			return nil, errors.New("invalid MAKEZ_CLI_AGENT configuration")
		}
		adapter, err := agent.NewCLI(config)
		if err != nil {
			return nil, errors.New("invalid MAKEZ_CLI_AGENT configuration")
		}
		identity := adapter.Identity()
		agents[identity.ID+"@"+identity.Version] = adapter
	}
	if raw := os.Getenv("MAKEZ_CODEX_AGENT"); raw != "" {
		var config agent.CodexConfig
		if decodeConfig([]byte(raw), &config) != nil {
			return nil, errors.New("invalid MAKEZ_CODEX_AGENT configuration")
		}
		adapter, err := agent.NewCodex(config)
		if err != nil {
			return nil, errors.New("invalid MAKEZ_CODEX_AGENT configuration")
		}
		identity := adapter.Identity()
		key := identity.ID + "@" + identity.Version
		if _, exists := agents[key]; exists {
			return nil, errors.New("duplicate agent registration")
		}
		agents[key] = adapter
	}
	return agents, nil
}

func loadSecrets(path string) (*secret.Resolver, error) {
	var registry secret.Registry
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return nil, errors.New("cannot load MAKEZ_SECRETS_FILE")
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxHostConfigBytes {
			return nil, errors.New("invalid MAKEZ_SECRETS_FILE configuration")
		}
		data, err := io.ReadAll(io.LimitReader(file, maxHostConfigBytes+1))
		if err != nil || decodeConfig(data, &registry) != nil {
			return nil, errors.New("invalid MAKEZ_SECRETS_FILE configuration")
		}
	}
	resolver, err := secret.New(registry)
	if err != nil {
		return nil, errors.New("invalid MAKEZ_SECRETS_FILE configuration")
	}
	return resolver, nil
}
