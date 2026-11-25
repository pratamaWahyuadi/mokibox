package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type envelope[T any] struct {
	Data T `json:"data"`
}

type registerRequest struct {
	DeviceID           string `json:"device_id"`
	IdentityCurve25519 string `json:"identity_curve25519"`
	IdentityEd25519    string `json:"identity_ed25519"`
}

type registerResponse struct {
	DeviceID       uuid.UUID `json:"device_id"`
	ReplacedDevice bool      `json:"replaced_device"`
}

type oneTimeKeyInput struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

type uploadKeysRequest struct {
	DeviceID    string            `json:"device_id"`
	Keys        []oneTimeKeyInput `json:"keys"`
	FallbackKey *oneTimeKeyInput  `json:"fallback_key"`
}

type uploadKeysResponse struct {
	Accepted int `json:"accepted"`
}

type sendRequest struct {
	DeviceID          string `json:"device_id"`
	RecipientUserID   string `json:"recipient_user_id"`
	RecipientDeviceID string `json:"recipient_device_id"`
	ClientMessageID   string `json:"client_message_id"`
	Protocol          string `json:"protocol"`
	ProtocolVersion   int16  `json:"protocol_version"`
	MessageType       int16  `json:"message_type"`
	Ciphertext        string `json:"ciphertext"`
}

type sendResponse struct {
	MessageID uuid.UUID `json:"message_id"`
	Sequence  int64     `json:"sequence"`
	ExpiresAt time.Time `json:"expires_at"`
}

type inboxMessage struct {
	MessageID uuid.UUID `json:"message_id"`
}

type pollResponse struct {
	Messages []inboxMessage `json:"messages"`
}

type ackRequest struct {
	DeviceID string `json:"device_id"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS scripts/smoketest/dm")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	apiURL := os.Getenv("DM_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}
	userA, err := uuid.Parse(requiredEnv("DM_SMOKE_USER_A"))
	if err != nil {
		return fmt.Errorf("DM_SMOKE_USER_A: %w", err)
	}
	userB, err := uuid.Parse(requiredEnv("DM_SMOKE_USER_B"))
	if err != nil {
		return fmt.Errorf("DM_SMOKE_USER_B: %w", err)
	}
	tokenA := requiredEnv("DM_SMOKE_TOKEN_A")
	tokenB := requiredEnv("DM_SMOKE_TOKEN_B")

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	q := db.New(sqlDB)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := q.DeleteDMInboxByUser(cleanupCtx, userA); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup inbox A:", err)
		}
		if _, err := q.DeleteDMInboxByUser(cleanupCtx, userB); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup inbox B:", err)
		}
		if _, err := q.DeleteDMDevicesByUser(cleanupCtx, userA); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup devices A:", err)
		}
		if _, err := q.DeleteDMDevicesByUser(cleanupCtx, userB); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup devices B:", err)
		}
		if _, err := sqlDB.ExecContext(cleanupCtx, `DELETE FROM follows WHERE (follower_id = $1 AND followee_id = $2) OR (follower_id = $2 AND followee_id = $1)`, userA, userB); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup follows:", err)
		}
	}()

	if err := assertSchema(ctx, sqlDB); err != nil {
		return err
	}
	if _, err := sqlDB.ExecContext(ctx, `
		INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2), ($2, $1)
		ON CONFLICT (follower_id, followee_id) DO NOTHING`, userA, userB); err != nil {
		return fmt.Errorf("seed mutual follows: %w", err)
	}

	deviceA, deviceB := uuid.New(), uuid.New()
	if err := registerDevice(ctx, apiURL, tokenA, deviceA); err != nil {
		return fmt.Errorf("register device A: %w", err)
	}
	if err := registerDevice(ctx, apiURL, tokenB, deviceB); err != nil {
		return fmt.Errorf("register device B: %w", err)
	}
	if err := uploadKeys(ctx, apiURL, tokenB, deviceB); err != nil {
		return fmt.Errorf("upload keys B: %w", err)
	}
	if err := assertFallbackKey(ctx, sqlDB, deviceB); err != nil {
		return err
	}
	if err := verifyConcurrentKeyClaim(ctx, sqlDB, q, deviceB); err != nil {
		return err
	}
	if err := verifyIdempotencyUpsert(ctx, sqlDB, q, deviceA); err != nil {
		return err
	}

	clientMessageID := uuid.New()
	ciphertext := base64.RawStdEncoding.EncodeToString([]byte("smoke-ciphertext-not-plaintext"))
	send := sendRequest{
		DeviceID: deviceA.String(), RecipientUserID: userB.String(),
		RecipientDeviceID: deviceB.String(), ClientMessageID: clientMessageID.String(),
		Protocol: "olm", ProtocolVersion: 1, MessageType: 0, Ciphertext: ciphertext,
	}
	var first envelope[sendResponse]
	if err := requestJSON(ctx, http.MethodPost, apiURL+"/api/dm/messages", tokenA, send, &first); err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	var retry envelope[sendResponse]
	if err := requestJSON(ctx, http.MethodPost, apiURL+"/api/dm/messages", tokenA, send, &retry); err != nil {
		return fmt.Errorf("retry message: %w", err)
	}
	if first.Data.MessageID != retry.Data.MessageID || first.Data.Sequence != retry.Data.Sequence {
		return fmt.Errorf("idempotent send mismatch: %+v vs %+v", first.Data, retry.Data)
	}

	pollURL := fmt.Sprintf("%s/api/dm/inbox?device_id=%s&after=0&limit=10", apiURL, deviceB)
	var poll envelope[pollResponse]
	if err := requestJSON(ctx, http.MethodGet, pollURL, tokenB, nil, &poll); err != nil {
		return fmt.Errorf("poll inbox: %w", err)
	}
	found := false
	for _, message := range poll.Data.Messages {
		if message.MessageID == first.Data.MessageID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("polled inbox does not contain %s", first.Data.MessageID)
	}
	if err := requestJSON(ctx, http.MethodPost,
		apiURL+"/api/dm/inbox/"+first.Data.MessageID.String()+"/ack", tokenB,
		ackRequest{DeviceID: deviceB.String()}, nil); err != nil {
		return fmt.Errorf("ack message: %w", err)
	}
	if err := assertAckedCiphertext(ctx, sqlDB, first.Data.MessageID); err != nil {
		return err
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
	var devices, fallback, senderFK, oldSenderFK bool
	err := sqlDB.QueryRowContext(ctx, `
		SELECT to_regclass('dm_devices') IS NOT NULL,
		       to_regclass('dm_fallback_keys') IS NOT NULL,
		       EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_dm_inbox_sender_user'),
		       EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_dm_inbox_sender_device')`).
		Scan(&devices, &fallback, &senderFK, &oldSenderFK)
	if err != nil {
		return fmt.Errorf("check DM schema: %w", err)
	}
	if !devices || !fallback || !senderFK || oldSenderFK {
		return fmt.Errorf("unexpected DM schema: devices=%v fallback=%v sender_user_fk=%v old_sender_device_fk=%v", devices, fallback, senderFK, oldSenderFK)
	}
	return nil
}

