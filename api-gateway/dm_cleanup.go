package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const dmInboxCleanupInterval = time.Minute

type dmInboxExpiryStore interface {
	DeleteExpiredDMInbox(ctx context.Context) (int64, error)
	DeleteExpiredDMSendIdempotency(ctx context.Context) (int64, error)
}

func runDMInboxJanitor(ctx context.Context, queries dmInboxExpiryStore) {
	ticker := time.NewTicker(dmInboxCleanupInterval)
	defer ticker.Stop()

	for {
		if err := purgeExpiredDMInbox(ctx, queries); err != nil && ctx.Err() == nil {
			slog.Error("api-gateway: expire DM relay data", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func purgeExpiredDMInbox(ctx context.Context, queries dmInboxExpiryStore) error {
	for {
		deletedInbox, err := queries.DeleteExpiredDMInbox(ctx)
		if err != nil {
			return err
		}
		deletedIdempotency, err := queries.DeleteExpiredDMSendIdempotency(ctx)
		if err != nil {
			return err
		}
		if deletedInbox < 1000 && deletedIdempotency < 1000 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

var _ dmInboxExpiryStore = (*db.Queries)(nil)
