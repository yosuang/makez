package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
)

// submit accepts a task with a workspace-scoped idempotency key.
// @Summary Submit task
// @Tags tasks
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param Idempotency-Key header string true "1–128 bytes; reuse only with identical input"
// @Param task body SubmitRequest true "Task input; unknown fields and multiple JSON objects are rejected"
// @Success 202 {object} TaskResponse "Created"
// @Success 200 {object} TaskResponse "Idempotent replay"
// @Failure 400,401,403,409,500 {object} ErrorResponse
// @Router /tasks [post]
func (s *Server) submit(c *echo.Context) error {
	r := c.Request()
	w := c.Response()
	var in SubmitRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		return echo.NewHTTPError(400, "invalid request JSON")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return echo.NewHTTPError(400, "expected one JSON object")
	}
	if string(in.Config) == "null" {
		in.Config = json.RawMessage(`{}`)
	}
	idem := r.Header.Get("Idempotency-Key")
	if len(idem) == 0 || len(idem) > 128 {
		return echo.NewHTTPError(400, "Idempotency-Key (1–128 characters) is required")
	}
	if strings.TrimSpace(in.Prompt) == "" || len(in.Prompt) > 64<<10 {
		return echo.NewHTTPError(400, "prompt must be 1–65536 bytes")
	}
	if in.AgentID == "" {
		in.AgentID = "demo"
	}
	if in.AgentVersion == "" {
		in.AgentVersion = "1"
	}
	if in.ProfileID == "" {
		in.ProfileID = "default"
	}
	if in.ProfileVersion == "" {
		in.ProfileVersion = "1"
	}
	a, ok := s.Agents[in.AgentID+"@"+in.AgentVersion]
	if !ok {
		return echo.NewHTTPError(400, "unknown agent/version")
	}
	if in.ProfileID != "default" || in.ProfileVersion != "1" {
		return echo.NewHTTPError(400, "unknown runtime profile/version")
	}
	plan, err := a.Plan(core.AgentInput{Prompt: in.Prompt, Config: in.Config})
	if err != nil {
		return echo.NewHTTPError(400, "invalid agent configuration")
	}
	if len(plan.SecretRefs) > 0 && (s.Secrets == nil || s.Secrets.Check(workspace(r), plan.SecretRefs) != nil) {
		return echo.NewHTTPError(403, "agent authentication is not configured for this workspace")
	}
	if !in.Deadline.IsZero() && in.TimeoutSeconds != 0 {
		return echo.NewHTTPError(400, "use deadline or timeout_seconds, not both")
	}
	if in.Deadline.IsZero() {
		if in.TimeoutSeconds == 0 {
			in.TimeoutSeconds = 3600
		}
		if in.TimeoutSeconds < 5 || in.TimeoutSeconds > 5400 {
			return echo.NewHTTPError(400, "timeout_seconds must be 5–5400")
		}
	}
	t, created, e := s.DB.Submit(r.Context(), workspace(r), idem, store.SubmitInput(in))
	if e != nil {
		return e
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	return c.JSON(status, taskResponseDTO(t))
}

// list returns the latest 100 tasks.
// @Summary List tasks
// @Tags tasks
// @Produce json
// @Security BearerAuth
// @Success 200 {array} TaskResponse
// @Failure 401,500 {object} ErrorResponse
// @Router /tasks [get]
func (s *Server) list(c *echo.Context) error {
	r := c.Request()
	v, e := s.DB.List(r.Context(), workspace(r))
	if e != nil {
		return e
	}
	return c.JSON(200, taskResponses(v))
}

// get returns a workspace task.
// @Summary Get task
// @Tags tasks
// @Produce json
// @Security BearerAuth
// @Param id path string true "Task ID"
// @Success 200 {object} TaskResponse
// @Failure 401,404,500 {object} ErrorResponse
// @Router /tasks/{id} [get]
func (s *Server) get(c *echo.Context) error {
	r := c.Request()
	v, e := s.DB.Get(r.Context(), workspace(r), c.Param("id"))
	if e != nil {
		return e
	}
	return c.JSON(200, taskResponseDTO(v))
}

// cancel requests cancellation and returns the current state.
// @Summary Cancel task
// @Tags tasks
// @Produce json
// @Security BearerAuth
// @Param id path string true "Task ID"
// @Success 200 {object} TaskResponse
// @Failure 401,404,409,500 {object} ErrorResponse
// @Router /tasks/{id}/cancel [post]
func (s *Server) cancel(c *echo.Context) error {
	r := c.Request()
	v, e := s.DB.Cancel(r.Context(), workspace(r), c.Param("id"))
	if e != nil {
		return e
	}
	return c.JSON(200, taskResponseDTO(v))
}