func registerDevice(ctx context.Context, apiURL, token string, deviceID uuid.UUID) error {
	var out envelope[registerResponse]
	return requestJSON(ctx, http.MethodPost, apiURL+"/api/dm/device", token, registerRequest{
		DeviceID:           deviceID.String(),
		IdentityCurve25519: encodedKey(0x11, 32),
		IdentityEd25519:    encodedKey(0x12, 32),
	}, &out)
}

func uploadKeys(ctx context.Context, apiURL, token string, deviceID uuid.UUID) error {
	var out envelope[uploadKeysResponse]
	err := requestJSON(ctx, http.MethodPost, apiURL+"/api/dm/device/one-time-keys", token, uploadKeysRequest{
		DeviceID: deviceID.String(),
		Keys: []oneTimeKeyInput{
			{KeyID: "smoke-otk-1", PublicKey: encodedKey(0x21, 32), Signature: encodedKey(0x22, 64)},
			{KeyID: "smoke-otk-2", PublicKey: encodedKey(0x23, 32), Signature: encodedKey(0x24, 64)},
		},
		FallbackKey: &oneTimeKeyInput{
			KeyID: "smoke-fallback", PublicKey: encodedKey(0x31, 32), Signature: encodedKey(0x32, 64),
		},
	}, &out)
	if err != nil {
		return err
	}
	if out.Data.Accepted != 2 {
		return fmt.Errorf("accepted one-time keys = %d, want 2", out.Data.Accepted)
	}
	return nil
}

func encodedKey(seed byte, size int) string {
	return base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{seed}, size))
}

