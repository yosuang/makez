package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
)

type Server struct {
	DB        *store.Store
	Artifacts *artifact.Store
	Keys      map[string]string
	Agents    map[string]core.AgentAdapter
	Secrets   core.SecretResolver
}
type workspaceKey struct{}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	secure := http.NewServeMux()
	secure.HandleFunc("GET /v1/agents", s.agents)
	secure.HandleFunc("POST /v1/tasks", s.submit)
	secure.HandleFunc("GET /v1/tasks", s.list)
	secure.HandleFunc("GET /v1/tasks/{id}", s.get)
	secure.HandleFunc("POST /v1/tasks/{id}/cancel", s.cancel)
	secure.HandleFunc("GET /v1/tasks/{id}/events", s.events)
	secure.HandleFunc("GET /v1/tasks/{id}/artifacts", s.artifacts)
	secure.HandleFunc("GET /v1/artifacts/{id}/content", s.download)
	mux.Handle("/v1/", s.auth(secure))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !bearer || token == "" {
			writeError(w, http.StatusUnauthorized, "Bearer API key required")
			return
		}
		for key, ws := range s.Keys {
			if subtle.ConstantTimeCompare([]byte(key), []byte(token)) == 1 {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), workspaceKey{}, ws)))
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "invalid API key")
	})
}
func workspace(r *http.Request) string { return r.Context().Value(workspaceKey{}).(string) }
func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	var ids []core.AdapterIdentity
	for _, a := range s.Agents {
		ids = append(ids, a.Identity())
	}
	writeJSON(w, 200, ids)
}
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var in store.SubmitInput
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		writeError(w, 400, "invalid request JSON")
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeError(w, 400, "expected one JSON object")
		return
	}
	if string(in.Config) == "null" {
		in.Config = json.RawMessage(`{}`)
	}
	idem := r.Header.Get("Idempotency-Key")
	if len(idem) == 0 || len(idem) > 128 {
		writeError(w, 400, "Idempotency-Key (1–128 characters) is required")
		return
	}
	if strings.TrimSpace(in.Prompt) == "" || len(in.Prompt) > 64<<10 {
		writeError(w, 400, "prompt must be 1–65536 bytes")
		return
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
		writeError(w, 400, "unknown agent/version")
		return
	}
	if in.ProfileID != "default" || in.ProfileVersion != "1" {
		writeError(w, 400, "unknown runtime profile/version")
		return
	}
	plan, err := a.Plan(core.AgentInput{Prompt: in.Prompt, Config: in.Config})
	if err != nil {
		writeError(w, 400, "invalid agent configuration")
		return
	}
	if len(plan.SecretRefs) > 0 && (s.Secrets == nil || s.Secrets.Check(workspace(r), plan.SecretRefs) != nil) {
		writeError(w, 403, "agent authentication is not configured for this workspace")
		return
	}
	if !in.Deadline.IsZero() && in.TimeoutSeconds != 0 {
		writeError(w, 400, "use deadline or timeout_seconds, not both")
		return
	}
	if in.Deadline.IsZero() {
		if in.TimeoutSeconds == 0 {
			in.TimeoutSeconds = 3600
		}
		if in.TimeoutSeconds < 5 || in.TimeoutSeconds > 5400 {
			writeError(w, 400, "timeout_seconds must be 5–5400")
			return
		}
	}
	t, created, e := s.DB.Submit(r.Context(), workspace(r), idem, in)
	if e != nil {
		respondError(w, e)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	writeJSON(w, status, t)
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	v, e := s.DB.List(r.Context(), workspace(r))
	if e != nil {
		respondError(w, e)
		return
	}
	if v == nil {
		v = []store.Task{}
	}
	writeJSON(w, 200, v)
}
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	v, e := s.DB.Get(r.Context(), workspace(r), r.PathValue("id"))
	if e != nil {
		respondError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	v, e := s.DB.Cancel(r.Context(), workspace(r), r.PathValue("id"))
	if e != nil {
		respondError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) artifacts(w http.ResponseWriter, r *http.Request) {
	v, e := s.DB.Artifacts(r.Context(), workspace(r), r.PathValue("id"))
	if e != nil {
		respondError(w, e)
		return
	}
	if v == nil {
		v = []store.Artifact{}
	}
	writeJSON(w, 200, v)
}
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	v, e := s.DB.Artifact(r.Context(), workspace(r), r.PathValue("id"))
	if e != nil {
		respondError(w, e)
		return
	}
	f, e := s.Artifacts.Open(v.StorageKey)
	if e != nil {
		respondError(w, e)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(v.Path)))
	w.Header().Set("Content-Length", strconv.FormatInt(v.Size, 10))
	io.Copy(w, f)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	var e error
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	if cursor != "" {
		after, e = strconv.ParseInt(cursor, 10, 64)
		if e != nil || after < 0 {
			writeError(w, 400, "invalid event cursor")
			return
		}
	}
	if _, e = s.DB.Get(r.Context(), workspace(r), r.PathValue("id")); e != nil {
		respondError(w, e)
		return
	}
	flush, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flush.Flush()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		evs, e := s.DB.Events(r.Context(), workspace(r), r.PathValue("id"), after)
		if e != nil {
			return
		}
		for _, event := range evs {
			b, _ := json.Marshal(event)
			if _, e = fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", event.Seq, b); e != nil {
				return
			}
			after = event.Seq
		}
		t, e := s.DB.Get(r.Context(), workspace(r), r.PathValue("id"))
		if e != nil {
			return
		}
		b, _ := json.Marshal(t)
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", b)
		flush.Flush()
		// Drain remaining pages before closing a terminal stream.
		if store.IsTerminal(t.State) && len(evs) == 0 {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
func respondError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, store.ErrInvalidInput):
		writeError(w, 400, "invalid task input or deadline")
	case errors.Is(e, store.ErrNotFound):
		writeError(w, 404, "not found")
	case errors.Is(e, store.ErrConflict):
		writeError(w, 409, "request conflicts with existing task")
	default:
		slog.Error("API request failed", "error", e)
		writeError(w, 500, "internal error")
	}
}
