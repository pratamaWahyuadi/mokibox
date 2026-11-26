package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type envelope[T any] struct {
	Data T `json:"data"`
}

type selfResponse struct {
	ID uuid.UUID `json:"id"`
}

type category struct {
	Slug string `json:"slug"`
}

type listingRequest struct {
	Category    string   `json:"category"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	PriceIDR    int64    `json:"price_idr"`
	City        string   `json:"city"`
	Condition   string   `json:"condition"`
	Latitude    *float64 `json:"latitude,omitempty"`
	Longitude   *float64 `json:"longitude,omitempty"`
}

type listing struct {
	ID         uuid.UUID `json:"id"`
	Title      string    `json:"title"`
	PriceIDR   int64     `json:"price_idr"`
	City       string    `json:"city"`
	Latitude   *float64  `json:"latitude"`
	Longitude  *float64  `json:"longitude"`
	DistanceKM *float64  `json:"distance_km"`
	Status     string    `json:"status"`
	Seller     struct {
		UserID uuid.UUID `json:"user_id"`
	} `json:"seller"`
}

type contactResponse struct {
	ListingID uuid.UUID `json:"listing_id"`
	SellerID  uuid.UUID `json:"seller_user_id"`
	Created   bool      `json:"created"`
}

type contactRevokeResponse struct {
	ListingID uuid.UUID `json:"listing_id"`
	Revoked   bool      `json:"revoked"`
}

type reportResponse struct {
	Reported bool `json:"reported"`
	Created  bool `json:"created"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS scripts/smoketest/marketplace")
}

