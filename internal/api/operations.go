package api

import (
	"context"
	"crypto/subtle"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func (s *Server) ready(c *echo.Context) error {
	if s.Shutdown != nil && s.Shutdown.Err() != nil {
		return echo.NewHTTPError(503, "not ready")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
	defer cancel()
	if s.Ready == nil || s.Ready(ctx) != nil || ctx.Err() != nil || (s.Shutdown != nil && s.Shutdown.Err() != nil) {
		return echo.NewHTTPError(503, "not ready")
	}
	return c.String(200, "ok")
}
func (s *Server) metrics(c *echo.Context) error {
	if s.OperationsKey == "" || s.Metrics == nil {
		return echo.ErrNotFound
	}
	token, ok := strings.CutPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.OperationsKey)) != 1 {
		return echo.NewHTTPError(401, "operations key required")
	}
	if s.DB != nil {
		ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		defer cancel()
		if err := s.DB.RefreshMetrics(ctx); err != nil {
			return echo.NewHTTPError(503, "metrics unavailable")
		}
	}
	promhttp.HandlerFor(s.Metrics.Registry, promhttp.HandlerOpts{}).ServeHTTP(c.Response(), c.Request())
	return nil
}
