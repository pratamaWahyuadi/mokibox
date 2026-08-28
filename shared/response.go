// Package shared contains code used by both api-gateway and
// transcoder-worker. Keep package-level state to a minimum;
// everything is passed via constructor injection.
//
// This file defines the JSON envelope required by
// planning/04_api_contracts.md and the helpers that
// handlers use to produce it. The envelope is the same on
// the wire regardless of HTTP status:
//
//	{"data": <T>}                                  // single resource
//	{"data": [...], "pagination": {...}}           // list
//	{"error": {"code": "...", "message": "...", "details": [...]}}
//
// Handlers MUST go through RespondOK / RespondCreated /
// RespondList / RespondNoContent / RespondError rather
// than calling c.JSON directly so the wire format stays
// consistent across endpoints.
package shared

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
)

// Pagination is the pagination block on list responses.
// NextCursor is null when there are no more pages, not ""
// or omitted, because clients distinguish "no more pages"
// from "page missing".
type Pagination struct {
	NextCursor *string `json:"next_cursor"`
}

// RespondOK serializes a single resource envelope.
// Status is always 200. The payload is set on c.Response()
// via the JSON codec so Content-Type and charset are
// correct.
func RespondOK(c echo.Context, data any) error {
	return c.JSON(http.StatusOK, envelope{Data: data})
}

// RespondCreated serializes a single resource envelope
// with status 201. Use it after POST that creates a
// resource (e.g. upload-intent, comment, like).
func RespondCreated(c echo.Context, data any) error {
	return c.JSON(http.StatusCreated, envelope{Data: data})
}

// RespondList serializes a list envelope with the given
// items and next cursor. Pass nil for nextCursor when the
// caller has reached the last page so the field is encoded
// as JSON null, not as an empty string.
func RespondList(c echo.Context, items any, nextCursor *string) error {
	return c.JSON(http.StatusOK, envelope{
		Data:       items,
		Pagination: &Pagination{NextCursor: nextCursor},
	})
}

// RespondNoContent sends 204 with no body. Echo's c.NoContent
// is correct here; we wrap it so the call site looks like
// the other Respond* helpers and a future change to the
// response shape stays in one file.
func RespondNoContent(c echo.Context) error {
	return c.NoContent(http.StatusNoContent)
}

// RespondError is the central error sink. It is the only
// place in the codebase allowed to map an error to an HTTP
// status and serialize the error envelope, so the wire
// format stays consistent. The original error is logged
// with structured fields (path, method) before the
// response is built, so the log retains the cause even
// when the response body is generic.
//
// Mapping rules:
//   - *APIError: use its Code/Message/Details/Status as-is.
//   - sentinel error (errors.Is matches one of the Err*
//     variables in errors.go): derive code + status from
//     the mapping tables, use the wrapped message.
//   - anything else: collapse to INTERNAL_ERROR 500. The
//     original error is logged but never sent to the
//     client.
func RespondError(c echo.Context, err error) error {
	status, code, message, details := classifyError(err)
	// Log the original error (or the APIError's cause)
	// before it is collapsed. The response below may be
	// a generic "internal server error" so the log is
	// the only place the cause survives.
	if err != nil {
		slog.Error("request failed",
			"err", err,
			"path", c.Path(),
			"method", c.Request().Method,
			"status", status,
		)
	}
	return c.JSON(status, errorEnvelope{
		Error: errorBody{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
}

// envelope is the {data,...} response shape. The pagination
// field is omitted on single-resource responses by leaving
// the pointer nil; encoding/json skips nil pointers.
type envelope struct {
	Data       any         `json:"data"`
	Pagination *Pagination `json:"pagination,omitempty"`
}

// errorEnvelope is the {error:{...}} response shape.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// errorBody is the inner object of the error envelope.
type errorBody struct {
	Code    ErrorCode   `json:"code"`
	Message string      `json:"message"`
	Details []FieldError `json:"details,omitempty"`
}

// classifyError is the single point that translates an
// arbitrary error into the (status, code, message, details)
// tuple used by RespondError. It is unexported because
// callers must always go through RespondError.
func classifyError(err error) (int, ErrorCode, string, []FieldError) {
	if err == nil {
		return http.StatusInternalServerError, CodeInternalError, "internal server error", nil
	}
	// *APIError short-circuits the sentinel mapping so
	// the caller can attach a custom Message and Details.
	var api *APIError
	if errors.As(err, &api) {
		code := api.Code
		if code == "" {
			code = codeFor(err)
		}
		status := api.Status
		if status == 0 {
			status = httpStatusFor(err)
		}
		msg := api.Message
		if msg == "" {
			msg = "request failed"
		}
		return status, code, msg, api.Details
	}
	// Sentinel error: derive code + status from the
	// mapping tables. err.Error() already includes the
	// caller's wrap context, so we use it as the message.
	return httpStatusFor(err), codeFor(err), err.Error(), nil
}
