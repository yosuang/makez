// Package api exposes workspace-scoped task and artifact operations over HTTP.
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

// Server holds API dependencies and access settings.
//
// @title Makez API
// @version 1.0
// @description Workspace-scoped task execution. SSE emits log events with resumable IDs and status snapshots; it closes after terminal events are drained.
// @BasePath /v1
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Enter Bearer followed by a Makez access token or configured machine key. Human business requests also require X-Makez-Workspace-ID.
type Server struct {
	DB             *store.Store
	Google         *GoogleAuth
	AllowedOrigins []string
	SessionPolicy  store.SessionPolicy
	Artifacts      *artifact.Store
	Keys           map[string]string
	Agents         map[string]core.AgentAdapter
	Secrets        core.SecretResolver
	Metrics        *telemetry.Metrics
	Development    bool
	OperationsOnly bool
	Ready          func(context.Context) error
	// Shutdown cancels stream contexts before http.Server.Shutdown drains them.
	Shutdown           context.Context
	StreamWriteTimeout time.Duration
}
type workspaceKey struct{}

// Handler constructs the API and operations routes with shared middleware.
func (s *Server) Handler() http.Handler {
	e := echo.NewWithConfig(echo.Config{Logger: slog.Default(), Router: echo.NewRouter(echo.RouterConfig{AutoHandleHEAD: true, OptionsMethodHandler: func(_ *echo.Context) error { return echo.ErrMethodNotAllowed }, UseEscapedPathForMatching: true, UnescapePathParamValues: true})})
	e.HTTPErrorHandler = s.handleError
	e.Use(s.observe, s.allowBrowser, s.authorize)
	e.GET("/healthz", func(c *echo.Context) error { return c.String(200, "ok") })
	e.GET("/readyz", s.ready)
	s.registerDocs(e)
	if !s.OperationsOnly {
		e.GET("/v1/auth/google/start", s.googleStart)
		e.GET("/v1/auth/google/callback", s.googleCallback)
		e.POST("/v1/auth/refresh", s.refresh)
		e.POST("/v1/auth/logout", s.logout)
		e.POST("/v1/auth/logout-all", s.logoutAll)
		e.GET("/v1/me", s.me)
		e.GET("/v1/workspaces", s.workspaces)
		e.GET("/v1/workspaces/:workspaceID/members", s.listMembers)
		e.PATCH("/v1/workspaces/:workspaceID/members/:userID", s.updateMember)
		e.DELETE("/v1/workspaces/:workspaceID/members/:userID", s.removeMember)
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
