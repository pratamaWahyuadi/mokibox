package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow CORS for WebSocket in dev
	},
}

// WSClient represents a single connected WebSocket client.
type WSClient struct {
	UserID uuid.UUID
	Conn   *websocket.Conn
	Send   chan []byte
	Hub    *ChatHub
}

// BroadcastPayload is sent to the hub to be routed either to every
// device of the listed users (RecipientIDs) or, when Client is set, to
// that single connection only.
type BroadcastPayload struct {
	ConversationID uuid.UUID
	RecipientIDs   []uuid.UUID
	Client         *WSClient
	Data           []byte
}

// ChatHub manages all active WebSocket connections and message routing.
type ChatHub struct {
	mu         sync.RWMutex
	clients    map[uuid.UUID]map[*WSClient]bool
	register   chan *WSClient
	unregister chan *WSClient
	broadcast  chan *BroadcastPayload
}

// NewChatHub constructs a ChatHub instance.
func NewChatHub() *ChatHub {
	return &ChatHub{
		clients:    make(map[uuid.UUID]map[*WSClient]bool),
		register:   make(chan *WSClient),
		unregister: make(chan *WSClient),
		broadcast:  make(chan *BroadcastPayload, 256),
	}
}

// dropLocked removes a client and closes its Send channel exactly once.
// The caller MUST hold h.mu for writing.
func (h *ChatHub) dropLocked(client *WSClient) {
	userClients, ok := h.clients[client.UserID]
	if !ok {
		return
	}
	if _, exists := userClients[client]; !exists {
		return
	}
	delete(userClients, client)
	close(client.Send)
	if len(userClients) == 0 {
		delete(h.clients, client.UserID)
	}
}

// sendLocked queues data for a client, dropping the client if its buffer
// is full. The caller MUST hold h.mu for writing.
func (h *ChatHub) sendLocked(client *WSClient, data []byte) {
	select {
	case client.Send <- data:
	default:
		// Buffer full: the client is too slow, disconnect it.
		h.dropLocked(client)
	}
}

// Run runs the main event loop for client registration, unregistration, and broadcasting.
func (h *ChatHub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case client := <-h.register:
			h.mu.Lock()
			if _, ok := h.clients[client.UserID]; !ok {
				h.clients[client.UserID] = make(map[*WSClient]bool)
			}
			h.clients[client.UserID][client] = true
			h.mu.Unlock()
			slog.Debug("WebSocket client registered", "user_id", client.UserID)

		case client := <-h.unregister:
			h.mu.Lock()
			h.dropLocked(client)
			h.mu.Unlock()
			slog.Debug("WebSocket client unregistered", "user_id", client.UserID)

		case payload := <-h.broadcast:
			// Full Lock (not RLock): delivering can drop slow clients,
			// which mutates the maps and closes channels.
			h.mu.Lock()
			if payload.Client != nil {
				// Targeted delivery; skip if the client already left.
				if userClients, ok := h.clients[payload.Client.UserID]; ok {
					if _, exists := userClients[payload.Client]; exists {
						h.sendLocked(payload.Client, payload.Data)
					}
				}
			} else {
				for _, recipientID := range payload.RecipientIDs {
					for client := range h.clients[recipientID] {
						h.sendLocked(client, payload.Data)
					}
				}
			}
			h.mu.Unlock()
		}
	}
}

// BroadcastToUsers sends a byte message to all online devices of specified user IDs.
func (h *ChatHub) BroadcastToUsers(userIDs []uuid.UUID, data []byte) {
	h.broadcast <- &BroadcastPayload{
		RecipientIDs: userIDs,
		Data:         data,
	}
}

// SendToClient sends a byte message to one specific connection only.
// It goes through the hub loop so it never races with channel close.
func (h *ChatHub) SendToClient(client *WSClient, data []byte) {
	h.broadcast <- &BroadcastPayload{
		Client: client,
		Data:   data,
	}
}

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 10 * 1024 // 10KB
	eventTimeout   = 10 * time.Second
)

// wsErrorFrom maps a handler error to a WebSocket error code + message.
func wsErrorFrom(err error) (shared.ErrorCode, string) {
	switch {
	case errors.Is(err, shared.ErrValidation):
		return shared.CodeValidationError, strings.TrimSuffix(err.Error(), ": "+shared.ErrValidation.Error())
	case errors.Is(err, shared.ErrNotFound):
		return shared.CodeNotFound, strings.TrimSuffix(err.Error(), ": "+shared.ErrNotFound.Error())
	default:
		return shared.CodeInternalError, "internal error"
	}
}

// sendError tells this client (and only this client) that its event failed.
func (c *WSClient) sendError(code shared.ErrorCode, message string) {
	out, _ := json.Marshal(map[string]any{
		"type":    "error",
		"code":    code,
		"message": message,
	})
	c.Hub.SendToClient(c, out)
}

