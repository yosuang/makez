package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/makez-dev/makez/internal/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// events streams persisted log events and task status.
// @Summary Stream task events
// @Description log events carry a monotonic id and EventResponse JSON. status events carry TaskResponse JSON without an id. Last-Event-ID takes precedence over after. Terminal streams close only after all stored events are drained. Reconnect after disconnection with the last received id.
// @Tags tasks
// @Produce text/event-stream
// @Security BearerAuth
// @Param id path string true "Task ID"
// @Param Last-Event-ID header integer false "Last received log event ID" minimum(0)
// @Param after query integer false "Fallback event cursor" minimum(0)
// @Success 200 {string} string "SSE frames: id, event (log/status), data (JSON)"
// @Failure 400,401,404,500 {object} ErrorResponse
// @Router /tasks/{id}/events [get]
func (s *Server) events(c *echo.Context) error {
	r, w := c.Request(), c.Response()
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	var after int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 {
			return echo.NewHTTPError(400, "invalid event cursor")
		}
		after = n
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if s.Shutdown != nil {
		stop := context.AfterFunc(s.Shutdown, cancel)
		defer stop()
	}
	_, err := s.DB.Get(ctx, workspace(r), c.Param("id"))
	if err != nil {
		return err
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		return c.NoContent(200)
	}
	timeout := s.StreamWriteTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	reason, err := streamEvents(ctx, w, after, timeout, func(ctx context.Context, after int64) (store.Task, []store.Event, error) {
		// Fetch status first. A terminal snapshot cannot precede its final committed events.
		task, err := s.DB.Get(ctx, workspace(r), c.Param("id"))
		if err != nil {
			return task, nil, err
		}
		events, err := s.DB.Events(ctx, workspace(r), c.Param("id"), after)
		return task, events, err
	}, s)
	if s.Shutdown != nil && s.Shutdown.Err() != nil {
		reason = "shutdown"
	}
	if s.Metrics != nil {
		s.Metrics.StreamEnds.Add(r.Context(), 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
	slog.InfoContext(r.Context(), "SSE closed", "request_id", w.Header().Get("X-Request-ID"), "task_id", c.Param("id"), "workspace_id", workspace(r), "reason", reason)
	return err
}

// streamEvents owns only the stream lifetime; the fetch closure keeps SQL and
// authorization in the caller and lets transport tests use a real TCP connection.
func streamEvents(ctx context.Context, w http.ResponseWriter, after int64, timeout time.Duration, fetch func(context.Context, int64) (store.Task, []store.Event, error), s *Server) (string, error) {
	if s.Metrics != nil {
		s.Metrics.Streams.Add(ctx, 1)
		defer s.Metrics.Streams.Add(ctx, -1)
	}
	controller := http.NewResponseController(w)
	// Cancellation must interrupt a blocked socket write, not just the polling loop.
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = controller.SetWriteDeadline(time.Now()); close(interrupted) })
	defer func() {
		if !stop() {
			<-interrupted
		}
		_ = controller.SetWriteDeadline(time.Time{})
	}()
	deadline := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := controller.SetWriteDeadline(time.Now().Add(timeout))
		if ctx.Err() != nil {
			_ = controller.SetWriteDeadline(time.Now())
			return ctx.Err()
		}
		if errors.Is(err, http.ErrNotSupported) {
			return nil
		}
		return err
	}
	if err := deadline(); err != nil {
		return streamWriteError(ctx, err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := controller.Flush(); err != nil {
		return streamWriteError(ctx, err)
	}
	for {
		if ctx.Err() != nil {
			return "canceled", nil
		}
		task, events, err := fetch(ctx, after)
		if err != nil {
			if ctx.Err() != nil {
				return "canceled", nil
			}
			return "read_error", err
		}
		if err := deadline(); err != nil {
			return streamWriteError(ctx, err)
		}
		for _, event := range events {
			b, err := json.Marshal(EventResponse(event))
			if err != nil {
				return "encode_error", err
			}
			if _, err = fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", event.Seq, b); err != nil {
				return streamWriteError(ctx, err)
			}
			after = event.Seq
		}
		b, err := json.Marshal(taskResponseDTO(task))
		if err != nil {
			return "encode_error", err
		}
		if _, err = fmt.Fprintf(w, "event: status\ndata: %s\n\n", b); err != nil {
			return streamWriteError(ctx, err)
		}
		if err = controller.Flush(); err != nil {
			return streamWriteError(ctx, err)
		}
		if store.IsTerminal(task.State) && len(events) < store.EventPageSize {
			return "terminal", nil
		}
		if len(events) == store.EventPageSize {
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "canceled", nil
		case <-timer.C:
		}
	}
}
func streamWriteError(ctx context.Context, err error) (string, error) {
	if ctx.Err() != nil {
		return "canceled", nil
	}
	if e, ok := errors.AsType[net.Error](err); ok && e.Timeout() {
		return "write_timeout", err
	}
	return "write_error", err
}
