package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const (
	marketplaceDefaultLimit = 20
	marketplaceMaxLimit     = 50
	marketplaceMaxRadiusKM  = 200.0
	marketplaceMaxImageSize = int64(10 * 1024 * 1024)
	marketplaceImageTTL     = 15 * time.Minute
	marketplaceBodyLimit    = 32 << 10

	marketplaceMaxListingsPerDay = 20
	marketplaceMaxReportsPerDay  = 20
	marketplaceMaxContactsPerDay = 20
)

var errMarketplaceQuotaExceeded = errors.New("marketplace quota exceeded")

func marketplacePairLockKey(a, b uuid.UUID) string {
	if a.String() > b.String() {
		a, b = b, a
	}
	return "marketplace:dm:" + a.String() + ":" + b.String()
}

var marketplaceCategories = map[string]struct{}{
	"electronics": {}, "vehicles": {}, "home_living": {}, "fashion": {},
	"sports_hobbies": {}, "baby_kids": {}, "books_media": {}, "other": {},
}

type marketplaceStore interface {
	ListMarketplaceCategories(context.Context) ([]db.MarketplaceCategory, error)
	LockMarketplaceQuota(context.Context, string) error
	CreateMarketplaceListing(context.Context, db.CreateMarketplaceListingParams) (db.MarketplaceListing, error)
	CountMarketplaceListingsBySellerSince(context.Context, db.CountMarketplaceListingsBySellerSinceParams) (int64, error)
	UpdateMarketplaceListing(context.Context, db.UpdateMarketplaceListingParams) (db.MarketplaceListing, error)
	UpdateMarketplaceListingStatus(context.Context, db.UpdateMarketplaceListingStatusParams) (db.MarketplaceListing, error)
	GetMarketplaceListing(context.Context, db.GetMarketplaceListingParams) (db.GetMarketplaceListingRow, error)
	ListMarketplaceListings(context.Context, db.ListMarketplaceListingsParams) ([]db.ListMarketplaceListingsRow, error)
	ListMarketplaceOwnListings(context.Context, db.ListMarketplaceOwnListingsParams) ([]db.ListMarketplaceOwnListingsRow, error)
	ListMarketplaceListingImages(context.Context, uuid.UUID) ([]db.MarketplaceListingImage, error)
	ListMarketplaceImagesForListings(context.Context, string) ([]db.MarketplaceListingImage, error)
	GetMarketplaceListingImageForOwner(context.Context, db.GetMarketplaceListingImageForOwnerParams) (db.MarketplaceListingImage, error)
	ConfirmMarketplaceListingImage(context.Context, db.ConfirmMarketplaceListingImageParams) (int64, error)
	DeleteMarketplaceListingImage(context.Context, db.DeleteMarketplaceListingImageParams) (string, error)
	ListMarketplaceImageKeysBySeller(context.Context, uuid.UUID) ([]string, error)
	IsMarketplaceFavoriteEligible(context.Context, db.IsMarketplaceFavoriteEligibleParams) (bool, error)
	InsertMarketplaceFavorite(context.Context, db.InsertMarketplaceFavoriteParams) (int64, error)
	DeleteMarketplaceFavorite(context.Context, db.DeleteMarketplaceFavoriteParams) error
	ListMarketplaceFavorites(context.Context, db.ListMarketplaceFavoritesParams) ([]db.ListMarketplaceFavoritesRow, error)
	IsMarketplaceReportEligible(context.Context, db.IsMarketplaceReportEligibleParams) (bool, error)
	InsertMarketplaceReport(context.Context, db.InsertMarketplaceReportParams) (int64, error)
	CountMarketplaceReportsByReporterSince(context.Context, db.CountMarketplaceReportsByReporterSinceParams) (int64, error)
	HasMarketplaceReport(context.Context, db.HasMarketplaceReportParams) (bool, error)
	CountMarketplaceContactsByBuyerSince(context.Context, db.CountMarketplaceContactsByBuyerSinceParams) (int64, error)
	InsertMarketplaceCleanupOutbox(context.Context, []string) error
	SweepExpiredMarketplaceListingImages(context.Context, int32) ([]db.MarketplaceListingImage, error)
}

type marketplaceTxStore interface {
	marketplaceStore
	GetMarketplaceListingForUpdate(context.Context, uuid.UUID) (db.GetMarketplaceListingForUpdateRow, error)
	DeleteExpiredMarketplaceListingImages(context.Context, uuid.UUID) ([]db.MarketplaceListingImage, error)
	GetNextMarketplaceListingImagePosition(context.Context, uuid.UUID) (int16, error)
	CreateMarketplaceListingImage(context.Context, db.CreateMarketplaceListingImageParams) (db.MarketplaceListingImage, error)
	ConfirmMarketplaceListingImage(context.Context, db.ConfirmMarketplaceListingImageParams) (int64, error)
	GetMarketplaceContactSeller(context.Context, db.GetMarketplaceContactSellerParams) (uuid.UUID, error)
	InsertMarketplaceContact(context.Context, db.InsertMarketplaceContactParams) (int64, error)
	GetMarketplaceContactParticipants(context.Context, db.GetMarketplaceContactParticipantsParams) (db.GetMarketplaceContactParticipantsRow, error)
	RenewExpiredMarketplaceContact(context.Context, db.RenewExpiredMarketplaceContactParams) (int64, error)
	RevokeMarketplaceContact(context.Context, db.RevokeMarketplaceContactParams) (int64, error)
	IsDMRecipientAllowed(context.Context, db.IsDMRecipientAllowedParams) (bool, error)
	DeleteDMIdempotencyForPairMessages(context.Context, db.DeleteDMIdempotencyForPairMessagesParams) (int64, error)
	DeleteDMInboxForPair(context.Context, db.DeleteDMInboxForPairParams) (int64, error)
}

type MarketplaceHandler struct {
	Queries marketplaceStore
	Run     txRunner[marketplaceTxStore]
	R2      r2ObjectStore
	Cfg     *shared.APIConfig
}

