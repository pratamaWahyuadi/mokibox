## 📋 TikTok Clone Backend - MVP Plan (Detail)

---

### Arsitektur

```mermaid
graph TB
    M[Mobile App] -->|HTTP + JWT| API[API Gateway<br/>Go + Echo]
    API -->|Validate JWT| Z[Zitadel]
    API -->|Query| PG[(PostgreSQL)]
    API -->|Enqueue| Q[Redis Queue<br/>Asynq]
    Q -->|Dequeue| T[Transcoder Worker<br/>FFmpeg]
    T -->|Download/Upload| R2[(Cloudflare R2)]
    API -->|Generate Presigned URL| R2
    M -->|Direct Upload| R2
```

**Cuma 2 service:**
1. **API Gateway** - Semua logic
2. **Transcoder Worker** - FFmpeg processing

---

### Tech Stack

| Layer | Teknologi |
|-------|-----------|
| API | Go + Echo |
| Auth | Zitadel (OIDC, JWT) |
| Database | PostgreSQL 16 |
| Queue | Redis + Asynq |
| Storage | Cloudflare R2 |
| Video | FFmpeg |
| Container | Docker |

---

### Database Schema

```sql
-- 001_init.sql

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    zitadel_id TEXT UNIQUE NOT NULL,
    username TEXT UNIQUE NOT NULL,
    display_name TEXT,
    bio TEXT,
    avatar_url TEXT,
    is_private BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE videos (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    title TEXT,
    description TEXT,
    r2_key TEXT NOT NULL,
    hls_url TEXT,
    thumbnail_url TEXT,
    duration_seconds INT,
    status TEXT DEFAULT 'PROCESSING',
    -- PROCESSING, READY, FAILED
    likes_count INT DEFAULT 0,
    views_count INT DEFAULT 0,
    comments_count INT DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_videos_user ON videos(user_id);
CREATE INDEX idx_videos_status ON videos(status);
CREATE INDEX idx_videos_created ON videos(created_at DESC);

CREATE TABLE follows (
    follower_id UUID REFERENCES users(id) ON DELETE CASCADE,
    followee_id UUID REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMP DEFAULT NOW(),
    PRIMARY KEY (follower_id, followee_id)
);

CREATE INDEX idx_follows_follower ON follows(follower_id);
CREATE INDEX idx_follows_followee ON follows(followee_id);

CREATE TABLE likes (
    user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    video_id UUID REFERENCES videos(id) ON DELETE CASCADE,
    created_at TIMESTAMP DEFAULT NOW(),
    PRIMARY KEY (user_id, video_id)
);

CREATE INDEX idx_likes_video ON likes(video_id);

CREATE TABLE comments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    video_id UUID REFERENCES videos(id) ON DELETE CASCADE,
    user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    parent_id UUID REFERENCES comments(id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_comments_video ON comments(video_id, created_at DESC);
CREATE INDEX idx_comments_parent ON comments(parent_id);

CREATE TABLE notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID REFERENCES users(id) ON DELETE CASCADE,
    type TEXT NOT NULL,
    -- follow, like, comment
    payload JSONB,
    is_read BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_notifications_user ON notifications(user_id, is_read, created_at DESC);
```

---

### Auth Flow

```mermaid
sequenceDiagram
    participant M as Mobile
    participant Z as Zitadel
    participant API as API Gateway
    participant PG as PostgreSQL

    M->>Z: Login (UI Zitadel)
    Z-->>M: JWT Token
    
    M->>API: Request + JWT Header
    API->>API: Validate JWT (RSA Public Key)
    API->>PG: SELECT * FROM users WHERE zitadel_id = $1
    
    alt User exists
        PG-->>API: User data
    else User not exists
        API->>PG: INSERT INTO users (zitadel_id, username)
        PG-->>API: New user data
    end
    
    API-->>M: Response
```

**Middleware Auth:**
```go
func AuthMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
    return func(c echo.Context) error {
        token := c.Request().Header.Get("Authorization")
        
        // Validate JWT dengan RSA Public Key Zitadel
        claims, err := validateJWT(token)
        if err != nil {
            return c.JSON(401, "Unauthorized")
        }
        
        // Get or create user di DB
        user, err := getOrCreateUser(claims.Subject)
        if err != nil {
            return c.JSON(500, "Internal error")
        }
        
        c.Set("user", user)
        return next(c)
    }
}
```