// otherMemberIDs returns the IDs of every member of the conversation
// except the given user.
func otherMemberIDs(members []db.ListConversationMembersRow, self uuid.UUID) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(members))
	for _, m := range members {
		if m.UserID != self {
			ids = append(ids, m.UserID)
		}
	}
	return ids
}

func (c *WSClient) readPump(handler *ChatHandler) {
	defer func() {
		c.Hub.unregister <- c
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(maxMessageSize)
	_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Warn("WebSocket read error", "error", err)
			}
			break
		}
		c.handleEvent(handler, message)
	}
}

// handleEvent processes one inbound client event.
func (c *WSClient) handleEvent(handler *ChatHandler, message []byte) {
	var event struct {
		Type           string          `json:"type"`
		ConversationID uuid.UUID       `json:"conversation_id"`
		MessageType    string          `json:"message_type"`
		Content        string          `json:"content"`
		MediaURL       string          `json:"media_url"`
		MediaMetadata  json.RawMessage `json:"media_metadata"`
		IsTyping       bool            `json:"is_typing"`
	}

	if err := json.Unmarshal(message, &event); err != nil {
		slog.Debug("Invalid WebSocket event payload", "error", err)
		c.sendError(shared.CodeValidationError, "invalid event payload")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
	defer cancel()

	switch event.Type {
	case "message.send":
		msg, err := handler.SendMessageInternal(ctx, c.UserID, event.ConversationID, event.MessageType, event.Content, event.MediaURL, event.MediaMetadata)
		if err != nil {
			slog.Warn("Failed to process WebSocket message.send", "error", err, "user_id", c.UserID)
			code, text := wsErrorFrom(err)
			c.sendError(code, text)
			return
		}

		// Broadcast to all conversation members (including the sender's devices).
		members, err := handler.Queries.ListConversationMembers(ctx, event.ConversationID)
		if err != nil {
			slog.Warn("Failed to list members for broadcast", "error", err)
			return
		}
		recipientIDs := make([]uuid.UUID, 0, len(members))
		for _, m := range members {
			recipientIDs = append(recipientIDs, m.UserID)
		}
		out, _ := json.Marshal(map[string]any{
			"type":    "message.new",
			"message": msg,
		})
		c.Hub.BroadcastToUsers(recipientIDs, out)

	case "typing":
		if !handler.isMember(ctx, event.ConversationID, c.UserID) {
			c.sendError(shared.CodeNotFound, "conversation not found")
			return
		}
		members, err := handler.Queries.ListConversationMembers(ctx, event.ConversationID)
		if err != nil {
			return
		}
		out, _ := json.Marshal(map[string]any{
			"type":            "typing",
			"conversation_id": event.ConversationID,
			"user_id":         c.UserID,
			"is_typing":       event.IsTyping,
		})
		c.Hub.BroadcastToUsers(otherMemberIDs(members, c.UserID), out)

	case "read":
		if !handler.isMember(ctx, event.ConversationID, c.UserID) {
			c.sendError(shared.CodeNotFound, "conversation not found")
			return
		}
		_ = handler.Queries.UpdateLastReadAt(ctx, db.UpdateLastReadAtParams{
			ConversationID: event.ConversationID,
			UserID:         c.UserID,
		})
		members, err := handler.Queries.ListConversationMembers(ctx, event.ConversationID)
		if err != nil {
			return
		}
		out, _ := json.Marshal(map[string]any{
			"type":            "read",
			"conversation_id": event.ConversationID,
			"user_id":         c.UserID,
		})
		c.Hub.BroadcastToUsers(otherMemberIDs(members, c.UserID), out)

	default:
		c.sendError(shared.CodeValidationError, "unsupported event type")
	}
}

func (c *WSClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			_, _ = w.Write(message)

			// Add queued chat messages to the current websocket frame
			n := len(c.Send)
			for i := 0; i < n; i++ {
				_, _ = w.Write([]byte{'\n'})
				_, _ = w.Write(<-c.Send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// HandleWebSocket upgrades HTTP requests to WebSocket connections.
func (h *ChatHandler) HandleWebSocket(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.Wrap(shared.ErrUnauthorized, "missing authentication")
	}

	conn, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		// Upgrade has already written the HTTP error response.
		slog.Debug("WebSocket upgrade failed", "error", err)
		return nil
	}

	client := &WSClient{
		UserID: user.ID,
		Conn:   conn,
		Send:   make(chan []byte, 256),
		Hub:    h.Hub,
	}
	client.Hub.register <- client

	go client.writePump()
	go client.readPump(h)

	return nil
}
