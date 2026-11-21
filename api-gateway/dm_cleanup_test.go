package main

import (
	"context"
	"errors"
	"testing"
)

type dmExpiryStub struct {
	deleted []int64
	err     error
	calls   int
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

func TestPurgeExpiredDMInboxContinuesFullBatches(t *testing.T) {
	store := &dmExpiryStub{deleted: []int64{1000, 1000, 8}}
	if err := purgeExpiredDMInbox(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if store.calls != 3 {
		t.Fatalf("delete calls = %d, want 3", store.calls)
	}
}

func TestPurgeExpiredDMInboxReturnsStoreError(t *testing.T) {
	want := errors.New("database unavailable")
	store := &dmExpiryStub{err: want}
	if err := purgeExpiredDMInbox(context.Background(), store); !errors.Is(err, want) {
		t.Fatalf("error = %v, want wrapped %v", err, want)
	}
}