func NewMarketplaceHandler(queries *db.Queries, dbHandle *sql.DB, r2 *shared.R2Client, cfg *shared.APIConfig) (*MarketplaceHandler, error) {
	if queries == nil {
		return nil, fmt.Errorf("NewMarketplaceHandler: queries is nil")
	}
	if dbHandle == nil {
		return nil, fmt.Errorf("NewMarketplaceHandler: db is nil")
	}
	if r2 == nil {
		return nil, fmt.Errorf("NewMarketplaceHandler: r2 is nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("NewMarketplaceHandler: config is nil")
	}
	return newMarketplaceHandlerForTest(
		queries,
		NewSQLTxRunner(dbHandle, func(tx *sql.Tx) marketplaceTxStore { return queries.WithTx(tx) }),
		r2, cfg,
	), nil
}

func newMarketplaceHandlerForTest(queries marketplaceStore, runner txRunner[marketplaceTxStore], r2 r2ObjectStore, cfg *shared.APIConfig) *MarketplaceHandler {
	return &MarketplaceHandler{Queries: queries, Run: runner, R2: r2, Cfg: cfg}
}

type marketplaceListingRequest struct {
	Category    string   `json:"category" validate:"required,oneof=electronics vehicles home_living fashion sports_hobbies baby_kids books_media other"`
	Title       string   `json:"title" validate:"required,max=120"`
	Description string   `json:"description" validate:"required,max=5000"`
	PriceIDR    int64    `json:"price_idr" validate:"required,gt=0"`
	City        string   `json:"city" validate:"required,max=100"`
	Condition   string   `json:"condition" validate:"required,oneof=new like_new good fair"`
	Latitude    *float64 `json:"latitude"`
	Longitude   *float64 `json:"longitude"`
}

type marketplaceStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=active sold archived"`
}

type marketplaceImageIntentRequest struct {
	ContentType string `json:"content_type" validate:"required"`
}

type marketplaceReportRequest struct {
	Reason  string `json:"reason" validate:"required,oneof=scam prohibited misleading other"`
	Details string `json:"details" validate:"max=1000"`
}

type marketplaceListingObject struct {
	ID          uuid.UUID                 `json:"id"`
	Seller      marketplaceSellerObject   `json:"seller"`
	Category    marketplaceCategoryObject `json:"category"`
	Title       string                    `json:"title"`
	Description string                    `json:"description"`
	PriceIDR    int64                     `json:"price_idr"`
	City        string                    `json:"city"`
	Latitude    *float64                  `json:"latitude,omitempty"`
	Longitude   *float64                  `json:"longitude,omitempty"`
	DistanceKM  *float64                  `json:"distance_km,omitempty"`
	Condition   string                    `json:"condition"`
	Status      string                    `json:"status"`
	Images      []marketplaceImageObject  `json:"images"`
	IsFavorite  bool                      `json:"is_favorite"`
	CreatedAt   string                    `json:"created_at"`
	UpdatedAt   string                    `json:"updated_at"`
}

type marketplaceSellerObject struct {
	UserID      uuid.UUID `json:"user_id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name"`
}

type marketplaceCategoryObject struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type marketplaceImageObject struct {
	ID        uuid.UUID `json:"id"`
	URL       string    `json:"url"`
	SortOrder int16     `json:"sort_order"`
}

type marketplaceContactResponse struct {
	ListingID uuid.UUID `json:"listing_id"`
	SellerID  uuid.UUID `json:"seller_user_id"`
	Created   bool      `json:"created"`
}

type marketplaceContactRevokeResponse struct {
	ListingID uuid.UUID `json:"listing_id"`
	Revoked   bool      `json:"revoked"`
}

type marketplaceReportResponse struct {
	Reported bool `json:"reported"`
	Created  bool `json:"created"`
}

type marketplaceListingFields struct {
	id, sellerID                                uuid.UUID
	username, categorySlug, categoryName        string
	displayName                                 sql.NullString
	title, description, city, condition, status string
	price                                       int64
	latitude, longitude, distanceKM             *float64
	createdAt, updatedAt                        time.Time
	favorite                                    bool
}

func marketplaceFieldsFromDetail(row db.GetMarketplaceListingRow) marketplaceListingFields {
	return marketplaceListingFields{
		id: row.ID, sellerID: row.SellerUserID, username: row.Username, displayName: row.DisplayName,
		categorySlug: row.CategorySlug, categoryName: row.CategoryName, title: row.Title,
		description: row.Description, city: row.City, condition: row.ItemCondition, status: row.Status,
		price: row.PriceIdr, latitude: marketplaceNullableFloat(row.Latitude),
		longitude: marketplaceNullableFloat(row.Longitude),
		createdAt: row.CreatedAt, updatedAt: row.UpdatedAt, favorite: row.IsFavorite,
	}
}

func marketplaceFieldsFromBrowse(row db.ListMarketplaceListingsRow) marketplaceListingFields {
	return marketplaceListingFields{
		id: row.ID, sellerID: row.SellerUserID, username: row.Username, displayName: row.DisplayName,
		categorySlug: row.CategorySlug, categoryName: row.CategoryName, title: row.Title,
		description: row.Description, city: row.City, condition: row.ItemCondition, status: row.Status,
		price: row.PriceIdr, latitude: marketplaceNullableFloat(row.Latitude),
		longitude: marketplaceNullableFloat(row.Longitude), distanceKM: marketplaceDistance(row.DistanceKm),
		createdAt: row.CreatedAt, updatedAt: row.UpdatedAt, favorite: row.IsFavorite,
	}
}

func marketplaceFieldsFromOwn(row db.ListMarketplaceOwnListingsRow) marketplaceListingFields {
	return marketplaceListingFields{
		id: row.ID, sellerID: row.SellerUserID, username: row.Username, displayName: row.DisplayName,
		categorySlug: row.CategorySlug, categoryName: row.CategoryName, title: row.Title,
		description: row.Description, city: row.City, condition: row.ItemCondition, status: row.Status,
		price: row.PriceIdr, latitude: marketplaceNullableFloat(row.Latitude),
		longitude: marketplaceNullableFloat(row.Longitude),
		createdAt: row.CreatedAt, updatedAt: row.UpdatedAt, favorite: row.IsFavorite,
	}
}

func marketplaceFieldsFromFavorite(row db.ListMarketplaceFavoritesRow) marketplaceListingFields {
	return marketplaceListingFields{
		id: row.ID, sellerID: row.SellerUserID, username: row.Username, displayName: row.DisplayName,
		categorySlug: row.CategorySlug, categoryName: row.CategoryName, title: row.Title,
		description: row.Description, city: row.City, condition: row.ItemCondition, status: row.Status,
		price: row.PriceIdr, latitude: marketplaceNullableFloat(row.Latitude),
		longitude: marketplaceNullableFloat(row.Longitude),
		createdAt: row.CreatedAt, updatedAt: row.UpdatedAt, favorite: row.IsFavorite,
	}
}

func (h *MarketplaceHandler) Categories(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	rows, err := h.Queries.ListMarketplaceCategories(c.Request().Context())
	if err != nil {
		slog.Error("ListMarketplaceCategories failed", "err", err)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "list marketplace categories"))
	}
	return shared.RespondOK(c, rows)
}

