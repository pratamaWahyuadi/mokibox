package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/handlers"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}

	ctx := context.Background()
	sqlDB, err := shared.NewSQLDB(ctx, dsn, 5, 2)
	if err != nil {
		log.Fatalf("Failed to connect to Postgres: %v", err)
	}
	defer sqlDB.Close()

	queries := db.New(sqlDB)
	hub := handlers.NewChatHub()
	go hub.Run(ctx)

	cfg := &shared.APIConfig{
		PresignUploadExpiry: 15 * time.Minute,
	}

	r2Dummy := &shared.R2Client{} // Stub or real R2
	handler := handlers.NewChatHandler(queries, r2Dummy, hub, cfg)
	e := echo.New()

	log.Println("=== STARTING END-TO-END CHAT SMOKE TEST ===")

	// 1. Seed Test Users (Alice & Bob)
	subAlice := "zitadel_sub_alice_" + uuid.New().String()[:8]
	subBob := "zitadel_sub_bob_" + uuid.New().String()[:8]

	alice, err := queries.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   subAlice,
		Username:    "alice_" + uuid.New().String()[:8],
		DisplayName: sql.NullString{String: "Alice Wonderland", Valid: true},
	})
	if err != nil {
		log.Fatalf("Failed to seed user Alice: %v", err)
	}

	bob, err := queries.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   subBob,
		Username:    "bob_" + uuid.New().String()[:8],
		DisplayName: sql.NullString{String: "Bob Builder", Valid: true},
	})
	if err != nil {
		log.Fatalf("Failed to seed user Bob: %v", err)
	}

	fmt.Printf("✅ Seeded test users: Alice (%s), Bob (%s)\n", alice.ID, bob.ID)

	// TEST 1: Create Direct Conversation (Alice -> Bob)
	createReqBody := fmt.Sprintf(`{"target_user_id":"%s"}`, bob.ID)
	req := httptest.NewRequest(http.MethodPost, "/api/chat/conversations", strings.NewReader(createReqBody))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("auth.currentUser", &alice)

	if err := handler.CreateConversation(c); err != nil {
		log.Fatalf("CreateConversation error: %v", err)
	}
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		log.Fatalf("Expected 200/201 on CreateConversation, got %d: %s", rec.Code, rec.Body.String())
	}

	var convRes handlers.ConversationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &convRes); err != nil {
		log.Fatalf("Failed to parse conversation response: %v", err)
	}
	fmt.Printf("✅ Test 1 Passed: Created Direct Conversation ID = %s\n", convRes.ID)

	// TEST 2: List Conversations for Alice
	reqList := httptest.NewRequest(http.MethodGet, "/api/chat/conversations", nil)
	recList := httptest.NewRecorder()
	cList := e.NewContext(reqList, recList)
	cList.Set("auth.currentUser", &alice)

	if err := handler.ListConversations(cList); err != nil {
		log.Fatalf("ListConversations error: %v", err)
	}
	if recList.Code != http.StatusOK {
		log.Fatalf("Expected 200 OK on ListConversations, got %d", recList.Code)
	}
	fmt.Printf("✅ Test 2 Passed: Listed Alice's Conversations\n")

	// TEST 3: Send Text Message (Alice -> Bob)
	msgTextBody := `{"message_type":"text","content":"Halo Bob! Ini pesan teks pertama dari Alice."}`
	reqMsg1 := httptest.NewRequest(http.MethodPost, "/api/chat/conversations/"+convRes.ID.String()+"/messages", strings.NewReader(msgTextBody))
	reqMsg1.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	recMsg1 := httptest.NewRecorder()
	cMsg1 := e.NewContext(reqMsg1, recMsg1)
	cMsg1.SetParamNames("id")
	cMsg1.SetParamValues(convRes.ID.String())
	cMsg1.Set("auth.currentUser", &alice)

	if err := handler.SendMessage(cMsg1); err != nil {
		log.Fatalf("SendMessage (text) error: %v", err)
	}
	if recMsg1.Code != http.StatusCreated {
		log.Fatalf("Expected 201 Created on SendMessage text, got %d: %s", recMsg1.Code, recMsg1.Body.String())
	}
	fmt.Printf("✅ Test 3 Passed: Sent Text Message\n")

	// TEST 4: Send Photo & Video Messages (Alice -> Bob)
	msgPhotoBody := `{"message_type":"photo","content":"Foto liburan","media_url":"https://storage.mokibox.com/chat/photo1.jpg"}`
	reqMsg2 := httptest.NewRequest(http.MethodPost, "/api/chat/conversations/"+convRes.ID.String()+"/messages", strings.NewReader(msgPhotoBody))
	reqMsg2.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	recMsg2 := httptest.NewRecorder()
	cMsg2 := e.NewContext(reqMsg2, recMsg2)
	cMsg2.SetParamNames("id")
	cMsg2.SetParamValues(convRes.ID.String())
	cMsg2.Set("auth.currentUser", &alice)

	if err := handler.SendMessage(cMsg2); err != nil {
		log.Fatalf("SendMessage (photo) error: %v", err)
	}
	if recMsg2.Code != http.StatusCreated {
		log.Fatalf("Expected 201 Created on SendMessage photo, got %d", recMsg2.Code)
	}
	fmt.Printf("✅ Test 4 Passed: Sent Photo Message\n")

	// TEST 5: Send Sticker & GIF Messages
	msgStickerBody := `{"message_type":"sticker","content":"sticker_pack_1_happy"}`
	reqMsg3 := httptest.NewRequest(http.MethodPost, "/api/chat/conversations/"+convRes.ID.String()+"/messages", strings.NewReader(msgStickerBody))
	reqMsg3.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	recMsg3 := httptest.NewRecorder()
	cMsg3 := e.NewContext(reqMsg3, recMsg3)
	cMsg3.SetParamNames("id")
	cMsg3.SetParamValues(convRes.ID.String())
	cMsg3.Set("auth.currentUser", &alice)

	if err := handler.SendMessage(cMsg3); err != nil {
		log.Fatalf("SendMessage (sticker) error: %v", err)
	}
	if recMsg3.Code != http.StatusCreated {
		log.Fatalf("Expected 201 Created on SendMessage sticker, got %d", recMsg3.Code)
	}
	fmt.Printf("✅ Test 5 Passed: Sent Sticker Message\n")

	// TEST 6: List Messages for Bob
	reqMsgs := httptest.NewRequest(http.MethodGet, "/api/chat/conversations/"+convRes.ID.String()+"/messages", nil)
	recMsgs := httptest.NewRecorder()
	cMsgs := e.NewContext(reqMsgs, recMsgs)
	cMsgs.SetParamNames("id")
	cMsgs.SetParamValues(convRes.ID.String())
	cMsgs.Set("auth.currentUser", &bob)

	if err := handler.ListMessages(cMsgs); err != nil {
		log.Fatalf("ListMessages error: %v", err)
	}
	if recMsgs.Code != http.StatusOK {
		log.Fatalf("Expected 200 OK on ListMessages, got %d", recMsgs.Code)
	}

	var msgsRes map[string][]map[string]any
	_ = json.Unmarshal(recMsgs.Body.Bytes(), &msgsRes)
	if len(msgsRes["messages"]) != 3 {
		log.Fatalf("Expected 3 messages in conversation history, got %d", len(msgsRes["messages"]))
	}
	fmt.Printf("✅ Test 6 Passed: Retried Message History (%d messages retrieved)\n", len(msgsRes["messages"]))

	// TEST 7: Mark Read for Bob
	reqRead := httptest.NewRequest(http.MethodPost, "/api/chat/conversations/"+convRes.ID.String()+"/read", nil)
	recRead := httptest.NewRecorder()
	cRead := e.NewContext(reqRead, recRead)
	cRead.SetParamNames("id")
	cRead.SetParamValues(convRes.ID.String())
	cRead.Set("auth.currentUser", &bob)

	if err := handler.MarkRead(cRead); err != nil {
		log.Fatalf("MarkRead error: %v", err)
	}
	if recRead.Code != http.StatusOK {
		log.Fatalf("Expected 200 OK on MarkRead, got %d", recRead.Code)
	}
	fmt.Printf("✅ Test 7 Passed: Bob Marked Conversation as Read\n")

	fmt.Println("🎉 ALL 7 E2E CHAT SMOKE TESTS PASSED CLEANLY! 🎉")
}
