// Package shared contains code used by both api-gateway and
// transcoder-worker. Keep package-level state to a minimum;
// everything is passed via constructor injection.
//
// This file defines the canonical application error codes
// declared in planning/04_api_contracts.md and the mapping
// from each code to the HTTP status returned to clients.
// Handlers wrap concrete failures with these sentinels via
// fmt.Errorf("...: %w", ErrXxx) so the central HTTP error
// handler in api-gateway can map them with errors.Is.
package shared

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorCode is the machine-readable identifier returned in
// the JSON error envelope. Values are kept stable because
// clients (and tests) depend on them.
type ErrorCode string

const (
	// CodeValidationError is returned for malformed request
	// bodies, missing required fields, or business rule
	// violations expressed as input validation. 400.
	CodeValidationError ErrorCode = "VALIDATION_ERROR"

	// CodeUnauthorized is returned when authentication is
	// missing, invalid, or expired. 401.
	CodeUnauthorized ErrorCode = "UNAUTHORIZED"

	// CodeForbidden is returned when the caller is
	// authenticated but not allowed to perform the action.
	// 403. (Per API contract, read-side authorization
	// failures normally map to 404 to avoid resource
	// enumeration; FORBIDDEN is reserved for explicit
	// denials such as self-follow.)
	CodeForbidden ErrorCode = "FORBIDDEN"

	// CodeNotFound is returned when the resource does not
	// exist or the caller is not allowed to know it exists.
	// 404.
	CodeNotFound ErrorCode = "NOT_FOUND"

	// CodeVideoStatusConflict is returned when a confirm
	// or status mutation is attempted on a video that is
	// not in the expected state (e.g. not PENDING_UPLOAD).
	// 409.
	CodeVideoStatusConflict ErrorCode = "VIDEO_STATUS_CONFLICT"

	// CodeVideoNotReady is returned when a client tries to
	// consume a video (play, like, comment) that has not
	// finished processing. 409.
	CodeVideoNotReady ErrorCode = "VIDEO_NOT_READY"

	// CodeUploadMissing is returned when the R2 object
	// expected for a PENDING_UPLOAD video is not present
	// at confirm time. 409.
	CodeUploadMissing ErrorCode = "UPLOAD_MISSING"

	// CodeUploadSizeInvalid is returned when the R2 object
	// size is outside the 1 KB - 200 MB allow-list enforced
	// at confirm. 400.
	CodeUploadSizeInvalid ErrorCode = "UPLOAD_SIZE_INVALID"

	// CodeSelfFollowNotAllowed is returned when a user
	// tries to follow themselves. 400.
	CodeSelfFollowNotAllowed ErrorCode = "SELF_FOLLOW_NOT_ALLOWED"

	// CodeWebhookInvalidSignature is returned when a
	// Zitadel Actions V2 webhook fails HMAC verification.
	// 401.
	CodeWebhookInvalidSignature ErrorCode = "WEBHOOK_INVALID_SIGNATURE"

	// CodeWebhookEventUnsupported is returned when a
	// verified webhook delivers an event_type the backend
	// does not handle. 400.
	CodeWebhookEventUnsupported ErrorCode = "WEBHOOK_EVENT_UNSUPPORTED"

	// CodeInternalError is the catch-all for unexpected
	// failures. 500. The original error must be logged
	// before being mapped to this code so the response
	// stays generic while the log retains the cause.
	CodeInternalError ErrorCode = "INTERNAL_ERROR"

	// CodeRateLimited is returned when a per-IP or
	// per-user throttle fires (webhook rate limit,
	// future social rate limit). 429. Not part of the
	// phase-2 explicit list but listed here because the
	// mapping table is the natural home for it and
	// avoiding it now would force a later edit.
	CodeRateLimited ErrorCode = "RATE_LIMITED"
)

// Sentinel errors. Wrap these with fmt.Errorf("...: %w",
// ErrXxx) so the central error handler can match with
// errors.Is.
var (
	ErrValidation          = errors.New("validation error")
	ErrUnauthorized        = errors.New("unauthorized")
	ErrForbidden           = errors.New("forbidden")
	ErrNotFound            = errors.New("not found")
	ErrVideoStatusConflict = errors.New("video status conflict")
	ErrVideoNotReady       = errors.New("video not ready")
	ErrUploadMissing       = errors.New("upload missing")
	ErrUploadSizeInvalid   = errors.New("upload size invalid")
	ErrSelfFollow          = errors.New("self follow not allowed")
	ErrWebhookSignature    = errors.New("webhook invalid signature")
	ErrWebhookEvent        = errors.New("webhook event unsupported")
	ErrInternal            = errors.New("internal error")
	ErrRateLimited         = errors.New("rate limited")
)

