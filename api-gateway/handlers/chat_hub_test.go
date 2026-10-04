package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func startTestHub(t *testing.T) *ChatHub {
	t.Helper()
	hub := NewChatHub()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Run(ctx)
	return hub
}

func newTestClient(hub *ChatHub, userID uuid.UUID, buf int) *WSClient {
	c := &WSClient{UserID: userID, Send: make(chan []byte, buf), Hub: hub}
	hub.register <- c
	return c
}

func mustRecv(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case data, ok := <-ch:
		if !ok {
			t.Fatal("channel closed, expected a message")
		}
		return data
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a message")
	}
	return nil
}

func expectClosed(t *testing.T, ch <-chan []byte) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("channel was not closed")
		}
	}
}

func expectNothing(t *testing.T, ch <-chan []byte, wait time.Duration) {
	t.Helper()
	select {
	case data, ok := <-ch:
		if ok {
			t.Fatalf("unexpected message: %s", data)
		}
	case <-time.After(wait):
	}
}

func TestChatHub_BroadcastReachesEveryDeviceOfEveryRecipient(t *testing.T) {
	hub := startTestHub(t)
	alice, bob, carol := uuid.New(), uuid.New(), uuid.New()

	a1 := newTestClient(hub, alice, 4)
	a2 := newTestClient(hub, alice, 4)
	b1 := newTestClient(hub, bob, 4)
	c1 := newTestClient(hub, carol, 4)

	hub.BroadcastToUsers([]uuid.UUID{alice, bob}, []byte("hello"))

	for name, ch := range map[string]chan []byte{"alice-1": a1.Send, "alice-2": a2.Send, "bob": b1.Send} {
		if got := string(mustRecv(t, ch)); got != "hello" {
			t.Errorf("%s got %q", name, got)
		}
	}
	expectNothing(t, c1.Send, 150*time.Millisecond)
}

func TestChatHub_SlowClientIsDroppedWithoutRace(t *testing.T) {
	hub := startTestHub(t)
	user := uuid.New()

	fast := newTestClient(hub, user, 16)
	slow := newTestClient(hub, user, 1)

	for i := 0; i < 3; i++ {
		hub.BroadcastToUsers([]uuid.UUID{user}, []byte("m"))
	}
	for i := 0; i < 3; i++ {
		mustRecv(t, fast.Send)
	}
	expectClosed(t, slow.Send)

	// The read pump of a dropped client still calls unregister later;
	// that must not double-close the channel.
	hub.unregister <- slow

	hub.BroadcastToUsers([]uuid.UUID{user}, []byte("after"))
	if got := string(mustRecv(t, fast.Send)); got != "after" {
		t.Fatalf("fast client got %q, want %q", got, "after")
	}

	hub.mu.RLock()
	_, stillRegistered := hub.clients[user][slow]
	hub.mu.RUnlock()
	if stillRegistered {
		t.Error("slow client should have been removed from the hub")
	}
}

func TestChatHub_SendToClientOnlyReachesThatConnection(t *testing.T) {
	hub := startTestHub(t)
	user := uuid.New()

	a := newTestClient(hub, user, 4)
	b := newTestClient(hub, user, 4)

	hub.SendToClient(a, []byte("only-a"))

	if got := string(mustRecv(t, a.Send)); got != "only-a" {
		t.Fatalf("a got %q", got)
	}
	expectNothing(t, b.Send, 150*time.Millisecond)
}

func TestChatHub_SendToClientAfterUnregisterIsIgnored(t *testing.T) {
	hub := startTestHub(t)
	user := uuid.New()

	a := newTestClient(hub, user, 4)
	b := newTestClient(hub, user, 4)

	hub.unregister <- a
	hub.SendToClient(a, []byte("late")) // must not panic on a closed channel

	// Sync point: once b receives this, the "late" payload was processed.
	hub.BroadcastToUsers([]uuid.UUID{user}, []byte("sync"))
	if got := string(mustRecv(t, b.Send)); got != "sync" {
		t.Fatalf("b got %q", got)
	}
	expectClosed(t, a.Send)
}
