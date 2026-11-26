package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type marketplaceFakeStore struct {
	browseRows              []db.ListMarketplaceListingsRow
	lastBrowseParams        db.ListMarketplaceListingsParams
	imageRows               []db.MarketplaceListingImage
	listingForLock          db.GetMarketplaceListingForUpdateRow
	contactSellerID         uuid.UUID
	contactRows             int64
	contactCalls            int
	imageForOwner           db.MarketplaceListingImage
	imagePosition           int16
	createdImage            db.MarketplaceListingImage
	confirmRows             int64
	lastConfirmParams       db.ConfirmMarketplaceListingImageParams
	eligible                bool
	listingCount            int64
	reportCount             int64
	contactCount            int64
	hasReport               bool
	participants            db.GetMarketplaceContactParticipantsRow
	participantsParams      db.GetMarketplaceContactParticipantsParams
	revokeRows              int64
	revokeParams            db.RevokeMarketplaceContactParams
	renewRows               int64
	pairStillAllowed        bool
	lockKeys                []string
	pairInboxDeletes        int
	pairIdempotencyDeletes  int
	lastPairInboxParams     db.DeleteDMInboxForPairParams
	lastPairIdempotencyArgs db.DeleteDMIdempotencyForPairMessagesParams
	cleanupOutbox           [][]string
	outboxErr               error
	sweptImages             []db.MarketplaceListingImage
	sweepErr                error
	deleteImageKeys         []string
	lastDeletedImage        db.DeleteMarketplaceListingImageParams
	updateErr               error
	detailRow               db.GetMarketplaceListingRow
	detailErr               error
	lastDetailParams        db.GetMarketplaceListingParams
	lastUpdatedListing      db.UpdateMarketplaceListingParams
	lastStatusUpdate        db.UpdateMarketplaceListingStatusParams
	lastLockedListing       uuid.UUID
	lastImageOwnerParams    db.GetMarketplaceListingImageForOwnerParams
	lastOwnListingParams    db.ListMarketplaceOwnListingsParams
	lastFavoriteInsert      db.InsertMarketplaceFavoriteParams
	lastFavoriteDelete      db.DeleteMarketplaceFavoriteParams
	lastFavoritesParams     db.ListMarketplaceFavoritesParams
	lastReportInsert        db.InsertMarketplaceReportParams
	lastContactLookup       db.GetMarketplaceContactSellerParams
	sellerRows              []db.ListMarketplaceListingsBySellerRow
	lastSellerParams        db.ListMarketplaceListingsBySellerParams
	reportRows              []db.ListMarketplaceReportsRow
	lastReportListParams    db.ListMarketplaceReportsParams
	reviewRows              []db.MarketplaceReport
	lastReviewParams        db.ReviewMarketplaceReportParams
	coverRow                db.MarketplaceListingImage
	coverErr                error
	lastCoverClear          uuid.UUID
	lastCoverParams         db.SetMarketplaceListingImageCoverParams
	expiredListingIDs       []uuid.UUID
	expiredListingKeys      []string
	archiveRows             int64
	lastExpiryBatch         int32
}

func unexpectedMarketplaceCall(name string) error {
	return fmt.Errorf("unexpected Marketplace store call: %s", name)
}

