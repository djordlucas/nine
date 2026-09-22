package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	"nine/internal/api/apigen"
)

// Spec-driven request validation.
//
// The document already states what a valid request looks like — required body
// fields, page bounds, enum members — and this checks incoming requests against
// it before a handler runs. Without it each of those has to be re-asserted by
// hand in Go, which is where the constraints drift apart: the document says
// limit is at most 1000 and only the handler enforces it.
//
// It validates only the operations the document describes. The routes that
// serve the document itself are registered outside it, since they are not
// operations it declares.

// requestValidator builds the validation middleware for the generated routes.
//
// A failure here returns the API's own error envelope: the middleware's default
// is plain text, which a client parsing our errors cannot decode.
func requestValidator() (apigen.MiddlewareFunc, error) {
	swagger, err := apigen.GetSwagger()
	if err != nil {
		return nil, fmt.Errorf("load embedded openapi document: %w", err)
	}

	// The document's server URL is templated ({scheme}://{host}:{port}/api/v1),
	// which the validator cannot match a concrete request against — it would
	// look for /skills and be handed /api/v1/skills. Replacing it with the bare
	// base path lets the router strip the prefix and find the operation.
	swagger.Servers = openapi3.Servers{{URL: apiBasePath}}

	return nethttpmiddleware.OapiRequestValidatorWithOptions(swagger,
		&nethttpmiddleware.Options{
			SilenceServersWarning: true,
			Options: openapi3filter.Options{
				// Authentication is authMiddleware's job, and only it knows
				// whether a token is configured — nine's bearer auth is
				// optional, while the document declares operations secured
				// unconditionally. Left to the validator, every request would
				// be rejected as unauthenticated on a server with auth off.
				AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			},
			ErrorHandlerWithOpts: func(
				ctx context.Context,
				err error,
				w http.ResponseWriter,
				r *http.Request,
				opts nethttpmiddleware.ErrorHandlerOpts,
			) {
				status := opts.StatusCode
				if status == 0 {
					status = http.StatusBadRequest
				}
				code := "invalid_request"
				switch status {
				case http.StatusNotFound:
					code = "not_found"
				case http.StatusMethodNotAllowed:
					code = "invalid_request"
				}
				writeError(w, status, code, err.Error(), nil)
			},
		}), nil
}

// validationMiddleware returns the validator, or nothing if the embedded
// document cannot be loaded.
//
// Failing open is deliberate: the document is embedded at build time and parsed
// by the generator, so this cannot fail in a built binary. Refusing to start
// over it would turn an impossible condition into an outage, while every check
// the handlers already make still runs.
func (s *Server) validationMiddleware() []apigen.MiddlewareFunc {
	validator, err := requestValidator()
	if err != nil {
		slog.Error("request validation disabled: could not load the embedded OpenAPI document",
			"err", err)
		return nil
	}
	return []apigen.MiddlewareFunc{validator}
}