func (h *MarketplaceHandler) CreateListing(c echo.Context) error {
	if h.Queries == nil || h.Run == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	var req marketplaceListingRequest
	if err := bindMarketplaceJSON(c, &req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	if err := validateMarketplaceListing(c, &req); err != nil {
		return shared.RespondError(c, err)
	}
	ctx := c.Request().Context()
	var listing db.MarketplaceListing
	err := h.Run.Run(ctx, func(q marketplaceTxStore) error {
		if err := q.LockMarketplaceQuota(ctx, "marketplace:listings:"+user.ID.String()); err != nil {
			return err
		}
		listed, err := q.CountMarketplaceListingsBySellerSince(ctx, db.CountMarketplaceListingsBySellerSinceParams{
			SellerUserID: user.ID, CreatedAt: time.Now().Add(-24 * time.Hour),
		})
		if err != nil {
			return err
		}
		if listed >= marketplaceMaxListingsPerDay {
			return errMarketplaceQuotaExceeded
		}
		listing, err = q.CreateMarketplaceListing(ctx, marketplaceCreateParams(user.ID, req))
		return err
	})
	if err != nil {
		if errors.Is(err, errMarketplaceQuotaExceeded) {
			return shared.RespondError(c, shared.Wrap(shared.ErrRateLimited, "listing quota exceeded"))
		}
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "active seller not found"))
		}
		slog.Error("CreateMarketplaceListing failed", "err", err, "user_id", user.ID)
		return marketplaceInternalError(c, "create marketplace listing", err)
	}
	item, err := h.loadListingObject(ctx, user.ID, listing.ID)
	if err != nil {
		return marketplaceInternalError(c, "load created marketplace listing", err)
	}
	return shared.RespondCreated(c, item)
}

func (h *MarketplaceHandler) Browse(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	params, limit, err := marketplaceBrowseParams(c, user.ID)
	if err != nil {
		return shared.RespondError(c, err)
	}
	rows, err := h.Queries.ListMarketplaceListings(c.Request().Context(), params)
	if err != nil {
		slog.Error("ListMarketplaceListings failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "browse marketplace listings"))
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	fields := make([]marketplaceListingFields, 0, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		fields = append(fields, marketplaceFieldsFromBrowse(row))
		ids = append(ids, row.ID)
	}
	items, err := h.listingObjects(c.Request().Context(), fields, ids)
	if err != nil {
		return marketplaceInternalError(c, "presign marketplace image URLs", err)
	}
	var next *string
	if hasMore && len(rows) > 0 {
		cursor := shared.EncodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		next = &cursor
	}
	return shared.RespondList(c, items, next)
}

func (h *MarketplaceHandler) GetListing(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	row, err := h.Queries.GetMarketplaceListing(c.Request().Context(), db.GetMarketplaceListingParams{ID: id, UserID: user.ID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "listing not found"))
		}
		slog.Error("GetMarketplaceListing failed", "err", err, "listing_id", id)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load marketplace listing"))
	}
	images, err := h.Queries.ListMarketplaceListingImages(c.Request().Context(), id)
	if err != nil {
		return marketplaceInternalError(c, "list marketplace images", err)
	}
	item, err := h.listingObject(c.Request().Context(), marketplaceFieldsFromDetail(row), images)
	if err != nil {
		return marketplaceInternalError(c, "presign marketplace image URLs", err)
	}
	return shared.RespondOK(c, item)
}

func (h *MarketplaceHandler) UpdateListing(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	var req marketplaceListingRequest
	if err := bindMarketplaceJSON(c, &req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	if err := validateMarketplaceListing(c, &req); err != nil {
		return shared.RespondError(c, err)
	}
	params := db.UpdateMarketplaceListingParams{
		ID: id, SellerUserID: user.ID, CategorySlug: req.Category,
		Title: req.Title, Description: req.Description, PriceIdr: req.PriceIDR,
		City: req.City, ItemCondition: req.Condition,
		Latitude:  marketplaceNullableFloat64(req.Latitude),
		Longitude: marketplaceNullableFloat64(req.Longitude),
	}
	listing, err := h.Queries.UpdateMarketplaceListing(c.Request().Context(), params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "listing not found"))
		}
		slog.Error("UpdateMarketplaceListing failed", "err", err, "listing_id", id, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update marketplace listing"))
	}
	item, err := h.loadListingObject(c.Request().Context(), user.ID, listing.ID)
	if err != nil {
		return marketplaceInternalError(c, "load updated marketplace listing", err)
	}
	return shared.RespondOK(c, item)
}

func (h *MarketplaceHandler) UpdateListingStatus(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	var req marketplaceStatusRequest
	if err := bindMarketplaceJSON(c, &req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	req.Status = strings.TrimSpace(req.Status)
	if err := c.Validate(&req); err != nil {
		return shared.RespondError(c, err)
	}
	listing, err := h.Queries.UpdateMarketplaceListingStatus(c.Request().Context(), db.UpdateMarketplaceListingStatusParams{
		ID: id, SellerUserID: user.ID, Status: req.Status,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "listing not found"))
		}
		slog.Error("UpdateMarketplaceListingStatus failed", "err", err, "listing_id", id, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update marketplace listing status"))
	}
	item, err := h.loadListingObject(c.Request().Context(), user.ID, listing.ID)
	if err != nil {
		return marketplaceInternalError(c, "load updated marketplace listing", err)
	}
	return shared.RespondOK(c, item)
}

func (h *MarketplaceHandler) DeleteListing(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	_, err = h.Queries.UpdateMarketplaceListingStatus(c.Request().Context(), db.UpdateMarketplaceListingStatusParams{
		ID: id, SellerUserID: user.ID, Status: "archived",
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "listing not found"))
		}
		slog.Error("archive marketplace listing failed", "err", err, "listing_id", id, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "archive marketplace listing"))
	}
	return shared.RespondNoContent(c)
}

