// phase_chat_ws is an end-to-end smoke test for the realtime side of the
// chat feature: two (three) users connect over real WebSockets to the real
// ChatHandler + ChatHub backed by the real Postgres, and we assert that
// messages, typing and read events reach the right people.
//
// Authentication is replaced by a tiny test middleware (X-Test-User header),
// so this needs only Postgres, not Zitadel or nginx.
//
// Usage (from the repo root, with the stack up):
//
//	IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' mokibox-postgres)
//	DATABASE_URL="$(grep '^DATABASE_URL=' .env | cut -d= -f2- | sed "s/@postgres:/@$IP:/")" \
//	  go run ./scripts/smoketest/phase_chat_ws
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/handlers"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const eventWait = 2 * time.Second
const quietWait = 400 * time.Millisecond

type wsClient struct {
	name string
	conn *websocket.Conn
	in   chan map[string]any
}

func dial(name, wsURL string, userID uuid.UUID) *wsClient {
	hdr := http.Header{}
	hdr.Set("X-Test-User", userID.String())
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		log.Fatalf("%s: websocket dial failed: %v (response: %v)", name, err, resp)
	}
	c := &wsClient{name: name, conn: conn, in: make(chan map[string]any, 64)}
	go func() {
		defer close(c.in)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			// The server may batch several events into one frame,
			// separated by '\n'.
			for _, line := range bytes.Split(data, []byte("\n")) {
				line = bytes.TrimSpace(line)
				if len(line) == 0 {
					continue
				}
				var ev map[string]any
				if json.Unmarshal(line, &ev) == nil {
					c.in <- ev
				}
			}
		}
	}()
	return c
}

func (c *wsClient) send(v map[string]any) {
	if err := c.conn.WriteJSON(v); err != nil {
		log.Fatalf("%s: websocket write failed: %v", c.name, err)
	}
}

// waitFor returns the first event of evType (matching pred, if given).
func (c *wsClient) waitFor(evType string, pred func(map[string]any) bool) (map[string]any, bool) {
	deadline := time.After(eventWait)
	for {
		select {
		case ev, ok := <-c.in:
			if !ok {
				return nil, false
			}
			if ev["type"] == evType && (pred == nil || pred(ev)) {
				return ev, true
			}
		case <-deadline:
			return nil, false
		}
	}
}

// expectNone reports true if no event of evType (matching pred) arrives
// during the quiet period.
func (c *wsClient) expectNone(evType string, pred func(map[string]any) bool) bool {
	deadline := time.After(quietWait)
	for {
		select {
		case ev, ok := <-c.in:
			if !ok {
				return true
			}
			if ev["type"] == evType && (pred == nil || pred(ev)) {
				return false
			}
		case <-deadline:
			return true
		}
	}
}

func contentOf(ev map[string]any) string {
	m, _ := ev["message"].(map[string]any)
	s, _ := m["content"].(string)
	return s
}

func hasContent(want string) func(map[string]any) bool {
	return func(ev map[string]any) bool { return contentOf(ev) == want }
}

func doJSON(method, url string, userID uuid.UUID, body any) (int, map[string]any) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		log.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", userID.String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (see the usage comment at the top of this file)")
	}
	os.Exit(run(dsn))
}

