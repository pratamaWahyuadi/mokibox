# API Contract — TikTok Clone Backend MVP

## 0. Asumsi Teknis & Konvensi

### Asumsi teknis

1. **Notifikasi lifecycle user dari Zitadel** memakai **Actions V2 Target**, bukan webhook generik (fitur itu tidak ada di Zitadel). Payload dikirim oleh Zitadel sesuai format event Actions V2:
   ```json
   {
     "aggregateID": "336494809936035843",
     "aggregateType": "user",
     "resourceOwner": "336392597046099971",
     "instanceID": "336392597046034435",
     "version": "v2",
     "sequence": 1,
     "event_type": "user.removed",
     "created_at": "2025-01-01T12:20:00Z",
     "userID": "336392597046755331",
     "event_payload": { }
   }
   ```
   User ID terdampak diambil dari field `userID`. `event_type` yang relevan: `user.removed` (user dihapus) dan `user.deactivated` (user dinonaktifkan) — nilai ini konstanta resmi Zitadel, bukan skema custom.

   Signature diletakkan di header `ZITADEL-Signature`, formatnya `t=<timestamp>,v1=<hmac_hex>`. HMAC dihitung dari `timestamp + "." + raw_body` menggunakan `signingKey` yang didapat sekali dari response `CreateTarget` (disimpan sebagai `ZITADEL_TARGET_SIGNING_KEY`), **bukan** secret statis yang dibuat sendiri. Verifikasi memakai helper resmi `actions.ValidateRequestPayload` dari `github.com/zitadel/zitadel-go/v3/pkg/actions`.

2. **`DELETE /api/users/me`** ditambahkan untuk memenuhi US-09 / FR-USER-03, karena PRD tidak mencantumkan endpoint eksplisit untuk user menghapus akunnya. Endpoint ini hanya menghapus data lokal; penghapusan akun di Zitadel tetap dilakukan via UI Zitadel atau webhook.

3. **HLS private** membutuhkan endpoint tambahan `GET /api/videos/:id/playlist.m3u8`. `hls_playlist_url` pada video detail akan menunjuk ke endpoint ini, bukan URL statis R2. Endpoint ini me-rewrite playlist agar berisi presigned URL untuk tiap segmen `.ts`.

4. **`avatar_url`** di response API dapat berupa presigned URL jika file avatar disimpan di R2 private.

5. **Username dianggap immutable** melalui API. `PUT /api/users/me` hanya mengubah `display_name`, `bio`, `avatar_url`, dan `is_private`.

6. **Visibility**:
   - User non-aktif (`is_active = false`) tidak bisa diakses publik.
   - Akun private: video hanya bisa dilihat oleh follower.
   - Video non-`READY` hanya bisa dilihat oleh pemiliknya.
   - Endpoint read pada resource yang tidak berhak akses mengembalikan `404`, bukan `403`, untuk mencegah resource enumeration.

### Konvensi

- **Base URL**: `https://api.example.com`
- **Semua endpoint** di bawah `/api/*` kecuali webhook membutuhkan header:
  ```
  Authorization: Bearer <access_token_zitadel>
  ```
- **Format JSON**: `snake_case`, mengikuti nama kolom database.
- **Timestamp**: RFC3339 UTC, contoh: `2025-01-01T10:00:00Z`.
- **Content-Type**: `application/json; charset=utf-8`.
- **Pagination**: semua list endpoint memakai query param:
  - `limit`: default `20`, maksimal `50`
  - `cursor`: string opaque dari response sebelumnya
  - Response pagination:
    ```json
    {
      "data": [],
      "pagination": {
        "next_cursor": null
      }
    }
    ```

### Format response sukses

- Resource tunggal:
  ```json
  {
    "data": { }
  }
  ```

- List resource:
  ```json
  {
    "data": [],
    "pagination": {
      "next_cursor": null
    }
  }
  ```

- DELETE tanpa body: `204 No Content`.

### Format error standar

Semua error menggunakan format:

```json
{
  "error": {
    "code": "ERROR_CODE",
    "message": "Deskripsi error untuk manusia",
    "details": [
      {
        "field": "content",
        "message": "Field error spesifik"
      }
    ]
  }
}
```

