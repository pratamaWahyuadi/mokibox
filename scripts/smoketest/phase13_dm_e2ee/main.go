package main

// Run after migration 009 against a dedicated integration database:
//
//	DATABASE_URL=postgres://... DM_INBOX_TTL=168h go run ./scripts/smoketest/phase13_dm_e2ee
//
// It creates and removes uniquely named test users and runs the relay flow
// twice consecutively. Random bytes are opaque test payloads, not ciphertext
// produced by a cryptographic implementation.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/handlers"
	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type smokeVerifier struct {
	userAName string
	userBName string
}

func (v smokeVerifier) CheckToken(_ context.Context, token string) (string, error) {
	switch token {
	case "smoke-user-a":
		return v.userAName, nil
	case "smoke-user-b":
		return v.userBName, nil
	default:
		return "", errors.New("unknown smoke token")
	}
}

type responseEnvelope[T any] struct {
	Data T `json:"data"`
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type deviceRequest struct {
	Protocol            string `json:"protocol"`
	Version             int32  `json:"version"`
	KeyBundle           []byte `json:"key_bundle"`
	FallbackKeyID       string `json:"fallback_key_id,omitempty"`
	FallbackKeyMaterial []byte `json:"fallback_key_material,omitempty"`
	OneTimeKeys         []struct {
		KeyID       string `json:"key_id"`
		KeyMaterial []byte `json:"key_material"`
	} `json:"one_time_keys"`
}

type deviceResponse struct {
	Protocol string    `json:"protocol"`
	Version  int32     `json:"version"`
	DeviceID uuid.UUID `json:"device_id"`
}

type keyBundle struct {
	Protocol   string `json:"protocol"`
	Version    int32  `json:"version"`
	KeyBundle  []byte `json:"key_bundle"`
	OneTimeKey *struct {
		KeyID       string `json:"key_id"`
		KeyMaterial []byte `json:"key_material"`
	} `json:"one_time_key"`
	FallbackKey *struct {
		KeyID       string `json:"key_id"`
		KeyMaterial []byte `json:"key_material"`
	} `json:"fallback_key"`
}

type sendRequest struct {
	RecipientUserID uuid.UUID `json:"recipient_user_id"`
	Protocol        string    `json:"protocol"`
	Version         int32     `json:"version"`
	ClientMessageID uuid.UUID `json:"client_message_id"`
	Ciphertext      []byte    `json:"ciphertext"`
}

type sendResponse struct {
	MessageID       uuid.UUID `json:"message_id"`
	Sequence        int64     `json:"sequence"`
	ClientMessageID uuid.UUID `json:"client_message_id"`
}

type inboxResponse struct {
	Messages []struct {
		MessageID  uuid.UUID `json:"message_id"`
		Sequence   int64     `json:"sequence"`
		Ciphertext []byte    `json:"ciphertext"`
	} `json:"messages"`
	NextAfter int64 `json:"next_after"`
}

func main() {
	for runNumber := 1; runNumber <= 2; runNumber++ {
		if err := run(); err != nil {
			log.Fatalf("FAIL phase13_dm_e2ee run %d: %v", runNumber, err)
		}
		log.Printf("PASS phase13_dm_e2ee run %d (relay behavior only; not a cryptography test)", runNumber)
	}
	log.Println("PASS phase13_dm_e2ee (2x consecutively; relay behavior only; not a cryptography test)")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	ttlText := os.Getenv("DM_INBOX_TTL")
	if ttlText == "" {
		return fmt.Errorf("DM_INBOX_TTL is required")
	}
	ttl, err := time.ParseDuration(ttlText)
	if err != nil || ttl <= 0 {
		return fmt.Errorf("DM_INBOX_TTL must be a positive Go duration")
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	queries := db.New(sqlDB)

	runID := uuid.NewString()
	userAName := "smoke-dm-e2ee-a-" + runID
	userBName := "smoke-dm-e2ee-b-" + runID
	userA, err := insertSmokeUser(ctx, sqlDB, userAName)
	if err != nil {
		return fmt.Errorf("insert user A: %w", err)
	}
	var userB uuid.UUID
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, cleanupErr := sqlDB.ExecContext(cleanupCtx, `DELETE FROM users WHERE id IN ($1, $2)`, userA, userB); cleanupErr != nil {
			log.Printf("WARN phase13_dm_e2ee cleanup failed: %v", cleanupErr)
		}
	}()
	userB, err = insertSmokeUser(ctx, sqlDB, userBName)
	if err != nil {
		return fmt.Errorf("insert user B: %w", err)
	}

	dmHandler, err := handlers.NewDMHandler(queries, sqlDB, ttl)
	if err != nil {
		return fmt.Errorf("build DM handler: %w", err)
	}
	e := echo.New()
	api := e.Group("/api", middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: smokeVerifier{userAName: userAName, userBName: userBName},
		Queries:  queries,
	}))
	dmHandler.RegisterRoutes(api)
	server := httptest.NewServer(e)
	defer server.Close()
	client := server.Client()

	keyBundleA, err := randomBytes(101)
	if err != nil {
		return fmt.Errorf("generate key bundle A: %w", err)
	}
	deviceA, err := registerDevice(ctx, client, server.URL, "smoke-user-a", deviceRequest{
		Protocol: "opaque-smoke-protocol", Version: 32768, KeyBundle: keyBundleA,
	})
	if err != nil {
		return fmt.Errorf("register device A: %w", err)
	}
	keyBundleB, err := randomBytes(89)
	if err != nil {
		return fmt.Errorf("generate key bundle B: %w", err)
	}
	fallbackKey, err := randomBytes(41)
	if err != nil {
		return fmt.Errorf("generate fallback key: %w", err)
	}
	oneTimeKey, err := randomBytes(53)
	if err != nil {
		return fmt.Errorf("generate one-time key: %w", err)
	}
	deviceBRequest := deviceRequest{
		Protocol: "opaque-smoke-protocol", Version: 32768, KeyBundle: keyBundleB,
		FallbackKeyID: "fallback-1", FallbackKeyMaterial: fallbackKey,
	}
	deviceBRequest.OneTimeKeys = append(deviceBRequest.OneTimeKeys, struct {
		KeyID       string `json:"key_id"`
		KeyMaterial []byte `json:"key_material"`
	}{KeyID: "one-time-1", KeyMaterial: oneTimeKey})
	deviceB, err := registerDevice(ctx, client, server.URL, "smoke-user-b", deviceBRequest)
	if err != nil {
		return fmt.Errorf("register device B: %w", err)
	}
	_, deniedSend, err := doRequest(ctx, client, server.URL, "smoke-user-a", http.MethodPost, "/api/dm/messages", sendRequest{
		RecipientUserID: userB, Protocol: "opaque-smoke-protocol", Version: 32768,
		ClientMessageID: uuid.New(), Ciphertext: []byte("opaque"),
	})
	if err != nil {
		return err
	}
	if deniedSend.StatusCode != http.StatusNotFound {
		return fmt.Errorf("send without mutual follow status=%d, want 404", deniedSend.StatusCode)
	}

	// Before mutual follows, an existing-but-ineligible recipient and an
	// unknown recipient must return the same not-found contract.
	ineligibleBody, ineligible, err := doRequest(ctx, client, server.URL, "smoke-user-a", http.MethodGet,
		"/api/dm/users/"+userB.String()+"/key-bundle", nil)
	if err != nil {
		return err
	}
	unknownID := uuid.New()
	unknownBody, unknown, err := doRequest(ctx, client, server.URL, "smoke-user-a", http.MethodGet,
		"/api/dm/users/"+unknownID.String()+"/key-bundle", nil)
	if err != nil {
		return err
	}
	if ineligible.StatusCode != http.StatusNotFound || unknown.StatusCode != ineligible.StatusCode {
		return fmt.Errorf("ineligible/unknown statuses = %d/%d, want both 404", ineligible.StatusCode, unknown.StatusCode)
	}
	if !bytes.Equal(ineligibleBody, unknownBody) {
		return fmt.Errorf("ineligible and unknown recipient responses differ")
	}

	if err := queries.FollowUser(ctx, db.FollowUserParams{FollowerID: userA, FolloweeID: userB}); err != nil {
		return fmt.Errorf("follow A to B: %w", err)
	}
	if err := queries.FollowUser(ctx, db.FollowUserParams{FollowerID: userB, FolloweeID: userA}); err != nil {
		return fmt.Errorf("follow B to A: %w", err)
	}

	bundleBody, status, err := doRequest(ctx, client, server.URL, "smoke-user-a", http.MethodGet,
		"/api/dm/users/"+userB.String()+"/key-bundle", nil)
	if err != nil || status.StatusCode != http.StatusOK {
		return fmt.Errorf("claim one-time bundle: status=%v err=%v body=%s", status, err, bundleBody)
	}
	var bundle responseEnvelope[keyBundle]
	if err := json.Unmarshal(bundleBody, &bundle); err != nil {
		return fmt.Errorf("decode first key bundle: %w", err)
	}
	if bundle.Data.Protocol != "opaque-smoke-protocol" || bundle.Data.Version != 32768 ||
		!bytes.Equal(bundle.Data.KeyBundle, keyBundleB) ||
		bundle.Data.OneTimeKey == nil || bundle.Data.OneTimeKey.KeyID != "one-time-1" ||
		!bytes.Equal(bundle.Data.OneTimeKey.KeyMaterial, oneTimeKey) {
		return fmt.Errorf("first bundle did not claim the one-time key")
	}
	bundleBody, status, err = doRequest(ctx, client, server.URL, "smoke-user-a", http.MethodGet,
		"/api/dm/users/"+userB.String()+"/key-bundle", nil)
	if err != nil || status.StatusCode != http.StatusOK {
		return fmt.Errorf("fallback bundle: status=%v err=%v body=%s", status, err, bundleBody)
	}
	if err := json.Unmarshal(bundleBody, &bundle); err != nil {
		return fmt.Errorf("decode fallback bundle: %w", err)
	}
	if bundle.Data.FallbackKey == nil || bundle.Data.FallbackKey.KeyID != "fallback-1" ||
		!bytes.Equal(bundle.Data.FallbackKey.KeyMaterial, fallbackKey) {
		return fmt.Errorf("exhausted bundle did not return its fallback key")
	}

	firstCiphertext, err := randomBytes(64)
	if err != nil {
		return fmt.Errorf("generate first ciphertext: %w", err)
	}
	firstRequest := sendRequest{
		RecipientUserID: userB, Protocol: "opaque-smoke-protocol", Version: 32768,
		ClientMessageID: uuid.New(),
		Ciphertext:      firstCiphertext,
	}
	first, err := sendMessage(ctx, client, server.URL, firstRequest)
	if err != nil {
		return fmt.Errorf("send first message: %w", err)
	}
	retry, err := sendMessage(ctx, client, server.URL, firstRequest)
	if err != nil {
		return fmt.Errorf("retry first message: %w", err)
	}
	if retry.MessageID != first.MessageID || retry.Sequence != first.Sequence {
		return fmt.Errorf("idempotent retry changed message id or sequence")
	}
	conflicting := firstRequest
	conflicting.Ciphertext, err = randomBytes(64)
	if err != nil {
		return fmt.Errorf("generate conflicting ciphertext: %w", err)
	}
	if _, status, err := doRequest(ctx, client, server.URL, "smoke-user-a", http.MethodPost, "/api/dm/messages", conflicting); err != nil ||
		status.StatusCode != http.StatusConflict {
		return fmt.Errorf("idempotency conflict status=%v err=%v", status, err)
	}
	secondCiphertext, err := randomBytes(79)
	if err != nil {
		return fmt.Errorf("generate second ciphertext: %w", err)
	}
	second, err := sendMessage(ctx, client, server.URL, sendRequest{
		RecipientUserID: userB, Protocol: "opaque-smoke-protocol", Version: 32768,
		ClientMessageID: uuid.New(),
		Ciphertext:      secondCiphertext,
	})
	if err != nil {
		return fmt.Errorf("send second message: %w", err)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		return fmt.Errorf("sequences = %d,%d, want 1,2", first.Sequence, second.Sequence)
	}
	const concurrentSends = 8
	concurrentResults := make([]sendResponse, concurrentSends)
	concurrentErrors := make(chan error, concurrentSends)
	var wait sync.WaitGroup
	for i := 0; i < concurrentSends; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			ciphertext, err := randomBytes(32 + index)
			if err != nil {
				concurrentErrors <- fmt.Errorf("generate concurrent ciphertext: %w", err)
				return
			}
			result, err := sendMessage(ctx, client, server.URL, sendRequest{
				RecipientUserID: userB, Protocol: "opaque-smoke-protocol", Version: 32768,
				ClientMessageID: uuid.New(),
				Ciphertext:      ciphertext,
			})
			if err != nil {
				concurrentErrors <- fmt.Errorf("send concurrent message: %w", err)
				return
			}
			concurrentResults[index] = result
		}(i)
	}
	wait.Wait()
	close(concurrentErrors)
	for err := range concurrentErrors {
		if err != nil {
			return err
		}
	}
	seenSequences := make(map[int64]bool, concurrentSends)
	for _, result := range concurrentResults {
		if result.Sequence < 3 || result.Sequence >= 3+concurrentSends || seenSequences[result.Sequence] {
			return fmt.Errorf("concurrent send sequence is duplicated or out of range: %d", result.Sequence)
		}
		seenSequences[result.Sequence] = true
	}

	pollBody, status, err := doRequest(ctx, client, server.URL, "smoke-user-b", http.MethodGet, "/api/dm/inbox?after=0", nil)
	if err != nil || status.StatusCode != http.StatusOK {
		return fmt.Errorf("poll inbox: status=%v err=%v body=%s", status, err, pollBody)
	}
	var inbox responseEnvelope[inboxResponse]
	if err := json.Unmarshal(pollBody, &inbox); err != nil {
		return fmt.Errorf("decode inbox: %w", err)
	}
	expectedMessages := 2 + concurrentSends
	if len(inbox.Data.Messages) != expectedMessages ||
		inbox.Data.NextAfter != int64(expectedMessages) {
		return fmt.Errorf("inbox order/cursor unexpected: %+v", inbox.Data)
	}
	for i, message := range inbox.Data.Messages {
		if message.Sequence != int64(i+1) {
			return fmt.Errorf("poll sequence at position %d = %d, want %d", i, message.Sequence, i+1)
		}
	}
	if !bytes.Equal(inbox.Data.Messages[0].Ciphertext, firstRequest.Ciphertext) ||
		!bytes.Equal(inbox.Data.Messages[1].Ciphertext, secondCiphertext) {
		return fmt.Errorf("poll ciphertext bytes differ from sent opaque bytes")
	}

	for _, message := range inbox.Data.Messages {
		_, status, err := doRequest(ctx, client, server.URL, "smoke-user-b", http.MethodPost,
			"/api/dm/inbox/"+message.MessageID.String()+"/ack", nil)
		if err != nil || status.StatusCode != http.StatusNoContent {
			return fmt.Errorf("ack message: status=%v err=%v", status, err)
		}
	}
	pollBody, status, err = doRequest(ctx, client, server.URL, "smoke-user-b", http.MethodGet, "/api/dm/inbox?after=0", nil)
	if err != nil || status.StatusCode != http.StatusOK {
		return fmt.Errorf("poll after ack: status=%v err=%v", status, err)
	}
	if err := json.Unmarshal(pollBody, &inbox); err != nil {
		return fmt.Errorf("decode empty inbox: %w", err)
	}
	if len(inbox.Data.Messages) != 0 {
		return fmt.Errorf("acknowledged inbox is not empty")
	}

	expiredCiphertext, err := randomBytes(48)
	if err != nil {
		return fmt.Errorf("generate expired ciphertext: %w", err)
	}
	expiredID := uuid.New()
	expiredSequence, err := queries.IncrementDMDeviceSequence(ctx, deviceB)
	if err != nil {
		return fmt.Errorf("allocate expired sequence: %w", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `
		INSERT INTO dm_inbox
			(recipient_device_id, recipient_user_id, sequence_no, sender_user_id,
			 sender_device_id, protocol, version, ciphertext, client_message_id, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NOW()-INTERVAL '2 hours',NOW()-INTERVAL '1 hour')`,
		deviceB, userB, expiredSequence, userA, deviceA, "opaque-smoke-protocol", 32768,
		expiredCiphertext, expiredID); err != nil {
		return fmt.Errorf("seed expired ciphertext: %w", err)
	}
	pollBody, status, err = doRequest(ctx, client, server.URL, "smoke-user-b", http.MethodGet, "/api/dm/inbox?after=2", nil)
	if err != nil || status.StatusCode != http.StatusOK {
		return fmt.Errorf("poll expired inbox: status=%v err=%v", status, err)
	}
	if err := json.Unmarshal(pollBody, &inbox); err != nil {
		return fmt.Errorf("decode expired inbox: %w", err)
	}
	if len(inbox.Data.Messages) != 0 {
		return fmt.Errorf("expired ciphertext was returned")
	}
	var expiredExists bool
	if err := sqlDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM dm_inbox WHERE client_message_id=$1)`, expiredID).Scan(&expiredExists); err != nil {
		return fmt.Errorf("check expired ciphertext cleanup: %w", err)
	}
	if expiredExists {
		return fmt.Errorf("expired ciphertext was not deleted")
	}
	return nil
}

func insertSmokeUser(ctx context.Context, sqlDB *sql.DB, username string) (uuid.UUID, error) {
	var id uuid.UUID
	err := sqlDB.QueryRowContext(ctx, `
		INSERT INTO users (zitadel_id, username, display_name)
		VALUES ($1, $2, $2)
		RETURNING id`, username, username).Scan(&id)
	return id, err
}

func registerDevice(ctx context.Context, client *http.Client, baseURL, token string, req deviceRequest) (uuid.UUID, error) {
	body, status, err := doRequest(ctx, client, baseURL, token, http.MethodPost, "/api/dm/device", req)
	if err != nil {
		return uuid.Nil, err
	}
	if status.StatusCode != http.StatusCreated {
		return uuid.Nil, fmt.Errorf("status=%d body=%s", status.StatusCode, body)
	}
	var response responseEnvelope[deviceResponse]
	if err := json.Unmarshal(body, &response); err != nil {
		return uuid.Nil, err
	}
	return response.Data.DeviceID, nil
}

func sendMessage(ctx context.Context, client *http.Client, baseURL string, req sendRequest) (sendResponse, error) {
	body, status, err := doRequest(ctx, client, baseURL, "smoke-user-a", http.MethodPost, "/api/dm/messages", req)
	if err != nil {
		return sendResponse{}, err
	}
	if status.StatusCode != http.StatusCreated {
		return sendResponse{}, fmt.Errorf("status=%d body=%s", status.StatusCode, body)
	}
	var response responseEnvelope[sendResponse]
	if err := json.Unmarshal(body, &response); err != nil {
		return sendResponse{}, err
	}
	return response.Data, nil
}

func doRequest(ctx context.Context, client *http.Client, baseURL, token, method, path string, body any) ([]byte, *http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	if body != nil {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, response, err
	}
	return responseBody, response, nil
}

func randomBytes(n int) ([]byte, error) {
	value := make([]byte, n)
	if _, err := rand.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}

var _ middleware.TokenVerifier = smokeVerifier{}
