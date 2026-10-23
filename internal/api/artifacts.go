package api

import (
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"

	"github.com/labstack/echo/v5"
)

// artifacts lists immutable results.
// @Summary List task artifacts
// @Tags tasks
// @Produce json
// @Security BearerAuth
// @Param id path string true "Task ID"
// @Success 200 {array} ArtifactResponse
// @Failure 401,404,500 {object} ErrorResponse
// @Router /tasks/{id}/artifacts [get]
func (s *Server) artifacts(c *echo.Context) error {
	r := c.Request()
	items, err := s.DB.Artifacts(r.Context(), workspace(r), c.Param("id"))
	if err != nil {
		return err
	}
	result := make([]ArtifactResponse, 0, len(items))
	for _, a := range items {
		result = append(result, artifactResponse(a))
	}
	return c.JSON(200, result)
}

// download streams a stored artifact.
// @Summary Download artifact
// @Tags artifacts
// @Produce octet-stream
// @Security BearerAuth
// @Param id path string true "Artifact ID"
// @Success 200 {file} file
// @Header 200 {string} Content-Disposition "attachment; filename=..."
// @Failure 401,404,500 {object} ErrorResponse
// @Router /artifacts/{id}/content [get]
func (s *Server) download(c *echo.Context) error {
	r, w := c.Request(), c.Response()
	a, err := s.DB.Artifact(r.Context(), workspace(r), c.Param("id"))
	if err != nil {
		return err
	}
	f, err := s.Artifacts.Open(a.StorageKey)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // The artifact is read-only and response streaming handles read errors.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(a.Path)))
	w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	if r.Method == http.MethodHead {
		return c.NoContent(200)
	}
	_, err = io.Copy(w, f)
	return err
}