---

### Upload Flow

```mermaid
sequenceDiagram
    participant M as Mobile
    participant API as API Gateway
    participant R2 as R2 Storage
    participant PG as PostgreSQL
    participant Q as Redis Queue

    M->>API: POST /videos/upload-intent<br/>{title, file_size}
    API->>API: Validate JWT
    API->>R2: Generate Presigned URL (15min)
    R2-->>API: presigned_url
    API-->>M: {upload_url, r2_key}
    
    M->>R2: PUT video.mp4 (direct)
    R2-->>M: 200 OK
    
    M->>API: POST /videos/confirm<br/>{r2_key, duration}
    API->>PG: INSERT videos (status=PROCESSING)
    PG-->>API: video_id
    API->>Q: Enqueue transcode_job
    API-->>M: {video_id, status: processing}
```

**Handler Upload:**
```go
func (h *VideoHandler) UploadIntent(c echo.Context) error {
    user := c.Get("user").(User)
    
    var req struct {
        Title    string `json:"title"`
        FileSize int64  `json:"file_size"`
    }
    c.Bind(&req)
    
    // Generate unique key
    videoID := uuid.New()
    r2Key := fmt.Sprintf("raw/%s/%s.mp4", user.ID, videoID)
    
    // Presigned URL
    uploadURL, err := h.r2.GeneratePresignedURL(r2Key, 15*time.Minute)
    if err != nil {
        return c.JSON(500, "Storage error")
    }
    
    return c.JSON(200, map[string]string{
        "upload_url": uploadURL,
        "r2_key":     r2Key,
    })
}

func (h *VideoHandler) ConfirmUpload(c echo.Context) error {
    user := c.Get("user").(User)
    
    var req struct {
        R2Key    string `json:"r2_key"`
        Duration int    `json:"duration"`
    }
    c.Bind(&req)
    
    // Insert video
    video := Video{
        ID:         uuid.New(),
        UserID:     user.ID,
        R2Key:      req.R2Key,
        Duration:   req.Duration,
        Status:     "PROCESSING",
    }
    
    h.db.Create(&video)
    
    // Enqueue transcode job
    job := asynq.NewTask("transcode:video", map[string]interface{}{
        "video_id": video.ID,
        "r2_key":   req.R2Key,
    })
    h.queue.Enqueue(job)
    
    return c.JSON(200, map[string]interface{}{
        "video_id": video.ID,
        "status":   "processing",
    })
}
```

---

### Transcoder Worker

```go
func main() {
    redisClient := redis.NewClient(&redis.Options{Addr: "redis:6379"})
    
    worker := asynq.NewServer(redisClient, asynq.Config{
        Concurrency: 5,
        Queues: map[string]int{
            "transcoder": 10,
        },
    })
    
    mux := asynq.NewServeMux()
    mux.HandleFunc("transcode:video", handleTranscode)
    
    worker.Run(mux)
}

func handleTranscode(ctx context.Context, task *asynq.Task) error {
    var payload map[string]interface{}
    json.Unmarshal(task.Payload(), &payload)
    
    videoID := payload["video_id"].(string)
    r2Key := payload["r2_key"].(string)
    
    // 1. Download raw dari R2
    rawPath := fmt.Sprintf("/tmp/%s.mp4", videoID)
    r2.Download(r2Key, rawPath)
    
    // 2. Transcode ke HLS
    hlsDir := fmt.Sprintf("/tmp/%s_hls", videoID)
    transcodeToHLS(rawPath, hlsDir)
    
    // 3. Generate thumbnail
    thumbnailPath := fmt.Sprintf("/tmp/%s_thumb.jpg", videoID)
    generateThumbnail(rawPath, thumbnailPath, 1) // frame 1 detik
    
    // 4. Upload HLS + thumbnail ke R2
    r2.UploadDirectory(hlsDir, fmt.Sprintf("hls/%s/", videoID))
    r2.Upload(thumbnailPath, fmt.Sprintf("thumbnails/%s.jpg", videoID))
    
    // 5. Update DB
    db.Exec(`
        UPDATE videos 
        SET hls_url = $1, thumbnail_url = $2, status = 'READY'
        WHERE id = $3
    `, 
        fmt.Sprintf("hls/%s/master.m3u8", videoID),
        fmt.Sprintf("thumbnails/%s.jpg", videoID),
        videoID,
    )
    
    // 6. Delete raw
    r2.Delete(r2Key)
    os.Remove(rawPath)
    
    return nil
}

func transcodeToHLS(input, output string) error {
    // FFmpeg command
    cmd := exec.Command("ffmpeg",
        "-i", input,
        "-vf", "scale=854:480", "-b:v", "1000k", "-c:a", "aac",
        "-f", "hls",
        "-hls_time", "4",
        "-hls_playlist_type", "vod",
        "-hls_segment_filename", output+"/480p_%03d.ts",
        output+"/480p.m3u8",
        // 720p
        "-vf", "scale=1280:720", "-b:v", "2500k", "-c:a", "aac",
        "-f", "hls",
        "-hls_time", "4",
        "-hls_playlist_type", "vod",
        "-hls_segment_filename", output+"/720p_%03d.ts",
        output+"/720p.m3u8",
    )
    
    return cmd.Run()
}
```