func (s *marketplaceFakeStore) ListMarketplaceCategories(context.Context) ([]db.MarketplaceCategory, error) {
	return nil, unexpectedMarketplaceCall("ListMarketplaceCategories")
}
func (s *marketplaceFakeStore) LockMarketplaceQuota(_ context.Context, key string) error {
	s.lockKeys = append(s.lockKeys, key)
	return nil
}
func (s *marketplaceFakeStore) CreateMarketplaceListing(context.Context, db.CreateMarketplaceListingParams) (db.MarketplaceListing, error) {
	return db.MarketplaceListing{}, unexpectedMarketplaceCall("CreateMarketplaceListing")
}
func (s *marketplaceFakeStore) CountMarketplaceListingsBySellerSince(context.Context, db.CountMarketplaceListingsBySellerSinceParams) (int64, error) {
	return s.listingCount, nil
}
func (s *marketplaceFakeStore) UpdateMarketplaceListing(_ context.Context, arg db.UpdateMarketplaceListingParams) (db.MarketplaceListing, error) {
	s.lastUpdatedListing = arg
	if s.updateErr != nil {
		return db.MarketplaceListing{}, s.updateErr
	}
	return db.MarketplaceListing{ID: arg.ID, SellerUserID: arg.SellerUserID}, nil
}
func (s *marketplaceFakeStore) UpdateMarketplaceListingStatus(_ context.Context, arg db.UpdateMarketplaceListingStatusParams) (db.MarketplaceListing, error) {
	s.lastStatusUpdate = arg
	if s.updateErr != nil {
		return db.MarketplaceListing{}, s.updateErr
	}
	return db.MarketplaceListing{ID: arg.ID, SellerUserID: arg.SellerUserID}, nil
}
func (s *marketplaceFakeStore) GetMarketplaceListing(_ context.Context, arg db.GetMarketplaceListingParams) (db.GetMarketplaceListingRow, error) {
	s.lastDetailParams = arg
	if s.detailErr != nil {
		return db.GetMarketplaceListingRow{}, s.detailErr
	}
	return s.detailRow, nil
}
func (s *marketplaceFakeStore) ListMarketplaceListings(_ context.Context, arg db.ListMarketplaceListingsParams) ([]db.ListMarketplaceListingsRow, error) {
	s.browseRows = append([]db.ListMarketplaceListingsRow(nil), s.browseRows...)
	s.lastBrowseParams = arg
	return s.browseRows, nil
}
func (s *marketplaceFakeStore) ListMarketplaceOwnListings(_ context.Context, arg db.ListMarketplaceOwnListingsParams) ([]db.ListMarketplaceOwnListingsRow, error) {
	s.lastOwnListingParams = arg
	return nil, nil
}
func (s *marketplaceFakeStore) ListMarketplaceListingsBySeller(_ context.Context, arg db.ListMarketplaceListingsBySellerParams) ([]db.ListMarketplaceListingsBySellerRow, error) {
	s.lastSellerParams = arg
	return s.sellerRows, nil
}
func (s *marketplaceFakeStore) ListMarketplaceListingImages(context.Context, uuid.UUID) ([]db.MarketplaceListingImage, error) {
	return nil, unexpectedMarketplaceCall("ListMarketplaceListingImages")
}
func (s *marketplaceFakeStore) ListMarketplaceImagesForListings(context.Context, string) ([]db.MarketplaceListingImage, error) {
	return s.imageRows, nil
}
func (s *marketplaceFakeStore) GetMarketplaceListingImageForOwner(_ context.Context, arg db.GetMarketplaceListingImageForOwnerParams) (db.MarketplaceListingImage, error) {
	s.lastImageOwnerParams = arg
	return s.imageForOwner, nil
}
func (s *marketplaceFakeStore) ConfirmMarketplaceListingImage(_ context.Context, arg db.ConfirmMarketplaceListingImageParams) (int64, error) {
	s.lastConfirmParams = arg
	return s.confirmRows, nil
}
func (s *marketplaceFakeStore) DeleteMarketplaceListingImage(_ context.Context, arg db.DeleteMarketplaceListingImageParams) (string, error) {
	s.lastDeletedImage = arg
	if s.updateErr != nil {
		return "", s.updateErr
	}
	if len(s.deleteImageKeys) == 0 {
		return "", sql.ErrNoRows
	}
	key := s.deleteImageKeys[0]
	s.deleteImageKeys = s.deleteImageKeys[1:]
	return key, nil
}
func (s *marketplaceFakeStore) InsertMarketplaceCleanupOutbox(_ context.Context, keys []string) error {
	if s.outboxErr != nil {
		return s.outboxErr
	}
	s.cleanupOutbox = append(s.cleanupOutbox, append([]string(nil), keys...))
	return nil
}
func (s *marketplaceFakeStore) SweepExpiredMarketplaceListingImages(context.Context, int32) ([]db.MarketplaceListingImage, error) {
	return s.sweptImages, s.sweepErr
}
func (s *marketplaceFakeStore) ListMarketplaceImageKeysBySeller(context.Context, uuid.UUID) ([]string, error) {
	return nil, unexpectedMarketplaceCall("ListMarketplaceImageKeysBySeller")
}
func (s *marketplaceFakeStore) IsMarketplaceFavoriteEligible(context.Context, db.IsMarketplaceFavoriteEligibleParams) (bool, error) {
	return s.eligible, nil
}
func (s *marketplaceFakeStore) InsertMarketplaceFavorite(_ context.Context, arg db.InsertMarketplaceFavoriteParams) (int64, error) {
	s.lastFavoriteInsert = arg
	return 1, nil
}
func (s *marketplaceFakeStore) DeleteMarketplaceFavorite(_ context.Context, arg db.DeleteMarketplaceFavoriteParams) error {
	s.lastFavoriteDelete = arg
	return nil
}
func (s *marketplaceFakeStore) ListMarketplaceFavorites(_ context.Context, arg db.ListMarketplaceFavoritesParams) ([]db.ListMarketplaceFavoritesRow, error) {
	s.lastFavoritesParams = arg
	return nil, nil
}
func (s *marketplaceFakeStore) IsMarketplaceReportEligible(context.Context, db.IsMarketplaceReportEligibleParams) (bool, error) {
	return s.eligible, nil
}
func (s *marketplaceFakeStore) InsertMarketplaceReport(_ context.Context, arg db.InsertMarketplaceReportParams) (int64, error) {
	s.lastReportInsert = arg
	return 1, nil
}
func (s *marketplaceFakeStore) CountMarketplaceReportsByReporterSince(context.Context, db.CountMarketplaceReportsByReporterSinceParams) (int64, error) {
	return s.reportCount, nil
}
func (s *marketplaceFakeStore) HasMarketplaceReport(context.Context, db.HasMarketplaceReportParams) (bool, error) {
	return s.hasReport, nil
}
func (s *marketplaceFakeStore) ListMarketplaceReports(_ context.Context, arg db.ListMarketplaceReportsParams) ([]db.ListMarketplaceReportsRow, error) {
	s.lastReportListParams = arg
	return s.reportRows, nil
}
func (s *marketplaceFakeStore) ReviewMarketplaceReport(_ context.Context, arg db.ReviewMarketplaceReportParams) (db.MarketplaceReport, error) {
	s.lastReviewParams = arg
	if len(s.reviewRows) == 0 {
		return db.MarketplaceReport{}, sql.ErrNoRows
	}
	return s.reviewRows[0], nil
}
func (s *marketplaceFakeStore) GetMarketplaceListingForUpdate(_ context.Context, id uuid.UUID) (db.GetMarketplaceListingForUpdateRow, error) {
	s.lastLockedListing = id
	return s.listingForLock, nil
}
func (s *marketplaceFakeStore) DeleteExpiredMarketplaceListingImages(context.Context, uuid.UUID) ([]db.MarketplaceListingImage, error) {
	return nil, nil
}
func (s *marketplaceFakeStore) GetNextMarketplaceListingImagePosition(context.Context, uuid.UUID) (int16, error) {
	return s.imagePosition, nil
}
func (s *marketplaceFakeStore) CreateMarketplaceListingImage(_ context.Context, arg db.CreateMarketplaceListingImageParams) (db.MarketplaceListingImage, error) {
	row := s.createdImage
	row.ListingID, row.R2Key, row.ContentType, row.SortOrder, row.ExpiresAt = arg.ListingID, arg.R2Key, arg.ContentType, arg.SortOrder, arg.ExpiresAt
	row.SizeBytes = arg.SizeBytes
	return row, nil
}
func (s *marketplaceFakeStore) ClearMarketplaceListingCover(_ context.Context, listingID uuid.UUID) error {
	s.lastCoverClear = listingID
	return nil
}
func (s *marketplaceFakeStore) SetMarketplaceListingImageCover(_ context.Context, arg db.SetMarketplaceListingImageCoverParams) (db.MarketplaceListingImage, error) {
	s.lastCoverParams = arg
	if s.coverErr != nil {
		return db.MarketplaceListingImage{}, s.coverErr
	}
	row := s.coverRow
	row.ID, row.ListingID, row.IsCover = arg.ID, arg.ID_2, true
	return row, nil
}
func (s *marketplaceFakeStore) ListExpiredMarketplaceListingsForUpdate(_ context.Context, batch int32) ([]uuid.UUID, error) {
	s.lastExpiryBatch = batch
	return s.expiredListingIDs, nil
}
func (s *marketplaceFakeStore) ArchiveExpiredMarketplaceListings(_ context.Context, ids []uuid.UUID) (int64, error) {
	return int64(len(ids)), nil
}
func (s *marketplaceFakeStore) ListMarketplaceImageKeysByListings(context.Context, []uuid.UUID) ([]string, error) {
	return s.expiredListingKeys, nil
}
func (s *marketplaceFakeStore) GetMarketplaceContactSeller(_ context.Context, arg db.GetMarketplaceContactSellerParams) (uuid.UUID, error) {
	s.lastContactLookup = arg
	if arg.ID == uuid.Nil || arg.ID_2 == uuid.Nil {
		return uuid.Nil, errors.New("invalid contact lookup")
	}
	return s.contactSellerID, nil
}
func (s *marketplaceFakeStore) InsertMarketplaceContact(_ context.Context, arg db.InsertMarketplaceContactParams) (int64, error) {
	if arg.SellerUserID != s.contactSellerID {
		return 0, errors.New("seller id changed")
	}
	s.contactCalls++
	if s.contactCalls <= int(s.contactRows) {
		return 1, nil
	}
	return 0, nil
}
func (s *marketplaceFakeStore) CountMarketplaceContactsByBuyerSince(context.Context, db.CountMarketplaceContactsByBuyerSinceParams) (int64, error) {
	return s.contactCount, nil
}
func (s *marketplaceFakeStore) GetMarketplaceContactParticipants(_ context.Context, arg db.GetMarketplaceContactParticipantsParams) (db.GetMarketplaceContactParticipantsRow, error) {
	s.participantsParams = arg
	if s.participants == (db.GetMarketplaceContactParticipantsRow{}) {
		return db.GetMarketplaceContactParticipantsRow{}, sql.ErrNoRows
	}
	return s.participants, nil
}
func (s *marketplaceFakeStore) RenewExpiredMarketplaceContact(context.Context, db.RenewExpiredMarketplaceContactParams) (int64, error) {
	return s.renewRows, nil
}
func (s *marketplaceFakeStore) RevokeMarketplaceContact(_ context.Context, arg db.RevokeMarketplaceContactParams) (int64, error) {
	s.revokeParams = arg
	return s.revokeRows, nil
}
func (s *marketplaceFakeStore) IsDMRecipientAllowed(context.Context, db.IsDMRecipientAllowedParams) (bool, error) {
	return s.pairStillAllowed, nil
}
func (s *marketplaceFakeStore) DeleteDMIdempotencyForPairMessages(_ context.Context, arg db.DeleteDMIdempotencyForPairMessagesParams) (int64, error) {
	s.pairIdempotencyDeletes++
	s.lastPairIdempotencyArgs = arg
	return 2, nil
}
func (s *marketplaceFakeStore) DeleteDMInboxForPair(_ context.Context, arg db.DeleteDMInboxForPairParams) (int64, error) {
	s.pairInboxDeletes++
	s.lastPairInboxParams = arg
	return 2, nil
}

