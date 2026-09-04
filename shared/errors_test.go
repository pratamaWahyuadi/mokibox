package shared

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestErrorMapping pins the sentinel -> (HTTP status, wire code)
// mapping for all 13 sentinels. The tables in errors.go are
// the single source of truth for the wire contract; a silent
// edit here would change every client's error handling.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		sentinel error
		status   int
		code     ErrorCode
	}{
		{ErrValidation, http.StatusBadRequest, CodeValidationError},
		{ErrUnauthorized, http.StatusUnauthorized, CodeUnauthorized},
		{ErrForbidden, http.StatusForbidden, CodeForbidden},
		{ErrNotFound, http.StatusNotFound, CodeNotFound},
		{ErrVideoStatusConflict, http.StatusConflict, CodeVideoStatusConflict},
		{ErrVideoNotReady, http.StatusConflict, CodeVideoNotReady},
		{ErrUploadMissing, http.StatusConflict, CodeUploadMissing},
		{ErrUploadSizeInvalid, http.StatusBadRequest, CodeUploadSizeInvalid},
		{ErrSelfFollow, http.StatusBadRequest, CodeSelfFollowNotAllowed},
		{ErrWebhookSignature, http.StatusUnauthorized, CodeWebhookInvalidSignature},
		{ErrWebhookEvent, http.StatusBadRequest, CodeWebhookEventUnsupported},
		{ErrRateLimited, http.StatusTooManyRequests, CodeRateLimited},
		{ErrInternal, http.StatusInternalServerError, CodeInternalError},
	}
	for _, c := range cases {
		t.Run(string(c.code), func(t *testing.T) {
			// Direct sentinel.
			if got := httpStatusFor(c.sentinel); got != c.status {
				t.Errorf("httpStatusFor(%v) = %d, want %d", c.sentinel, got, c.status)
			}
			if got := codeFor(c.sentinel); got != c.code {
				t.Errorf("codeFor(%v) = %s, want %s", c.sentinel, got, c.code)
			}
			// Wrapped sentinel must still match (handlers wrap
			// with fmt.Errorf("...: %w", ErrXxx)).
			wrapped := fmt.Errorf("handler context: %w", c.sentinel)
			if got := httpStatusFor(wrapped); got != c.status {
				t.Errorf("httpStatusFor(wrapped %v) = %d, want %d", c.sentinel, got, c.status)
			}
			if got := codeFor(wrapped); got != c.code {
				t.Errorf("codeFor(wrapped %v) = %s, want %s", c.sentinel, got, c.code)
			}
			// ClassifyError must agree with the tables.
			s, gotCode, _, _ := ClassifyError(wrapped)
			if s != c.status || gotCode != c.code {
				t.Errorf("ClassifyError(wrapped %v) = (%d, %s), want (%d, %s)",
					c.sentinel, s, gotCode, c.status, c.code)
			}
		})
	}
}

func TestErrorMapping_UnknownFallsToInternal(t *testing.T) {
	unknown := errors.New("something unexpected")
	if got := httpStatusFor(unknown); got != http.StatusInternalServerError {
		t.Errorf("unknown error status = %d, want 500", got)
	}
	if got := codeFor(unknown); got != CodeInternalError {
		t.Errorf("unknown error code = %s, want INTERNAL_ERROR", got)
	}
}

func TestErrorMapping_APIErrorOverrides(t *testing.T) {
	// *APIError with explicit Code/Status wins over the
	// sentinel mapping so callers can diverge when needed.
	api := NewAPIError(CodeValidationError, "custom message").
		WithStatus(http.StatusUnprocessableEntity).
		WithDetails(FieldError{Field: "title", Message: "required"})
	status, code, msg, details := ClassifyError(api)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", status)
	}
	if code != CodeValidationError {
		t.Errorf("code = %s, want VALIDATION_ERROR", code)
	}
	if msg != "custom message" {
		t.Errorf("message = %q, want %q", msg, "custom message")
	}
	if len(details) != 1 || details[0].Field != "title" {
		t.Errorf("details = %+v, want one title field error", details)
	}
}

func TestErrorMapping_APIErrorDefaultsFromSentinel(t *testing.T) {
	// Zero Code/Status on the APIError falls back to the
	// sentinel table via the wrapped cause.
	api := NewAPIError("", "plain").WithCause(ErrNotFound)
	status, code, _, _ := ClassifyError(api)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if code != CodeNotFound {
		t.Errorf("code = %s, want NOT_FOUND", code)
	}
	// errors.Is still reaches the sentinel through Unwrap.
	if !errors.Is(api, ErrNotFound) {
		t.Error("errors.Is(APIError with ErrNotFound cause) should be true")
	}
}

func TestErrorMapping_WrapKeepsSentinelMatchable(t *testing.T) {
	wrapped := Wrap(ErrVideoNotReady, "video 123 is still processing")
	if !errors.Is(wrapped, ErrVideoNotReady) {
		t.Error("Wrap must keep the sentinel matchable via errors.Is")
	}
	if got := httpStatusFor(wrapped); got != http.StatusConflict {
		t.Errorf("wrapped status = %d, want 409", got)
	}
}