`details` hanya wajib untuk `400 VALIDATION_ERROR`.

Contoh per status:

```json
{
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "Validation failed",
    "details": [
      {
        "field": "content",
        "message": "must be between 1 and 1000 characters"
      }
    ]
  }
}
```

```json
{
  "error": {
    "code": "UNAUTHORIZED",
    "message": "Missing or invalid access token"
  }
}
```

```json
{
  "error": {
    "code": "FORBIDDEN",
    "message": "You are not allowed to perform this action"
  }
}
```

```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "Resource not found"
  }
}
```

```json
{
  "error": {
    "code": "VIDEO_STATUS_CONFLICT",
    "message": "Video is not in a confirmable state"
  }
}
```

```json
{
  "error": {
    "code": "INTERNAL_ERROR",
    "message": "Internal server error"
  }
}
```

---

## 1. Daftar Endpoint

| Method | Path | Deskripsi | Auth |
|---|---|---|---|
| GET | `/api/users/me` | Profil sendiri | JWT |
| PUT | `/api/users/me` | Update profil sendiri | JWT |
| DELETE | `/api/users/me` | Hapus akun sendiri + data lokal | JWT |
| GET | `/api/users/:id` | Profil user lain | JWT |
| GET | `/api/users/:id/videos` | List video milik user | JWT |
| POST | `/api/users/:id/follow` | Follow user | JWT |
| DELETE | `/api/users/:id/follow` | Unfollow user | JWT |
| GET | `/api/users/:id/followers` | List followers | JWT |
| GET | `/api/users/:id/following` | List following | JWT |
| GET | `/api/feed/home` | Home feed | JWT |
| POST | `/api/videos/upload-intent` | Minta presigned URL upload | JWT |
| POST | `/api/videos/confirm` | Konfirmasi upload selesai | JWT |
| GET | `/api/videos/:id` | Detail video + signed media URL | JWT |
| GET | `/api/videos/:id/status` | Status processing video | JWT (owner) |
| GET | `/api/videos/:id/playlist.m3u8` | HLS playlist dengan presigned segment URL | JWT / signed token |
| DELETE | `/api/videos/:id` | Hapus video | JWT (owner) |
| POST | `/api/videos/:id/like` | Like video | JWT |
| DELETE | `/api/videos/:id/like` | Unlike video | JWT |
| POST | `/api/videos/:id/comments` | Buat comment | JWT |
| GET | `/api/videos/:id/comments` | List comment | JWT |
| DELETE | `/api/comments/:id` | Hapus comment | JWT (owner) |
| POST | `/api/comments/:id/reply` | Reply comment | JWT |
| POST | `/api/videos/:id/view` | Track view | JWT |
| GET | `/api/notifications` | List notifikasi | JWT |
| PUT | `/api/notifications/read-all` | Tandai semua notifikasi dibaca | JWT |
| POST | `/api/webhooks/zitadel` | Actions V2 target — notifikasi lifecycle user | HMAC signature (`ZITADEL-Signature`) |

---

## 2. Object Umum

### UserSummary

Dipakai di dalam video/comment/feed.

```json
{
  "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
  "username": "alice",
  "display_name": "Alice Wonder",
  "avatar_url": "https://r2.example.com/avatars/alice.jpg",
  "is_private": false
}
```

### UserProfile

Dipakai di endpoint profil.

```json
{
  "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
  "username": "alice",
  "display_name": "Alice Wonder",
  "bio": "Cat lover",
  "avatar_url": "https://r2.example.com/avatars/alice.jpg",
  "is_private": false,
  "is_active": true,
  "created_at": "2025-01-01T10:00:00Z"
}
```

Untuk `GET /api/users/:id`, ditambahkan:

```json
{
  "is_following": true,
  "follower_count": 10,
  "following_count": 7
}
```

### VideoObject

```json
{
  "id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
  "user_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
  "title": "My first video",
  "description": "Cute cat video",
  "duration_seconds": 45,
  "status": "READY",
  "retry_count": 0,
  "likes_count": 12,
  "views_count": 340,
  "comments_count": 5,
  "created_at": "2025-01-01T12:00:00Z",
  "thumbnail_url": "https://r2.example.com/thumb/...?...",
  "hls_playlist_url": "https://api.example.com/api/videos/d3f4.../playlist.m3u8?token=...",
  "liked_by_me": false,
  "is_owner": false,
  "user": {
    "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "username": "alice",
    "display_name": "Alice Wonder",
    "avatar_url": "https://r2.example.com/avatars/alice.jpg",
    "is_private": false
  }
}
```