type marketplaceFakeRunner struct {
	store     marketplaceTxStore
	committed bool
}

func (r *marketplaceFakeRunner) Run(ctx context.Context, fn func(marketplaceTxStore) error) error {
	if err := fn(r.store); err != nil {
		return err
	}
	r.committed = true
	return nil
}

func TestMarketplaceBrowseParsesFiltersAndReturnsImages(t *testing.T) {
	viewer := viewerUser()
	listingID := uuid.New()
	store := &marketplaceFakeStore{
		browseRows: []db.ListMarketplaceListingsRow{{
			ID: listingID, SellerUserID: uuid.New(), Username: "seller",
			CategorySlug: "home_living", CategoryName: "Home & Living",
			Title: "Desk", Description: "Oak desk", PriceIdr: 750000,
			City: "Jakarta", ItemCondition: "good", Status: "active",
			Latitude:   sql.NullFloat64{Float64: -6.2088, Valid: true},
			Longitude:  sql.NullFloat64{Float64: 106.8456, Valid: true},
			CreatedAt:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			UpdatedAt:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			DistanceKm: float64(1.25),
		}},
	}
	r2 := &stubR2{
		presignGet: func(_ context.Context, key string, _ time.Duration) (string, error) {
			return "https://r2.example/" + key, nil
		},
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, r2, &shared.APIConfig{})
	e := newDMTestEcho(viewer, http.MethodGet, "/api/marketplace/listings", h.Browse)
	rec := doJSON(e, http.MethodGet,
		"/api/marketplace/listings?q=desk&category=home_living&city=jakarta&min_price_idr=100&max_price_idr=900000&latitude=-6.2&longitude=106.8&radius_km=5&limit=7", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.lastBrowseParams.ViewerID != viewer.ID ||
		store.lastBrowseParams.CategorySlug.String != "home_living" ||
		store.lastBrowseParams.City.String != "jakarta" ||
		store.lastBrowseParams.QueryText.String != "desk" ||
		store.lastBrowseParams.MinPrice.Int64 != 100 ||
		store.lastBrowseParams.MaxPrice.Int64 != 900000 ||
		!store.lastBrowseParams.Latitude.Valid || store.lastBrowseParams.Latitude.Float64 != -6.2 ||
		!store.lastBrowseParams.Longitude.Valid || store.lastBrowseParams.Longitude.Float64 != 106.8 ||
		!store.lastBrowseParams.RadiusKm.Valid || store.lastBrowseParams.RadiusKm.Float64 != 5 ||
		store.lastBrowseParams.PageSize != 8 {
		t.Fatalf("browse SQL params = %+v", store.lastBrowseParams)
	}
	var response struct {
		Data []marketplaceListingObject `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0].ID != listingID || response.Data[0].Category.Slug != "home_living" {
		t.Fatalf("browse response = %+v", response.Data)
	}
	if response.Data[0].DistanceKM == nil || *response.Data[0].DistanceKM != 1.25 ||
		response.Data[0].Latitude == nil || *response.Data[0].Latitude != -6.2088 ||
		response.Data[0].Longitude == nil || *response.Data[0].Longitude != 106.8456 {
		t.Fatalf("location response = %+v", response.Data[0])
	}
}

func TestMarketplaceBrowseRejectsPartialLocationFilter(t *testing.T) {
	store := &marketplaceFakeStore{}
	h := newMarketplaceHandlerForTest(store, nil, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(viewerUser(), http.MethodGet, "/api/marketplace/listings", h.Browse)
	for _, target := range []string{
		"/api/marketplace/listings?latitude=-6.2&longitude=106.8",
		"/api/marketplace/listings?latitude=-6.2&radius_km=5",
		"/api/marketplace/listings?latitude=91&longitude=106.8&radius_km=5",
		"/api/marketplace/listings?latitude=-6.2&longitude=106.8&radius_km=201",
	} {
		assertErrCode(t, doJSON(e, http.MethodGet, target, ""), http.StatusBadRequest, shared.CodeValidationError)
	}
}

func TestMarketplaceBrowseRejectsInvertedPriceRange(t *testing.T) {
	store := &marketplaceFakeStore{}
	h := newMarketplaceHandlerForTest(store, nil, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(viewerUser(), http.MethodGet, "/api/marketplace/listings", h.Browse)
	rec := doJSON(e, http.MethodGet, "/api/marketplace/listings?min_price_idr=500&max_price_idr=100", "")
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
}

func TestMarketplaceContactCreatesPersistentDMGrantIdempotently(t *testing.T) {
	buyer, seller := viewerUser(), uuid.New()
	listingID := uuid.New()
	store := &marketplaceFakeStore{
		contactSellerID: seller,
		contactRows:     1,
	}
	runner := &marketplaceFakeRunner{store: store}
	h := newMarketplaceHandlerForTest(store, runner, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(buyer, http.MethodPost, "/api/marketplace/listings/:id/contact", h.ContactSeller)
	path := "/api/marketplace/listings/" + listingID.String() + "/contact"

	for i, wantCreated := range []bool{true, false} {
		rec := doJSON(e, http.MethodPost, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, body=%s", i, rec.Code, rec.Body.String())
		}
		var response struct {
			Data marketplaceContactResponse `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Data.ListingID != listingID || response.Data.SellerID != seller || response.Data.Created != wantCreated {
			t.Fatalf("request %d response = %+v", i, response.Data)
		}
	}
	if !runner.committed || store.contactCalls != 2 {
		t.Fatalf("contact transaction committed=%v, insert calls=%d", runner.committed, store.contactCalls)
	}
	if len(store.lockKeys) != 4 {
		t.Fatalf("quota lock keys = %v", store.lockKeys)
	}
}

func TestMarketplaceContactRenewsExpiredGrant(t *testing.T) {
	buyer, seller := viewerUser(), uuid.New()
	listingID := uuid.New()
	store := &marketplaceFakeStore{
		contactSellerID: seller,
		contactRows:     1,
		renewRows:       1,
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(buyer, http.MethodPost, "/api/marketplace/listings/:id/contact", h.ContactSeller)
	path := "/api/marketplace/listings/" + listingID.String() + "/contact"

	for i, wantCreated := range []bool{true, true} {
		rec := doJSON(e, http.MethodPost, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, body=%s", i, rec.Code, rec.Body.String())
		}
		var response struct {
			Data marketplaceContactResponse `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Data.Created != wantCreated {
			t.Fatalf("request %d response = %+v", i, response.Data)
		}
	}
}

func TestMarketplaceImageIntentIsOwnerScopedAndReturnsImageID(t *testing.T) {
	user, listingID, imageID := viewerUser(), uuid.New(), uuid.New()
	store := &marketplaceFakeStore{
		listingForLock: db.GetMarketplaceListingForUpdateRow{
			ID: listingID, SellerUserID: user.ID, Status: "active",
		},
		createdImage: db.MarketplaceListingImage{ID: imageID},
	}
	runner := &marketplaceFakeRunner{store: store}
	r2 := &stubR2{
		presignPut: func(_ context.Context, key, contentType string, expiry time.Duration) (string, error) {
			if !strings.HasPrefix(key, "marketplace/listings/"+listingID.String()+"/") ||
				contentType != "image/png" || expiry != marketplaceImageTTL {
				t.Fatalf("PresignPut(%q, %q, %s)", key, contentType, expiry)
			}
			return "https://r2.example/upload", nil
		},
	}
	h := newMarketplaceHandlerForTest(store, runner, r2, &shared.APIConfig{})
	e := newDMTestEcho(user, http.MethodPost, "/api/marketplace/listings/:id/images/upload-intent", h.UploadImageIntent)
	path := "/api/marketplace/listings/" + listingID.String() + "/images/upload-intent"
	rec := doJSON(e, http.MethodPost, path, `{"content_type":"image/png","size_bytes":1234}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data struct {
			ImageID       uuid.UUID         `json:"image_id"`
			UploadURL     string            `json:"upload_url"`
			UploadHeaders map[string]string `json:"upload_headers"`
			MaxSizeBytes  int64             `json:"max_size_bytes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.ImageID != imageID || response.Data.UploadURL != "https://r2.example/upload" ||
		response.Data.UploadHeaders["Content-Type"] != "image/png" || response.Data.MaxSizeBytes != marketplaceMaxImageSize {
		t.Fatalf("upload intent response = %+v", response.Data)
	}
	if !runner.committed {
		t.Fatal("upload intent transaction did not commit")
	}
}

func TestMarketplaceImageConfirmValidatesImageBytes(t *testing.T) {
	user, listingID, imageID := viewerUser(), uuid.New(), uuid.New()
	store := &marketplaceFakeStore{
		imageForOwner: db.MarketplaceListingImage{
			ID: imageID, ListingID: listingID, R2Key: "marketplace/listings/photo.png",
			ContentType: "image/png", Status: "pending",
			ExpiresAt: sql.NullTime{Time: time.Now().Add(time.Minute), Valid: true},
			SizeBytes: sql.NullInt64{Int64: int64(len(avatarTestPNG(t))), Valid: true},
		},
		confirmRows: 1,
	}
	var finalKey string
	keyPrefix := fmt.Sprintf("marketplace/listings/%s/images/%s-", listingID, imageID)
	r2 := &stubR2{
		headObject: func(context.Context, string) (int64, error) { return int64(len(avatarTestPNG(t))), nil },
		getObject:  func(context.Context, string, int64) ([]byte, error) { return avatarTestPNG(t), nil },
		presignGet: func(_ context.Context, key string, _ time.Duration) (string, error) {
			if key != finalKey {
				t.Fatalf("PresignGet key = %q, want %q", key, finalKey)
			}
			return "https://r2.example/photo", nil
		},
		putObject: func(_ context.Context, key, contentType string, body []byte) error {
			if !strings.HasPrefix(key, keyPrefix) || !strings.HasSuffix(key, ".png") ||
				contentType != "image/png" || !bytes.Equal(body, avatarTestPNG(t)) {
				t.Fatalf("PutObject(%q, %q, %d bytes)", key, contentType, len(body))
			}
			finalKey = key
			return nil
		},
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, r2, &shared.APIConfig{})
	e := newDMTestEcho(user, http.MethodPost, "/api/marketplace/listings/:id/images/:image_id/confirm", h.ConfirmImage)
	path := fmt.Sprintf("/api/marketplace/listings/%s/images/%s/confirm", listingID, imageID)
	rec := doJSON(e, http.MethodPost, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data marketplaceImageObject `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.ID != imageID || response.Data.URL != "https://r2.example/photo" {
		t.Fatalf("confirm response = %+v", response.Data)
	}
	if store.lastConfirmParams.R2Key != finalKey {
		t.Fatalf("confirmed r2_key = %q, want %q", store.lastConfirmParams.R2Key, finalKey)
	}
}

func TestMarketplaceConfirmImageRetryUsesUniqueFinalKey(t *testing.T) {
	user, listingID, imageID := viewerUser(), uuid.New(), uuid.New()
	store := &marketplaceFakeStore{
		imageForOwner: db.MarketplaceListingImage{
			ID: imageID, ListingID: listingID, R2Key: "marketplace/listings/photo.png",
			ContentType: "image/png", Status: "pending",
			ExpiresAt: sql.NullTime{Time: time.Now().Add(time.Minute), Valid: true},
			SizeBytes: sql.NullInt64{Int64: int64(len(avatarTestPNG(t))), Valid: true},
		},
		confirmRows: 1,
	}
	var putKeys []string
	var presignedKey string
	r2 := &stubR2{
		headObject: func(context.Context, string) (int64, error) { return int64(len(avatarTestPNG(t))), nil },
		getObject:  func(context.Context, string, int64) ([]byte, error) { return avatarTestPNG(t), nil },
		presignGet: func(_ context.Context, key string, _ time.Duration) (string, error) {
			presignedKey = key
			return "https://r2.example/photo", nil
		},
		putObject: func(_ context.Context, key, _ string, _ []byte) error {
			putKeys = append(putKeys, key)
			return nil
		},
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, r2, &shared.APIConfig{})
	e := newDMTestEcho(user, http.MethodPost, "/api/marketplace/listings/:id/images/:image_id/confirm", h.ConfirmImage)
	path := fmt.Sprintf("/api/marketplace/listings/%s/images/%s/confirm", listingID, imageID)

	if rec := doJSON(e, http.MethodPost, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("first confirm status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(putKeys) != 1 || presignedKey != putKeys[0] {
		t.Fatalf("first confirm keys: put=%v presigned=%q", putKeys, presignedKey)
	}
	winningKey := putKeys[0]

	store.confirmRows = 0
	if rec := doJSON(e, http.MethodPost, path, ""); rec.Code != http.StatusConflict {
		t.Fatalf("second confirm status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(putKeys) != 2 || putKeys[0] == putKeys[1] {
		t.Fatalf("confirm attempts reused a final key: %v", putKeys)
	}
	if store.lastConfirmParams.R2Key != putKeys[1] {
		t.Fatalf("failed attempt confirmed r2_key = %q, want %q", store.lastConfirmParams.R2Key, putKeys[1])
	}
	if len(store.cleanupOutbox) != 2 {
		t.Fatalf("cleanup outbox batches = %d, want 2: %+v", len(store.cleanupOutbox), store.cleanupOutbox)
	}
	if len(store.cleanupOutbox[0]) != 1 || store.cleanupOutbox[0][0] != "marketplace/listings/photo.png" {
		t.Fatalf("first cleanup batch = %v", store.cleanupOutbox[0])
	}
	if len(store.cleanupOutbox[1]) != 1 || store.cleanupOutbox[1][0] != putKeys[1] {
		t.Fatalf("second cleanup batch = %v", store.cleanupOutbox[1])
	}
	for _, batch := range store.cleanupOutbox {
		for _, key := range batch {
			if key == winningKey {
				t.Fatalf("cleanup deleted the winning final key %q", key)
			}
		}
	}
}

func TestMarketplaceCreateListingEnforcesDailyQuota(t *testing.T) {
	store := &marketplaceFakeStore{listingCount: marketplaceMaxListingsPerDay}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(viewerUser(), http.MethodPost, "/api/marketplace/listings", h.CreateListing)
	body := `{"category":"electronics","title":"Phone","description":"Good phone","price_idr":1000000,"city":"Jakarta","condition":"good"}`
	assertErrCode(t, doJSON(e, http.MethodPost, "/api/marketplace/listings", body), http.StatusTooManyRequests, shared.CodeRateLimited)
}

func TestMarketplaceCreateListingRejectsInvalidCoordinates(t *testing.T) {
	store := &marketplaceFakeStore{}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(viewerUser(), http.MethodPost, "/api/marketplace/listings", h.CreateListing)
	body := `{"category":"electronics","title":"Phone","description":"Good phone","price_idr":1000000,"city":"Jakarta","condition":"good","latitude":91,"longitude":106.8}`
	assertErrCode(t, doJSON(e, http.MethodPost, "/api/marketplace/listings", body), http.StatusBadRequest, shared.CodeValidationError)
}

func TestMarketplaceReportEnforcesDailyQuotaAndOtherRequiresDetails(t *testing.T) {
	user := viewerUser()
	listingID := uuid.New()
	store := &marketplaceFakeStore{eligible: true, reportCount: marketplaceMaxReportsPerDay}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(user, http.MethodPost, "/api/marketplace/listings/:id/report", h.Report)
	path := "/api/marketplace/listings/" + listingID.String() + "/report"
	assertErrCode(t, doJSON(e, http.MethodPost, path, `{"reason":"scam"}`), http.StatusTooManyRequests, shared.CodeRateLimited)

	store.reportCount = 0
	rec := doJSON(e, http.MethodPost, path, `{"reason":"other"}`)
	body := rec.Body.String()
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
	if !strings.Contains(body, "details") {
		t.Fatalf("validation body = %s", body)
	}
}

func TestMarketplaceContactEnforcesDailyQuota(t *testing.T) {
	buyer := viewerUser()
	store := &marketplaceFakeStore{
		contactCount:    marketplaceMaxContactsPerDay,
		contactSellerID: uuid.New(),
		contactRows:     1,
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(buyer, http.MethodPost, "/api/marketplace/listings/:id/contact", h.ContactSeller)
	path := "/api/marketplace/listings/" + uuid.NewString() + "/contact"
	assertErrCode(t, doJSON(e, http.MethodPost, path, ""), http.StatusTooManyRequests, shared.CodeRateLimited)
}

func TestMarketplaceRevokeContactPurgesPairInbox(t *testing.T) {
	buyer, seller := viewerUser(), uuid.New()
	store := &marketplaceFakeStore{
		participants: db.GetMarketplaceContactParticipantsRow{BuyerUserID: buyer.ID, SellerUserID: seller},
		revokeRows:   1,
	}
	runner := &marketplaceFakeRunner{store: store}
	h := newMarketplaceHandlerForTest(store, runner, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(buyer, http.MethodDelete, "/api/marketplace/listings/:id/contact", h.RevokeContact)
	listingID := uuid.New()
	rec := doJSON(e, http.MethodDelete, "/api/marketplace/listings/"+listingID.String()+"/contact", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !runner.committed || store.pairInboxDeletes != 1 || store.pairIdempotencyDeletes != 1 {
		t.Fatalf("committed=%v, inbox deletes=%d, idempotency deletes=%d",
			runner.committed, store.pairInboxDeletes, store.pairIdempotencyDeletes)
	}
	if store.lastPairInboxParams.SenderID != buyer.ID || store.lastPairInboxParams.RecipientID != seller ||
		store.lastPairIdempotencyArgs.SenderID != buyer.ID || store.lastPairIdempotencyArgs.RecipientID != seller {
		t.Fatalf("purge params: inbox=%+v idempotency=%+v",
			store.lastPairInboxParams, store.lastPairIdempotencyArgs)
	}
	var response struct {
		Data marketplaceContactRevokeResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.ListingID != listingID || !response.Data.Revoked {
		t.Fatalf("revoke response = %+v", response.Data)
	}
}

func TestMarketplaceRevokeContactKeepsInboxWhenPairStillAllowed(t *testing.T) {
	buyer, seller := viewerUser(), uuid.New()
	store := &marketplaceFakeStore{
		participants:     db.GetMarketplaceContactParticipantsRow{BuyerUserID: buyer.ID, SellerUserID: seller},
		revokeRows:       1,
		pairStillAllowed: true,
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(buyer, http.MethodDelete, "/api/marketplace/listings/:id/contact", h.RevokeContact)
	rec := doJSON(e, http.MethodDelete, "/api/marketplace/listings/"+uuid.NewString()+"/contact", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.pairInboxDeletes != 0 || store.pairIdempotencyDeletes != 0 {
		t.Fatalf("purges = inbox:%d idempotency:%d; want 0", store.pairInboxDeletes, store.pairIdempotencyDeletes)
	}
}

func TestMarketplaceRevokeContactBySellerTargetsSelectedBuyer(t *testing.T) {
	buyer, seller := uuid.New(), viewerUser()
	store := &marketplaceFakeStore{
		participants: db.GetMarketplaceContactParticipantsRow{BuyerUserID: buyer, SellerUserID: seller.ID},
		revokeRows:   1,
	}
	runner := &marketplaceFakeRunner{store: store}
	h := newMarketplaceHandlerForTest(store, runner, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(seller, http.MethodDelete, "/api/marketplace/listings/:id/contact", h.RevokeContact)
	listingID := uuid.New()
	rec := doJSON(e, http.MethodDelete,
		"/api/marketplace/listings/"+listingID.String()+"/contact?buyer_id="+buyer.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.participantsParams.ListingID != listingID || store.participantsParams.BuyerUserID != buyer {
		t.Fatalf("participant lookup = %+v, want buyer %s", store.participantsParams, buyer)
	}
	if store.revokeParams.ListingID != listingID || store.revokeParams.BuyerUserID != buyer ||
		store.revokeParams.SellerUserID != seller.ID {
		t.Fatalf("revoke params = %+v", store.revokeParams)
	}
	if store.pairInboxDeletes != 1 || store.lastPairInboxParams.SenderID != buyer ||
		store.lastPairInboxParams.RecipientID != seller.ID {
		t.Fatalf("inbox purge = %d, params=%+v", store.pairInboxDeletes, store.lastPairInboxParams)
	}
}

func TestMarketplaceRevokeContactRejectsOtherBuyersGrant(t *testing.T) {
	caller, otherBuyer, seller := viewerUser(), uuid.New(), uuid.New()
	store := &marketplaceFakeStore{
		participants: db.GetMarketplaceContactParticipantsRow{BuyerUserID: otherBuyer, SellerUserID: seller},
		revokeRows:   1,
	}
	runner := &marketplaceFakeRunner{store: store}
	h := newMarketplaceHandlerForTest(store, runner, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(caller, http.MethodDelete, "/api/marketplace/listings/:id/contact", h.RevokeContact)
	rec := doJSON(e, http.MethodDelete,
		"/api/marketplace/listings/"+uuid.NewString()+"/contact?buyer_id="+otherBuyer.String(), "")
	assertErrCode(t, rec, http.StatusForbidden, shared.CodeForbidden)
	if runner.committed || store.revokeParams != (db.RevokeMarketplaceContactParams{}) ||
		store.pairInboxDeletes != 0 {
		t.Fatalf("committed=%v, revoke=%+v, inbox deletes=%d",
			runner.committed, store.revokeParams, store.pairInboxDeletes)
	}
}

func TestMarketplaceOwnerMutationsAreScopedToAuthenticatedSeller(t *testing.T) {
	user, listingID := viewerUser(), uuid.New()
	const listingBody = `{"category":"electronics","title":"Kamera","description":"Bagus","price_idr":1200000,"city":"Jakarta","condition":"good"}`
	cases := []struct {
		name     string
		method   string
		template string
		suffix   string
		body     string
		route    func(*MarketplaceHandler) echo.HandlerFunc
		check    func(*testing.T, *marketplaceFakeStore, uuid.UUID, uuid.UUID)
	}{
		{
			name: "update listing", method: http.MethodPut, template: "/api/marketplace/listings/:id", body: listingBody,
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.UpdateListing },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, id uuid.UUID) {
				if store.lastUpdatedListing.SellerUserID != userID || store.lastUpdatedListing.ID != id {
					t.Fatalf("update params = %+v", store.lastUpdatedListing)
				}
			},
		},
		{
			name: "update status", method: http.MethodPatch, template: "/api/marketplace/listings/:id/status", suffix: "/status", body: `{"status":"sold"}`,
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.UpdateListingStatus },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, id uuid.UUID) {
				if store.lastStatusUpdate.SellerUserID != userID || store.lastStatusUpdate.ID != id || store.lastStatusUpdate.Status != "sold" {
					t.Fatalf("status params = %+v", store.lastStatusUpdate)
				}
			},
		},
		{
			name: "delete listing", method: http.MethodDelete, template: "/api/marketplace/listings/:id",
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.DeleteListing },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, id uuid.UUID) {
				if store.lastStatusUpdate.SellerUserID != userID || store.lastStatusUpdate.ID != id || store.lastStatusUpdate.Status != "archived" {
					t.Fatalf("archive params = %+v", store.lastStatusUpdate)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &marketplaceFakeStore{updateErr: sql.ErrNoRows}
			h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
			e := newDMTestEcho(user, tc.method, tc.template, tc.route(h))
			target := "/api/marketplace/listings/" + listingID.String() + tc.suffix
			rec := doJSON(e, tc.method, target, tc.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			tc.check(t, store, user.ID, listingID)
		})
	}
}

func TestMarketplaceReadsAndBuyerActionsUseAuthenticatedViewer(t *testing.T) {
	user, listingID := viewerUser(), uuid.New()
	cases := []struct {
		name     string
		method   string
		template string
		target   string
		body     string
		status   int
		store    *marketplaceFakeStore
		route    func(*MarketplaceHandler) echo.HandlerFunc
		check    func(*testing.T, *marketplaceFakeStore, uuid.UUID, uuid.UUID)
	}{
		{
			name: "browse", method: http.MethodGet, template: "/api/marketplace/listings", target: "/api/marketplace/listings", status: http.StatusOK,
			store: &marketplaceFakeStore{},
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.Browse },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, _ uuid.UUID) {
				if store.lastBrowseParams.ViewerID != userID {
					t.Fatalf("browse viewer = %s", store.lastBrowseParams.ViewerID)
				}
			},
		},
		{
			name: "get detail", method: http.MethodGet, template: "/api/marketplace/listings/:id", target: "/api/marketplace/listings/" + listingID.String(), status: http.StatusNotFound,
			store: &marketplaceFakeStore{detailErr: sql.ErrNoRows},
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.GetListing },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, id uuid.UUID) {
				if store.lastDetailParams.UserID != userID || store.lastDetailParams.ID != id {
					t.Fatalf("detail params = %+v", store.lastDetailParams)
				}
			},
		},
		{
			name: "my listings", method: http.MethodGet, template: "/api/marketplace/me/listings", target: "/api/marketplace/me/listings", status: http.StatusOK,
			store: &marketplaceFakeStore{},
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.MyListings },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, _ uuid.UUID) {
				if store.lastOwnListingParams.SellerUserID != userID {
					t.Fatalf("own listings seller = %s", store.lastOwnListingParams.SellerUserID)
				}
			},
		},
		{
			name: "my favorites", method: http.MethodGet, template: "/api/marketplace/me/favorites", target: "/api/marketplace/me/favorites", status: http.StatusOK,
			store: &marketplaceFakeStore{},
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.MyFavorites },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, _ uuid.UUID) {
				if store.lastFavoritesParams.UserID != userID {
					t.Fatalf("favorites user = %s", store.lastFavoritesParams.UserID)
				}
			},
		},
		{
			name: "favorite", method: http.MethodPost, template: "/api/marketplace/listings/:id/favorite", target: "/api/marketplace/listings/" + listingID.String() + "/favorite", status: http.StatusOK,
			store: &marketplaceFakeStore{eligible: true},
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.Favorite },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, id uuid.UUID) {
				if store.lastFavoriteInsert.UserID != userID || store.lastFavoriteInsert.ListingID != id {
					t.Fatalf("favorite params = %+v", store.lastFavoriteInsert)
				}
			},
		},
		{
			name: "unfavorite", method: http.MethodDelete, template: "/api/marketplace/listings/:id/favorite", target: "/api/marketplace/listings/" + listingID.String() + "/favorite", status: http.StatusNoContent,
			store: &marketplaceFakeStore{},
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.Unfavorite },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, id uuid.UUID) {
				if store.lastFavoriteDelete.UserID != userID || store.lastFavoriteDelete.ListingID != id {
					t.Fatalf("unfavorite params = %+v", store.lastFavoriteDelete)
				}
			},
		},
		{
			name: "report", method: http.MethodPost, template: "/api/marketplace/listings/:id/report", target: "/api/marketplace/listings/" + listingID.String() + "/report", status: http.StatusCreated, body: `{"reason":"scam","details":"test"}`,
			store: &marketplaceFakeStore{eligible: true},
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.Report },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, id uuid.UUID) {
				if store.lastReportInsert.UserID != userID || store.lastReportInsert.ListingID != id {
					t.Fatalf("report params = %+v", store.lastReportInsert)
				}
			},
		},
		{
			name: "contact", method: http.MethodPost, template: "/api/marketplace/listings/:id/contact", target: "/api/marketplace/listings/" + listingID.String() + "/contact", status: http.StatusOK,
			store: &marketplaceFakeStore{contactSellerID: uuid.New()},
			route: func(h *MarketplaceHandler) echo.HandlerFunc { return h.ContactSeller },
			check: func(t *testing.T, store *marketplaceFakeStore, userID, id uuid.UUID) {
				if store.lastContactLookup.ID_2 != userID || store.lastContactLookup.ID != id {
					t.Fatalf("contact params = %+v", store.lastContactLookup)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newMarketplaceHandlerForTest(tc.store, &marketplaceFakeRunner{store: tc.store}, &stubR2{}, &shared.APIConfig{})
			e := newDMTestEcho(user, tc.method, tc.template, tc.route(h))
			rec := doJSON(e, tc.method, tc.target, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tc.status, rec.Body.String())
			}
			tc.check(t, tc.store, user.ID, listingID)
		})
	}
}

