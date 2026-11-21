package main

import (
	"context"
	"errors"
	"testing"
)

type dmExpiryStub struct {
	deleted            []int64
	deletedIdempotency []int64
	err                error
	idempotencyErr     error
	calls              int
	idempotencyCalls   int
}

func (s *dmExpiryStub) DeleteExpiredDMInbox(context.Context) (int64, error) {
	s.calls++
	if s.err != nil {
		return 0, s.err
	}
	if s.calls > len(s.deleted) {
		return 0, nil
	}
	return s.deleted[s.calls-1], nil
}

func (s *dmExpiryStub) DeleteExpiredDMSendIdempotency(context.Context) (int64, error) {
	s.idempotencyCalls++
	if s.idempotencyErr != nil {
		return 0, s.idempotencyErr
	}
	if s.idempotencyCalls > len(s.deletedIdempotency) {
		return 0, nil
	}
	return s.deletedIdempotency[s.idempotencyCalls-1], nil
}

func TestPurgeExpiredDMInboxContinuesFullBatches(t *testing.T) {
	store := &dmExpiryStub{
		deleted:            []int64{1000, 1000, 8},
		deletedIdempotency: []int64{0, 0, 0},
	}
	if err := purgeExpiredDMInbox(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if store.calls != 3 {
		t.Fatalf("delete calls = %d, want 3", store.calls)
	}
	if store.idempotencyCalls != 3 {
		t.Fatalf("idempotency delete calls = %d, want 3", store.idempotencyCalls)
	}
}

func TestPurgeExpiredDMInboxContinuesFullIdempotencyBatches(t *testing.T) {
	store := &dmExpiryStub{
		deleted:            []int64{0, 0},
		deletedIdempotency: []int64{1000, 6},
	}
	if err := purgeExpiredDMInbox(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if store.idempotencyCalls != 2 {
		t.Fatalf("idempotency delete calls = %d, want 2", store.idempotencyCalls)
	}
}

func TestPurgeExpiredDMInboxReturnsStoreError(t *testing.T) {
	want := errors.New("database unavailable")
	store := &dmExpiryStub{err: want}
	if err := purgeExpiredDMInbox(context.Background(), store); !errors.Is(err, want) {
		t.Fatalf("error = %v, want wrapped %v", err, want)
	}
}

func TestPurgeExpiredDMInboxReturnsIdempotencyStoreError(t *testing.T) {
	want := errors.New("database unavailable")
	store := &dmExpiryStub{idempotencyErr: want}
	if err := purgeExpiredDMInbox(context.Background(), store); !errors.Is(err, want) {
		t.Fatalf("error = %v, want wrapped %v", err, want)
	}
}