Untuk video non-`READY`, `thumbnail_url` dan `hls_playlist_url` bernilai `null`, dan `duration_seconds` bisa `null`.

### CommentObject

```json
{
  "id": "c0ffe-1234-....",
  "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
  "user_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
  "parent_id": null,
  "content": "Nice video!",
  "created_at": "2025-01-01T12:05:00Z",
  "user": {
    "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "username": "alice",
    "display_name": "Alice Wonder",
    "avatar_url": "https://r2.example.com/avatars/alice.jpg",
    "is_private": false
  }
}
```

### NotificationObject

```json
{
  "id": "n-1234-....",
  "user_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
  "actor_id": "9f7d3c86-....",
  "type": "like",
  "payload": {
    "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "username": "bob"
  },
  "is_read": false,
  "created_at": "2025-01-01T12:06:00Z"
}
```

---

## 3. Auth & User

### `GET /api/users/me`

Mengembalikan profil user yang sedang login.

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": {
    "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "username": "alice",
    "display_name": "Alice Wonder",
    "bio": "Cat lover",
    "avatar_url": "https://r2.example.com/avatars/alice.jpg",
    "is_private": false,
    "is_active": true,
    "created_at": "2025-01-01T10:00:00Z"
  }
}
```

Error: `401`, `500`.

---

### `PUT /api/users/me`

Mengupdate profil sendiri.

Headers:

```
Authorization: Bearer <access_token>
Content-Type: application/json
```

Request body:

```json
{
  "display_name": "Alice W",
  "bio": "New bio",
  "avatar_url": "https://r2.example.com/avatars/alice-new.jpg",
  "is_private": true
}
```

Semua field opsional. `username` tidak bisa diubah.

Response `200 OK`:

```json
{
  "data": {
    "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "username": "alice",
    "display_name": "Alice W",
    "bio": "New bio",
    "avatar_url": "https://r2.example.com/avatars/alice-new.jpg",
    "is_private": true,
    "is_active": true,
    "created_at": "2025-01-01T10:00:00Z"
  }
}
```

Error: `400 VALIDATION_ERROR`, `401`, `500`.

---

### `DELETE /api/users/me`

Menghapus akun lokal beserta seluruh data user: profil, video, like, comment, follow, notifikasi, dan file R2. File R2 dihapus async maksimal 1×24 jam.

Headers:

```
Authorization: Bearer <access_token>
```

Response `204 No Content`.

Error: `401`, `500`.

---

### `GET /api/users/:id`

Mengambil profil user lain.

Path Parameter:

| Nama | Tipe |
|---|---|
| `id` | UUID |

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": {
    "id": "9f7d3c86-....",
    "username": "bob",
    "display_name": "Bob",
    "bio": "Hello!",
    "avatar_url": "https://r2.example.com/avatars/bob.jpg",
    "is_private": true,
    "is_active": true,
    "created_at": "2025-01-01T10:00:00Z",
    "is_following": true,
    "follower_count": 42,
    "following_count": 8
  }
}
```

Error: `401`, `404`, `500`.

---

### `GET /api/users/:id/videos`

List video milik user.

Query Parameters:

| Nama | Tipe | Keterangan |
|---|---|---|
| `limit` | integer | Default 20, max 50 |
| `cursor` | string | Opsional |

