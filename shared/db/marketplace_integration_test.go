//go:build integration

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMarketplaceQueriesIntegration(t *testing.T) {
	sqlDB, q := newIntegrationDB(t)
	ctx := context.Background()
	seller := newTempUser(t, sqlDB, q)
	buyer := newTempUser(t, sqlDB, q)

	categories, err := q.ListMarketplaceCategories(ctx)
	if err != nil {
		t.Fatalf("ListMarketplaceCategories: %v", err)
	}
	if len(categories) != 8 {
		t.Fatalf("category count = %d, want 8", len(categories))
	}

	listing, err := q.CreateMarketplaceListing(ctx, CreateMarketplaceListingParams{
		SellerUserID: seller.ID, CategorySlug: "electronics", Title: "Smoke-test camera",
		Description: "Temporary SQL integration listing", PriceIdr: 1200000,
		City: "Jakarta", ItemCondition: "good",
		Latitude:  sql.NullFloat64{Float64: -6.2088, Valid: true},
		Longitude: sql.NullFloat64{Float64: 106.8456, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateMarketplaceListing: %v", err)
	}

	browse, err := q.ListMarketplaceListings(ctx, ListMarketplaceListingsParams{
		ViewerID:     buyer.ID,
		CategorySlug: sql.NullString{String: "electronics", Valid: true},
		City:         sql.NullString{String: "jakarta", Valid: true},
		QueryText:    sql.NullString{String: "camera", Valid: true},
		MinPrice:     sql.NullInt64{Int64: 1000000, Valid: true},
		MaxPrice:     sql.NullInt64{Int64: 1500000, Valid: true},
		PageSize:     10,
	})
	if err != nil {
		t.Fatalf("ListMarketplaceListings: %v", err)
	}
	if len(browse) != 1 || browse[0].ID != listing.ID {
		t.Fatalf("filtered browse rows = %+v, want listing %s", browse, listing.ID)
	}

	near, err := q.ListMarketplaceListings(ctx, ListMarketplaceListingsParams{
		ViewerID:  buyer.ID,
		QueryText: sql.NullString{String: "camera", Valid: true},
		Latitude:  sql.NullFloat64{Float64: -6.21, Valid: true},
		Longitude: sql.NullFloat64{Float64: 106.84, Valid: true},
		RadiusKm:  sql.NullFloat64{Float64: 5, Valid: true},
		PageSize:  10,
	})
	if err != nil {
		t.Fatalf("ListMarketplaceListings radius: %v", err)
	}
	if len(near) != 1 || near[0].ID != listing.ID {
		t.Fatalf("radius browse rows = %+v, want listing %s", near, listing.ID)
	}
	distance, ok := near[0].DistanceKm.(float64)
	if !ok || distance < 0 || distance > 5 {
		t.Fatalf("distance_km = %v (%T), want 0..5", near[0].DistanceKm, near[0].DistanceKm)
	}
	far, err := q.ListMarketplaceListings(ctx, ListMarketplaceListingsParams{
		ViewerID:  buyer.ID,
		QueryText: sql.NullString{String: "camera", Valid: true},
		Latitude:  sql.NullFloat64{Float64: 0, Valid: true},
		Longitude: sql.NullFloat64{Float64: 0, Valid: true},
		RadiusKm:  sql.NullFloat64{Float64: 1, Valid: true},
		PageSize:  10,
	})
	if err != nil {
		t.Fatalf("ListMarketplaceListings far radius: %v", err)
	}
	if len(far) != 0 {
		t.Fatalf("far radius browse rows = %+v, want none", far)
	}

	sellerID, err := q.GetMarketplaceContactSeller(ctx, GetMarketplaceContactSellerParams{
		ID: listing.ID, ID_2: buyer.ID,
	})
	if err != nil {
		t.Fatalf("GetMarketplaceContactSeller: %v", err)
	}
	if sellerID != seller.ID {
		t.Fatalf("seller id = %s, want %s", sellerID, seller.ID)
	}
	if _, err := q.InsertMarketplaceContact(ctx, InsertMarketplaceContactParams{
		ID: listing.ID, BuyerUserID: buyer.ID, SellerUserID: sellerID,
	}); err != nil {
		t.Fatalf("InsertMarketplaceContact: %v", err)
	}
	for _, pair := range []IsDMRecipientAllowedParams{
		{ID: buyer.ID, ID_2: seller.ID},
		{ID: seller.ID, ID_2: buyer.ID},
	} {
		allowed, err := q.IsDMRecipientAllowed(ctx, pair)
		if err != nil || !allowed {
			t.Fatalf("contact DM permission for %s -> %s = %v, err=%v", pair.ID, pair.ID_2, allowed, err)
		}
	}

	if _, err := sqlDB.ExecContext(ctx, `
		UPDATE marketplace_contacts
		SET created_at = NOW() - INTERVAL '31 days',
		    expires_at = NOW() - INTERVAL '1 day'
		WHERE listing_id = $1 AND buyer_user_id = $2`, listing.ID, buyer.ID); err != nil {
		t.Fatalf("expire marketplace contact: %v", err)
	}
	allowed, err := q.IsDMRecipientAllowed(ctx, IsDMRecipientAllowedParams{ID: buyer.ID, ID_2: seller.ID})
	if err != nil || allowed {
		t.Fatalf("expired contact DM permission = %v, err=%v; want false", allowed, err)
	}
	renewed, err := q.RenewExpiredMarketplaceContact(ctx, RenewExpiredMarketplaceContactParams{
		ListingID: listing.ID, BuyerUserID: buyer.ID,
	})
	if err != nil || renewed != 1 {
		t.Fatalf("RenewExpiredMarketplaceContact rows = %d, err=%v", renewed, err)
	}
	allowed, err = q.IsDMRecipientAllowed(ctx, IsDMRecipientAllowedParams{ID: buyer.ID, ID_2: seller.ID})
	if err != nil || !allowed {
		t.Fatalf("renewed contact DM permission = %v, err=%v; want true", allowed, err)
	}

	if _, err := q.UpdateMarketplaceListingStatus(ctx, UpdateMarketplaceListingStatusParams{
		ID: listing.ID, SellerUserID: seller.ID, Status: "sold",
	}); err != nil {
		t.Fatalf("mark listing sold: %v", err)
	}
	allowed, err = q.IsDMRecipientAllowed(ctx, IsDMRecipientAllowedParams{ID: buyer.ID, ID_2: seller.ID})
	if err != nil || !allowed {
		t.Fatalf("sold listing DM permission = %v, err=%v; want permission retained", allowed, err)
	}

	revoked, err := q.RevokeMarketplaceContact(ctx, RevokeMarketplaceContactParams{
		ListingID: listing.ID, BuyerUserID: seller.ID,
	})
	if err != nil || revoked != 1 {
		t.Fatalf("RevokeMarketplaceContact rows = %d, err=%v", revoked, err)
	}
	allowed, err = q.IsDMRecipientAllowed(ctx, IsDMRecipientAllowedParams{ID: buyer.ID, ID_2: seller.ID})
	if err != nil || allowed {
		t.Fatalf("revoked contact DM permission = %v, err=%v; want false", allowed, err)
	}
	image, err := q.CreateMarketplaceListingImage(ctx, CreateMarketplaceListingImageParams{
		ListingID: listing.ID, R2Key: "marketplace/integration/" + listing.ID.String() + ".png",
		ContentType: "image/png", SortOrder: 0,
		ExpiresAt: sql.NullTime{Time: time.Now().Add(15 * time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateMarketplaceListingImage: %v", err)
	}
	keys, err := q.ListMarketplaceImageKeysBySeller(ctx, seller.ID)
	if err != nil || len(keys) != 1 || keys[0] != image.R2Key {
		t.Fatalf("ListMarketplaceImageKeysBySeller = %v, err=%v", keys, err)
	}

	if err := q.DeleteMarketplaceContactsByUser(ctx, buyer.ID); err != nil {
		t.Fatalf("DeleteMarketplaceContactsByUser: %v", err)
	}
	if err := q.DeleteMarketplaceReportsByUser(ctx, seller.ID); err != nil {
		t.Fatalf("DeleteMarketplaceReportsByUser: %v", err)
	}
	if err := q.DeleteMarketplaceFavoritesByUser(ctx, seller.ID); err != nil {
		t.Fatalf("DeleteMarketplaceFavoritesByUser: %v", err)
	}
	if err := q.DeleteMarketplaceListingsBySeller(ctx, seller.ID); err != nil {
		t.Fatalf("DeleteMarketplaceListingsBySeller: %v", err)
	}
	if keys, err = q.ListMarketplaceImageKeysBySeller(ctx, seller.ID); err != nil || len(keys) != 0 {
		t.Fatalf("verify Marketplace image cleanup = %v, err=%v; want no keys", keys, err)
	}
	allowed, err = q.IsDMRecipientAllowed(ctx, IsDMRecipientAllowedParams{ID: buyer.ID, ID_2: seller.ID})
	if err != nil || allowed {
		t.Fatalf("deleted account contact permission = %v, err=%v; want false", allowed, err)
	}
}

func TestMarketplaceListingQuotaLockIntegration(t *testing.T) {
	sqlDB, q := newIntegrationDB(t)
	ctx := context.Background()
	seller := newTempUser(t, sqlDB, q)
	t.Cleanup(func() {
		if err := q.DeleteMarketplaceListingsBySeller(ctx, seller.ID); err != nil {
			t.Errorf("cleanup: delete temp listings: %v", err)
		}
	})

	lockKey := "marketplace:listings:" + seller.ID.String()
	var created atomic.Int64
	var wg sync.WaitGroup
	errCh := make(chan error, 21)
	for i := 0; i < 21; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := sqlDB.BeginTx(ctx, &sql.TxOptions{})
			if err != nil {
				errCh <- err
				return
			}
			defer tx.Rollback()
			qtx := q.WithTx(tx)
			if err := qtx.LockMarketplaceQuota(ctx, lockKey); err != nil {
				errCh <- err
				return
			}
			count, err := qtx.CountMarketplaceListingsBySellerSince(ctx, CountMarketplaceListingsBySellerSinceParams{
				SellerUserID: seller.ID, CreatedAt: time.Now().Add(-24 * time.Hour),
			})
			if err != nil {
				errCh <- err
				return
			}
			if count >= 20 {
				return
			}
			if _, err := qtx.CreateMarketplaceListing(ctx, CreateMarketplaceListingParams{
				SellerUserID: seller.ID, CategorySlug: "electronics",
				Title:       fmt.Sprintf("Concurrent quota listing %d", i),
				Description: "Temporary SQL integration listing", PriceIdr: 100000,
				City: "Jakarta", ItemCondition: "good",
			}); err != nil {
				errCh <- err
				return
			}
			if err := tx.Commit(); err != nil {
				errCh <- err
				return
			}
			created.Add(1)
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent quota request: %v", err)
	}
	if created.Load() != 20 {
		t.Fatalf("created listings = %d, want 20", created.Load())
	}
	count, err := q.CountMarketplaceListingsBySellerSince(ctx, CountMarketplaceListingsBySellerSinceParams{
		SellerUserID: seller.ID, CreatedAt: time.Now().Add(-24 * time.Hour),
	})
	if err != nil || count != 20 {
		t.Fatalf("persistent listing count = %d, err=%v; want 20", count, err)
	}
}

func TestMarketplaceVisibilitySweepAndCleanupOutboxIntegration(t *testing.T) {
	sqlDB, q := newIntegrationDB(t)
	ctx := context.Background()
	seller := newTempUser(t, sqlDB, q)
	buyer := newTempUser(t, sqlDB, q)
	t.Cleanup(func() {
		if err := q.DeleteMarketplaceListingsBySeller(ctx, seller.ID); err != nil {
			t.Errorf("cleanup listings: %v", err)
		}
	})
	var outboxIDs []int64
	t.Cleanup(func() {
		for _, id := range outboxIDs {
			if _, err := sqlDB.ExecContext(ctx, `DELETE FROM marketplace_cleanup_outbox WHERE id = $1`, id); err != nil {
				t.Errorf("cleanup outbox row %d: %v", id, err)
			}
		}
	})

	listing, err := q.CreateMarketplaceListing(ctx, CreateMarketplaceListingParams{
		SellerUserID: seller.ID, CategorySlug: "electronics", Title: "Visibility test",
		Description: "Temporary listing", PriceIdr: 500000, City: "Jakarta", ItemCondition: "new",
	})
	if err != nil {
		t.Fatalf("create listing: %v", err)
	}

	browse := ListMarketplaceListingsParams{ViewerID: buyer.ID, PageSize: 10}
	rows, err := q.ListMarketplaceListings(ctx, browse)
	if err != nil || len(rows) != 1 || rows[0].ID != listing.ID {
		t.Fatalf("active browse = %+v, err=%v", rows, err)
	}
	if _, err := q.GetMarketplaceListing(ctx, GetMarketplaceListingParams{ID: listing.ID, UserID: buyer.ID}); err != nil {
		t.Fatalf("active detail for buyer: %v", err)
	}

	if _, err := q.UpdateMarketplaceListingStatus(ctx, UpdateMarketplaceListingStatusParams{
		ID: listing.ID, SellerUserID: seller.ID, Status: "archived",
	}); err != nil {
		t.Fatalf("archive listing: %v", err)
	}
	rows, err = q.ListMarketplaceListings(ctx, browse)
	if err != nil || len(rows) != 0 {
		t.Fatalf("archived browse for buyer = %+v, err=%v; want empty", rows, err)
	}
	if _, err := q.GetMarketplaceListing(ctx, GetMarketplaceListingParams{ID: listing.ID, UserID: buyer.ID}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("archived detail for buyer err = %v, want sql.ErrNoRows", err)
	}
	if _, err := q.GetMarketplaceListing(ctx, GetMarketplaceListingParams{ID: listing.ID, UserID: seller.ID}); err != nil {
		t.Fatalf("archived detail for seller: %v", err)
	}

	pendingKey := "marketplace/integration/outbox-" + listing.ID.String() + ".png"
	if err := q.InsertMarketplaceCleanupOutbox(ctx, []string{pendingKey}); err != nil {
		t.Fatalf("insert outbox row: %v", err)
	}
	due, err := q.ListDueMarketplaceCleanupOutbox(ctx, 10)
	if err != nil || len(due) != 1 || len(due[0].R2Keys) != 1 || due[0].R2Keys[0] != pendingKey {
		t.Fatalf("due outbox = %+v, err=%v", due, err)
	}
	outboxIDs = append(outboxIDs, due[0].ID)
	if err := q.MarkMarketplaceCleanupOutboxDone(ctx, due[0].ID); err != nil {
		t.Fatalf("mark outbox done: %v", err)
	}
	if due, err = q.ListDueMarketplaceCleanupOutbox(ctx, 10); err != nil || len(due) != 0 {
		t.Fatalf("done outbox still due = %+v, err=%v", due, err)
	}

	failedKey := "marketplace/integration/failed-" + listing.ID.String() + ".png"
	if err := q.InsertMarketplaceCleanupOutbox(ctx, []string{failedKey}); err != nil {
		t.Fatalf("insert failed outbox row: %v", err)
	}
	due, err = q.ListDueMarketplaceCleanupOutbox(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due failed outbox = %+v, err=%v", due, err)
	}
	outboxIDs = append(outboxIDs, due[0].ID)
	if err := q.FailMarketplaceCleanupOutbox(ctx, FailMarketplaceCleanupOutboxParams{
		ID: due[0].ID, MaxAttempts: 10, NextAttemptAt: time.Now().Add(time.Hour), LastError: "redis down",
	}); err != nil {
		t.Fatalf("fail outbox row: %v", err)
	}
	if due, err = q.ListDueMarketplaceCleanupOutbox(ctx, 10); err != nil || len(due) != 0 {
		t.Fatalf("backed-off outbox still due = %+v, err=%v", due, err)
	}

	imageKey := "marketplace/integration/expired-" + listing.ID.String() + ".png"
	if _, err := q.CreateMarketplaceListingImage(ctx, CreateMarketplaceListingImageParams{
		ListingID: listing.ID, R2Key: imageKey, ContentType: "image/png",
		ExpiresAt: sql.NullTime{Time: time.Now().Add(-time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("create expired image: %v", err)
	}
	swept, err := q.SweepExpiredMarketplaceListingImages(ctx, 10)
	if err != nil || len(swept) != 1 || swept[0].R2Key != imageKey {
		t.Fatalf("swept images = %+v, err=%v", swept, err)
	}
	if keys, err := q.ListMarketplaceImageKeysBySeller(ctx, seller.ID); err != nil || len(keys) != 0 {
		t.Fatalf("image keys after sweep = %v, err=%v; want empty", keys, err)
	}
	if due, err = q.ListDueMarketplaceCleanupOutbox(ctx, 10); err != nil || len(due) != 0 {
		t.Fatalf("SQL sweep outbox = %+v, err=%v; want empty (handler enqueues cleanup)", due, err)
	}
	if err := q.InsertMarketplaceCleanupOutbox(ctx, []string{imageKey}); err != nil {
		t.Fatalf("insert swept key into outbox: %v", err)
	}
	if due, err = q.ListDueMarketplaceCleanupOutbox(ctx, 10); err != nil || len(due) != 1 || len(due[0].R2Keys) != 1 || due[0].R2Keys[0] != imageKey {
		t.Fatalf("swept key outbox = %+v, err=%v", due, err)
	}
	outboxIDs = append(outboxIDs, due[0].ID)
}
