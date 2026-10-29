package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v5"
	"github.com/makez-dev/makez/internal/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// allowBrowser accepts only exact configured origins with credentialed CORS.
func (s *Server) allowBrowser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		origin := c.Request().Header.Get("Origin")
		h := c.Response().Header()
		h.Add("Vary", "Origin")
		if origin != "" {
			if !s.originAllowed(origin) {
				return echo.NewHTTPError(403, "origin rejected")
			}
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, Last-Event-ID, X-Makez-Workspace-ID, X-Makez-CSRF")
			h.Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Type, X-Request-ID")
		}
		if c.Request().Method == http.MethodOptions {
			return c.NoContent(http.StatusNoContent)
		}
		return next(c)
	}
}

func (s *Server) authenticate(r *http.Request) (store.Principal, string, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" || len(token) > 4096 {
		return store.Principal{}, "", store.ErrUnauthenticated
	}
	if strings.HasPrefix(token, "mza_") {
		p, err := s.DB.AuthenticateAccess(r.Context(), token)
		return p, "", err
	}
	if strings.HasPrefix(token, "mzr_") {
		return store.Principal{}, "", store.ErrUnauthenticated
	}
	// The configured legacy namespace remains distinct from human access tokens.
	if _, ok := s.Keys[token]; !ok {
		return store.Principal{}, "", store.ErrUnauthenticated
	}
	return s.DB.AuthenticateMachine(r.Context(), token)
}
func (s *Server) authorize(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		r := c.Request()
		if !strings.HasPrefix(r.URL.Path, "/v1/") || publicAuth(r.Method, r.URL.Path) {
			return next(c)
		}
		p, bound, err := s.authenticate(r)
		if err != nil {
			return err
		}
		ctx := store.WithPrincipal(r.Context(), p)
		if unscopedAuth(r.URL.Path) {
			c.SetRequest(r.WithContext(ctx))
			return next(c)
		}
		ws := r.Header.Get("X-Makez-Workspace-ID")
		if route := c.Param("workspaceID"); route != "" {
			if ws != "" && ws != route {
				return store.ErrForbidden
			}
			ws = route
		}
		if bound != "" {
			if ws != "" && ws != bound {
				return store.ErrForbidden
			}
			ws = bound
		}
		if ws == "" || len(ws) > 128 {
			return echo.NewHTTPError(400, "workspace selection required")
		}
		if err = s.DB.Authorize(ctx, p, ws, actionFor(r)); err != nil {
			return err
		}
		c.SetRequest(r.WithContext(context.WithValue(ctx, workspaceKey{}, ws)))
		return next(c)
	}
}

func requestID(value string) string {
	if len(value) > 0 && len(value) <= 64 {
		valid := true
		for _, r := range value {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
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
		c.SetRequest(c.Request().WithContext(store.WithRequestID(c.Request().Context(), h.Get("X-Request-ID"))))
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
				s.Metrics.HTTP.Record(c.Request().Context(), time.Since(start).Seconds(), metric.WithAttributes(attribute.String("method", method), attribute.String("route", route), attribute.String("status", strconv.Itoa(status))))
			}
			slog.InfoContext(c.Request().Context(), "HTTP request", "request_id", h.Get("X-Request-ID"), "method", method, "route", route, "status", status, "duration", time.Since(start))
		}()
		return next(c)
	}
}

func (s *Server) handleError(c *echo.Context, err error) {
	status, msg := 500, "internal error"
	switch {
	case errors.Is(err, store.ErrUnauthenticated):
		status, msg = 401, "authentication required"
	case errors.Is(err, store.ErrForbidden):
		status, msg = 403, "permission denied"
	case errors.Is(err, store.ErrInvalidInput):
		status, msg = 400, "invalid task input or deadline"
	case errors.Is(err, store.ErrNotFound):
		status, msg = 404, "not found"
	case errors.Is(err, store.ErrConflict):
		status, msg = 409, "request conflicts with existing task"
	case errors.Is(err, context.DeadlineExceeded), pgconn.SafeToRetry(err):
		status, msg = 503, "service temporarily unavailable"
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
		http.Error(c.Response(), "404 page not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, echo.ErrMethodNotAllowed) {
		http.Error(c.Response(), "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	code := map[int]string{400: "invalid_request", 401: "authentication_required", 403: "permission_denied", 404: "not_found", 409: "conflict", 503: "service_unavailable"}[status]
	if code == "" {
		code = "internal_error"
	}
	if err := c.JSON(status, ErrorResponse{Error: msg, Code: code, RequestID: c.Response().Header().Get("X-Request-ID")}); err != nil {
		slog.Warn("error response write failed", "request_id", c.Response().Header().Get("X-Request-ID"))
	}
}
