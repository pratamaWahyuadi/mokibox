package main

import (
	"errors"
	"testing"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

type requestValidatorTestBody struct {
	Category string `json:"category" validate:"required,oneof=electronics vehicles"`
	PriceIDR int64  `json:"price_idr" validate:"gt=0"`
}

func TestRequestValidatorUsesJSONFieldNames(t *testing.T) {
	err := NewRequestValidator().Validate(&requestValidatorTestBody{Category: "unknown"})
	var apiErr *shared.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *shared.APIError", err)
	}
	if len(apiErr.Details) != 2 {
		t.Fatalf("details = %+v", apiErr.Details)
	}
	fields := map[string]string{}
	for _, detail := range apiErr.Details {
		fields[detail.Field] = detail.Message
	}
	if _, ok := fields["category"]; !ok {
		t.Fatalf("category detail missing: %+v", fields)
	}
	if _, ok := fields["price_idr"]; !ok {
		t.Fatalf("price_idr detail missing: %+v", fields)
	}
	if want := "price_idr must be greater than 0"; fields["price_idr"] != want {
		t.Fatalf("price message = %q, want %q", fields["price_idr"], want)
	}
}
