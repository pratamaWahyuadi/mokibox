package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type dmExpiryStore interface {
	DeleteExpiredDMInbox(ctx context.Context) (int64, error)
	DeleteExpiredDMIdempotency(ctx context.Context) (int64, error)
}

func runDMJanitor(ctx context.Context, queries *db.Queries, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sweep := func() {
		if err := sweepExpiredDM(ctx, queries); err != nil {
			slog.Error("DM expiry sweep failed", "err", err)
		}
	}
	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func sweepExpiredDM(ctx context.Context, store dmExpiryStore) error {
	if store == nil {
		return fmt.Errorf("DM expiry store is nil")
	}
	if _, err := store.DeleteExpiredDMIdempotency(ctx); err != nil {
		return fmt.Errorf("delete expired DM idempotency records: %w", err)
	}
	if _, err := store.DeleteExpiredDMInbox(ctx); err != nil {
		return fmt.Errorf("delete expired DM inbox messages: %w", err)
	}
	return nil
}
