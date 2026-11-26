package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/hibiken/asynq"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const (
	marketplaceCleanupMaxAttempts int32         = 10
	marketplaceCleanupRetryBase   time.Duration = 30 * time.Second
	marketplaceCleanupRetryCap    time.Duration = time.Hour
	marketplaceCleanupErrorLimit  int           = 500
	marketplaceCleanupRetention   time.Duration = 24 * time.Hour
)

type marketplaceCleanupStore interface {
	ListDueMarketplaceCleanupOutbox(context.Context, int32) ([]db.ListDueMarketplaceCleanupOutboxRow, error)
	MarkMarketplaceCleanupOutboxDone(context.Context, int64) error
	FailMarketplaceCleanupOutbox(context.Context, db.FailMarketplaceCleanupOutboxParams) error
	PruneMarketplaceCleanupOutbox(context.Context, int64) (int64, error)
}

type marketplaceImageSweeper interface {
	SweepExpiredImages(context.Context, int32) (int, error)
	ArchiveExpiredListings(context.Context, int32) (int, error)
}

type marketplaceCleanupEnqueuer interface {
	EnqueueCleanupObjects(shared.CleanupObjectsPayload) (*asynq.TaskInfo, error)
}

func runMarketplaceJanitor(ctx context.Context, store marketplaceCleanupStore, sweeper marketplaceImageSweeper, enqueuer marketplaceCleanupEnqueuer, interval time.Duration, batchSize int32) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	tick := func() {
		swept, err := sweepMarketplaceImages(ctx, sweeper, batchSize)
		if err != nil {
			slog.Error("marketplace image expiry sweep failed", "err", err)
		} else if swept > 0 {
			slog.Info("marketplace image expiry sweep", "rows", swept)
		}
		expired, err := sweepMarketplaceListings(ctx, sweeper, batchSize)
		if err != nil {
			slog.Error("marketplace listing expiry sweep failed", "err", err)
		} else if expired > 0 {
			slog.Info("marketplace listing expiry sweep", "rows", expired)
		}
		dispatched, err := dispatchMarketplaceCleanup(ctx, store, enqueuer, batchSize)
		if err != nil {
			slog.Error("marketplace cleanup outbox dispatch failed", "err", err)
		} else if dispatched > 0 {
			slog.Info("marketplace cleanup outbox dispatch", "rows", dispatched)
		}
		pruned, err := pruneMarketplaceCleanupOutbox(ctx, store)
		if err != nil {
			slog.Error("marketplace cleanup outbox prune failed", "err", err)
		} else if pruned > 0 {
			slog.Info("marketplace cleanup outbox prune", "rows", pruned)
		}
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

func sweepMarketplaceImages(ctx context.Context, sweeper marketplaceImageSweeper, batchSize int32) (int, error) {
	if sweeper == nil {
		return 0, fmt.Errorf("marketplace image sweeper is nil")
	}
	if batchSize <= 0 {
		return 0, fmt.Errorf("marketplace sweep batch size must be > 0")
	}
	return sweeper.SweepExpiredImages(ctx, batchSize)
}

func sweepMarketplaceListings(ctx context.Context, sweeper marketplaceImageSweeper, batchSize int32) (int, error) {
	if sweeper == nil {
		return 0, fmt.Errorf("marketplace listing sweeper is nil")
	}
	if batchSize <= 0 {
		return 0, fmt.Errorf("marketplace listing sweep batch size must be > 0")
	}
	return sweeper.ArchiveExpiredListings(ctx, batchSize)
}

func dispatchMarketplaceCleanup(ctx context.Context, store marketplaceCleanupStore, enqueuer marketplaceCleanupEnqueuer, batchSize int32) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("marketplace cleanup store is nil")
	}
	if enqueuer == nil {
		return 0, fmt.Errorf("marketplace cleanup enqueuer is nil")
	}
	if batchSize <= 0 {
		return 0, fmt.Errorf("marketplace cleanup batch size must be > 0")
	}
	rows, err := store.ListDueMarketplaceCleanupOutbox(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("list due marketplace cleanup outbox: %w", err)
	}
	dispatched := 0
	for _, row := range rows {
		if len(row.R2Keys) == 0 {
			if err := store.MarkMarketplaceCleanupOutboxDone(ctx, row.ID); err != nil {
				return dispatched, fmt.Errorf("close empty marketplace cleanup outbox %d: %w", row.ID, err)
			}
			continue
		}
		if _, err := enqueuer.EnqueueCleanupObjects(shared.CleanupObjectsPayload{Keys: row.R2Keys}); err != nil {
			lastError := truncateMarketplaceCleanupError(err.Error())
			failErr := store.FailMarketplaceCleanupOutbox(ctx, db.FailMarketplaceCleanupOutboxParams{
				ID:            row.ID,
				MaxAttempts:   marketplaceCleanupMaxAttempts,
				NextAttemptAt: marketplaceCleanupRetryAt(row.Attempts + 1),
				LastError:     lastError,
			})
			if failErr != nil {
				return dispatched, fmt.Errorf("record marketplace cleanup enqueue failure for %d: %w", row.ID, failErr)
			}
			if row.Attempts+1 >= marketplaceCleanupMaxAttempts {
				slog.Error("marketplace cleanup outbox quarantined", "outbox_id", row.ID,
					"attempts", row.Attempts+1, "last_error", lastError)
			}
			continue
		}
		if err := store.MarkMarketplaceCleanupOutboxDone(ctx, row.ID); err != nil {
			return dispatched, fmt.Errorf("close marketplace cleanup outbox %d: %w", row.ID, err)
		}
		dispatched++
	}
	return dispatched, nil
}

func pruneMarketplaceCleanupOutbox(ctx context.Context, store marketplaceCleanupStore) (int64, error) {
	if store == nil {
		return 0, fmt.Errorf("marketplace cleanup store is nil")
	}
	return store.PruneMarketplaceCleanupOutbox(ctx, int64(marketplaceCleanupRetention/time.Second))
}

func marketplaceCleanupRetryAt(attempt int32) time.Time {
	if attempt < 1 {
		attempt = 1
	}
	delay := marketplaceCleanupRetryBase
	for i := int32(1); i < attempt; i++ {
		delay *= 2
		if delay >= marketplaceCleanupRetryCap {
			delay = marketplaceCleanupRetryCap
			break
		}
	}
	return time.Now().Add(delay)
}

func truncateMarketplaceCleanupError(message string) string {
	if utf8.RuneCountInString(message) <= marketplaceCleanupErrorLimit {
		return message
	}
	return string([]rune(message)[:marketplaceCleanupErrorLimit])
}