Rules:
- `:id` = user sendiri → semua status kecuali `DELETED`.
- `:id` = user lain → hanya video `READY`.
- Jika akun target private dan requester bukan follower → `404`.
- Jika user target non-aktif → `404`.

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": [
    {
      "id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
      "user_id": "9f7d3c86-....",
      "title": "Bob's video",
      "description": "Travel vlog",
      "duration_seconds": 60,
      "status": "READY",
      "retry_count": 0,
      "likes_count": 5,
      "views_count": 120,
      "comments_count": 2,
      "created_at": "2025-01-01T12:00:00Z",
      "thumbnail_url": "https://r2.example.com/thumb/...",
      "hls_playlist_url": "https://api.example.com/api/videos/d3f4.../playlist.m3u8?token=...",
      "liked_by_me": false,
      "is_owner": false,
      "user": {
        "id": "9f7d3c86-....",
        "username": "bob",
        "display_name": "Bob",
        "avatar_url": "https://r2.example.com/avatars/bob.jpg",
        "is_private": true
      }
    }
  ],
  "pagination": {
    "next_cursor": null
  }
}
```

Error: `400`, `401`, `404`, `500`.

---

## 4. Upload & Transcode

### `POST /api/videos/upload-intent`

Membuat atau me-reuse record video `PENDING_UPLOAD`, lalu mengembalikan presigned URL untuk upload ke R2.

Headers:

```
Authorization: Bearer <access_token>
Content-Type: application/json
```

Request body:

```json
{
  "title": "My cat video",
  "description": "Cute cat"
}
```

`title` dan `description` opsional.

Response `201 Created` jika record `PENDING_UPLOAD` baru dibuat.  
Response `200 OK` jika user sudah punya record `PENDING_UPLOAD` dan presigned URL baru diterbitkan.

```json
{
  "data": {
    "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "r2_key": "uploads/3f7d3c86-a1b2-4e5f-9c0d-1234567890ab/d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456/source.mp4",
    "http_method": "PUT",
    "upload_url": "https://r2.example.com/uploads/.../source.mp4?X-Amz-...",
    "upload_headers": {
      "Content-Type": "application/octet-stream"
    },
    "min_size_bytes": 1024,
    "max_size_bytes": 209715200,
    "expires_at": "2025-01-01T12:15:00Z"
  }
}
```

Catatan upload:
- Upload dilakukan dengan `PUT` ke `upload_url`.
- Wajib mengirim header `Content-Type: application/octet-stream`.
- Body adalah file mentah video.
- Jika presigned URL expired, panggil ulang `upload-intent`. Record `PENDING_UPLOAD` yang sama akan di-reuse.

Error: `400`, `401`, `500`.

---

### `POST /api/videos/confirm`

Konfirmasi bahwa file sudah diupload ke R2. Memicu enqueue job transcode.

Headers:

```
Authorization: Bearer <access_token>
Content-Type: application/json
```

Request body:

```json
{
  "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
  "r2_key": "uploads/3f7d3c86-a1b2-4e5f-9c0d-1234567890ab/d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456/source.mp4"
}
```

Response `200 OK`:

```json
{
  "data": {
    "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "status": "PROCESSING",
    "retry_count": 0
  }
}
```

Error:
- `400 VALIDATION_ERROR` — body tidak valid / `r2_key` tidak cocok.
- `401`
- `404` — video tidak ditemukan.
- `409 VIDEO_STATUS_CONFLICT` — status bukan `PENDING_UPLOAD`.
- `409 UPLOAD_MISSING` — objek tidak ditemukan di R2.
- `400 UPLOAD_SIZE_INVALID` — ukuran aktual objek di luar 1 KB – 200 MB.
- `500`

---

### `GET /api/videos/:id`

Mengambil detail video.

Rules:
- Owner: bisa akses semua status.
- Non-owner: hanya bisa akses video `READY`, dan hanya jika akun pemilik publik atau requester sudah follow.
- Jika video private dan bukan follower → `404`.

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": {
    "id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "user_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "title": "My cat video",
    "description": "Cute cat",
    "duration_seconds": 45,
    "status": "READY",
    "retry_count": 0,
    "likes_count": 12,
    "views_count": 340,
    "comments_count": 5,
    "created_at": "2025-01-01T12:00:00Z",
    "thumbnail_url": "https://r2.example.com/thumb/...",
    "hls_playlist_url": "https://api.example.com/api/videos/d3f4.../playlist.m3u8?token=...",
    "liked_by_me": false,
    "is_owner": false,
    "user": {
      "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
      "username": "alice",
      "display_name": "Alice Wonder",
      "avatar_url": "https://r2.example.com/avatars/alice.jpg",
      "is_private": false
    }
  }
}
```

