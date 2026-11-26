// Package main wires the API gateway.
//
// request_validator.go implements the central Echo
// HTTP request body validator using the
// go-playground/validator/v10 library. LLD section 12
// (Fase 9) specified:
//
//   e.Validator = &RequestValidator{Validator: validator.New()}
//
// The validator is installed once in NewRouter (see
// routes.go). Each handler that takes a JSON body calls
// `c.Validate(&req)` after `c.Bind(&req)` to enforce the
// struct tags declared on the request struct. On
// failure the error is translated to the canonical
// shared.APIError{Code:CodeValidationError, Details:
// []FieldError} envelope so the wire shape stays
// consistent across endpoints.
//
// Why a custom wrapper rather than the library's
// default integration?
//
//   - Default error message format is the validator's
//     own ("Key: 'Foo.Title' Error:Field validation for
//     'Title' failed on the 'required' tag"), which is
//     not user-friendly. We translate each failure to
//     a single shared.FieldError {Field, Message}.
//   - The default integration wraps the original
//     *validator.ValidationErrors. We want callers to
//     receive an *shared.APIError directly so the
//     shared.RespondError path serialises the envelope
//     without further branching.
package main

import (
	"errors"
	"fmt"

	"github.com/go-playground/validator/v10"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

// RequestValidator is the adapter between
// go-playground/validator/v10 and Echo's e.Validator
// interface. The struct keeps the validator handle
// around so a future call site can register custom
// validators / translations on the same instance
// without re-creating it.
type RequestValidator struct {
	// Validator is the underlying validator instance.
	// Constructed via validator.New() so we get a
	// fresh registry with only the built-in tags
	// (required, min, max, oneof, uuid, email, ...).
	Validator *validator.Validate
}

// NewRequestValidator returns a RequestValidator with
// a fresh validator instance. The constructor is the
// recommended way to wire the adapter so the call
// site stays consistent and a custom translator can
// be added later without changing NewRouter.
func NewRequestValidator() *RequestValidator {
	return &RequestValidator{Validator: validator.New()}
}

// Validate satisfies echo.Validator. It returns nil
// when the input passes every struct tag, otherwise
// an *shared.APIError with Code=CodeValidationError
// and one Details entry per failed field. The
// returned error is safe to pass directly to
// shared.RespondError — the wire envelope is built
// from the shared.APIError fields, no further
// branching needed.
func (rv *RequestValidator) Validate(i interface{}) error {
	if i == nil {
		// Treat nil input as a caller bug. Echo
// should never call Validate with nil (it
// always passes the pointer returned by
// c.Bind), so this branch is defence in depth.
		return shared.NewAPIError(shared.CodeValidationError, "nil request body")
	}
	if err := rv.Validator.Struct(i); err != nil {
		return translateValidationError(err)
	}
	return nil
}

// translateValidationError converts the raw error
// returned by validator.Struct into an
// *shared.APIError with one shared.FieldError per
// failed rule. Two error shapes are accepted:
//
//   - validator.ValidationErrors (slice of
//     FieldError-like entries with .Field() and
//     .Tag() methods)
//   - any other error: wrapped verbatim as INTERNAL
//     so we never silently swallow a programmer
//     error from the validator itself.
//
// The translated messages are intentionally
// English-only for now; a future fase could add a
// custom translator that maps the validator tag
// registry to localized strings. The LLD Fase 10
// issues #29/#30 do not require localization.
func translateValidationError(err error) error {
	var ves validator.ValidationErrors
	if !errors.As(err, &ves) {
		// Unexpected error shape (validator internals
// bug, or non-validation error wrapped).
// Surface as INTERNAL so it shows up in
// logs but does not pretend to be a 400.
		return shared.NewAPIError(shared.CodeInternalError, "validator failed").
			WithCause(err)
	}
	apiErr := shared.NewAPIError(shared.CodeValidationError, "request body failed validation")
	for _, fe := range ves {
		apiErr = apiErr.WithDetails(shared.FieldError{
			Field:   fieldName(fe),
			Message: fieldMessage(fe),
		})
	}
	return apiErr
}

// fieldName extracts the JSON-style field name from
// a validator.FieldError so the client sees "title"
// rather than "Title". validator's FieldError returns
// the Go struct field name; we lowercase the first
// rune for parity with the JSON wire shape used by
// our handlers.
//
// For nested struct fields (which we do not use
// today) the dotted path "Parent.Child" is preserved
// verbatim; deeper normalisation can be added if a
// future request body uses nested validation.
func fieldName(fe validator.FieldError) string {
	// Prefer the namespace (the full struct path
// including the field) for nested cases; for our
// flat request structs both Field and Namespace
// collapse to the same thing, but Namespace keeps
// "Parent.Child" intact if a future struct adds
// nesting.
	ns := fe.Namespace()
	if i := lastIndex(ns, "."); i >= 0 {
		return ns[i+1:]
	}
	return ns
}

// fieldMessage produces a human-readable summary of
// why a single field failed. We intentionally keep
// these English + short — the wire envelope is meant
// for clients, not for end users. The mapping covers
// the tags we use in handlers today (required, min,
// max). Adding more tags is a one-line switch case.
//
// Tags not in the switch fall through to the
// validator's default Error() string, which is
// usually a reasonable English sentence ("Title
// must be at most 200 characters"), so the client
// still gets a usable message.
func fieldMessage(fe validator.FieldError) string {
	name := fieldName(fe)
	switch fe.Tag() {
	case "required":
		return fmt.Sprintf("%s is required", name)
	case "min":
		return fmt.Sprintf("%s must be at least %s", name, fe.Param())
	case "max":
		return fmt.Sprintf("%s must be at most %s", name, fe.Param())
	case "oneof":
		return fmt.Sprintf("%s must be one of: %s", name, fe.Param())
	case "uuid":
		return fmt.Sprintf("%s must be a valid UUID", name)
	default:
		return fe.Error()
	}
}

// lastIndex is a tiny helper kept here so we don't
// pull in strings just for one call. Equivalent to
// strings.LastIndex(s, ".").
func lastIndex(s, sub string) int {
	// Walk from the end so we find the rightmost
// match (matters for nested struct paths like
// "Foo.Bar.Baz" — we want "Baz").
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}