func assertFallbackKey(ctx context.Context, sqlDB *sql.DB, deviceID uuid.UUID) error {
	var count int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM dm_fallback_keys WHERE device_id = $1`, deviceID).Scan(&count); err != nil {
		return fmt.Errorf("count fallback key: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("fallback key rows = %d, want 1", count)
	}
	return nil
}

func verifyConcurrentKeyClaim(ctx context.Context, sqlDB *sql.DB, q *db.Queries, deviceID uuid.UUID) error {
	type claimResult struct {
		keyID string
		err   error
	}

	firstTx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin first key claim: %w", err)
	}
	defer firstTx.Rollback()

	first, err := q.WithTx(firstTx).ClaimDMOneTimeKey(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("first key claim: %w", err)
	}

	claimCtx, claimCancel := context.WithTimeout(ctx, 3*time.Second)
	defer claimCancel()
	done := make(chan claimResult, 1)
	go func() {
		tx, err := sqlDB.BeginTx(claimCtx, nil)
		if err != nil {
			done <- claimResult{err: err}
			return
		}
		defer tx.Rollback()
		row, err := q.WithTx(tx).ClaimDMOneTimeKey(claimCtx, deviceID)
		if err != nil {
			done <- claimResult{err: err}
			return
		}
		if err := tx.Commit(); err != nil {
			done <- claimResult{err: err}
			return
		}
		done <- claimResult{keyID: row.KeyID}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			return fmt.Errorf("overlapping key claim: %w", result.err)
		}
		if result.keyID == first.KeyID {
			return fmt.Errorf("overlapping claims returned the same key %q", result.keyID)
		}
		if err := firstTx.Commit(); err != nil {
			return fmt.Errorf("commit first key claim: %w", err)
		}
		return nil
	case <-claimCtx.Done():
		return fmt.Errorf("overlapping claim blocked on locked row: %w", claimCtx.Err())
	}
}

func verifyIdempotencyUpsert(ctx context.Context, sqlDB *sql.DB, q *db.Queries, deviceID uuid.UUID) error {
	clientMessageID := uuid.New()
	originalDigest := sha256.Sum256([]byte("expired idempotency record"))
	if _, err := sqlDB.ExecContext(ctx, `
		INSERT INTO dm_idempotency
		    (sender_device_id, client_message_id, request_digest, message_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, NOW() - INTERVAL '2 minutes', NOW() - INTERVAL '1 minute')`,
		deviceID, clientMessageID, originalDigest[:], uuid.New()); err != nil {
		return fmt.Errorf("seed expired idempotency: %w", err)
	}

	replacementDigest := sha256.Sum256([]byte("replacement digest"))
	created, err := q.CreateDMIdempotency(ctx, db.CreateDMIdempotencyParams{
		SenderDeviceID: deviceID, ClientMessageID: clientMessageID,
		RequestDigest: replacementDigest[:], MessageID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		return fmt.Errorf("upsert expired idempotency: %w", err)
	}
	if !bytes.Equal(created.RequestDigest, replacementDigest[:]) {
		return errors.New("expired idempotency row was not replaced")
	}
	conflictDigest := sha256.Sum256([]byte("conflicting digest"))
	_, err = q.CreateDMIdempotency(ctx, db.CreateDMIdempotencyParams{
		SenderDeviceID: deviceID, ClientMessageID: clientMessageID,
		RequestDigest: conflictDigest[:], MessageID: uuid.New(), ExpiresAt: time.Now().Add(2 * time.Hour),
	})
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("live idempotency upsert: got %v, want sql.ErrNoRows", err)
	}
	var stored []byte
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT request_digest FROM dm_idempotency
		WHERE sender_device_id = $1 AND client_message_id = $2`, deviceID, clientMessageID).Scan(&stored); err != nil {
		return fmt.Errorf("read idempotency row: %w", err)
	}
	if !bytes.Equal(stored, replacementDigest[:]) {
		return errors.New("live idempotency row was overwritten while still valid")
	}
	return nil
}

func assertAckedCiphertext(ctx context.Context, sqlDB *sql.DB, messageID uuid.UUID) error {
	var ciphertext []byte
	var acknowledged bool
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT ciphertext, acknowledged_at IS NOT NULL FROM dm_inbox WHERE message_id = $1`, messageID).
		Scan(&ciphertext, &acknowledged); err != nil {
		return fmt.Errorf("read acked message: %w", err)
	}
	if ciphertext != nil || !acknowledged {
		return fmt.Errorf("acked message: ciphertext=%v acknowledged=%v", ciphertext != nil, acknowledged)
	}
	var plaintextRows int
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM dm_inbox
		WHERE encode(ciphertext, 'escape') LIKE '%smoke-ciphertext%'`).Scan(&plaintextRows); err != nil {
		return fmt.Errorf("scan plaintext: %w", err)
	}
	if plaintextRows != 0 {
		return fmt.Errorf("found %d rows containing smoke plaintext", plaintextRows)
	}
	return nil
}

func requestJSON(ctx context.Context, method, endpoint, token string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
