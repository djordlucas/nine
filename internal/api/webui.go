package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// webUI is the static console served by the API server. It is plain
// HTML/CSS/JS with no build step: the single binary the project ships must not
// grow a Node toolchain. The console talks to the API below /api/v1 like any
// other client — it adds no routes of its own to the API surface.
//
//go:embed web/index.html
var webUI embed.FS

// registerWebUIRoutes serves the console at /ui.
//
// The UI lives at /ui rather than /, so the root stays free and every unknown
// path under the API prefix keeps its single JSON answer. The shell carries no
// data of its own: when a bearer token is configured, the browser loads the
// shell unauthenticated and the JavaScript inside it sends the token the
// operator enters, on every API request it makes.
func (s *Server) registerWebUIRoutes(mux *http.ServeMux) {
	sub, err := fs.Sub(webUI, "web")
	if err != nil {
		// Cannot fail: the embed pattern above compiles only if
		// web/index.html exists.
		return
	}
	mux.Handle("GET /ui", http.StripPrefix("/ui", http.FileServer(http.FS(sub))))
	mux.Handle("GET /ui/", http.StripPrefix("/ui", http.FileServer(http.FS(sub))))
}
