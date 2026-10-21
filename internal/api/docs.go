package api

import (
	"net"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/makez-dev/makez/internal/api/docs"
	swaggerFiles "github.com/swaggo/files/v2"
)

func (s *Server) registerDocs(e *echo.Echo) {
	if !s.Development || s.OperationsOnly {
		return
	}
	e.GET("/swagger/*", func(c *echo.Context) error {
		host, _, err := net.SplitHostPort(c.Request().RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return echo.ErrNotFound
		}
		// Serve only the assets used here. In particular, do not expose the
		// upstream example initializer, which loads an external Petstore spec.
		switch c.Param("*") {
		case "", "index.html":
			return c.HTML(http.StatusOK, swaggerIndex)
		case "doc.json":
			return c.JSONBlob(http.StatusOK, []byte(docs.SwaggerInfo.ReadDoc()))
		case "swagger-ui.css", "swagger-ui-bundle.js":
			return c.FileFS(c.Param("*"), swaggerFiles.FS)
		default:
			return echo.ErrNotFound
		}
	})
}

// The pinned swaggo/files module embeds the official Swagger UI distribution.
// All assets and the specification are local; validation and URL overrides are
// disabled so viewing these docs never sends the spec or API keys to a third party.
const swaggerIndex = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Makez API</title><link rel="icon" href="data:,"><link rel="stylesheet" href="/swagger/swagger-ui.css"></head>
<body><div id="swagger-ui"></div><script src="/swagger/swagger-ui-bundle.js"></script>
<script>window.ui = SwaggerUIBundle({url: "/swagger/doc.json", dom_id: "#swagger-ui", validatorUrl: null,
queryConfigEnabled: false, persistAuthorization: false, deepLinking: true});</script></body></html>`