// httpStatusFor maps a sentinel error to the HTTP status
// code required by the API contract. The table is the
// single source of truth; HTTPErrorHandler in api-gateway
// must call this rather than re-deriving the mapping.
func httpStatusFor(err error) int {
	switch {
	case errors.Is(err, ErrValidation):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrVideoStatusConflict):
		return http.StatusConflict
	case errors.Is(err, ErrVideoNotReady):
		return http.StatusConflict
	case errors.Is(err, ErrUploadMissing):
		return http.StatusConflict
	case errors.Is(err, ErrUploadSizeInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrSelfFollow):
		return http.StatusBadRequest
	case errors.Is(err, ErrWebhookSignature):
		return http.StatusUnauthorized
	case errors.Is(err, ErrWebhookEvent):
		return http.StatusBadRequest
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrInternal):
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// codeFor maps a sentinel error to its ErrorCode. Anything
// not matched collapses to CodeInternalError so the wire
// format always carries a code, never an empty string.
func codeFor(err error) ErrorCode {
	switch {
	case errors.Is(err, ErrValidation):
		return CodeValidationError
	case errors.Is(err, ErrUnauthorized):
		return CodeUnauthorized
	case errors.Is(err, ErrForbidden):
		return CodeForbidden
	case errors.Is(err, ErrNotFound):
		return CodeNotFound
	case errors.Is(err, ErrVideoStatusConflict):
		return CodeVideoStatusConflict
	case errors.Is(err, ErrVideoNotReady):
		return CodeVideoNotReady
	case errors.Is(err, ErrUploadMissing):
		return CodeUploadMissing
	case errors.Is(err, ErrUploadSizeInvalid):
		return CodeUploadSizeInvalid
	case errors.Is(err, ErrSelfFollow):
		return CodeSelfFollowNotAllowed
	case errors.Is(err, ErrWebhookSignature):
		return CodeWebhookInvalidSignature
	case errors.Is(err, ErrWebhookEvent):
		return CodeWebhookEventUnsupported
	case errors.Is(err, ErrRateLimited):
		return CodeRateLimited
	default:
		return CodeInternalError
	}
}

// FieldError describes a single field-level validation
// failure. It populates the "details" array of the error
// envelope when code == CodeValidationError.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// APIError is the structured error returned to handlers so
// they can pass a code + human message + optional field
// details through the central error handler without having
// to know the wire format.
//
// It satisfies `error` so a value of this type can be
// returned directly from a service, but the recommended
// pattern is to wrap a sentinel (ErrXxx) so callers can
// use errors.Is. Use NewAPIError only when the sentinel
// does not fit.
type APIError struct {
	// Code is the public error code. When zero, the
	// central handler substitutes CodeInternalError.
	Code ErrorCode
	// Message is the human-readable summary.
	Message string
	// Details is non-empty only for validation errors.
	Details []FieldError
	// Cause is the wrapped original error, if any. It is
	// logged by the central handler but never sent to the
	// client.
	Cause error
	// Status overrides the default HTTP status when a
	// caller needs to diverge from the sentinel mapping
	// (e.g. 422 Unprocessable Entity for a validation
	// case). Zero means "use the sentinel mapping".
	Status int
}

// NewAPIError builds an APIError with the given code and
// message. Cause and details can be added by chaining the
// field assignments; helpers WithCause and WithDetails are
// provided for readability.
func NewAPIError(code ErrorCode, message string) *APIError {
	return &APIError{Code: code, Message: message}
}

// WithCause attaches the underlying error to the APIError.
// The cause is logged but not serialized to the client.
func (e *APIError) WithCause(cause error) *APIError {
	e.Cause = cause
	return e
}

// WithDetails attaches per-field validation errors. The
// central handler copies these into the wire envelope.
func (e *APIError) WithDetails(details ...FieldError) *APIError {
	e.Details = append(e.Details, details...)
	return e
}

// WithStatus overrides the default HTTP status for this
// specific error. Use sparingly; prefer adding a sentinel
// so the mapping table stays the source of truth.
func (e *APIError) WithStatus(status int) *APIError {
	e.Status = status
	return e
}

// Error implements the error interface. When the APIError
// was built from a sentinel via Wrap, the sentinel's
// message is preferred for log readability; the APIError's
// own Message remains the user-facing text.
func (e *APIError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the cause to errors.Is / errors.As.
func (e *APIError) Unwrap() error { return e.Cause }

// Wrap returns a new error that wraps err with the given
// sentinel, preserving the original via %w so errors.Is
// still matches. The returned error is a plain *fmt.wrapError
// carrying the sentinel; the central handler will recover
// the sentinel and map it. Use this when you do not need a
// custom Message or Details.
func Wrap(sentinel error, msg string) error {
	return fmt.Errorf("%s: %w", msg, sentinel)
}
