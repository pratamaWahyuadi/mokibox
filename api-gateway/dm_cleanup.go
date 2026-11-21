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
}

func runDMInboxJanitor(ctx context.Context, queries dmInboxExpiryStore) {
	ticker := time.NewTicker(dmInboxCleanupInterval)
	defer ticker.Stop()

	for {
		if err := purgeExpiredDMInbox(ctx, queries); err != nil && ctx.Err() == nil {
			slog.Error("api-gateway: expire DM inbox", "err", err)
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
		deleted, err := queries.DeleteExpiredDMInbox(ctx)
		if err != nil {
			return err
		}
		if deleted < 1000 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

var _ dmInboxExpiryStore = (*db.Queries)(nil)