func (h *MarketplaceHandler) MyListings(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	params, limit, err := marketplaceOwnListingParams(c, user.ID)
	if err != nil {
		return shared.RespondError(c, err)
	}
	rows, err := h.Queries.ListMarketplaceOwnListings(c.Request().Context(), params)
	if err != nil {
		return marketplaceInternalError(c, "list own marketplace listings", err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	fields, ids := make([]marketplaceListingFields, 0, len(rows)), make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		fields = append(fields, marketplaceFieldsFromOwn(row))
		ids = append(ids, row.ID)
	}
	items, err := h.listingObjects(c.Request().Context(), fields, ids)
	if err != nil {
		return marketplaceInternalError(c, "presign marketplace image URLs", err)
	}
	var next *string
	if hasMore && len(rows) > 0 {
		cursor := shared.EncodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		next = &cursor
	}
	return shared.RespondList(c, items, next)
}

func (h *MarketplaceHandler) UploadImageIntent(c echo.Context) error {
	if !h.configuredForImages() || h.Run == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace image upload is not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	listingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	var req marketplaceImageIntentRequest
	if err := bindMarketplaceJSON(c, &req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	req.ContentType = strings.ToLower(strings.TrimSpace(req.ContentType))
	if err := c.Validate(&req); err != nil {
		return shared.RespondError(c, err)
	}
	contentType := req.ContentType
	extension, valid := avatarExtension(contentType)
	if !valid {
		return marketplaceFieldError(c, "content_type", "must be image/jpeg, image/png, or image/webp")
	}
	ctx := c.Request().Context()
	key := fmt.Sprintf("marketplace/listings/%s/%s.%s", listingID, uuid.NewString(), extension)
	expiresAt := time.Now().UTC().Add(marketplaceImageTTL)
	var position int16
	var imageID uuid.UUID
	err = h.Run.Run(ctx, func(q marketplaceTxStore) error {
		listing, err := q.GetMarketplaceListingForUpdate(ctx, listingID)
		if err != nil {
			return err
		}
		if listing.SellerUserID != user.ID || listing.Status == "archived" {
			return sql.ErrNoRows
		}
		expired, err := q.DeleteExpiredMarketplaceListingImages(ctx, listingID)
		if err != nil {
			return fmt.Errorf("delete expired image intents: %w", err)
		}
		if err := recordMarketplaceCleanup(ctx, q, imageKeys(expired)); err != nil {
			return fmt.Errorf("record expired image cleanup: %w", err)
		}
		position, err = q.GetNextMarketplaceListingImagePosition(ctx, listingID)
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrMarketplaceConflict, "listing already has the maximum of 8 images")
		}
		if err != nil {
			return fmt.Errorf("find free image position: %w", err)
		}
		imageRow, err := q.CreateMarketplaceListingImage(ctx, db.CreateMarketplaceListingImageParams{
			ListingID: listingID, R2Key: key, ContentType: contentType, SortOrder: position,
			ExpiresAt: sql.NullTime{Time: expiresAt, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("create image upload intent: %w", err)
		}
		imageID = imageRow.ID
		return nil
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "listing not found"))
		}
		if errors.Is(err, shared.ErrMarketplaceConflict) {
			return shared.RespondError(c, err)
		}
		return marketplaceInternalError(c, "create marketplace image intent", err)
	}
	uploadURL, err := h.R2.PresignPut(ctx, key, contentType, marketplaceImageTTL)
	if err != nil || uploadURL == "" {
		slog.Error("PresignPut for marketplace image failed", "err", err, "user_id", user.ID, "listing_id", listingID)
		if cleanupErr := h.discardImageIntent(ctx, listingID, imageID, user.ID, key); cleanupErr != nil {
			slog.Error("discard marketplace upload intent after presign failure failed",
				"err", cleanupErr, "listing_id", listingID)
		}
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "presign marketplace image upload"))
	}
	return shared.RespondCreated(c, map[string]any{
		"image_id":       imageID,
		"upload_url":     uploadURL,
		"http_method":    http.MethodPut,
		"upload_headers": map[string]string{"Content-Type": contentType},
		"max_size_bytes": marketplaceMaxImageSize,
		"expires_at":     expiresAt.Format(time.RFC3339),
	})
}

func (h *MarketplaceHandler) ConfirmImage(c echo.Context) error {
	if !h.configuredForImages() {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace image upload is not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	listingID, imageID, err := marketplaceImageIDs(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	ctx := c.Request().Context()
	imageRow, err := h.Queries.GetMarketplaceListingImageForOwner(ctx, db.GetMarketplaceListingImageForOwnerParams{
		ID: imageID, ID_2: listingID, SellerUserID: user.ID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "image upload not found"))
		}
		return marketplaceInternalError(c, "load marketplace image upload", err)
	}
	if imageRow.Status != "pending" || !imageRow.ExpiresAt.Valid || !imageRow.ExpiresAt.Time.After(time.Now()) {
		return shared.RespondError(c, shared.Wrap(shared.ErrMarketplaceConflict, "image upload intent expired"))
	}
	size, err := h.R2.HeadObject(ctx, imageRow.R2Key)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return shared.RespondError(c, shared.Wrap(shared.ErrUploadMissing, "marketplace image upload not found"))
		}
		return marketplaceInternalError(c, "inspect marketplace image upload", err)
	}
	if size <= 0 || size > marketplaceMaxImageSize {
		return h.rejectMarketplaceImage(c, user.ID, listingID, imageID,
			shared.Wrap(shared.ErrUploadSizeInvalid, "image size is outside the 1 byte to 10 MiB range"))
	}
	data, err := h.R2.GetObject(ctx, imageRow.R2Key, marketplaceMaxImageSize)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return shared.RespondError(c, shared.Wrap(shared.ErrUploadMissing, "marketplace image upload not found"))
		}
		return marketplaceInternalError(c, "read marketplace image upload", err)
	}
	if err := validateAvatarImage(data, imageRow.ContentType); err != nil {
		return h.rejectMarketplaceImage(c, user.ID, listingID, imageID,
			shared.NewAPIError(shared.CodeValidationError, "uploaded file is not a valid supported image").
				WithDetails(shared.FieldError{Field: "image_id", Message: err.Error()}))
	}
	extension, valid := avatarExtension(imageRow.ContentType)
	if !valid {
		return marketplaceInternalError(c, "confirm marketplace image", fmt.Errorf("unsupported stored content type %q", imageRow.ContentType))
	}
	finalKey := fmt.Sprintf("marketplace/listings/%s/images/%s-%s.%s", listingID, imageID, uuid.NewString(), extension)
	if err := h.R2.PutObject(ctx, finalKey, imageRow.ContentType, data); err != nil {
		h.recordCleanup(ctx, []string{finalKey})
		slog.Error("PutObject for confirmed marketplace image failed", "err", err, "user_id", user.ID, "listing_id", listingID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "promote marketplace image upload"))
	}
	var updated int64
	err = h.Run.Run(ctx, func(q marketplaceTxStore) error {
		updated, err = q.ConfirmMarketplaceListingImage(ctx, db.ConfirmMarketplaceListingImageParams{
			ID: imageID, ID_2: listingID, SellerUserID: user.ID, R2Key: finalKey,
		})
		if err != nil {
			return err
		}
		if updated != 1 {
			return nil
		}
		return recordMarketplaceCleanup(ctx, q, []string{imageRow.R2Key})
	})
	if err != nil || updated != 1 {
		h.recordCleanup(ctx, []string{finalKey})
		if err != nil {
			return marketplaceInternalError(c, "confirm marketplace image", err)
		}
		return shared.RespondError(c, shared.Wrap(shared.ErrMarketplaceConflict, "image upload intent expired or was replaced"))
	}
	imageURL, err := h.R2.PresignGet(ctx, finalKey, presignTTL(h.Cfg))
	if err != nil || imageURL == "" {
		slog.Error("PresignGet for marketplace image failed", "err", err, "user_id", user.ID, "listing_id", listingID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "presign marketplace image URL"))
	}
	return shared.RespondOK(c, marketplaceImageObject{ID: imageRow.ID, URL: imageURL, SortOrder: imageRow.SortOrder})
}