func run(dsn string) int {
	ctx := context.Background()

	sqlDB, err := shared.NewSQLDB(ctx, dsn, 5, 2)
	if err != nil {
		log.Fatalf("Failed to connect to Postgres: %v", err)
	}
	defer sqlDB.Close()
	queries := db.New(sqlDB)

	failed := 0
	check := func(name string, ok bool, detail string) {
		if ok {
			fmt.Printf("✅ %s\n", name)
			return
		}
		fmt.Printf("❌ %s — %s\n", name, detail)
		failed++
	}

	// ---- seed users -------------------------------------------------
	seed := func(prefix string) db.User {
		u, err := queries.CreateUser(ctx, db.CreateUserParams{
			ZitadelID:   "zitadel_sub_" + prefix + "_" + uuid.New().String()[:8],
			Username:    prefix + "_" + uuid.New().String()[:8],
			DisplayName: sql.NullString{String: prefix, Valid: true},
		})
		if err != nil {
			log.Fatalf("seed %s: %v", prefix, err)
		}
		return u
	}
	alice, bob, carol := seed("alice"), seed("bob"), seed("carol")
	users := map[uuid.UUID]*db.User{alice.ID: &alice, bob.ID: &bob, carol.ID: &carol}
	fmt.Printf("Seeded users: alice=%s bob=%s carol=%s\n", alice.ID, bob.ID, carol.ID)

	// ---- wire the real handler behind a fake-auth router ------------
	hub := handlers.NewChatHub()
	go hub.Run(ctx)
	ch := handlers.NewChatHandler(queries, &shared.R2Client{}, hub, &shared.APIConfig{PresignUploadExpiry: 15 * time.Minute})

	e := echo.New()
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, shared.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, shared.ErrValidation):
			status = http.StatusBadRequest
		case errors.Is(err, shared.ErrUnauthorized):
			status = http.StatusUnauthorized
		}
		if !c.Response().Committed {
			_ = c.JSON(status, map[string]string{"error": err.Error()})
		}
	}
	api := e.Group("/api", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			id, perr := uuid.Parse(c.Request().Header.Get("X-Test-User"))
			u, ok := users[id]
			if perr != nil || !ok {
				return shared.Wrap(shared.ErrUnauthorized, "unknown test user")
			}
			c.Set("auth.currentUser", u)
			return next(c)
		}
	})
	api.POST("/chat/conversations", ch.CreateConversation)
	api.GET("/chat/conversations", ch.ListConversations)
	api.GET("/chat/conversations/:id/messages", ch.ListMessages)
	api.POST("/chat/conversations/:id/messages", ch.SendMessage)
	api.POST("/chat/conversations/:id/read", ch.MarkRead)
	api.GET("/chat/ws", ch.HandleWebSocket)

	srv := httptest.NewServer(e)
	defer srv.Close()
	httpBase := srv.URL + "/api"
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/chat/ws"

	// ---- REST: conversation setup and validation --------------------
	status, conv := doJSON(http.MethodPost, httpBase+"/chat/conversations", alice.ID, map[string]any{"target_user_id": bob.ID})
	convID, _ := conv["id"].(string)
	check("REST: Alice creates a direct conversation with Bob", (status == 201 || status == 200) && convID != "", fmt.Sprintf("status=%d body=%v", status, conv))
	if convID == "" {
		return 1
	}
	defer func() {
		// Best-effort cleanup; the API role may not be allowed to delete users.
		_, _ = sqlDB.ExecContext(ctx, `DELETE FROM conversations WHERE id = $1`, convID)
		for _, u := range []db.User{alice, bob, carol} {
			if _, derr := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, u.ID); derr != nil {
				fmt.Printf("(cleanup) could not delete test user %s: %v\n", u.Username, derr)
				break
			}
		}
	}()

	status, _ = doJSON(http.MethodPost, httpBase+"/chat/conversations", alice.ID, map[string]any{"target_user_id": uuid.New()})
	check("REST: chatting with a non-existent user is rejected (404)", status == 404, fmt.Sprintf("status=%d", status))

	status, _ = doJSON(http.MethodPost, httpBase+"/chat/conversations", alice.ID, map[string]any{"target_user_id": alice.ID})
	check("REST: chatting with yourself is rejected (400)", status == 400, fmt.Sprintf("status=%d", status))

	// ---- WebSocket: connect ----------------------------------------
	aliceWS := dial("alice", wsURL, alice.ID)
	bobWS := dial("bob", wsURL, bob.ID)
	carolWS := dial("carol", wsURL, carol.ID)
	time.Sleep(300 * time.Millisecond) // let the hub register everyone
	fmt.Println("Connected 3 WebSocket clients (alice, bob, carol)")

	// ---- realtime message -------------------------------------------
	aliceWS.send(map[string]any{"type": "message.send", "conversation_id": convID, "message_type": "text", "content": "halo bob"})
	_, ok := bobWS.waitFor("message.new", hasContent("halo bob"))
	check("WS: Bob receives Alice's message in realtime", ok, "no message.new within timeout")
	_, ok = aliceWS.waitFor("message.new", hasContent("halo bob"))
	check("WS: Alice gets her own message echoed (for her other devices)", ok, "no message.new within timeout")
	check("WS: Carol (not a member) receives nothing", carolWS.expectNone("message.new", nil), "carol received a message.new")

	// ---- typing and read --------------------------------------------
	bobWS.send(map[string]any{"type": "typing", "conversation_id": convID, "is_typing": true})
	_, ok = aliceWS.waitFor("typing", func(ev map[string]any) bool {
		return ev["user_id"] == bob.ID.String() && ev["is_typing"] == true
	})
	check("WS: Alice sees Bob typing", ok, "no typing event")
	check("WS: Bob does not get his own typing event", bobWS.expectNone("typing", nil), "bob received his own typing event")

	bobWS.send(map[string]any{"type": "read", "conversation_id": convID})
	_, ok = aliceWS.waitFor("read", func(ev map[string]any) bool { return ev["user_id"] == bob.ID.String() })
	check("WS: Alice is told Bob read the conversation", ok, "no read event")

	// ---- REST send is pushed over WS ---------------------------------
	status, _ = doJSON(http.MethodPost, httpBase+"/chat/conversations/"+convID+"/messages", alice.ID, map[string]any{"message_type": "text", "content": "via rest"})
	check("REST: Alice sends a message (201)", status == 201, fmt.Sprintf("status=%d", status))
	_, ok = bobWS.waitFor("message.new", hasContent("via rest"))
	check("WS: a message sent over REST reaches Bob in realtime", ok, "no message.new within timeout")

	// ---- authorization on the WebSocket -----------------------------
	carolWS.send(map[string]any{"type": "typing", "conversation_id": convID, "is_typing": true})
	_, ok = carolWS.waitFor("error", func(ev map[string]any) bool { return ev["code"] == string(shared.CodeNotFound) })
	check("WS: non-member typing is refused with an error event", ok, "carol got no NOT_FOUND error")
	check("WS: non-member typing is not forwarded to Alice", aliceWS.expectNone("typing", func(ev map[string]any) bool { return ev["user_id"] == carol.ID.String() }), "alice saw carol's typing")

	carolWS.send(map[string]any{"type": "message.send", "conversation_id": convID, "message_type": "text", "content": "hack"})
	_, ok = carolWS.waitFor("error", func(ev map[string]any) bool { return ev["code"] == string(shared.CodeNotFound) })
	check("WS: non-member message.send is refused with an error event", ok, "carol got no NOT_FOUND error")
	check("WS: non-member message never reaches Bob", bobWS.expectNone("message.new", hasContent("hack")), "bob received carol's message")

	aliceWS.send(map[string]any{"type": "message.send", "conversation_id": convID, "message_type": "text", "content": "   "})
	_, ok = aliceWS.waitFor("error", func(ev map[string]any) bool { return ev["code"] == string(shared.CodeValidationError) })
	check("WS: empty text message is refused with a validation error", ok, "alice got no VALIDATION_ERROR")

	// ---- history ------------------------------------------------------
	status, hist := doJSON(http.MethodGet, httpBase+"/chat/conversations/"+convID+"/messages", bob.ID, nil)
	msgs, _ := hist["messages"].([]any)
	var texts []string
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		s, _ := mm["content"].(string)
		texts = append(texts, s)
	}
	check("REST: Bob's history has exactly the 2 valid messages", status == 200 && len(texts) == 2, fmt.Sprintf("status=%d messages=%v", status, texts))

	status, _ = doJSON(http.MethodGet, httpBase+"/chat/conversations/"+convID+"/messages", carol.ID, nil)
	check("REST: non-member cannot read history (404)", status == 404, fmt.Sprintf("status=%d", status))

	// ---- multi-device and offline recipients --------------------------
	bob2WS := dial("bob2", wsURL, bob.ID)
	time.Sleep(300 * time.Millisecond)
	aliceWS.send(map[string]any{"type": "message.send", "conversation_id": convID, "message_type": "text", "content": "for both devices"})
	_, ok1 := bobWS.waitFor("message.new", hasContent("for both devices"))
	_, ok2 := bob2WS.waitFor("message.new", hasContent("for both devices"))
	check("WS: both of Bob's devices receive the message", ok1 && ok2, fmt.Sprintf("device1=%v device2=%v", ok1, ok2))

	_ = bobWS.conn.Close()
	_ = bob2WS.conn.Close()
	time.Sleep(300 * time.Millisecond)
	aliceWS.send(map[string]any{"type": "message.send", "conversation_id": convID, "message_type": "text", "content": "bob is offline"})
	_, ok = aliceWS.waitFor("message.new", hasContent("bob is offline"))
	check("WS: sending still works while the recipient is offline", ok, "alice got no echo")

	_ = aliceWS.conn.Close()
	_ = carolWS.conn.Close()

	fmt.Println()
	if failed == 0 {
		fmt.Println("🎉 ALL WEBSOCKET CHAT CHECKS PASSED")
		return 0
	}
	fmt.Printf("💥 %d CHECK(S) FAILED\n", failed)
	return 1
}
