// Package main wires the API gateway. error_handler.go
// is the central HTTPErrorHandler installed on the
// Echo instance via routes.go.
//
// Why a separate file
//
// Echo calls e.HTTPErrorHandler when a handler returns
// a non-nil error, when a middleware rejects the
// request, or when the router cannot match a route
// (404) or method (405). Phase 3-8 handlers funnel
// their domain errors through shared.RespondError which
// writes the envelope directly and returns nil, so the
// HTTPErrorHandler only sees:
//
//   - Echo framework errors (*echo.HTTPError wrapping
//     404 / 405 / body-bind failures)
//   - errors that bypassed RespondError by mistake
//     (defence in depth; treated as INTERNAL_ERROR)
//   - nil (when a handler has already written its
//     response; we MUST NOT overwrite)
//
// Design: keep shared.RespondError as the primary
// path (zero behaviour change for the existing 10
// handlers across phase 3-8) and use HTTPErrorHandler
// only as the safety net. Handlers MUST keep calling
// shared.RespondError; this handler exists so
// framework-level errors also serialise as the
// canonical envelope rather than Echo's default
// `{"message": "Not Found"}` shape.
package main

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

// HTTPErrorHandler is the safety-net error sink
// installed on the Echo instance. It MUST be set via
// e.HTTPErrorHandler = HTTPErrorHandler exactly once
// in NewRouter.
//
// Behaviour:
//
//   - *echo.HTTPError (Echo framework - 404, 405,
//     bind failure, etc.): map the Code to a shared
//     error code so the wire envelope stays
//     consistent. Echo's default body is replaced
//     with the same {error:{code,message}} envelope
//     RespondError uses, so clients see one format
//     across every endpoint.
//   - *shared.APIError or wrapped sentinel: delegate
//     to shared.ClassifyError so the mapping table
//     in shared/errors.go is the single source of
//     truth.
//   - anything else: log + 500 INTERNAL_ERROR envelope.
//   - c.Response().Committed: skip (matches Echo's
//     default; the response has already been sent so
//     re-writing would corrupt the body).
//
// The original error is logged once with path +
// method so on-call can trace the cause without it
// leaking into the response body.
func HTTPErrorHandler(err error, c echo.Context) {
	// Mirror Echo's default guard: a handler that
// has written a response (e.g. via shared.RespondOK)
	// followed by a return err would otherwise be
	// silently overwritten by us. Bail.
	if c.Response().Committed {
		return
	}
	if err == nil {
		// Nothing to do. Echo's default would still
// write a body; we leave the response alone so
// the request handler owns its 2xx outcome.
		return
	}

	status, code, message, details := mapError(err)

	// Log the original error once. Keep it structured
// so log aggregation can group by status / path.
	slog.Error("api-gateway: request failed",
		"err", err,
		"path", c.Path(),
		"method", c.Request().Method,
		"status", status,
		"code", string(code),
	)

	// Build the envelope. We can't go through
// shared.RespondError here because Echo calls
// HTTPErrorHandler instead of returning the error
// to the chain, so the envelope shape is duplicated
// for consistency. details is nil unless the error
// was a *shared.APIError that explicitly attached
// field errors.
	if writeErr := c.JSON(status, sharedEnvelope{
		Error: sharedBody{
			Code:    code,
			Message: message,
			Details: details,
		},
	}); writeErr != nil {
		slog.Error("api-gateway: error envelope write failed",
			"err", writeErr,
			"path", c.Path(),
		)
	}
}

// mapError converts an arbitrary error into the
// (status, code, message, details) tuple that drives
// the envelope shape. The order of checks matters:
//
//  1. *echo.HTTPError: framework error. Map the Code
//     to a shared error code so 404 / 405 / 400 body-bind
//     failures produce the same envelope shape as domain
//     errors. Echo's default 404 wire body is
//     `{"message":"Not Found"}` which breaks the
//     canonical envelope contract in
//     planning/04_api_contracts.md section 6.
//  2. anything else: delegate to shared.ClassifyError,
//     which already handles *shared.APIError,
//     wrap-sentinel, and the unknown collapse.
func mapError(err error) (int, shared.ErrorCode, string, []shared.FieldError) {
	var he *echo.HTTPError
	if errors.As(err, &he) {
		return mapEchoHTTPError(he)
	}
	return shared.ClassifyError(err)
}

// mapEchoHTTPError translates an Echo framework error
// into the canonical envelope. The mapping mirrors
// shared/httpStatusFor for the codes both worlds
// share, but uses ErrorCode constants from
// shared/errors.go so the wire format stays
// consistent.
func mapEchoHTTPError(he *echo.HTTPError) (int, shared.ErrorCode, string, []shared.FieldError) {
	// he.Message can be string or json.Marshaler or
// error. For 404/405 Echo uses http.StatusText; for
// bind failures it uses the binding error text. We
// stringify uniformly.
	msg := http.StatusText(he.Code)
	if s, ok := he.Message.(string); ok && s != "" {
		msg = s
	}
	switch he.Code {
	case http.StatusNotFound:
		return he.Code, shared.CodeNotFound, "resource not found", nil
	case http.StatusMethodNotAllowed:
		return he.Code, shared.CodeValidationError, "method not allowed", nil
	case http.StatusBadRequest:
		// Body bind failure (malformed JSON, wrong
// content-type, etc.). Surface as a validation
// error so clients see the same envelope shape as
// domain validation failures.
		return he.Code, shared.CodeValidationError, msg, nil
	case http.StatusRequestTimeout, http.StatusServiceUnavailable:
		return he.Code, shared.CodeInternalError, msg, nil
	default:
		return he.Code, shared.CodeInternalError, msg, nil
	}
}

// sharedEnvelope mirrors shared.errorEnvelope so the
// wire format produced here matches the one produced
// by shared.RespondError. Duplicating the shape (rather
// than importing shared.errorEnvelope which is
// unexported) keeps shared.respond-error call sites
// and this safety net on the same wire contract.
type sharedEnvelope struct {
	Error sharedBody `json:"error"`
}

type sharedBody struct {
	Code    shared.ErrorCode  `json:"code"`
	Message string            `json:"message"`
	Details []shared.FieldError `json:"details,omitempty"`
}