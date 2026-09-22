package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"gopkg.in/yaml.v3"
)

// specYAML is the OpenAPI 3.1 document, and the source of truth for this API:
// internal/api/apigen is generated from it, so a handler that disagrees with it
// fails to compile. Embedding it means the running server hands out exactly the
// document its own code was generated from, with no build step in between.
//
//go:embed openapi.yaml
var specYAML []byte

var (
	specJSONOnce sync.Once
	specJSON     []byte
	specJSONErr  error
)

// openAPIJSON renders the document as JSON for tooling that will not read YAML.
// The conversion runs once and is cached; it cannot fail in practice, since the
// same bytes were parsed by the generator at build time.
func openAPIJSON() ([]byte, error) {
	specJSONOnce.Do(func() {
		var doc any
		if err := yaml.Unmarshal(specYAML, &doc); err != nil {
			specJSONErr = fmt.Errorf("parse embedded openapi.yaml: %w", err)
			return
		}
		specJSON, specJSONErr = json.Marshal(doc)
	})
	return specJSON, specJSONErr
}

// registerSpecRoutes serves the OpenAPI document and a browser for it.
//
// The path is /api/v1/openapi rather than /api/v1/docs, which is already the
// documentation-topic endpoint.
func (s *Server) registerSpecRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(specYAML) //nolint:errcheck
	})

	mux.HandleFunc("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		body, err := openAPIJSON()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", err.Error(), nil)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	})

	mux.HandleFunc("GET /api/v1/openapi/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(specBrowser) //nolint:errcheck
	})
}

// specBrowser renders the document with Scalar, which reads OpenAPI 3.1.
// Swagger UI, which the swaggo integration bundled, only partly supports 3.1.
//
// The renderer loads from a CDN, so this page needs network access; the
// document itself is served from the two routes above and needs none.
var specBrowser = []byte(`<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Nine API</title>
  </head>
  <body>
    <script id="api-reference" data-url="/api/v1/openapi.json"></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
  </body>
</html>
`)
