package main

import (
	"context"
	"errors"
	"testing"
)

type dmJanitorFakeStore struct {
	calls          []string
	idempotencyErr error
	inboxErr       error
}

func (s *dmJanitorFakeStore) DeleteExpiredDMIdempotency(context.Context) (int64, error) {
	s.calls = append(s.calls, "idempotency")
	return 2, s.idempotencyErr
}

func (s *dmJanitorFakeStore) DeleteExpiredDMInbox(context.Context) (int64, error) {
	s.calls = append(s.calls, "inbox")
	return 3, s.inboxErr
}

func TestSweepExpiredDMDeletesIdempotencyThenInbox(t *testing.T) {
	store := &dmJanitorFakeStore{}
	if err := sweepExpiredDM(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 2 || store.calls[0] != "idempotency" || store.calls[1] != "inbox" {
		t.Fatalf("calls = %v", store.calls)
	}
}

func TestSweepExpiredDMStopsOnIdempotencyFailure(t *testing.T) {
	store := &dmJanitorFakeStore{idempotencyErr: errors.New("db down")}
	if err := sweepExpiredDM(context.Background(), store); err == nil {
		t.Fatal("expected error")
	}
	if len(store.calls) != 1 || store.calls[0] != "idempotency" {
		t.Fatalf("calls = %v", store.calls)
	}
}

func TestSweepExpiredDMRejectsNilStore(t *testing.T) {
	if err := sweepExpiredDM(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	}
}