func (h *MarketplaceHandler) DeleteImage(c echo.Context) error {
	if h.Queries == nil || h.Run == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace image cleanup is not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	listingID, imageID, err := marketplaceImageIDs(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	ctx := c.Request().Context()
	err = h.Run.Run(ctx, func(q marketplaceTxStore) error {
		key, err := q.DeleteMarketplaceListingImage(ctx, db.DeleteMarketplaceListingImageParams{
			ID: imageID, ID_2: listingID, SellerUserID: user.ID,
		})
		if err != nil {
			return err
		}
		return recordMarketplaceCleanup(ctx, q, []string{key})
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "listing image not found"))
		}
		return marketplaceInternalError(c, "delete marketplace image", err)
	}
	return shared.RespondNoContent(c)
}

func (h *MarketplaceHandler) Favorite(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	listingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	arg := db.IsMarketplaceFavoriteEligibleParams{UserID: user.ID, ListingID: listingID}
	eligible, err := h.Queries.IsMarketplaceFavoriteEligible(c.Request().Context(), arg)
	if err != nil {
		return marketplaceInternalError(c, "check marketplace favorite eligibility", err)
	}
	if !eligible {
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "listing not found"))
	}
	inserted, err := h.Queries.InsertMarketplaceFavorite(c.Request().Context(), db.InsertMarketplaceFavoriteParams{
		UserID: user.ID, ListingID: listingID,
	})
	if err != nil {
		return marketplaceInternalError(c, "save marketplace favorite", err)
	}
	if inserted == 0 {
		eligible, err = h.Queries.IsMarketplaceFavoriteEligible(c.Request().Context(), arg)
		if err != nil {
			return marketplaceInternalError(c, "recheck marketplace favorite eligibility", err)
		}
		if !eligible {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "listing not found"))
		}
	}
	return shared.RespondOK(c, map[string]bool{"favorited": true})
}

func (h *MarketplaceHandler) Unfavorite(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	listingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	if err := h.Queries.DeleteMarketplaceFavorite(c.Request().Context(), db.DeleteMarketplaceFavoriteParams{
		UserID: user.ID, ListingID: listingID,
	}); err != nil {
		return marketplaceInternalError(c, "remove marketplace favorite", err)
	}
	return shared.RespondNoContent(c)
}

func (h *MarketplaceHandler) MyFavorites(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	limit, err := marketplaceLimit(c.QueryParam("limit"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	cursorTime, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid cursor"))
	}
	params := db.ListMarketplaceFavoritesParams{UserID: user.ID, PageSize: int32(limit + 1)}
	if !cursorTime.IsZero() {
		params.CursorCreatedAt = sql.NullTime{Time: cursorTime, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}
	rows, err := h.Queries.ListMarketplaceFavorites(c.Request().Context(), params)
	if err != nil {
		return marketplaceInternalError(c, "list marketplace favorites", err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	fields, ids := make([]marketplaceListingFields, 0, len(rows)), make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		fields = append(fields, marketplaceFieldsFromFavorite(row))
		ids = append(ids, row.ID)
	}
	items, err := h.listingObjects(c.Request().Context(), fields, ids)
	if err != nil {
		return marketplaceInternalError(c, "presign marketplace image URLs", err)
	}
	var next *string
	if hasMore && len(rows) > 0 {
		cursor := shared.EncodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		next = &cursor
	}
	return shared.RespondList(c, items, next)
}

func (h *MarketplaceHandler) Report(c echo.Context) error {
	if h.Queries == nil || h.Run == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	listingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	var req marketplaceReportRequest
	if err := bindMarketplaceJSON(c, &req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	req.Reason, req.Details = strings.TrimSpace(req.Reason), strings.TrimSpace(req.Details)
	if err := c.Validate(&req); err != nil {
		return shared.RespondError(c, err)
	}
	if req.Reason == "other" && req.Details == "" {
		return marketplaceFieldError(c, "details", "is required when reason is other")
	}
	ctx := c.Request().Context()
	var created bool
	err = h.Run.Run(ctx, func(q marketplaceTxStore) error {
		if err := q.LockMarketplaceQuota(ctx, "marketplace:reports:"+user.ID.String()); err != nil {
			return err
		}
		reported, err := q.CountMarketplaceReportsByReporterSince(ctx, db.CountMarketplaceReportsByReporterSinceParams{
			ReporterUserID: user.ID, CreatedAt: time.Now().Add(-24 * time.Hour),
		})
		if err != nil {
			return err
		}
		if reported >= marketplaceMaxReportsPerDay {
			return errMarketplaceQuotaExceeded
		}
		eligible, err := q.IsMarketplaceReportEligible(ctx, db.IsMarketplaceReportEligibleParams{
			UserID: user.ID, ListingID: listingID,
		})
		if err != nil {
			return err
		}
		if !eligible {
			return sql.ErrNoRows
		}
		inserted, err := q.InsertMarketplaceReport(ctx, db.InsertMarketplaceReportParams{
			UserID: user.ID, ListingID: listingID, Reason: req.Reason, Details: req.Details,
		})
		if err != nil {
			return err
		}
		if inserted == 0 {
			exists, err := q.HasMarketplaceReport(ctx, db.HasMarketplaceReportParams{
				ListingID: listingID, ReporterUserID: user.ID,
			})
			if err != nil {
				return err
			}
			if !exists {
				return sql.ErrNoRows
			}
			return nil
		}
		created = true
		return nil
	})
	if err != nil {
		if errors.Is(err, errMarketplaceQuotaExceeded) {
			return shared.RespondError(c, shared.Wrap(shared.ErrRateLimited, "report quota exceeded"))
		}
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "listing not found"))
		}
		return marketplaceInternalError(c, "report marketplace listing", err)
	}
	if !created {
		return shared.RespondOK(c, marketplaceReportResponse{Reported: true})
	}
	return shared.RespondCreated(c, marketplaceReportResponse{Reported: true, Created: true})
}

func (h *MarketplaceHandler) ContactSeller(c echo.Context) error {
	if h.Queries == nil || h.Run == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace contact is not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	listingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	ctx := c.Request().Context()
	var sellerID uuid.UUID
	var created bool
	err = h.Run.Run(ctx, func(q marketplaceTxStore) error {
		if err := q.LockMarketplaceQuota(ctx, "marketplace:contacts:"+user.ID.String()); err != nil {
			return err
		}
		contacts, err := q.CountMarketplaceContactsByBuyerSince(ctx, db.CountMarketplaceContactsByBuyerSinceParams{
			BuyerUserID: user.ID, CreatedAt: time.Now().Add(-24 * time.Hour),
		})
		if err != nil {
			return err
		}
		if contacts >= marketplaceMaxContactsPerDay {
			return errMarketplaceQuotaExceeded
		}
		sellerID, err = q.GetMarketplaceContactSeller(ctx, db.GetMarketplaceContactSellerParams{
			ID: listingID, ID_2: user.ID,
		})
		if err != nil {
			return err
		}
		if err := q.LockMarketplaceQuota(ctx, marketplacePairLockKey(user.ID, sellerID)); err != nil {
			return err
		}
		inserted, err := q.InsertMarketplaceContact(ctx, db.InsertMarketplaceContactParams{
			ID: listingID, BuyerUserID: user.ID, SellerUserID: sellerID,
		})
		if err != nil {
			return err
		}
		created = inserted > 0
		if inserted == 0 {
			renewed, err := q.RenewExpiredMarketplaceContact(ctx, db.RenewExpiredMarketplaceContactParams{
				ListingID: listingID, BuyerUserID: user.ID,
			})
			if err != nil {
				return err
			}
			created = renewed > 0
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errMarketplaceQuotaExceeded) {
			return shared.RespondError(c, shared.Wrap(shared.ErrRateLimited, "contact quota exceeded"))
		}
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "active listing not found"))
		}
		return marketplaceInternalError(c, "create marketplace contact", err)
	}
	return shared.RespondOK(c, marketplaceContactResponse{ListingID: listingID, SellerID: sellerID, Created: created})
}

