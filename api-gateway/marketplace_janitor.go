package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const (
	marketplaceCleanupMaxAttempts int32         = 10
	marketplaceCleanupRetryBase   time.Duration = 30 * time.Second
	marketplaceCleanupRetryCap    time.Duration = time.Hour
	marketplaceCleanupErrorLimit  int           = 500
)

type marketplaceCleanupStore interface {
	ListDueMarketplaceCleanupOutbox(context.Context, int32) ([]db.ListDueMarketplaceCleanupOutboxRow, error)
	MarkMarketplaceCleanupOutboxDone(context.Context, int64) error
	FailMarketplaceCleanupOutbox(context.Context, db.FailMarketplaceCleanupOutboxParams) error
}

type marketplaceImageSweeper interface {
	SweepExpiredImages(context.Context, int32) (int, error)
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
		dispatched, err := dispatchMarketplaceCleanup(ctx, store, enqueuer, batchSize)
		if err != nil {
			slog.Error("marketplace cleanup outbox dispatch failed", "err", err)
		} else if dispatched > 0 {
			slog.Info("marketplace cleanup outbox dispatch", "rows", dispatched)
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
			failErr := store.FailMarketplaceCleanupOutbox(ctx, db.FailMarketplaceCleanupOutboxParams{
				ID:            row.ID,
				MaxAttempts:   marketplaceCleanupMaxAttempts,
				NextAttemptAt: marketplaceCleanupRetryAt(row.Attempts + 1),
				LastError:     truncateMarketplaceCleanupError(err.Error()),
			})
			if failErr != nil {
				return dispatched, fmt.Errorf("record marketplace cleanup enqueue failure for %d: %w", row.ID, failErr)
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
	if len(message) <= marketplaceCleanupErrorLimit {
		return message
	}
	return message[:marketplaceCleanupErrorLimit]
}