func run() (retErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dsn := requiredEnv("DATABASE_URL")
	apiURL := strings.TrimRight(os.Getenv("MARKETPLACE_API_URL"), "/")
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}
	sellerID, err := uuid.Parse(requiredEnv("MARKETPLACE_SMOKE_SELLER_USER"))
	if err != nil {
		return fmt.Errorf("MARKETPLACE_SMOKE_SELLER_USER: %w", err)
	}
	buyerID, err := uuid.Parse(requiredEnv("MARKETPLACE_SMOKE_BUYER_USER"))
	if err != nil {
		return fmt.Errorf("MARKETPLACE_SMOKE_BUYER_USER: %w", err)
	}
	if sellerID == buyerID {
		return errors.New("seller and buyer must be different users")
	}
	sellerToken := requiredEnv("MARKETPLACE_SMOKE_SELLER_TOKEN")
	buyerToken := requiredEnv("MARKETPLACE_SMOKE_BUYER_TOKEN")

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	if err := assertSchema(ctx, sqlDB); err != nil {
		return err
	}
	if err := assertIdentity(ctx, apiURL, sellerToken, sellerID); err != nil {
		return fmt.Errorf("seller identity: %w", err)
	}
	if err := assertIdentity(ctx, apiURL, buyerToken, buyerID); err != nil {
		return fmt.Errorf("buyer identity: %w", err)
	}

	marker := uuid.NewString()
	title := "Marketplace smoke " + marker
	city := "Smoke City " + marker
	listingID := uuid.Nil
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, cleanupErr := sqlDB.ExecContext(cleanupCtx,
			`DELETE FROM marketplace_listings
			 WHERE seller_user_id = $1 AND (id = $2 OR title = $3)`,
			sellerID, listingID, title)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("cleanup marketplace smoke listing: %w", cleanupErr)
			if retErr == nil {
				retErr = cleanupErr
			} else {
				fmt.Fprintln(os.Stderr, cleanupErr)
			}
		}
	}()

	var categories envelope[[]category]
	if _, err := requestJSON(ctx, http.MethodGet, apiURL+"/api/marketplace/categories", sellerToken, nil, &categories); err != nil {
		return fmt.Errorf("list categories: %w", err)
	}
	hasOther := false
	for _, item := range categories.Data {
		if item.Slug == "other" {
			hasOther = true
			break
		}
	}
	if !hasOther {
		return errors.New("category set does not include other")
	}

	latitude, longitude := -6.2088, 106.8456
	createBody := listingRequest{
		Category: "other", Title: title, Description: "Marketplace HTTP smoke listing",
		PriceIDR: 123456, City: city, Condition: "good",
		Latitude: &latitude, Longitude: &longitude,
	}
	var created envelope[listing]
	status, err := requestJSON(ctx, http.MethodPost, apiURL+"/api/marketplace/listings", sellerToken, createBody, &created)
	if err != nil {
		return fmt.Errorf("create listing: %w", err)
	}
	if status != http.StatusCreated || created.Data.ID == uuid.Nil {
		return fmt.Errorf("create listing: status=%d, id=%s", status, created.Data.ID)
	}
	listingID = created.Data.ID

	query := url.Values{
		"q":             {marker},
		"category":      {"other"},
		"city":          {strings.ToLower(city)},
		"min_price_idr": {"123456"},
		"max_price_idr": {"123456"},
		"latitude":      {"-6.21"},
		"longitude":     {"106.84"},
		"radius_km":     {"5"},
	}
	var results envelope[[]listing]
	if _, err := requestJSON(ctx, http.MethodGet,
		apiURL+"/api/marketplace/listings?"+query.Encode(), buyerToken, nil, &results); err != nil {
		return fmt.Errorf("browse listing: %w", err)
	}
	if len(results.Data) != 1 || results.Data[0].ID != listingID ||
		results.Data[0].Seller.UserID != sellerID || results.Data[0].Status != "active" {
		return fmt.Errorf("filtered browse did not return created listing: %+v", results.Data)
	}
	if results.Data[0].DistanceKM == nil || *results.Data[0].DistanceKM < 0 || *results.Data[0].DistanceKM > 5 {
		return fmt.Errorf("browse distance_km = %v, want 0..5", results.Data[0].DistanceKM)
	}
	if results.Data[0].Latitude == nil || results.Data[0].Longitude == nil {
		return errors.New("browse response missing listing coordinates")
	}

	farQuery := url.Values{
		"q":         {marker},
		"category":  {"other"},
		"latitude":  {"0"},
		"longitude": {"0"},
		"radius_km": {"1"},
	}
	var farResults envelope[[]listing]
	if _, err := requestJSON(ctx, http.MethodGet,
		apiURL+"/api/marketplace/listings?"+farQuery.Encode(), buyerToken, nil, &farResults); err != nil {
		return fmt.Errorf("far radius browse: %w", err)
	}
	if len(farResults.Data) != 0 {
		return fmt.Errorf("far radius browse returned listings: %+v", farResults.Data)
	}

	favoriteURL := fmt.Sprintf("%s/api/marketplace/listings/%s/favorite", apiURL, listingID)
	for range 2 {
		var favorite envelope[map[string]bool]
		if _, err := requestJSON(ctx, http.MethodPost, favoriteURL, buyerToken, nil, &favorite); err != nil {
			return fmt.Errorf("favorite listing: %w", err)
		}
		if !favorite.Data["favorited"] {
			return errors.New("favorite endpoint did not return favorited=true")
		}
	}

	reportURL := fmt.Sprintf("%s/api/marketplace/listings/%s/report", apiURL, listingID)
	reportBody := map[string]string{"reason": "other", "details": "Marketplace smoke report"}
	for attempt := 0; attempt < 2; attempt++ {
		var report envelope[reportResponse]
		status, err := requestJSON(ctx, http.MethodPost, reportURL, buyerToken, reportBody, &report)
		if err != nil {
			return fmt.Errorf("report listing: %w", err)
		}
		if !report.Data.Reported || (attempt == 0 && (status != http.StatusCreated || !report.Data.Created)) ||
			(attempt == 1 && (status != http.StatusOK || report.Data.Created)) {
			return fmt.Errorf("unexpected idempotent report response: status=%d response=%+v", status, report.Data)
		}
	}

	contactURL := fmt.Sprintf("%s/api/marketplace/listings/%s/contact", apiURL, listingID)
	for attempt := 0; attempt < 2; attempt++ {
		var contact envelope[contactResponse]
		status, err := requestJSON(ctx, http.MethodPost, contactURL, buyerToken, nil, &contact)
		if err != nil {
			return fmt.Errorf("contact seller: %w", err)
		}
		if status != http.StatusOK || contact.Data.ListingID != listingID || contact.Data.SellerID != sellerID ||
			contact.Data.Created != (attempt == 0) {
			return fmt.Errorf("unexpected idempotent contact response: status=%d response=%+v", status, contact.Data)
		}
	}

	var favoriteCount, reportCount, contactCount int
	err = sqlDB.QueryRowContext(ctx, `
		SELECT
		    (SELECT COUNT(*) FROM marketplace_favorites WHERE listing_id = $1 AND user_id = $2),
		    (SELECT COUNT(*) FROM marketplace_reports WHERE listing_id = $1 AND reporter_user_id = $2),
		    (SELECT COUNT(*) FROM marketplace_contacts WHERE listing_id = $1 AND buyer_user_id = $2 AND seller_user_id = $3)`,
		listingID, buyerID, sellerID).Scan(&favoriteCount, &reportCount, &contactCount)
	if err != nil {
		return fmt.Errorf("verify marketplace rows: %w", err)
	}
	if favoriteCount != 1 || reportCount != 1 || contactCount != 1 {
		return fmt.Errorf("unexpected persistent rows: favorites=%d reports=%d contacts=%d",
			favoriteCount, reportCount, contactCount)
	}

	if _, err := sqlDB.ExecContext(ctx, `
		UPDATE marketplace_contacts
		SET created_at = NOW() - INTERVAL '31 days',
		    expires_at = NOW() - INTERVAL '1 day'
		WHERE listing_id = $1 AND buyer_user_id = $2 AND seller_user_id = $3`,
		listingID, buyerID, sellerID); err != nil {
		return fmt.Errorf("expire contact grant: %w", err)
	}
	var renewed envelope[contactResponse]
	status, err = requestJSON(ctx, http.MethodPost, contactURL, buyerToken, nil, &renewed)
	if err != nil {
		return fmt.Errorf("renew contact: %w", err)
	}
	if status != http.StatusOK || !renewed.Data.Created {
		return fmt.Errorf("unexpected renew response: status=%d response=%+v", status, renewed.Data)
	}
	var activeAfterRenew int
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM marketplace_contacts
		WHERE listing_id = $1 AND buyer_user_id = $2 AND seller_user_id = $3
		  AND revoked_at IS NULL AND expires_at > NOW()`,
		listingID, buyerID, sellerID).Scan(&activeAfterRenew); err != nil {
		return fmt.Errorf("verify renewed contact: %w", err)
	}
	if activeAfterRenew != 1 {
		return fmt.Errorf("renewed contact active rows = %d", activeAfterRenew)
	}

	var revoke envelope[contactRevokeResponse]
	status, err = requestJSON(ctx, http.MethodDelete,
		contactURL+"?buyer_id="+buyerID.String(), sellerToken, nil, &revoke)
	if err != nil {
		return fmt.Errorf("revoke contact: %w", err)
	}
	if status != http.StatusOK || revoke.Data.ListingID != listingID || !revoke.Data.Revoked {
		return fmt.Errorf("unexpected revoke response: status=%d response=%+v", status, revoke.Data)
	}
	var revoked, alive int
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE revoked_at IS NOT NULL),
		       COUNT(*) FILTER (WHERE revoked_at IS NULL AND expires_at > NOW())
		FROM marketplace_contacts
		WHERE listing_id = $1 AND buyer_user_id = $2 AND seller_user_id = $3`,
		listingID, buyerID, sellerID).Scan(&revoked, &alive); err != nil {
		return fmt.Errorf("verify revoked contact: %w", err)
	}
	if revoked != 1 || alive != 0 {
		return fmt.Errorf("unexpected revoked contact state: revoked=%d active=%d", revoked, alive)
	}
	return nil
}

func requiredEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		fmt.Fprintln(os.Stderr, "missing required env:", name)
		os.Exit(1)
	}
	return value
}

func assertSchema(ctx context.Context, sqlDB *sql.DB) error {
	var listingTable, contactTable, latitudeColumn, longitudeColumn bool
	var categoryCount int
	err := sqlDB.QueryRowContext(ctx, `
		SELECT to_regclass('marketplace_listings') IS NOT NULL,
		       to_regclass('marketplace_contacts') IS NOT NULL,
		       (SELECT COUNT(*) FROM marketplace_categories),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_name = 'marketplace_listings' AND column_name = 'latitude'
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_name = 'marketplace_listings' AND column_name = 'longitude'
		       )`).Scan(
		&listingTable, &contactTable, &categoryCount, &latitudeColumn, &longitudeColumn)
	if err != nil {
		return fmt.Errorf("check Marketplace schema (apply migrations 012 and 014 first): %w", err)
	}
	if !listingTable || !contactTable || categoryCount != 8 || !latitudeColumn || !longitudeColumn {
		return fmt.Errorf("unexpected Marketplace schema: listings=%v contacts=%v categories=%d latitude=%v longitude=%v",
			listingTable, contactTable, categoryCount, latitudeColumn, longitudeColumn)
	}
	return nil
}

func assertIdentity(ctx context.Context, apiURL, token string, want uuid.UUID) error {
	var self envelope[selfResponse]
	if _, err := requestJSON(ctx, http.MethodGet, apiURL+"/api/users/me", token, nil, &self); err != nil {
		return err
	}
	if self.Data.ID != want {
		return fmt.Errorf("token user id = %s, expected %s", self.Data.ID, want)
	}
	return nil
}

func requestJSON(ctx context.Context, method, endpoint, token string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}