func (h *MarketplaceHandler) RevokeContact(c echo.Context) error {
	if h.Queries == nil || h.Run == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "marketplace contact is not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	listingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid listing id"))
	}
	var buyerID, sellerID uuid.UUID
	var revoked bool
	ctx := c.Request().Context()
	err = h.Run.Run(ctx, func(q marketplaceTxStore) error {
		participants, err := q.GetMarketplaceContactParticipants(ctx, db.GetMarketplaceContactParticipantsParams{
			ListingID: listingID, BuyerUserID: user.ID,
		})
		if err != nil {
			return err
		}
		buyerID, sellerID = participants.BuyerUserID, participants.SellerUserID
		if err := q.LockMarketplaceQuota(ctx, marketplacePairLockKey(buyerID, sellerID)); err != nil {
			return err
		}
		rows, err := q.RevokeMarketplaceContact(ctx, db.RevokeMarketplaceContactParams{
			ListingID: listingID, BuyerUserID: user.ID,
		})
		if err != nil {
			return err
		}
		revoked = rows > 0
		if !revoked {
			return nil
		}
		stillAllowed, err := q.IsDMRecipientAllowed(ctx, db.IsDMRecipientAllowedParams{
			ID: sellerID, ID_2: buyerID,
		})
		if err != nil {
			return err
		}
		if stillAllowed {
			return nil
		}
		if _, err := q.DeleteDMIdempotencyForPairMessages(ctx, db.DeleteDMIdempotencyForPairMessagesParams{
			SenderID: buyerID, RecipientID: sellerID,
		}); err != nil {
			return err
		}
		_, err = q.DeleteDMInboxForPair(ctx, db.DeleteDMInboxForPairParams{
			SenderID: buyerID, RecipientID: sellerID,
		})
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "active contact not found"))
		}
		return marketplaceInternalError(c, "revoke marketplace contact", err)
	}
	return shared.RespondOK(c, marketplaceContactRevokeResponse{ListingID: listingID, Revoked: revoked})
}

func (h *MarketplaceHandler) configuredForImages() bool {
	return h.Queries != nil && h.Run != nil && h.R2 != nil && h.Cfg != nil
}

// recordMarketplaceCleanup writes the cleanup instruction in the same
// transaction as the marketplace row change it follows. The janitor
// dispatches the outbox row and retries if Redis is unavailable, so a
// committed row deletion can never lose its R2 cleanup.
func recordMarketplaceCleanup(ctx context.Context, q marketplaceTxStore, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	return q.InsertMarketplaceCleanupOutbox(ctx, keys)
}

func (h *MarketplaceHandler) recordCleanup(ctx context.Context, keys []string) {
	if len(keys) == 0 || h.Run == nil {
		return
	}
	if err := h.Run.Run(ctx, func(q marketplaceTxStore) error {
		return recordMarketplaceCleanup(ctx, q, keys)
	}); err != nil {
		slog.Error("record marketplace cleanup outbox failed", "err", err, "key_count", len(keys))
	}
}