func TestMarketplaceSweepExpiredImagesRecordsCleanupOutbox(t *testing.T) {
	image := db.MarketplaceListingImage{R2Key: "marketplace/listings/expired.png"}
	store := &marketplaceFakeStore{sweptImages: []db.MarketplaceListingImage{image}}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})

	rows, err := h.SweepExpiredImages(context.Background(), 5)
	if err != nil || rows != 1 {
		t.Fatalf("SweepExpiredImages = %d, err=%v", rows, err)
	}
	if len(store.cleanupOutbox) != 1 || len(store.cleanupOutbox[0]) != 1 || store.cleanupOutbox[0][0] != image.R2Key {
		t.Fatalf("cleanup outbox = %+v", store.cleanupOutbox)
	}
}

func TestMarketplaceImageMutationsAreScopedToOwnerAndListing(t *testing.T) {
	user, listingID, imageID := viewerUser(), uuid.New(), uuid.New()
	store := &marketplaceFakeStore{
		listingForLock:  db.GetMarketplaceListingForUpdateRow{SellerUserID: uuid.New()},
		imageForOwner:   db.MarketplaceListingImage{ID: imageID, ListingID: listingID},
		deleteImageKeys: nil,
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})

	intentEcho := newDMTestEcho(user, http.MethodPost, "/api/marketplace/listings/:id/images/upload-intent", h.UploadImageIntent)
	rec := doJSON(intentEcho, http.MethodPost, "/api/marketplace/listings/"+listingID.String()+"/images/upload-intent", `{"content_type":"image/png","size_bytes":1234}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("upload-intent status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.lastLockedListing != listingID {
		t.Fatalf("locked listing = %s", store.lastLockedListing)
	}

	confirmEcho := newDMTestEcho(user, http.MethodPost, "/api/marketplace/listings/:id/images/:image_id/confirm", h.ConfirmImage)
	rec = doJSON(confirmEcho, http.MethodPost, fmt.Sprintf("/api/marketplace/listings/%s/images/%s/confirm", listingID, imageID), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.lastImageOwnerParams.ID != imageID || store.lastImageOwnerParams.ID_2 != listingID || store.lastImageOwnerParams.SellerUserID != user.ID {
		t.Fatalf("confirm params = %+v", store.lastImageOwnerParams)
	}

	deleteEcho := newDMTestEcho(user, http.MethodDelete, "/api/marketplace/listings/:id/images/:image_id", h.DeleteImage)
	rec = doJSON(deleteEcho, http.MethodDelete, fmt.Sprintf("/api/marketplace/listings/%s/images/%s", listingID, imageID), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete image status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.lastDeletedImage.ID != imageID || store.lastDeletedImage.ID_2 != listingID || store.lastDeletedImage.SellerUserID != user.ID {
		t.Fatalf("delete image params = %+v", store.lastDeletedImage)
	}
}

func TestMarketplaceImageIntentValidatesDeclaredSize(t *testing.T) {
	user, listingID := viewerUser(), uuid.New()
	store := &marketplaceFakeStore{
		listingForLock: db.GetMarketplaceListingForUpdateRow{ID: listingID, SellerUserID: user.ID, Status: "active"},
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(user, http.MethodPost, "/api/marketplace/listings/:id/images/upload-intent", h.UploadImageIntent)
	path := "/api/marketplace/listings/" + listingID.String() + "/images/upload-intent"
	for _, body := range []string{
		`{"content_type":"image/png","size_bytes":0}`,
		fmt.Sprintf(`{"content_type":"image/png","size_bytes":%d}`, marketplaceMaxImageSize+1),
	} {
		if rec := doJSON(e, http.MethodPost, path, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestMarketplaceConfirmImageSizeMismatchAndIdempotentRetry(t *testing.T) {
	user, listingID, imageID := viewerUser(), uuid.New(), uuid.New()
	pendingKey := "marketplace/listings/pending.png"
	finalKey := "marketplace/listings/final.png"
	image := db.MarketplaceListingImage{
		ID: imageID, ListingID: listingID, R2Key: pendingKey, ContentType: "image/png",
		Status: "pending", ExpiresAt: sql.NullTime{Time: time.Now().Add(time.Minute), Valid: true},
		SizeBytes: sql.NullInt64{Int64: int64(len(avatarTestPNG(t))), Valid: true},
	}
	store := &marketplaceFakeStore{
		imageForOwner:   image,
		deleteImageKeys: []string{pendingKey},
		confirmRows:     1,
	}
	r2 := &stubR2{
		headObject: func(context.Context, string) (int64, error) { return 7, nil },
		presignGet: func(context.Context, string, time.Duration) (string, error) {
			return "https://r2.example/photo", nil
		},
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, r2, &shared.APIConfig{})
	e := newDMTestEcho(user, http.MethodPost, "/api/marketplace/listings/:id/images/:image_id/confirm", h.ConfirmImage)
	path := fmt.Sprintf("/api/marketplace/listings/%s/images/%s/confirm", listingID, imageID)
	if rec := doJSON(e, http.MethodPost, path, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched size status = %d, body=%s", rec.Code, rec.Body.String())
	}

	active := image
	active.Status, active.R2Key = "active", finalKey
	store.imageForOwner = active
	rec := doJSON(e, http.MethodPost, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent confirm status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data marketplaceImageObject `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.ID != imageID || response.Data.URL != "https://r2.example/photo" || response.Data.IsCover {
		t.Fatalf("idempotent confirm response = %+v", response.Data)
	}
}

