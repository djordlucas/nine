package api

import (
	"fmt"
	"net/http"
	"strings"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// Server implements every operation the document declares. This assertion is
// what makes a spec change a build failure: add an operation to openapi.yaml,
// regenerate, and the package stops compiling until a handler exists for it.
var _ apigen.StrictServerInterface = (*Server)(nil)

// This file holds the plumbing shared by the generated-interface handlers in
// handlers_*.go: how a daemon error becomes a status code, how paging arrives
// as typed optional parameters, and how the generated router reports a request
// it could not bind.

// errNotFound classifies a daemon reply as a missing resource.
//
// The wire protocol returns errors as strings, so this matches on text. That is
// fragile — a reword upstream turns a 404 into a 500 — and the durable fix is
// sentinel errors in internal/protocol. Until then the matching lives in one
// place instead of being repeated at every call site.
func errNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// errorBody builds the standard error payload.
func errorBody(code, message string, details map[string]any) apigen.ErrorResponse {
	body := apigen.ErrorResponse{
		Error: apigen.ErrorDetails{
			Code:    apigen.ErrorDetailsCode(code),
			Message: message,
		},
	}
	if len(details) > 0 {
		body.Error.Details = &details
	}
	return body
}

// errConflict classifies a daemon reply that refused a request because of the
// target's state. Unlike errNotFound it matches a prefix the daemon sets on
// purpose (protocol.ConflictPrefix), so a reworded message cannot move it.
func errConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), protocol.ConflictPrefix)
}

// page turns the generated optional limit/offset into validated bounds.
//
// The generated router parses the types and the request validator enforces the
// range the document declares, so neither a non-integer nor an out-of-range
// value normally reaches here. These checks stay as the backstop that makes
// validate.go's fail-open safe: if the embedded document could not be loaded,
// validation is skipped and this is what still holds the bound.
func page(limit, offset *int) (pageParams, error) {
	p := pageParams{Limit: DefaultPageLimit}

	if limit != nil {
		if *limit < 1 {
			return p, fmt.Errorf("limit must be at least 1, got %d", *limit)
		}
		if *limit > MaxPageLimit {
			return p, fmt.Errorf("limit must be at most %d, got %d", MaxPageLimit, *limit)
		}
		p.Limit = *limit
	}

	if offset != nil {
		if *offset < 0 {
			return p, fmt.Errorf("offset must not be negative, got %d", *offset)
		}
		p.Offset = *offset
	}

	return p, nil
}

// derefBool reads an optional flag, absent meaning false.
func derefBool(v *bool) bool { return v != nil && *v }

// requestBindingError renders a failure from the generated router — an
// unparseable query parameter, a malformed JSON body — in the API's own error
// shape. Without it those failures return net/http's plain-text default, so a
// client parsing errors would hit a body it cannot decode.
func requestBindingError(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
}