func (h *MarketplaceHandler) discardImageIntent(ctx context.Context, listingID, imageID, userID uuid.UUID, key string) error {
	if h.Run == nil {
		return fmt.Errorf("marketplace transaction runner is nil")
	}
	return h.Run.Run(ctx, func(q marketplaceTxStore) error {
		if _, err := q.DeleteMarketplaceListingImage(ctx, db.DeleteMarketplaceListingImageParams{
			ID: imageID, ID_2: listingID, SellerUserID: userID,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return recordMarketplaceCleanup(ctx, q, []string{key})
	})
}

// SweepExpiredImages removes expired pending image intents in batches and
// records their R2 keys for cleanup in the same transaction. It is called
// by the api-gateway janitor, not by request handlers.
func (h *MarketplaceHandler) SweepExpiredImages(ctx context.Context, batchSize int32) (int, error) {
	if h.Queries == nil || h.Run == nil {
		return 0, fmt.Errorf("marketplace sweeper is not configured")
	}
	if batchSize <= 0 {
		return 0, fmt.Errorf("marketplace sweep batch size must be > 0")
	}
	swept := 0
	err := h.Run.Run(ctx, func(q marketplaceTxStore) error {
		rows, err := q.SweepExpiredMarketplaceListingImages(ctx, batchSize)
		if err != nil {
			return err
		}
		swept = len(rows)
		return recordMarketplaceCleanup(ctx, q, imageKeys(rows))
	})
	return swept, err
}

func (h *MarketplaceHandler) listingObject(ctx context.Context, fields marketplaceListingFields, rows []db.MarketplaceListingImage) (marketplaceListingObject, error) {
	items, err := h.imageObjects(ctx, rows)
	if err != nil {
		return marketplaceListingObject{}, err
	}
	var displayName *string
	if fields.displayName.Valid {
		displayName = &fields.displayName.String
	}
	return marketplaceListingObject{
		ID:       fields.id,
		Seller:   marketplaceSellerObject{UserID: fields.sellerID, Username: fields.username, DisplayName: displayName},
		Category: marketplaceCategoryObject{Slug: fields.categorySlug, Name: fields.categoryName},
		Title:    fields.title, Description: fields.description, PriceIDR: fields.price, City: fields.city,
		Latitude: fields.latitude, Longitude: fields.longitude, DistanceKM: fields.distanceKM,
		Condition: fields.condition, Status: fields.status, Images: items, IsFavorite: fields.favorite,
		CreatedAt: formatVideoTime(fields.createdAt), UpdatedAt: formatVideoTime(fields.updatedAt),
	}, nil
}

func (h *MarketplaceHandler) listingObjects(ctx context.Context, fields []marketplaceListingFields, ids []uuid.UUID) ([]marketplaceListingObject, error) {
	if len(fields) == 0 {
		return []marketplaceListingObject{}, nil
	}
	rows, err := h.Queries.ListMarketplaceImagesForListings(ctx, joinUUIDs(ids))
	if err != nil {
		return nil, err
	}
	byListing := make(map[uuid.UUID][]db.MarketplaceListingImage, len(fields))
	for _, row := range rows {
		byListing[row.ListingID] = append(byListing[row.ListingID], row)
	}
	items := make([]marketplaceListingObject, 0, len(fields))
	for _, field := range fields {
		images := byListing[field.id]
		if images == nil {
			images = []db.MarketplaceListingImage{}
		}
		item, err := h.listingObject(ctx, field, images)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (h *MarketplaceHandler) imageObjects(ctx context.Context, rows []db.MarketplaceListingImage) ([]marketplaceImageObject, error) {
	items := make([]marketplaceImageObject, 0, len(rows))
	for _, row := range rows {
		url, err := h.R2.PresignGet(ctx, row.R2Key, presignTTL(h.Cfg))
		if err != nil {
			return nil, fmt.Errorf("presign image %s: %w", row.ID, err)
		}
		if url == "" {
			return nil, fmt.Errorf("presign image %s returned an empty URL", row.ID)
		}
		items = append(items, marketplaceImageObject{ID: row.ID, URL: url, SortOrder: row.SortOrder})
	}
	return items, nil
}

func marketplaceCreateParams(userID uuid.UUID, req marketplaceListingRequest) db.CreateMarketplaceListingParams {
	return db.CreateMarketplaceListingParams{
		SellerUserID: userID, CategorySlug: strings.TrimSpace(req.Category),
		Title: strings.TrimSpace(req.Title), Description: strings.TrimSpace(req.Description),
		PriceIdr: req.PriceIDR, City: strings.TrimSpace(req.City), ItemCondition: req.Condition,
		Latitude:  marketplaceNullableFloat64(req.Latitude),
		Longitude: marketplaceNullableFloat64(req.Longitude),
	}
}

func (h *MarketplaceHandler) loadListingObject(ctx context.Context, viewerID, listingID uuid.UUID) (marketplaceListingObject, error) {
	row, err := h.Queries.GetMarketplaceListing(ctx, db.GetMarketplaceListingParams{ID: listingID, UserID: viewerID})
	if err != nil {
		return marketplaceListingObject{}, err
	}
	images, err := h.Queries.ListMarketplaceListingImages(ctx, listingID)
	if err != nil {
		return marketplaceListingObject{}, err
	}
	return h.listingObject(ctx, marketplaceFieldsFromDetail(row), images)
}

func validateMarketplaceListing(c echo.Context, req *marketplaceListingRequest) error {
	req.Category = strings.TrimSpace(req.Category)
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)
	req.City = strings.TrimSpace(req.City)
	req.Condition = strings.TrimSpace(req.Condition)
	if err := c.Validate(req); err != nil {
		return err
	}
	return validateMarketplaceCoordinates(req.Latitude, req.Longitude)
}

func marketplaceBrowseParams(c echo.Context, viewerID uuid.UUID) (db.ListMarketplaceListingsParams, int, error) {
	limit, err := marketplaceLimit(c.QueryParam("limit"))
	if err != nil {
		return db.ListMarketplaceListingsParams{}, 0, shared.Wrap(shared.ErrValidation, err.Error())
	}
	cursorTime, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return db.ListMarketplaceListingsParams{}, 0, shared.Wrap(shared.ErrValidation, "invalid cursor")
	}
	params := db.ListMarketplaceListingsParams{ViewerID: viewerID, PageSize: int32(limit + 1)}
	if category := strings.TrimSpace(c.QueryParam("category")); category != "" {
		if _, ok := marketplaceCategories[category]; !ok {
			return params, 0, marketplaceValidationError("category", "must be a supported Marketplace category")
		}
		params.CategorySlug = sql.NullString{String: category, Valid: true}
	}
	if city := strings.TrimSpace(c.QueryParam("city")); city != "" {
		if len([]rune(city)) > 100 {
			return params, 0, marketplaceValidationError("city", "must be at most 100 characters")
		}
		params.City = sql.NullString{String: city, Valid: true}
	}
	latRaw := strings.TrimSpace(c.QueryParam("latitude"))
	lonRaw := strings.TrimSpace(c.QueryParam("longitude"))
	radiusRaw := strings.TrimSpace(c.QueryParam("radius_km"))
	if latRaw != "" || lonRaw != "" || radiusRaw != "" {
		if latRaw == "" || lonRaw == "" || radiusRaw == "" {
			return params, 0, marketplaceValidationError("radius_km", "latitude, longitude, and radius_km must be provided together")
		}
		latitude, err := parseMarketplaceCoordinate(latRaw)
		if err != nil {
			return params, 0, marketplaceValidationError("latitude", err.Error())
		}
		longitude, err := parseMarketplaceCoordinate(lonRaw)
		if err != nil {
			return params, 0, marketplaceValidationError("longitude", err.Error())
		}
		radiusKM, err := parseMarketplaceCoordinate(radiusRaw)
		if err != nil {
			return params, 0, marketplaceValidationError("radius_km", err.Error())
		}
		if err := validateMarketplaceCoordinates(&latitude, &longitude); err != nil {
			return params, 0, err
		}
		if radiusKM <= 0 || radiusKM > marketplaceMaxRadiusKM {
			return params, 0, marketplaceValidationError("radius_km", fmt.Sprintf("must be between 0 and %.0f", marketplaceMaxRadiusKM))
		}
		params.Latitude = sql.NullFloat64{Float64: latitude, Valid: true}
		params.Longitude = sql.NullFloat64{Float64: longitude, Valid: true}
		params.RadiusKm = sql.NullFloat64{Float64: radiusKM, Valid: true}
	}
	if query := strings.TrimSpace(c.QueryParam("q")); query != "" {
		if len([]rune(query)) > 200 {
			return params, 0, marketplaceValidationError("q", "must be at most 200 characters")
		}
		params.QueryText = sql.NullString{String: query, Valid: true}
	}
	if raw := c.QueryParam("min_price_idr"); raw != "" {
		value, err := parseMarketplaceFilterPrice(raw)
		if err != nil {
			return params, 0, marketplaceValidationError("min_price_idr", err.Error())
		}
		params.MinPrice = sql.NullInt64{Int64: value, Valid: true}
	}
	if raw := c.QueryParam("max_price_idr"); raw != "" {
		value, err := parseMarketplaceFilterPrice(raw)
		if err != nil {
			return params, 0, marketplaceValidationError("max_price_idr", err.Error())
		}
		params.MaxPrice = sql.NullInt64{Int64: value, Valid: true}
	}
	if params.MinPrice.Valid && params.MaxPrice.Valid && params.MinPrice.Int64 > params.MaxPrice.Int64 {
		return params, 0, marketplaceValidationError("max_price_idr", "must be greater than or equal to min_price_idr")
	}
	if !cursorTime.IsZero() {
		params.CursorCreatedAt = sql.NullTime{Time: cursorTime, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}
	return params, limit, nil
}

func marketplaceOwnListingParams(c echo.Context, userID uuid.UUID) (db.ListMarketplaceOwnListingsParams, int, error) {
	limit, err := marketplaceLimit(c.QueryParam("limit"))
	if err != nil {
		return db.ListMarketplaceOwnListingsParams{}, 0, shared.Wrap(shared.ErrValidation, err.Error())
	}
	cursorTime, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return db.ListMarketplaceOwnListingsParams{}, 0, shared.Wrap(shared.ErrValidation, "invalid cursor")
	}
	params := db.ListMarketplaceOwnListingsParams{SellerUserID: userID, PageSize: int32(limit + 1)}
	if status := c.QueryParam("status"); status != "" {
		if status != "active" && status != "sold" && status != "archived" {
			return params, 0, marketplaceValidationError("status", "must be active, sold, or archived")
		}
		params.Status = sql.NullString{String: status, Valid: true}
	}
	if !cursorTime.IsZero() {
		params.CursorCreatedAt = sql.NullTime{Time: cursorTime, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}
	return params, limit, nil
}

func marketplaceLimit(raw string) (int, error) {
	if raw == "" {
		return marketplaceDefaultLimit, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer")
	}
	if value > marketplaceMaxLimit {
		value = marketplaceMaxLimit
	}
	return value, nil
}

func parseMarketplaceFilterPrice(raw string) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("must be a non-negative integer")
	}
	return value, nil
}

func parseMarketplaceCoordinate(raw string) (float64, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("must be a finite number")
	}
	return value, nil
}