---

### Like System (Langsung DB)

```go
func (h *SocialHandler) LikeVideo(c echo.Context) error {
    user := c.Get("user").(User)
    videoID := c.Param("id")
    
    // Transaction
    tx := h.db.Begin()
    
    // 1. Insert like
    err := tx.Exec(`
        INSERT INTO likes (user_id, video_id) VALUES ($1, $2)
        ON CONFLICT DO NOTHING
    `, user.ID, videoID)
    
    if err != nil {
        tx.Rollback()
        return c.JSON(500, "Error")
    }
    
    // 2. Update counter
    tx.Exec(`
        UPDATE videos SET likes_count = likes_count + 1
        WHERE id = $1
    `, videoID)
    
    tx.Commit()
    
    // 3. Create notification (async)
    go createNotification(videoOwnerID, "like", user.ID, videoID)
    
    return c.JSON(200, "Liked")
}

func (h *SocialHandler) UnlikeVideo(c echo.Context) error {
    user := c.Get("user").(User)
    videoID := c.Param("id")
    
    tx := h.db.Begin()
    
    // 1. Delete like
    tx.Exec(`
        DELETE FROM likes WHERE user_id = $1 AND video_id = $2
    `, user.ID, videoID)
    
    // 2. Update counter
    tx.Exec(`
        UPDATE videos SET likes_count = GREATEST(likes_count - 1, 0)
        WHERE id = $1
    `, videoID)
    
    tx.Commit()
    
    return c.JSON(200, "Unliked")
}
```

---

### Feed Query

```sql
-- GET /api/feed/home
SELECT v.*, u.username, u.avatar_url
FROM videos v
JOIN users u ON v.user_id = u.id
WHERE v.status = 'READY'
  AND (
    v.user_id IN (SELECT followee_id FROM follows WHERE follower_id = $1)
    OR u.is_private = FALSE
  )
  AND v.user_id != $1
ORDER BY v.created_at DESC
LIMIT 20;
```

```go
func (h *FeedHandler) HomeFeed(c echo.Context) error {
    user := c.Get("user").(User)
    
    var videos []Video
    h.db.Raw(`
        SELECT v.*, u.username, u.avatar_url
        FROM videos v
        JOIN users u ON v.user_id = u.id
        WHERE v.status = 'READY'
          AND (
            v.user_id IN (SELECT followee_id FROM follows WHERE follower_id = ?)
            OR u.is_private = FALSE
          )
          AND v.user_id != ?
        ORDER BY v.created_at DESC
        LIMIT 20
    `, user.ID, user.ID).Scan(&videos)
    
    return c.JSON(200, videos)
}
```

---

### View Tracking

```go
func (h *VideoHandler) TrackView(c echo.Context) error {
    videoID := c.Param("id")
    
    // Simple update
    h.db.Exec(`
        UPDATE videos SET views_count = views_count + 1
        WHERE id = $1
    `, videoID)
    
    return c.JSON(200, "OK")
}
```

---

### Notification

