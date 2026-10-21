package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/makez-dev/makez/internal/store"
)

func (s *Server) authorize(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		r := c.Request()
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			return next(c)
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			return echo.NewHTTPError(401, "Bearer API key required")
		}
		for key, ws := range s.Keys {
			if subtle.ConstantTimeCompare([]byte(key), []byte(token)) == 1 {
				c.SetRequest(r.WithContext(context.WithValue(r.Context(), workspaceKey{}, ws)))
				return next(c)
			}
		}
		return echo.NewHTTPError(401, "invalid API key")
	}
}

func requestID(value string) string {
	if len(value) > 0 && len(value) <= 64 {
		valid := true
		for _, r := range value {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				valid = false
				break
			}
		}
		if valid {
			return value
		}
	}
	return rand.Text()
}

func (s *Server) observe(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) (err error) {
		start := time.Now()
		h := c.Response().Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Request-ID", requestID(c.Request().Header.Get("X-Request-ID")))
		defer func() {
			if recover() != nil {
				err = errors.New("handler panic")
			}
			if err != nil {
				s.handleError(c, err)
				err = nil
			}
			response, _ := echo.UnwrapResponse(c.Response())
			status := 200
			if response != nil && response.Status != 0 {
				status = response.Status
			}
			route := c.Path()
			if route == "" {
				route = "unmatched"
			}
			method := c.Request().Method
			switch method {
			case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
			default:
				method = "OTHER"
			}
			if s.Metrics != nil {
				s.Metrics.HTTP.WithLabelValues(method, route, strconv.Itoa(status)).Observe(time.Since(start).Seconds())
			}
			slog.InfoContext(c.Request().Context(), "HTTP request", "request_id", h.Get("X-Request-ID"), "method", method, "route", route, "status", status, "duration", time.Since(start))
		}()
		return next(c)
	}
}

func (s *Server) handleError(c *echo.Context, err error) {
	status, msg := 500, "internal error"
	switch {
	case errors.Is(err, store.ErrInvalidInput):
		status, msg = 400, "invalid task input or deadline"
	case errors.Is(err, store.ErrNotFound):
		status, msg = 404, "not found"
	case errors.Is(err, store.ErrConflict):
		status, msg = 409, "request conflicts with existing task"
	default:
		if he, ok := errors.AsType[*echo.HTTPError](err); ok {
			status, msg = he.Code, he.Message
		} else if code := echo.StatusCode(err); code != 0 {
			status, msg = code, http.StatusText(code)
		}
	}
	if status >= 500 {
		// Error values may contain SQL arguments, URLs or credentials. Log type and
		// request identity, never arbitrary error strings or request bodies.
		slog.ErrorContext(c.Request().Context(), "API request failed", "request_id", c.Response().Header().Get("X-Request-ID"), "route", c.Path(), "error_type", fmt.Sprintf("%T", err))
	}
	if response, _ := echo.UnwrapResponse(c.Response()); response != nil && response.Committed {
		return
	}
	if errors.Is(err, echo.ErrNotFound) {
		http.Error(c.Response(), "404 page not found", 404)
		return
	}
	if errors.Is(err, echo.ErrMethodNotAllowed) {
		http.Error(c.Response(), "Method Not Allowed", 405)
		return
	}
	if err := c.JSON(status, ErrorResponse{Error: msg}); err != nil {
		slog.Warn("error response write failed", "request_id", c.Response().Header().Get("X-Request-ID"))
	}
}
