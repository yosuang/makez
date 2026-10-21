package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
	"github.com/makez-dev/makez/internal/telemetry"
)

// @title Makez API
// @version 1.0
// @description Workspace-scoped task execution. SSE emits log events with resumable IDs and status snapshots; it closes after terminal events are drained.
// @BasePath /v1
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Enter Bearer followed by the workspace API key.
type Server struct {
	DB             *store.Store
	Artifacts      *artifact.Store
	Keys           map[string]string
	Agents         map[string]core.AgentAdapter
	Secrets        core.SecretResolver
	Metrics        *telemetry.Metrics
	OperationsKey  string
	Development    bool
	OperationsOnly bool
	Ready          func(context.Context) error
	// Shutdown cancels stream contexts before http.Server.Shutdown drains them.
	Shutdown           context.Context
	StreamWriteTimeout time.Duration
}
type workspaceKey struct{}

func (s *Server) Handler() http.Handler {
	e := echo.NewWithConfig(echo.Config{Logger: slog.Default(), Router: echo.NewRouter(echo.RouterConfig{AutoHandleHEAD: true, OptionsMethodHandler: func(c *echo.Context) error { return echo.ErrMethodNotAllowed }, UseEscapedPathForMatching: true, UnescapePathParamValues: true})})
	e.HTTPErrorHandler = s.handleError
	e.Use(s.observe, s.authorize)
	e.GET("/healthz", func(c *echo.Context) error { return c.String(200, "ok") })
	e.GET("/readyz", s.ready)
	e.GET("/metrics", s.metrics)
	s.registerDocs(e)
	if !s.OperationsOnly {
		e.GET("/v1/agents", s.agents)
		e.POST("/v1/tasks", s.submit)
		e.GET("/v1/tasks", s.list)
		e.GET("/v1/tasks/:id", s.get)
		e.POST("/v1/tasks/:id/cancel", s.cancel)
		e.GET("/v1/tasks/:id/events", s.events)
		e.GET("/v1/tasks/:id/artifacts", s.artifacts)
		e.GET("/v1/artifacts/:id/content", s.download)
	}
	return e
}
func workspace(r *http.Request) string { return r.Context().Value(workspaceKey{}).(string) }

// agents lists registered adapters.
// @Summary List agents
// @Tags agents
// @Produce json
// @Security BearerAuth
// @Success 200 {array} core.AdapterIdentity
// @Failure 401 {object} ErrorResponse
// @Router /agents [get]
func (s *Server) agents(c *echo.Context) error {
	var ids []core.AdapterIdentity
	for _, a := range s.Agents {
		ids = append(ids, a.Identity())
	}
	return c.JSON(200, ids)
}
