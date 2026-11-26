package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hibiken/asynq"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type marketplaceJanitorFakeStore struct {
	rows       []db.ListDueMarketplaceCleanupOutboxRow
	listErr    error
	doneIDs    []int64
	doneErr    error
	failCalls  []db.FailMarketplaceCleanupOutboxParams
	failErr    error
	pruneCalls []int64
	pruneRows  int64
	pruneErr   error
}

func (s *marketplaceJanitorFakeStore) ListDueMarketplaceCleanupOutbox(context.Context, int32) ([]db.ListDueMarketplaceCleanupOutboxRow, error) {
	return s.rows, s.listErr
}

func (s *marketplaceJanitorFakeStore) MarkMarketplaceCleanupOutboxDone(_ context.Context, id int64) error {
	if s.doneErr != nil {
		return s.doneErr
	}
	s.doneIDs = append(s.doneIDs, id)
	return nil
}

func (s *marketplaceJanitorFakeStore) FailMarketplaceCleanupOutbox(_ context.Context, arg db.FailMarketplaceCleanupOutboxParams) error {
	if s.failErr != nil {
		return s.failErr
	}
	s.failCalls = append(s.failCalls, arg)
	return nil
}

func (s *marketplaceJanitorFakeStore) PruneMarketplaceCleanupOutbox(_ context.Context, retention int64) (int64, error) {
	s.pruneCalls = append(s.pruneCalls, retention)
	return s.pruneRows, s.pruneErr
}

type marketplaceJanitorFakeSweeper struct {
	batch int32
	rows  int
	err   error
}

func (s *marketplaceJanitorFakeSweeper) SweepExpiredImages(_ context.Context, batch int32) (int, error) {
	s.batch = batch
	return s.rows, s.err
}

type marketplaceJanitorFakeEnqueuer struct {
	payloads []shared.CleanupObjectsPayload
	err      error
}

func (e *marketplaceJanitorFakeEnqueuer) EnqueueCleanupObjects(payload shared.CleanupObjectsPayload) (*asynq.TaskInfo, error) {
	if e.err != nil {
		return nil, e.err
	}
	e.payloads = append(e.payloads, payload)
	return &asynq.TaskInfo{}, nil
}

func TestDispatchMarketplaceCleanupClosesAndRetriesRows(t *testing.T) {
	store := &marketplaceJanitorFakeStore{rows: []db.ListDueMarketplaceCleanupOutboxRow{
		{ID: 1},
		{ID: 2, R2Keys: []string{"marketplace/listings/a.png"}, Attempts: 1},
	}}
	enqueuer := &marketplaceJanitorFakeEnqueuer{}

	dispatched, err := dispatchMarketplaceCleanup(context.Background(), store, enqueuer, 25)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1", dispatched)
	}
	if len(store.doneIDs) != 2 || store.doneIDs[0] != 1 || store.doneIDs[1] != 2 {
		t.Fatalf("done IDs = %v", store.doneIDs)
	}
	if len(enqueuer.payloads) != 1 {
		t.Fatalf("enqueue payloads = %+v", enqueuer.payloads)
	}
	if len(enqueuer.payloads[0].Keys) != 1 || enqueuer.payloads[0].Keys[0] != "marketplace/listings/a.png" {
		t.Fatalf("payload = %+v", enqueuer.payloads[0])
	}
}

func TestDispatchMarketplaceCleanupRecordsEnqueueFailure(t *testing.T) {
	store := &marketplaceJanitorFakeStore{rows: []db.ListDueMarketplaceCleanupOutboxRow{
		{ID: 7, R2Keys: []string{"marketplace/listings/c.png"}, Attempts: 3},
	}}
	enqueuer := &marketplaceJanitorFakeEnqueuer{err: errors.New(strings.Repeat("x", marketplaceCleanupErrorLimit+20))}

	dispatched, err := dispatchMarketplaceCleanup(context.Background(), store, enqueuer, 10)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched != 0 {
		t.Fatalf("dispatched = %d, want 0", dispatched)
	}
	if len(store.failCalls) != 1 {
		t.Fatalf("fail calls = %+v", store.failCalls)
	}
	call := store.failCalls[0]
	if call.ID != 7 || call.MaxAttempts != marketplaceCleanupMaxAttempts {
		t.Fatalf("fail call = %+v", call)
	}
	if len(call.LastError) != marketplaceCleanupErrorLimit {
		t.Fatalf("last_error length = %d, want %d", len(call.LastError), marketplaceCleanupErrorLimit)
	}
	if call.NextAttemptAt.Before(time.Now().Add(30 * time.Second)) {
		t.Fatalf("next attempt too soon: %s", call.NextAttemptAt)
	}
}

func TestDispatchMarketplaceCleanupErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := dispatchMarketplaceCleanup(ctx, nil, &marketplaceJanitorFakeEnqueuer{}, 1); err == nil {
		t.Fatal("nil store must fail")
	}
	if _, err := dispatchMarketplaceCleanup(ctx, &marketplaceJanitorFakeStore{}, nil, 1); err == nil {
		t.Fatal("nil enqueuer must fail")
	}
	if _, err := dispatchMarketplaceCleanup(ctx, &marketplaceJanitorFakeStore{}, &marketplaceJanitorFakeEnqueuer{}, 0); err == nil {
		t.Fatal("non-positive batch must fail")
	}

	listErr := errors.New("list failed")
	if _, err := dispatchMarketplaceCleanup(ctx, &marketplaceJanitorFakeStore{listErr: listErr}, &marketplaceJanitorFakeEnqueuer{}, 1); !errors.Is(err, listErr) {
		t.Fatalf("list error = %v", err)
	}

	store := &marketplaceJanitorFakeStore{
		rows:    []db.ListDueMarketplaceCleanupOutboxRow{{ID: 1, R2Keys: []string{"key"}}},
		doneErr: errors.New("done failed"),
	}
	if _, err := dispatchMarketplaceCleanup(ctx, store, &marketplaceJanitorFakeEnqueuer{}, 1); err == nil {
		t.Fatal("done failure must be returned")
	}

	store = &marketplaceJanitorFakeStore{
		rows:    []db.ListDueMarketplaceCleanupOutboxRow{{ID: 1, R2Keys: []string{"key"}}},
		failErr: errors.New("fail update failed"),
	}
	enqueuer := &marketplaceJanitorFakeEnqueuer{err: errors.New("redis down")}
	if _, err := dispatchMarketplaceCleanup(ctx, store, enqueuer, 1); err == nil {
		t.Fatal("fail update error must be returned")
	}
}

func TestSweepMarketplaceImagesValidatesInputs(t *testing.T) {
	ctx := context.Background()
	if _, err := sweepMarketplaceImages(ctx, nil, 10); err == nil {
		t.Fatal("nil sweeper must fail")
	}
	if _, err := sweepMarketplaceImages(ctx, &marketplaceJanitorFakeSweeper{}, 0); err == nil {
		t.Fatal("non-positive batch must fail")
	}
	sweeper := &marketplaceJanitorFakeSweeper{rows: 4}
	rows, err := sweepMarketplaceImages(ctx, sweeper, 12)
	if err != nil || rows != 4 || sweeper.batch != 12 {
		t.Fatalf("rows=%d err=%v batch=%d", rows, err, sweeper.batch)
	}
}

func TestPruneMarketplaceCleanupOutbox(t *testing.T) {
	ctx := context.Background()
	store := &marketplaceJanitorFakeStore{pruneRows: 3}
	rows, err := pruneMarketplaceCleanupOutbox(ctx, store)
	if err != nil || rows != 3 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
	if len(store.pruneCalls) != 1 || store.pruneCalls[0] != int64(marketplaceCleanupRetention/time.Second) {
		t.Fatalf("prune calls = %v", store.pruneCalls)
	}
	if _, err := pruneMarketplaceCleanupOutbox(ctx, nil); err == nil {
		t.Fatal("nil store must fail")
	}
}

func TestMarketplaceCleanupRetryAtCapsBackoff(t *testing.T) {
	first := marketplaceCleanupRetryAt(1)
	if first.Before(time.Now().Add(29*time.Second)) || first.After(time.Now().Add(31*time.Second)) {
		t.Fatalf("first retry = %s", first)
	}
	capped := marketplaceCleanupRetryAt(int32(marketplaceCleanupMaxAttempts))
	if capped.Before(time.Now().Add(marketplaceCleanupRetryCap - time.Second)) {
		t.Fatalf("capped retry = %s", capped)
	}
	if got := truncateMarketplaceCleanupError(strings.Repeat("x", marketplaceCleanupErrorLimit+1)); len(got) != marketplaceCleanupErrorLimit {
		t.Fatalf("truncated length = %d", len(got))
	}
	multibyte := truncateMarketplaceCleanupError(strings.Repeat("é", marketplaceCleanupErrorLimit+1))
	if utf8.RuneCountInString(multibyte) != marketplaceCleanupErrorLimit || !utf8.ValidString(multibyte) {
		t.Fatalf("multibyte truncate = %q (%d runes)", multibyte, utf8.RuneCountInString(multibyte))
	}
}
