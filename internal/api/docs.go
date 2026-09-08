// Package api is the Nine HTTP API layer.
//
// @title           Nine API
// @version         1.0
// @description     HTTP/REST API for the Nine AI agent runtime. Provides feature parity with the CLI — conversations, goals, workflows, tools, plugins, skills, and more.
// @description     The API runs as a separate process that communicates with the daemon via Unix socket.
// @termsOfService  https://github.com/djordlucas/nine

// @contact.name   Nine
// @contact.url    https://github.com/djordlucas/nine

// @license.name   GPL-3.0
// @license.url    https://www.gnu.org/licenses/gpl-3.0.html

// @host           localhost:8080
// @BasePath       /api/v1
// @accepts        json
// @produces       json

// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Bearer token authentication (optional; required only when auth_token is configured)
package api