func validateMarketplaceCoordinates(latitude, longitude *float64) error {
	if (latitude == nil) != (longitude == nil) {
		return marketplaceValidationError("latitude", "latitude and longitude must be provided together")
	}
	if latitude == nil {
		return nil
	}
	if *latitude < -90 || *latitude > 90 {
		return marketplaceValidationError("latitude", "must be between -90 and 90")
	}
	if *longitude < -180 || *longitude > 180 {
		return marketplaceValidationError("longitude", "must be between -180 and 180")
	}
	return nil
}

func marketplaceNullableFloat64(value *float64) sql.NullFloat64 {
	if value == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *value, Valid: true}
}

func marketplaceNullableFloat(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	coordinate := value.Float64
	return &coordinate
}

func marketplaceDistance(value any) *float64 {
	distance, ok := value.(float64)
	if !ok {
		return nil
	}
	return &distance
}

func marketplaceImageIDs(c echo.Context) (uuid.UUID, uuid.UUID, error) {
	listingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, uuid.Nil, shared.Wrap(shared.ErrValidation, "invalid listing id")
	}
	imageID, err := uuid.Parse(c.Param("image_id"))
	if err != nil {
		return uuid.Nil, uuid.Nil, shared.Wrap(shared.ErrValidation, "invalid image id")
	}
	return listingID, imageID, nil
}

func (h *MarketplaceHandler) rejectMarketplaceImage(c echo.Context, userID, listingID, imageID uuid.UUID, responseErr error) error {
	ctx := c.Request().Context()
	err := h.Run.Run(ctx, func(q marketplaceTxStore) error {
		key, err := q.DeleteMarketplaceListingImage(ctx, db.DeleteMarketplaceListingImageParams{
			ID: imageID, ID_2: listingID, SellerUserID: userID,
		})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return recordMarketplaceCleanup(ctx, q, []string{key})
	})
	if err != nil {
		return marketplaceInternalError(c, "remove rejected marketplace image", err)
	}
	return shared.RespondError(c, responseErr)
}

func imageKeys(rows []db.MarketplaceListingImage) []string {
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.R2Key)
	}
	return keys
}

func joinUUIDs(ids []uuid.UUID) string {
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, id.String())
	}
	return strings.Join(values, ",")
}

func marketplaceValidationError(field, message string) error {
	return shared.NewAPIError(shared.CodeValidationError, "invalid Marketplace request").
		WithDetails(shared.FieldError{Field: field, Message: message})
}

func marketplaceFieldError(c echo.Context, field, message string) error {
	return shared.RespondError(c, marketplaceValidationError(field, message))
}

func marketplaceInternalError(c echo.Context, operation string, err error) error {
	slog.Error(operation+" failed", "err", err)
	return shared.RespondError(c, shared.Wrap(shared.ErrInternal, operation))
}

func bindMarketplaceJSON(c echo.Context, dst any) error {
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, marketplaceBodyLimit)
	return c.Bind(dst)
}
