// Phase 7.C smoke test for the notification inbox.
//
// Hermetic, in-network smoke (runs inside the
// mokibox_backend docker network). Verifies:
//
//  1. ListNotifications returns the current user's
//     rows only, created_at DESC, payload intact.
//  2. Cursor pagination walks the full list in pages
//     of N without gaps or duplicates.
//  3. MarkAllNotificationsRead flips every unread row
//     to is_read=TRUE and reports rows affected; a
//     second call is idempotent (0 rows).
//  4. Other users' notifications are untouched.
//
// Usage (host-side):
//
//	docker run --rm --network mokibox_backend \
//	    -v $PWD:/repo -w /repo -e DATABASE_URL=... \
//	    golang:1.25.5-alpine \
//	    go run ./scripts/smoketest/phase7_notification
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const usernamePrefix = "smoke-test-phase7c-"

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS phase7_notification")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL required (superuser DSN)")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	q := db.New(sqlDB)

	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE username LIKE $1`, usernamePrefix+"%"); err != nil {
		return fmt.Errorf("pre-clean users: %w", err)
	}

	recipient, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"recipient")
	if err != nil {
		return fmt.Errorf("seed recipient: %w", err)
	}
	actor, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"actor")
	if err != nil {
		return fmt.Errorf("seed actor: %w", err)
	}

	// Seed 5 notifications: follower -> recipient.
	payload, err := json.Marshal(map[string]string{"username": actor.Username})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	total := 5
	for i := 0; i < total; i++ {
		if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
			UserID:  recipient.ID,
			ActorID: actor.ID,
			Type:    "follow",
			Payload: payload,
		}); err != nil {
			return fmt.Errorf("insert notif %d: %w", i, err)
		}
	}
	// One for the actor themself - must NOT appear in
	// recipient's list nor be flipped by the recipient's
	// mark-all.
	if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
		UserID:  actor.ID,
		ActorID: recipient.ID,
		Type:    "follow",
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("insert other-user notif: %w", err)
	}

	// --- 1+2: paged list, created_at DESC.
	pageSize := 2
	seen := map[string]struct{}{}
	var cursorCreated sql.NullTime
	var cursorID uuid.NullUUID
	for page := 0; ; page++ {
		rows, err := q.ListNotifications(ctx, db.ListNotificationsParams{
			UserID:        recipient.ID,
			CursorCreated: cursorCreated,
			CursorID:      cursorID,
			PageLimit:     int32(pageSize),
		})
		if err != nil {
			return fmt.Errorf("list page %d: %w", page, err)
		}
		for _, r := range rows {
			if _, dup := seen[r.ID.String()]; dup {
				return fmt.Errorf("duplicate row across pages: %s", r.ID)
			}
			seen[r.ID.String()] = struct{}{}
			if r.UserID != recipient.ID {
				return fmt.Errorf("leaked other-user notification %s", r.ID)
			}
			if len(r.Payload) == 0 {
				return fmt.Errorf("empty payload on %s", r.ID)
			}
		}
		if len(rows) < pageSize {
			break
		}
		last := rows[len(rows)-1]
		cursorCreated = sql.NullTime{Time: last.CreatedAt, Valid: true}
		cursorID = uuid.NullUUID{UUID: last.ID, Valid: true}
	}
	if len(seen) != total {
		return fmt.Errorf("paged list saw %d, want %d", len(seen), total)
	}

	// --- 3: mark all read; then idempotent re-run.
	updated, err := q.MarkAllNotificationsRead(ctx, recipient.ID)
	if err != nil {
		return fmt.Errorf("mark all read: %w", err)
	}
	if updated != int64(total) {
		return fmt.Errorf("updated=%d, want %d", updated, total)
	}
	again, err := q.MarkAllNotificationsRead(ctx, recipient.ID)
	if err != nil {
		return fmt.Errorf("mark all read (idempotent): %w", err)
	}
	if again != 0 {
		return fmt.Errorf("idempotent mark returned %d, want 0", again)
	}

	// --- 4: other user's row untouched.
	var otherUnread int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND is_read=false`, actor.ID).Scan(&otherUnread); err != nil {
		return err
	}
	if otherUnread != 1 {
		return fmt.Errorf("actor unread=%d, want 1 (untouched)", otherUnread)
	}
	var recipientUnread int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND is_read=false`, recipient.ID).Scan(&recipientUnread); err != nil {
		return err
	}
	if recipientUnread != 0 {
		return fmt.Errorf("recipient unread=%d, want 0", recipientUnread)
	}

	log.Printf("ok: list (5 rows paged by 2), mark-all idempotent, cross-user isolation verified")
	return nil
}

func ensureUser(ctx context.Context, sqlDB *sql.DB, q *db.Queries, username string) (db.User, error) {
	var u db.User
	row := sqlDB.QueryRowContext(ctx,
		`SELECT id, zitadel_id, username, display_name, bio, avatar_url, is_private, is_active, deleted_at, created_at FROM users WHERE username = $1`, username)
	if err := row.Scan(&u.ID, &u.ZitadelID, &u.Username, &u.DisplayName, &u.Bio, &u.AvatarUrl, &u.IsPrivate, &u.IsActive, &u.DeletedAt, &u.CreatedAt); err == nil {
		return u, nil
	}
	return q.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   "smoke-phase7c-" + username,
		Username:    username,
		DisplayName: sql.NullString{String: username, Valid: true},
	})
}
