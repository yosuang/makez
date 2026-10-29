// Command makez runs the API, workflow worker, or both.
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
	"github.com/makez-dev/makez/internal/telemetry"
	orchestration "github.com/makez-dev/makez/internal/workflow"
	"go.temporal.io/sdk/client"
	temporallog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func main() {
	slog.SetDefault(telemetry.JSONLogger(os.Stderr))
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
	if mode == "revoke-machine" || mode == "repair-bootstrap" || mode == "replay-outbox" || mode == "verify-artifacts" {
		return maintenance(mode, os.Args[2:])
	}
	if mode == "migrate" {
		if os.Getenv("MAKEZ_DATABASE_URL") == "" {
			return errors.New("MAKEZ_DATABASE_URL is required for migration")
		}
		adopt := len(os.Args) == 3 && os.Args[2] == "--adopt-legacy"
		if len(os.Args) > 2 && !adopt {
			return errors.New("usage: makez migrate [--adopt-legacy]")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		db, err := store.Open(ctx, os.Getenv("MAKEZ_DATABASE_URL"))
		if err != nil {
			return err
		}
		defer db.Close()
		return db.MigrateLegacy(ctx, adopt)
	}
	if mode != "all" && mode != "api" && mode != "worker" {
		return fmt.Errorf("usage: makez [all|api|worker]")
	}
	config, err := loadSettings()
	if err != nil {
		return err
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
	{
		listenSetting := "MAKEZ_LISTEN"
		address := env(listenSetting, "127.0.0.1:8088")
		if mode == "worker" {
			listenSetting = "MAKEZ_OPERATIONS_LISTEN"
			address = env(listenSetting, "127.0.0.1:8089")
		}
		listener, err = net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("cannot listen on %s; stop the existing process or set %s: %w", address, listenSetting, err)
		}
		defer listener.Close() //nolint:errcheck // Serve or Shutdown may already have closed this listener.
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.OpenWithLimit(ctx, env("MAKEZ_DATABASE_URL", "postgres://makez:makez-local@127.0.0.1:55432/makez?sslmode=disable"), int32(config.DatabaseConnections))
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.CheckSchema(ctx); err != nil {
		return err
	}
	provider, shutdownMetrics, err := configureMetrics(ctx)
	if err != nil {
		return err
	}
	// Registered before worker/dispatcher cleanup, but after database cleanup:
	// stop producers first, then flush metrics while the database is still open.
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownMetrics(flushCtx); err != nil {
			slog.Warn("metrics shutdown failed", "error", err)
		}
	}()
	metrics, err := telemetry.New(provider.Meter("github.com/makez-dev/makez"))
	if err != nil {
		return err
	}
	db.Metrics = metrics
	if err := metrics.RegisterBacklog(db.ReadBacklog); err != nil {
		return err
	}
	files, err := artifact.New(env("MAKEZ_ARTIFACT_DIR", ".local/artifacts"))
	if err != nil {
		return err
	}
	defer files.Close() //nolint:errcheck // The artifact root is a directory handle; file writes are closed separately.
	tc, err := client.NewLazyClient(client.Options{Logger: temporallog.NewStructuredLogger(slog.Default()), HostPort: env("MAKEZ_TEMPORAL_ADDRESS", "127.0.0.1:17233"), Namespace: env("MAKEZ_TEMPORAL_NAMESPACE", "default")})
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
		w := worker.New(tc, orchestration.Queue, worker.Options{WorkerStopTimeout: 10 * time.Second, MaxConcurrentActivityExecutionSize: config.ActivityConcurrency, MaxConcurrentWorkflowTaskExecutionSize: config.WorkflowConcurrency})
		w.RegisterWorkflow(orchestration.Task)
		w.RegisterWorkflow(orchestration.Cleanup)
		w.RegisterWorkflow(orchestration.Recovery)
		w.RegisterActivity(acts)
		if err = w.Start(); err != nil {
			return err
		}
		defer w.Stop()
	}
	google, origins, policy, err := configureAuth(ctx, config.Development)
	if err != nil {
		return err
	}
	var keys map[string]string
	if mode != "worker" {
		if raw := os.Getenv("MAKEZ_API_KEYS"); raw != "" {
			if err = json.Unmarshal([]byte(raw), &keys); err != nil {
				return errors.New("MAKEZ_API_KEYS must be a JSON object mapping API key to workspace ID")
			}
		}
		if len(keys) == 0 && google == nil {
			return errors.New("configure Google login or machine API keys")
		}
		for k, ws := range keys {
			if len(k) < 16 || ws == "" {
				return errors.New("API keys must contain at least 16 characters and a workspace ID")
			}
		}
		if err = db.RegisterMachines(ctx, keys); err != nil {
			return err
		}
		dispatchCtx, stopDispatch := context.WithCancel(ctx)
		dispatchDone := make(chan struct{})
		go func() { defer close(dispatchDone); orchestration.Dispatch(dispatchCtx, db, tc) }()
		defer func() { stopDispatch(); <-dispatchDone }()
	}
	handler := (&api.Server{DB: db, Google: google, AllowedOrigins: origins, SessionPolicy: policy, Artifacts: files, Keys: keys, Agents: agents, Secrets: secrets, Metrics: metrics, Development: config.Development, OperationsOnly: mode == "worker", Shutdown: ctx, Ready: func(check context.Context) error {
		if err := db.Ping(check); err != nil {
			return err
		}
		_, err := tc.CheckHealth(check, &client.CheckHealthRequest{})
		return err
	}}).Handler()
	srv := &http.Server{Addr: listener.Addr().String(), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	defer srv.Close() //nolint:errcheck // Fallback cleanup after the checked Shutdown path.
	done := make(chan error, 1)
	go func() {
		slog.Info("makez HTTP listening", "address", srv.Addr, "mode", mode)
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
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
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
		defer file.Close() //nolint:errcheck // The configuration file is read-only; read errors are handled below.
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
