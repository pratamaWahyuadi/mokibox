// API-contract docs: OpenAPI spec + Swagger UI index.
//
// registerDocsRoutes mounts the public, unauthenticated endpoints:
//
//	GET /api/docs/swagger.json — raw OpenAPI 3.0 document
//	GET /api/docs/            — minimal Swagger UI page
//	GET /api/docs             — same page (trailing-slash-free alias)
//	GET /api/docs/oauth2-redirect.html — swagger-ui-dist OAuth2 redirect page
//	GET /api/docs/oauth2-redirect.js   — companion script of the redirect page
//
// The spec (api-gateway/docs/swagger.json) documents every route
// registered in api-gateway/routes.go, including request bodies,
// query parameters, response envelopes, and per-endpoint error
// codes. Both files are embedded via go:embed so they are part of
// the binary and available in the distroless Docker runtime.
package main

import (
	_ "embed"
	"net/http"

	"github.com/labstack/echo/v4"
)

// swaggerJSON is the raw OpenAPI document served at GET /api/docs/swagger.json.
// Generated/maintained by hand; see api-gateway/docs/swagger.json.
//
//go:embed docs/swagger.json
var swaggerJSON []byte

// swaggerIndex is the minimal Swagger UI page served at GET /api/docs/.
//
//go:embed docs/index.html
var swaggerIndex []byte

// oauth2RedirectHTML is the swagger-ui-dist OAuth2 redirect page
// (docs/oauth2-redirect.html). Swagger UI navigates the browser here
// at the end of the interactive OAuth2 authorization-code chain; this
// page posts the result back to the Swagger UI instance via window.opener
// (see oauth2-redirect.js).
//
//go:embed docs/oauth2-redirect.html
var oauth2RedirectHTML []byte

// oauth2RedirectJS is the companion script of the redirect page
// (docs/oauth2-redirect.js), served next to the page.
//
//go:embed docs/oauth2-redirect.js
var oauth2RedirectJS []byte

// registerDocsRoutes registers the public API-contract doc routes on the
// root Echo. They are deliberately OUTSIDE the auth group: the contract is
// documentation, and anonymous reads keep Swagger UI usable for anyone
// (including security reviewers probing the API surface).
func registerDocsRoutes(e *echo.Echo) {
	e.GET("/api/docs/swagger.json", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/json", swaggerJSON)
	})
	e.GET("/api/docs/", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "text/html; charset=utf-8", swaggerIndex)
	})
	e.GET("/api/docs", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "text/html; charset=utf-8", swaggerIndex)
	})
	e.GET("/api/docs/oauth2-redirect.html", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "text/html; charset=utf-8", oauth2RedirectHTML)
	})
	e.GET("/api/docs/oauth2-redirect.js", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/javascript; charset=utf-8", oauth2RedirectJS)
	})
}
