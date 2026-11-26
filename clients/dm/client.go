package dm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("HTTP %d %s: %s", e.StatusCode, e.Code, e.Message)
}

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("base URL must use http or https")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("base URL must include a host")
	}
	if parsed.Scheme == "http" && !loopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("base URL must use https for non-loopback hosts")
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type RegisterDeviceResult struct {
	DeviceID       string `json:"device_id"`
	ReplacedDevice bool   `json:"replaced_device"`
}

type KeyBundle struct {
	UserID      string      `json:"user_id"`
	DeviceID    string      `json:"device_id"`
	Identity    Identity    `json:"-"`
	OneTimeKey  *OneTimeKey `json:"one_time_key"`
	FallbackKey *OneTimeKey `json:"fallback_key"`
}

func (b *KeyBundle) UnmarshalJSON(raw []byte) error {
	var wire struct {
		UserID             string      `json:"user_id"`
		DeviceID           string      `json:"device_id"`
		IdentityCurve25519 string      `json:"identity_curve25519"`
		IdentityEd25519    string      `json:"identity_ed25519"`
		OneTimeKey         *OneTimeKey `json:"one_time_key"`
		FallbackKey        *OneTimeKey `json:"fallback_key"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	b.UserID = wire.UserID
	b.DeviceID = wire.DeviceID
	b.Identity = Identity{Curve25519: wire.IdentityCurve25519, Ed25519: wire.IdentityEd25519}
	b.OneTimeKey = wire.OneTimeKey
	b.FallbackKey = wire.FallbackKey
	return nil
}

type SendRequest struct {
	DeviceID          string `json:"device_id"`
	RecipientUserID   string `json:"recipient_user_id"`
	RecipientDeviceID string `json:"recipient_device_id"`
	ClientMessageID   string `json:"client_message_id"`
	Protocol          string `json:"protocol"`
	ProtocolVersion   int16  `json:"protocol_version"`
	MessageType       int16  `json:"message_type"`
	Ciphertext        string `json:"ciphertext"`
}

type SendResult struct {
	MessageID string    `json:"message_id"`
	Sequence  int64     `json:"sequence"`
	ExpiresAt time.Time `json:"expires_at"`
}

type InboxMessage struct {
	MessageID         string    `json:"message_id"`
	SenderUserID      string    `json:"sender_user_id"`
	SenderDeviceID    string    `json:"sender_device_id"`
	RecipientUserID   string    `json:"recipient_user_id"`
	RecipientDeviceID string    `json:"recipient_device_id"`
	Sequence          int64     `json:"sequence"`
	Protocol          string    `json:"protocol"`
	ProtocolVersion   int16     `json:"protocol_version"`
	MessageType       int16     `json:"message_type"`
	Ciphertext        string    `json:"ciphertext"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type PollResult struct {
	Messages  []InboxMessage `json:"messages"`
	NextAfter int64          `json:"next_after"`
	HasMore   bool           `json:"has_more"`
}

func (c *Client) RegisterDevice(ctx context.Context, deviceID string, identity Identity) (RegisterDeviceResult, error) {
	var out struct {
		Data RegisterDeviceResult `json:"data"`
	}
	body := struct {
		DeviceID           string `json:"device_id"`
		IdentityCurve25519 string `json:"identity_curve25519"`
		IdentityEd25519    string `json:"identity_ed25519"`
	}{DeviceID: deviceID, IdentityCurve25519: identity.Curve25519, IdentityEd25519: identity.Ed25519}
	_, err := c.do(ctx, http.MethodPost, "/api/dm/device", body, &out)
	return out.Data, err
}

func (c *Client) UploadOneTimeKeys(ctx context.Context, deviceID string, keys []OneTimeKey, fallback *OneTimeKey) (int, error) {
	var out struct {
		Data struct {
			Accepted int `json:"accepted"`
		} `json:"data"`
	}
	body := struct {
		DeviceID    string       `json:"device_id"`
		Keys        []OneTimeKey `json:"keys"`
		FallbackKey *OneTimeKey  `json:"fallback_key"`
	}{DeviceID: deviceID, Keys: keys, FallbackKey: fallback}
	_, err := c.do(ctx, http.MethodPost, "/api/dm/device/one-time-keys", body, &out)
	return out.Data.Accepted, err
}

func (c *Client) GetKeyBundle(ctx context.Context, callerDeviceID, targetUserID string, includeOneTimeKey bool) (KeyBundle, error) {
	var out struct {
		Data KeyBundle `json:"data"`
	}
	query := url.Values{}
	query.Set("device_id", callerDeviceID)
	query.Set("include_one_time_key", strconv.FormatBool(includeOneTimeKey))
	path := "/api/dm/users/" + url.PathEscape(targetUserID) + "/key-bundle?" + query.Encode()
	_, err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Data, err
}

func (c *Client) SendMessage(ctx context.Context, req SendRequest) (SendResult, error) {
	var out struct {
		Data SendResult `json:"data"`
	}
	_, err := c.do(ctx, http.MethodPost, "/api/dm/messages", req, &out)
	return out.Data, err
}

func (c *Client) PollInbox(ctx context.Context, deviceID string, after int64, limit int) (PollResult, error) {
	var out struct {
		Data PollResult `json:"data"`
	}
	query := url.Values{}
	query.Set("device_id", deviceID)
	query.Set("after", strconv.FormatInt(after, 10))
	query.Set("limit", strconv.Itoa(limit))
	_, err := c.do(ctx, http.MethodGet, "/api/dm/inbox?"+query.Encode(), nil, &out)
	return out.Data, err
}

func (c *Client) Acknowledge(ctx context.Context, deviceID, messageID string) error {
	body := struct {
		DeviceID string `json:"device_id"`
	}{DeviceID: deviceID}
	status, err := c.do(ctx, http.MethodPost, "/api/dm/inbox/"+url.PathEscape(messageID)+"/ack", body, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("unexpected ack status %d", status)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	if strings.TrimSpace(c.token) == "" {
		return 0, fmt.Errorf("access token is required")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var payload struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &payload); err == nil && payload.Error.Code != "" {
			return resp.StatusCode, &APIError{StatusCode: resp.StatusCode, Code: payload.Error.Code, Message: payload.Error.Message}
		}
		return resp.StatusCode, fmt.Errorf("unexpected HTTP %d response", resp.StatusCode)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}