Error: `401`, `404`, `500`.

---

### `GET /api/videos/:id/status`

Mengambil status processing video. Hanya pemilik video.

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": {
    "id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "status": "PROCESSING",
    "retry_count": 0,
    "duration_seconds": null
  }
}
```

Jika sudah `READY`, response:

```json
{
  "data": {
    "id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "status": "READY",
    "retry_count": 0,
    "duration_seconds": 45
  }
}
```

Error: `401`, `404` (jika bukan owner / tidak ditemukan), `500`.

---

### `GET /api/videos/:id/playlist.m3u8`

Mengembalikan HLS playlist yang sudah di-rewrite. Setiap URL segmen di dalam playlist adalah presigned URL R2.

Auth: bisa menggunakan header Bearer JWT, atau query parameter `?token=` jika client media player tidak bisa mengirim header.

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

Content-Type: `application/vnd.apple.mpegurl`

```
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.0,
https://r2.example.com/hls/<user_id>/<video_id>/segment0.ts?X-Amz-...
#EXTINF:6.0,
https://r2.example.com/hls/<user_id>/<video_id>/segment1.ts?X-Amz-...
#EXT-X-ENDLIST
```

Error: `401`, `403`, `404`, `500`.

---

### `DELETE /api/videos/:id`

Menghapus video. Hanya pemilik video.

Status video diubah menjadi `DELETED` dan `deleted_at` diisi. Cleanup R2 berjalan maksimal 1×24 jam.

Headers:

```
Authorization: Bearer <access_token>
```

Response `204 No Content`.

Error: `401`, `403`, `404`, `500`.

---

## 5. Feed

### `GET /api/feed/home`

Menampilkan video `READY` dari:
- user yang di-follow oleh requester
- user publik

Tidak termasuk video milik requester sendiri.

Query Parameters:

| Nama | Tipe | Keterangan |
|---|---|---|
| `limit` | integer | Default 20, max 50 |
| `cursor` | string | Opsional |

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": [
    {
      "id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
      "user_id": "9f7d3c86-....",
      "title": "Hello world",
      "description": "First video",
      "duration_seconds": 30,
      "status": "READY",
      "retry_count": 0,
      "likes_count": 2,
      "views_count": 50,
      "comments_count": 1,
      "created_at": "2025-01-01T12:00:00Z",
      "thumbnail_url": "https://r2.example.com/thumb/...",
      "hls_playlist_url": "https://api.example.com/api/videos/d3f4.../playlist.m3u8?token=...",
      "liked_by_me": false,
      "is_owner": false,
      "user": {
        "id": "9f7d3c86-....",
        "username": "bob",
        "display_name": "Bob",
        "avatar_url": "https://r2.example.com/avatars/bob.jpg",
        "is_private": false
      }
    }
  ],
  "pagination": {
    "next_cursor": null
  }
}
```

Error: `400`, `401`, `500`.

---

## 6. Follow

### `POST /api/users/:id/follow`

Follow user lain. Idempotent: jika sudah follow, tetap `200 OK`.

Headers:

```
Authorization: Bearer <access_token>
```

Tidak ada request body.

Response `200 OK`:

```json
{
  "data": {
    "follower_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "followee_id": "9f7d3c86-....",
    "is_following": true,
    "created_at": "2025-01-01T12:10:00Z"
  }
}
```

Error:
- `400 SELF_FOLLOW_NOT_ALLOWED` — follow diri sendiri.
- `401`
- `404` — target user tidak ditemukan / non-aktif.
- `500`

---

### `DELETE /api/users/:id/follow`

Unfollow user. Idempotent: jika belum follow, tetap `200 OK`.

Headers:

```
Authorization: Bearer <access_token>
```

Tidak ada request body.

Response `200 OK`:

```json
{
  "data": {
    "follower_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "followee_id": "9f7d3c86-....",
    "is_following": false
  }
}
```

Error: `401`, `404`, `500`.

---

### `GET /api/users/:id/followers`

List followers dari seorang user.

Query Parameters:

| Nama | Tipe | Keterangan |
|---|---|---|
| `limit` | integer | Default 20, max 50 |
| `cursor` | string | Opsional |

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": [
    {
      "user": {
        "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
        "username": "alice",
        "display_name": "Alice Wonder",
        "avatar_url": "https://r2.example.com/avatars/alice.jpg",
        "is_private": false
      },
      "created_at": "2025-01-01T11:00:00Z"
    }
  ],
  "pagination": {
    "next_cursor": null
  }
}
```

Error: `400`, `401`, `404`, `500`.

---

### `GET /api/users/:id/following`

List following dari seorang user.

Query Parameters:

| Nama | Tipe | Keterangan |
|---|---|---|
| `limit` | integer | Default 20, max 50 |
| `cursor` | string | Opsional |

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": [
    {
      "user": {
        "id": "9f7d3c86-....",
        "username": "bob",
        "display_name": "Bob",
        "avatar_url": "https://r2.example.com/avatars/bob.jpg",
        "is_private": false
      },
      "created_at": "2025-01-01T11:30:00Z"
    }
  ],
  "pagination": {
    "next_cursor": null
  }
}
```

Error: `400`, `401`, `404`, `500`.

---

## 7. Like

### `POST /api/videos/:id/like`

Like video. Idempotent: jika sudah like, tetap `200 OK` dengan `liked: true`.

Video harus berstatus `READY` dan boleh diakses requester.

Headers:

```
Authorization: Bearer <access_token>
```

Tidak ada request body.

Response `200 OK`:

```json
{
  "data": {
    "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "liked": true,
    "likes_count": 13
  }
}
```

Error:
- `401`
- `404` — video tidak ditemukan / tidak bisa diakses.
- `409 VIDEO_NOT_READY` — video belum siap (khusus owner).
- `500`

---

### `DELETE /api/videos/:id/like`

Unlike video. Idempotent: jika belum like, tetap `200 OK` dengan `liked: false`.

Headers:

```
Authorization: Bearer <access_token>
```

Tidak ada request body.

Response `200 OK`:

```json
{
  "data": {
    "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "liked": false,
    "likes_count": 12
  }
}
```

Error: `401`, `404`, `500`.

---

## 8. Comment

### `POST /api/videos/:id/comments`

Membuat comment pada video.

Headers:

```
Authorization: Bearer <access_token>
Content-Type: application/json
```

Request body:

```json
{
  "content": "Nice video!"
}
```

Response `201 Created`:

```json
{
  "data": {
    "id": "c0ffe-1234-....",
    "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "user_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "parent_id": null,
    "content": "Nice video!",
    "created_at": "2025-01-01T12:05:00Z",
    "user": {
      "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
      "username": "alice",
      "display_name": "Alice Wonder",
      "avatar_url": "https://r2.example.com/avatars/alice.jpg",
      "is_private": false
    }
  }
}
```

Error:
- `400 VALIDATION_ERROR` — content kosong / lebih dari 1000 karakter.
- `401`
- `404` — video tidak ditemukan / tidak bisa diakses.
- `409 VIDEO_NOT_READY`
- `500`

---

### `GET /api/videos/:id/comments`

List comment pada video. Mengembalikan semua comment termasuk reply secara flat, diurutkan `created_at DESC`.

Query Parameters:

| Nama | Tipe | Keterangan |
|---|---|---|
| `limit` | integer | Default 20, max 50 |
| `cursor` | string | Opsional |

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": [
    {
      "id": "c0ffe-1234-....",
      "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
      "user_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
      "parent_id": null,
      "content": "Nice video!",
      "created_at": "2025-01-01T12:05:00Z",
      "user": {
        "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
        "username": "alice",
        "display_name": "Alice Wonder",
        "avatar_url": "https://r2.example.com/avatars/alice.jpg",
        "is_private": false
      }
    }
  ],
  "pagination": {
    "next_cursor": null
  }
}
```

Error: `400`, `401`, `404`, `500`.

---

### `DELETE /api/comments/:id`

Menghapus comment. Hanya pemilik comment. Jika comment adalah parent, semua reply ikut terhapus oleh `ON DELETE CASCADE`.

Headers:

```
Authorization: Bearer <access_token>
```

Response `204 No Content`.

Error: `401`, `403`, `404`, `500`.

---

### `POST /api/comments/:id/reply`

Membalas comment. Parent comment harus ada dan videonya tetap bisa diakses.

Headers:

```
Authorization: Bearer <access_token>
Content-Type: application/json
```

Request body:

```json
{
  "content": "I agree!"
}
```

Response `201 Created`:

```json
{
  "data": {
    "id": "reply-1234-....",
    "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "user_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "parent_id": "c0ffe-1234-....",
    "content": "I agree!",
    "created_at": "2025-01-01T12:07:00Z",
    "user": {
      "id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
      "username": "alice",
      "display_name": "Alice Wonder",
      "avatar_url": "https://r2.example.com/avatars/alice.jpg",
      "is_private": false
    }
  }
}
```

Error:
- `400 VALIDATION_ERROR`
- `401`
- `404` — parent comment tidak ditemukan / video tidak bisa diakses.
- `409 VIDEO_NOT_READY`
- `500`

---

## 9. View Tracking

### `POST /api/videos/:id/view`

Increment `views_count`. Tidak ada deduplication per user; setiap panggilan dihitung.

Video harus berstatus `READY` dan boleh diakses requester.

Headers:

```
Authorization: Bearer <access_token>
```

Tidak ada request body.

Response `200 OK`:

```json
{
  "data": {
    "video_id": "d3f4c9e4-8b2a-4f5e-9c0d-abcdef123456",
    "views_count": 341
  }
}
```

Error:
- `401`
- `404` — video tidak ditemukan / tidak bisa diakses.
- `409 VIDEO_NOT_READY`
- `500`

---

## 10. Notifikasi

### `GET /api/notifications`

List notifikasi untuk user yang sedang login.

Query Parameters:

| Nama | Tipe | Keterangan |
|---|---|---|
| `limit` | integer | Default 20, max 50 |
| `cursor` | string | Opsional |

Headers:

```
Authorization: Bearer <access_token>
```

Response `200 OK`:

```json
{
  "data": [
    {
      "id": "n-1234-....",
      "user_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
      "actor_id": "9f7d3c86-....",
      "type": "follow",
      "payload": {
        "username": "bob"
      },
      "is_read": false,
      "created_at": "2025-01-01T12:08:00Z"
    }
  ],
  "pagination": {
    "next_cursor": null
  }
}
```

Error: `400`, `401`, `500`.

---

### `PUT /api/notifications/read-all`

Menandai semua notifikasi user yang sedang login sebagai `is_read = true`.

Headers:

```
Authorization: Bearer <access_token>
```

Tidak ada request body.

Response `200 OK`:

```json
{
  "data": {
    "updated_count": 5
  }
}
```

Error: `401`, `500`.

---

## 11. Webhook Zitadel (Actions V2 Target)

### `POST /api/webhooks/zitadel`

Dipanggil oleh Zitadel Actions V2 saat event `user.removed` atau `user.deactivated` terjadi pada instance (di-bind lewat `SetExecution`, lihat LLD Plan Fase 0).

Headers:

```
Content-Type: application/json
ZITADEL-Signature: t=<timestamp>,v1=<hmac_hex>
```

Request body (format bawaan Actions V2, bukan skema custom):

```json
{
  "aggregateID": "336494809936035843",
  "aggregateType": "user",
  "resourceOwner": "336392597046099971",
  "instanceID": "336392597046034435",
  "version": "v2",
  "sequence": 1,
  "event_type": "user.removed",
  "created_at": "2025-01-01T12:20:00Z",
  "userID": "336392597046755331",
  "event_payload": { }
}
```

Behavior:
- `event_type == "user.removed"` → tombstone user lokal (`userID`), nonaktifkan `is_active = false`, set `deleted_at`, hapus data relasional, enqueue cleanup R2.
- `event_type == "user.deactivated"` → set `is_active = false` untuk `userID`.
- `event_type` lain yang ke-trigger tak sengaja → `400 WEBHOOK_EVENT_UNSUPPORTED`.

Response `200 OK`:

```json
{
  "data": {
    "status": "processed",
    "event": "user.removed"
  }
}
```

Error:
- `400 WEBHOOK_EVENT_UNSUPPORTED`
- `401 WEBHOOK_INVALID_SIGNATURE`
- `429 RATE_LIMITED`
- `500`

---