```go
func createNotification(userID string, notifType string, actorID string, videoID string) {
    payload := map[string]interface{}{
        "actor_id": actorID,
        "video_id": videoID,
    }
    
    db.Exec(`
        INSERT INTO notifications (user_id, type, payload)
        VALUES ($1, $2, $3)
    `, userID, notifType, payload)
}
```

---

### Struktur Direktori

```text
tiktok-backend/
├── docker-compose.yml
├── Makefile
├── .env
│
├── api-gateway/
│   ├── main.go
│   ├── handlers/
│   │   ├── user.go
│   │   ├── video.go
│   │   ├── feed.go
│   │   ├── social.go
│   │   └── notification.go
│   ├── middleware/
│   │   └── auth.go
│   └── Dockerfile
│
├── transcoder-worker/
│   ├── main.go
│   └── Dockerfile
│
├── shared/
│   ├── db.go
│   ├── redis.go
│   ├── r2.go
│   └── models.go
│
└── migrations/
    └── 001_init.sql
```

---

### Docker Compose

```yaml
version: '3.8'

services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_USER: tiktok
      POSTGRES_PASSWORD: tiktok
      POSTGRES_DB: tiktok
    ports:
      - "5432:5432"
    volumes:
      - pg_data:/var/lib/postgresql/data
      - ./migrations:/migrations

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"

  api-gateway:
    build: ./api-gateway
    ports:
      - "8080:8080"
    environment:
      - DATABASE_URL=postgres://tiktok:tiktok@postgres:5432/tiktok
      - REDIS_URL=redis:6379
      - ZITADEL_ISSUER=https://auth.yourdomain.com
      - R2_BUCKET=tiktok-videos
      - R2_ACCOUNT_ID=xxx
      - R2_ACCESS_KEY=xxx
      - R2_SECRET_KEY=xxx
    depends_on:
      - postgres
      - redis

  transcoder-worker:
    build: ./transcoder-worker
    environment:
      - REDIS_URL=redis:6379
      - R2_BUCKET=tiktok-videos
      - DATABASE_URL=postgres://tiktok:tiktok@postgres:5432/tiktok
    depends_on:
      - redis
      - postgres
    volumes:
      - /tmp/transcoder:/tmp

volumes:
  pg_data:
```

---

### Makefile

```makefile
up:
	docker-compose up -d

down:
	docker-compose down

build:
	docker-compose build

logs:
	docker-compose logs -f

migrate:
	docker-compose exec postgres psql -U tiktok -d tiktok -f /migrations/001_init.sql

test:
	cd api-gateway && go test ./...

run-api:
	cd api-gateway && go run main.go

run-worker:
	cd transcoder-worker && go run main.go
```

---

### API Endpoints

```yaml
Auth:
  GET    /api/users/me               # Profil sendiri
  PUT    /api/users/me               # Update profil
  GET    /api/users/:id              # Profil user lain
  GET    /api/users/:id/videos       # Video user

Social:
  POST   /api/users/:id/follow       # Follow
  DELETE /api/users/:id/follow       # Unfollow
  GET    /api/users/:id/followers    # List followers
  GET    /api/users/:id/following    # List following

Feed:
  GET    /api/feed/home              # Home feed

Video:
  POST   /api/videos/upload-intent   # Presigned URL
  POST   /api/videos/confirm         # Confirm upload
  GET    /api/videos/:id             # Video detail
  GET    /api/videos/:id/status      # Processing status
  DELETE /api/videos/:id             # Delete video

Interactions:
  POST   /api/videos/:id/like        # Like
  DELETE /api/videos/:id/like        # Unlike
  POST   /api/videos/:id/comments    # Comment
  GET    /api/videos/:id/comments    # List comments
  DELETE /api/comments/:id           # Delete comment
  POST   /api/comments/:id/reply     # Reply
  POST   /api/videos/:id/view        # Track view

Notifications:
  GET    /api/notifications          # List notifications
  PUT    /api/notifications/read-all # Mark all read
```

---

### Development Timeline

| Hari | Fokus |
|------|-------|
| 1 | Setup Docker, DB, migrations |
| 2-3 | Auth (Zitadel) + User API |
| 4-6 | Upload + Transcode |
| 7-8 | Feed + Social Graph |
| 9-10 | Like + Comment + View |
| 11-12 | Notifications |
| 13-14 | Testing + Polish |

**Total: 2 minggu**