func TestMarketplaceSetCoverImage(t *testing.T) {
	user, listingID, imageID := viewerUser(), uuid.New(), uuid.New()
	store := &marketplaceFakeStore{
		listingForLock: db.GetMarketplaceListingForUpdateRow{ID: listingID, SellerUserID: user.ID, Status: "active"},
		coverRow:       db.MarketplaceListingImage{ID: imageID, ListingID: listingID},
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(user, http.MethodPost, "/api/marketplace/listings/:id/images/:image_id/cover", h.SetCoverImage)
	path := fmt.Sprintf("/api/marketplace/listings/%s/images/%s/cover", listingID, imageID)
	if rec := doJSON(e, http.MethodPost, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("set cover status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.lastCoverClear != listingID || store.lastCoverParams.ID != imageID || store.lastCoverParams.ID_2 != listingID {
		t.Fatalf("cover params clear=%s set=%+v", store.lastCoverClear, store.lastCoverParams)
	}
}

func TestMarketplaceSellerListingsUsesSellerAndViewer(t *testing.T) {
	user, sellerID, listingID := viewerUser(), uuid.New(), uuid.New()
	store := &marketplaceFakeStore{
		sellerRows: []db.ListMarketplaceListingsBySellerRow{{
			ID: listingID, SellerUserID: sellerID, Username: "seller", CategorySlug: "other",
			CategoryName: "Other", Title: "Chair", Status: "active",
			CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		}},
	}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	e := newDMTestEcho(user, http.MethodGet, "/api/marketplace/users/:seller_user_id/listings", h.SellerListings)
	path := "/api/marketplace/users/" + sellerID.String() + "/listings"
	if rec := doJSON(e, http.MethodGet, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("seller listings status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.lastSellerParams.SellerUserID != sellerID || store.lastSellerParams.ViewerID != user.ID {
		t.Fatalf("seller listing params = %+v", store.lastSellerParams)
	}
}

func TestMarketplaceModerationAllowlist(t *testing.T) {
	moderator := viewerUser()
	other := uuid.New()
	reportID := uuid.New()
	store := &marketplaceFakeStore{
		reportRows: []db.ListMarketplaceReportsRow{{
			ID: reportID, ListingID: uuid.New(), ReporterUserID: other,
			Reason: "scam", Status: "open", CreatedAt: time.Now(), ListingTitle: "Phone",
		}},
		reviewRows: []db.MarketplaceReport{{
			ID: reportID, ListingID: uuid.New(), ReporterUserID: other,
			Reason: "scam", Status: "dismissed", CreatedAt: time.Now(),
			ReviewedAt:     sql.NullTime{Time: time.Now(), Valid: true},
			ReviewerUserID: uuid.NullUUID{UUID: moderator.ID, Valid: true},
			ReviewNote:     "reviewed", Resolution: sql.NullString{String: "dismissed", Valid: true},
		}},
	}
	cfg := &shared.APIConfig{MarketplaceModeratorIDs: []uuid.UUID{moderator.ID}}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, cfg)

	listEcho := newDMTestEcho(moderator, http.MethodGet, "/api/marketplace/moderation/reports", h.ListReports)
	if rec := doJSON(listEcho, http.MethodGet, "/api/marketplace/moderation/reports", ""); rec.Code != http.StatusOK {
		t.Fatalf("moderator list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.lastReportListParams.PageSize != marketplaceDefaultLimit+1 {
		t.Fatalf("report page size = %d", store.lastReportListParams.PageSize)
	}

	reviewEcho := newDMTestEcho(moderator, http.MethodPost, "/api/marketplace/moderation/reports/:id/review", h.ReviewReport)
	path := "/api/marketplace/moderation/reports/" + reportID.String() + "/review"
	if rec := doJSON(reviewEcho, http.MethodPost, path, `{"resolution":"banned"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid review status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(reviewEcho, http.MethodPost, path, `{"resolution":"dismissed","note":"reviewed"}`); rec.Code != http.StatusOK {
		t.Fatalf("valid review status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if store.lastReviewParams.ID != reportID || store.lastReviewParams.ReviewerUserID != moderator.ID ||
		store.lastReviewParams.Resolution != "dismissed" {
		t.Fatalf("review params = %+v", store.lastReviewParams)
	}

	denied := newDMTestEcho(db.User{ID: other, IsActive: true}, http.MethodGet, "/api/marketplace/moderation/reports", h.ListReports)
	if rec := doJSON(denied, http.MethodGet, "/api/marketplace/moderation/reports", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-moderator status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestMarketplaceArchiveExpiredListingsRecordsCleanup(t *testing.T) {
	ids := []uuid.UUID{uuid.New()}
	store := &marketplaceFakeStore{expiredListingIDs: ids, expiredListingKeys: []string{"marketplace/listings/old.png"}}
	h := newMarketplaceHandlerForTest(store, &marketplaceFakeRunner{store: store}, &stubR2{}, &shared.APIConfig{})
	archived, err := h.ArchiveExpiredListings(context.Background(), 5)
	if err != nil || archived != 1 {
		t.Fatalf("ArchiveExpiredListings = %d, err=%v", archived, err)
	}
	if store.lastExpiryBatch != 5 || len(store.cleanupOutbox) != 1 || len(store.cleanupOutbox[0]) != 1 {
		t.Fatalf("batch=%d outbox=%+v", store.lastExpiryBatch, store.cleanupOutbox)
	}
}
