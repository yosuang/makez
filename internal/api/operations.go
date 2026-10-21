package api

import (
	"context"
	"time"

	"github.com/labstack/echo/v5"
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
