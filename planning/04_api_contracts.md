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
   User ID terdampak diambil dari field `aggregateID` (user yang terdampak
  event; `userID` hanya fallback bila `aggregateID` kosong — `userID`
  biasanya adalah AKTOR/admin).
  `event_type` yang relevan: `user.removed` (user dihapus) dan `user.deactivated` (user dinonaktifkan) — nilai ini konstanta resmi Zitadel, bukan skema custom.

   Signature diletakkan di header `ZITADEL-Signature`, formatnya `t=<timestamp>,v1=<hmac_hex>`. HMAC dihitung dari `timestamp + "." + raw_body` menggunakan `signingKey` yang didapat sekali dari response `CreateTarget` (disimpan sebagai `ZITADEL_TARGET_SIGNING_KEY`), **bukan** secret statis yang dibuat sendiri. Verifikasi memakai helper resmi `actions.ValidateRequestPayload` dari `github.com/zitadel/zitadel-go/v3/pkg/actions`.

2. **`DELETE /api/users/me`** ditambahkan untuk memenuhi US-09 / FR-USER-03, karena PRD tidak mencantumkan endpoint eksplisit untuk user menghapus akunnya. Endpoint ini hanya menghapus data lokal; penghapusan akun di Zitadel tetap dilakukan via UI Zitadel atau webhook.

3. **HLS private** membutuhkan endpoint tambahan `GET /api/videos/:id/playlist.m3u8`. `hls_playlist_url` pada video detail akan menunjuk ke endpoint ini, bukan URL statis R2. Endpoint ini me-rewrite playlist agar berisi presigned URL untuk tiap segmen `.ts`.

4. **Avatar privat di R2:** `avatar_url` response adalah presigned GET URL untuk `avatar_key`; avatar tetap dapat dilihat siapa pun yang memiliki URL, termasuk dari akun privat.

5. **Username dianggap immutable** melalui API. `PUT /api/users/me` mengubah `display_name`, `bio`, `avatar_url` eksternal (kompatibilitas), dan `is_private`; bila `avatar_url` diberikan, avatar R2 aktif dan intent upload tertunda dibersihkan.

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

## Direct Messages — E2EE v1

Semua endpoint berada di `/api/dm`, memerlukan JWT Zitadel dan hanya
diregistrasikan saat `DM_E2EE_ENABLED=true`. API tidak pernah menerima
plaintext atau private key. Semua binary key/ciphertext dikirim sebagai
base64. User/device authorization tidak bergantung pada UUID rahasia.
Rate limit dibagi dua bucket per user: kontrol (device, key upload, bundle)
20 burst 1/3 detik, dan pengiriman (send/poll/ack) 120 burst 10/detik.
Keduanya memakai group auth tersendiri, sehingga send/poll/ack tidak kena
limiter auth generik 60/menit.

### `POST /api/dm/device`

Register atau re-register caller's DM device.

```json
{
  "device_id": "uuid",
  "identity_curve25519": "base64-public-key",
  "identity_ed25519": "base64-public-key"
}
```

Response `200`:

```json
{
  "data": {
    "device_id": "uuid",
    "replaced_device": false
  }
}
```

Registration dengan device ID dan public keys yang sama bersifat idempotent.
Mengganti device ID atau identity key membatalkan device lama, one-time keys,
dan inbox-nya; kontak akan melihat fingerprint baru dan wajib memverifikasinya
lagi.

### `POST /api/dm/device/one-time-keys`

Upload batch maksimum 100 signed Olm one-time keys. Device harus device aktif
milik caller. Field `fallback_key` opsional dan formatnya sama seperti satu
one-time key; server menyimpan satu fallback key per device untuk dipakai saat
one-time key habis.

```json
{
  "device_id": "uuid",
  "keys": [
    {
      "key_id": "key-id",
      "public_key": "base64-public-key",
      "signature": "base64-ed25519-signature"
    }
  ],
  "fallback_key": {
    "key_id": "fallback-key-id",
    "public_key": "base64-public-key",
    "signature": "base64-ed25519-signature"
  }
}
```

Response `200`: `{ "data": { "accepted": 1 } }` (`accepted` hanya menghitung
one-time key). Server menyimpan signatures secara opaque; peer client wajib
memverifikasinya dengan identity key.

### `GET /api/dm/users/:id/key-bundle?device_id=uuid&include_one_time_key=true`

Caller harus memiliki device aktif. Target harus aktif dan mutual-follow.
Response `200` berisi target `user_id`, `device_id`, kedua identity public
keys, dan default-nya satu one-time key yang diklaim secara atomik. Jika tidak
ada one-time key, server mengembalikan `fallback_key` yang tersimpan (tidak
dikonsumsi per sesi). Dengan `include_one_time_key=false`, response hanya berisi
identity keys dan `one_time_key: null`; client memakai opsi ini untuk
fingerprint checking agar tidak membakar key. Jika key diminta tetapi neither
one-time nor fallback key tersedia, response `409 DM_KEY_UNAVAILABLE`. Target
yang tidak ditemukan atau tidak memenuhi mutual-follow sama-sama
`404 NOT_FOUND`.

### `POST /api/dm/messages`

Mengirim satu Olm envelope opaque ke target device yang termasuk dalam key bundle yang sudah
diambil. Caller harus memiliki device aktif dan kedua akun harus mutual-follow.
Jika target sudah mengganti device, pengiriman gagal dengan
`409 DM_DEVICE_STALE`; client harus mengambil bundle dan memverifikasi ulang.

```json
{
  "device_id": "uuid",
  "recipient_user_id": "uuid",
  "recipient_device_id": "uuid",
  "client_message_id": "uuid",
  "protocol": "olm",
  "protocol_version": 1,
  "message_type": 0,
  "ciphertext": "base64"
}
```

`message_type` mengikuti binding Olm; server tidak menafsirkan ciphertext.
Digest idempotensi memakai encoding canonical dari seluruh request body.
Retry dengan ID dan digest sama mengembalikan ID/sequence pesan awal; ID sama
dengan digest berbeda mengembalikan `409 DM_IDEMPOTENCY_CONFLICT`. Maksimum
1,000 pesan pending per recipient device; jika penuh `409 DM_INBOX_FULL`.
Response pertama `201` berisi `message_id`, `sequence`, dan `expires_at`.
Retry yang cocok mengembalikan body yang sama dengan status `200`.

### `GET /api/dm/inbox?device_id=uuid&after=0&limit=100`

`after` wajib berupa integer non-negatif; `limit` default 100, maksimum 100.
Hanya pesan pending untuk device aktif caller dengan `sequence > after` yang
dikembalikan, urut sequence menaik. Response berisi IDs/routing metadata,
Olm envelope, ciphertext, dan timestamps. Poll tidak memperpanjang TTL dan
tidak acknowledge pesan. Client memajukan cursor setelah ack sukses. Pesan
dengan envelope/ciphertext rusak, decrypt gagal, session hilang, atau
fingerprint berubah dikarantina lokal (tidak di-ack, tidak menghentikan
sequence berikutnya); cursor tetap maju melewati pesan tersebut.

### `POST /api/dm/inbox/:message_id/ack`

```json
{ "device_id": "uuid" }
```

Acknowledge hanya pesan yang dialamatkan ke device aktif caller. Pengulangan
ack berhasil dengan `204`; ciphertext dihapus dari penyimpanan inbox.

### Error contract

| HTTP | Code | Kondisi |
|---|---|---|
| 400 | `VALIDATION_ERROR` | Body/cursor/encoding tidak valid |
| 401 | `UNAUTHORIZED` | JWT hilang, invalid, atau user nonaktif |
| 404 | `NOT_FOUND` | Target tak ada atau mutual-follow tidak terpenuhi |
| 409 | `DM_DEVICE_STALE` | Device ID tidak aktif untuk caller atau dimiliki user lain |
| 409 | `DM_KEY_UNAVAILABLE` | Tidak ada one-time atau fallback key untuk target |
| 409 | `DM_IDEMPOTENCY_CONFLICT` | ID client dipakai ulang dengan body berbeda |
| 409 | `DM_INBOX_FULL` | Recipient memiliki 1,000 pesan pending |
| 429 | `RATE_LIMITED` | Per-user DM control (20 burst) atau delivery (120 burst) limiter menolak request |
| 500 | `INTERNAL_ERROR` | Kegagalan DB/internal; detail hanya di log |

Pengiriman diperiksa mutual-follow pada saat send. Jika follow relation berubah
setelah enqueue, pesan yang sudah berada di inbox tetap dapat diambil sampai
ack/expiry. Ini konsekuensi kontrak v1.

`GET /api/dm/users/:id/key-bundle` dengan `include_one_time_key=true` mengonsumsi
satu one-time key target. Klien harus memakai `include_one_time_key=false`
saat hanya ingin memeriksa fingerprint; rate limit membatasi, tetapi tidak
menghilangkan, kemungkinan pengurasan key oleh mutual follower.

FK `fk_dm_inbox_sender_user` memakai `ON DELETE RESTRICT`. Deploy saat ini
hanya tombstone user, sehingga aman. Alur hard-delete di masa depan harus
menghapus baris inbox DM terlebih dahulu.

## 1. Daftar Endpoint

| Method | Path | Deskripsi | Auth |
|---|---|---|---|
| GET | `/api/users/me` | Profil sendiri | JWT |
| PUT | `/api/users/me` | Update profil sendiri | JWT |
| POST | `/api/users/me/avatar/upload-intent` | Minta presigned URL foto profil | JWT |
| POST | `/api/users/me/avatar/confirm` | Validasi dan aktifkan foto profil | JWT |
| DELETE | `/api/users/me` | Hapus akun sendiri + data lokal | JWT |
| GET | `/api/users/:id` | Profil user lain | JWT |
| GET | `/api/users/:id/videos` | List video milik user | JWT |
| POST | `/api/users/:id/follow` | Follow user | JWT |
| DELETE | `/api/users/:id/follow` | Unfollow user | JWT |
| GET | `/api/users/:id/followers` | List followers | JWT |
| GET | `/api/users/:id/following` | List following | JWT |
| GET | `/api/feed/home` | Home feed | JWT opsional (anonim = timeline publik) |
| GET | `/api/search` | Search users + videos | JWT opsional (anonim = hasil publik) |
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
| POST | `/api/dm/device` | Register/re-register DM device | JWT + feature flag |
| POST | `/api/dm/device/one-time-keys` | Upload signed Olm one-time keys | JWT + feature flag |
| GET | `/api/dm/users/:id/key-bundle` | Claim peer key bundle | JWT + feature flag |
| POST | `/api/dm/messages` | Kirim Olm ciphertext | JWT + feature flag |
| GET | `/api/dm/inbox` | Poll inbox ciphertext | JWT + feature flag |
| POST | `/api/dm/inbox/:message_id/ack` | Acknowledge dan hapus ciphertext | JWT + feature flag |
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
`avatar_url` hanya untuk URL eksternal lama. Untuk foto yang diunggah ke MokiBox gunakan endpoint upload-intent/confirm. Jika URL eksternal yang berbeda diberikan, referensi avatar R2 aktif dan upload tertunda dibersihkan. Jika `avatar_url` berisi presigned URL avatar R2 milik user yang sama (misalnya klien mengirim balik nilai dari GET), backend memperlakukannya sebagai nilai tidak berubah dan mempertahankan avatar. Klien sebaiknya tidak mengirim `avatar_url` kecuali memang mengubah URL eksternal legacy.

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

### `POST /api/users/me/avatar/upload-intent`

Minta presigned PUT URL untuk mengunggah foto profil langsung ke R2 privat.

Headers: `Authorization: Bearer <access_token>`, `Content-Type: application/json`.

Request body:

```json
{
  "content_type": "image/png"
}
```

`content_type` hanya boleh `image/jpeg`, `image/png`, atau `image/webp`.

Response `201 Created`:

```json
{
  "data": {
    "avatar_key": "avatars/<userID>/<uploadUUID>.png",
    "http_method": "PUT",
    "upload_url": "https://r2.example.com/...",
    "upload_headers": {
      "Content-Type": "image/png"
    },
    "max_size_bytes": 5242880,
    "expires_at": "2025-01-01T10:15:00Z"
  }
}
```

Client wajib mengirim PUT dengan header `Content-Type` yang sama. URL berlaku sesuai `PRESIGN_UPLOAD_EXPIRY` (default 15 menit). Batas 5 MiB diverifikasi saat confirm; presigned PUT tidak membatasi ukuran di sisi R2. R2 bucket CORS harus mengizinkan metode `PUT`, header `Content-Type`, dan origin frontend.

Error: `400 VALIDATION_ERROR`, `401`, `404`, `500`.

---

### `POST /api/users/me/avatar/confirm`

Memvalidasi dan mengaktifkan foto yang sudah di-upload.

Request body:

```json
{
  "avatar_key": "avatars/<userID>/<uploadUUID>.png"
}
```

Backend memastikan key cocok dengan intent user yang masih berlaku, object ada di R2, ukuran aktual > 0 dan ≤ 5 MiB, serta format hasil decode cocok dengan JPEG/PNG/WebP yang diminta. Key aktif disimpan pada `users.avatar_key`; response memberi URL GET bertanda tangan:

```json
{
  "data": {
    "avatar_url": "https://r2.example.com/<presigned-get>"
  }
}
```

Avatar bersifat publik, termasuk pada akun privat. URL presigned dapat diakses siapa pun yang memilikinya hingga kedaluwarsa. Foto lama dan upload yang ditolak dijadwalkan untuk cleanup.

Error: `400 VALIDATION_ERROR`, `400 UPLOAD_SIZE_INVALID`, `401`, `409 UPLOAD_MISSING`, `409 AVATAR_UPLOAD_CONFLICT`, `500`.

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
      "thumbnail_url": null,
      "hls_playlist_url": null,
      "liked_by_me": null,
      "is_owner": false
    }
  ],
  "pagination": {
    "next_cursor": null
  }
}
```

Catatan: pada list ini `thumbnail_url`, `hls_playlist_url`, dan `liked_by_me` selalu `null` (pointer tidak pernah diisi di handler), dan TIDAK ada objek `user` embedded (beda dengan detail video / feed).

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

`title` WAJIB (1–200 karakter setelah trim; whitespace-only ditolak dengan `400 VALIDATION_ERROR`), `description` opsional (maks 5000).

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

Query parameter opsional: `?variant=480p|720p` untuk variant playlist (tanpa parameter → master playlist; nilai lain → `400 VALIDATION_ERROR`).

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

Error: `401`, `404` (bukan owner / video tidak ditemukan — anti-enumerasi, bukan 403), `500`.

---

### `DELETE /api/videos/:id`

Menghapus video. Hanya pemilik video.

Status video diubah menjadi `DELETED` dan `deleted_at` diisi. Cleanup R2 berjalan maksimal 1×24 jam.

Headers:

```
Authorization: Bearer <access_token>
```

Response `204 No Content`.

Error: `401`, `404` (bukan owner / tidak ditemukan — anti-enumerasi, bukan 403), `500`.

---

## 5. Feed

### `GET /api/feed/home`

**Auth: OPSIONAL (Fitur A, 2026-11-26).** Endpoint berada di LUAR grup auth `/api` dan memakai `middleware.AuthenticateOptional`:

- **Tanpa header `Authorization`** → anonim: public timeline berisi video `READY` dari akun aktif **non-privat** saja. Semua `liked_by_me=false`, `is_owner=false`. Akun privat TIDAK pernah muncul untuk anonim.
- **Token valid** → feed personal (perilaku lama TIDAK berubah): video `READY` dari user yang di-follow requester + user publik; video milik requester sendiri di-exclude.
- **Header ADA tapi token tidak valid/kedaluwarsa** → `401` fail-closed (tidak pernah turun ke anonim).

Respons memakai header `Vary: Authorization` (ditambahkan, bukan menimpa `Vary: Origin` dari nginx). Endpoint memakai limiter gabungan per-user/per-IP (lihat SEC-09): `FEED_RATE_LIMIT_PER_MIN` (default 120/menit, burst anonim 40).

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

Response `200 OK` (`created_at` kosong `""` — tidak ada row untuk diambil timestamp-nya):

```json
{
  "data": {
    "follower_id": "3f7d3c86-a1b2-4e5f-9c0d-1234567890ab",
    "followee_id": "9f7d3c86-....",
    "is_following": false,
    "created_at": ""
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
- `404` — video tidak ditemukan / tidak bisa diakses. (Catatan: video non-READY untuk non-owner juga 404 via anti-enumerasi; kode tidak lagi mengembalikan `409 VIDEO_NOT_READY`.)
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
- `404` — video tidak ditemukan / tidak bisa diakses (video non-READY juga 404, anti-enumerasi).
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

Error: `401`, `404` (bukan owner / tidak ditemukan — anti-enumerasi, bukan 403), `500`.

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
- `404` — parent comment tidak ditemukan / video tidak bisa diakses (video non-READY juga 404, anti-enumerasi).
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
- `404` — video tidak ditemukan / tidak bisa diakses (video non-READY juga 404, anti-enumerasi).
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
- `event_type == "user.removed"` → tombstone user lokal (`aggregateID`, fallback `userID`), nonaktifkan `is_active = false`, set `deleted_at`, hapus data relasional, enqueue cleanup R2.
- `event_type == "user.deactivated"` → set `is_active = false` untuk user terdampak.
- `event_type` lain yang ke-trigger tak sengaja → `400 WEBHOOK_EVENT_UNSUPPORTED`.

Verifikasi signature: HMAC-SHA256 atas string `"<unix-seconds>.<raw_body>"`, header `ZITADEL-Signature: t=<unix-seconds>,v1=<hex>`, toleransi timestamp 300 detik.

Response `200 OK` (tanpa field `event`; `sequence` bertipe number):

```json
{
  "data": {
    "status": "processed"
  }
}
```

Error:
- `400 WEBHOOK_EVENT_UNSUPPORTED`
- `401 WEBHOOK_INVALID_SIGNATURE`
- `429 RATE_LIMITED`
- `500`

---

---

## 99. Utilitas & Dokumentasi

### `GET /healthz`

Health check gateway. Tanpa auth, di luar grup `/api`.

Response `200 OK`: `{"data":{"status":"ok"}}` (tanpa cek dependensi — verifier-retry safe).

Error: `500` (gateway internal).

---

### `GET /api/search`

**Auth: OPSIONAL (Fitur A, 2026-11-26).** Endpoint berada di LUAR grup auth `/api` dan memakai `middleware.AuthenticateOptional` (semantik sama dengan `/api/feed/home`: tanpa header = anonim, token valid = personal, token rusak = `401` fail-closed). Query Parameters:

| Nama | Tipe | Keterangan |
|---|---|---|
| `q` | string | Wajib, 1–100 karakter (rune count) |
| `type` | string | `all` (default) / `users` / `videos` |
| `limit` | integer | Default 20, max 50 |
| `cursor` | string | Opsional, offset-based (`offset:<n>` base64) |

Rules:
- Hanya akun aktif & video `READY` + `deleted_at IS NULL`.
- **Anonim**: hanya video dari akun publik; `liked_by_me=false`, `is_owner=false`; offset cursor dibatasi **maks 1000** (anti-scraping).
- **Viewer login**: video privat hanya muncul untuk owner; akun privat yang di-follow ikut muncul; offset maks 100000 (perilaku lama).
- Pencarian user: akun privat tetap muncul sebagai `UserSummary` (profil dasar publik, Q1) — video tetap dijaga aturan visibilitas.
- Exact-match username (case-insensitive) diberi ranking lebih tinggi.
- `%`/`_` di-escape (bukan wildcard).

Limiter gabungan per-user/per-IP (SEC-09): `SEARCH_RATE_LIMIT_PER_MIN` (default 60/menit, burst anonim 15).

Error: `400 VALIDATION_ERROR`, `401`, `429 RATE_LIMITED`, `500`.

---

### `GET /api/docs` (dan `/api/docs/swagger.json`, `/api/docs/oauth2-redirect.html`)

Swagger UI + spec + OAuth2 redirect helper. Tanpa auth (di-root Echo, luar grup `/api`), di-serve gateway via `go:embed` — perubahan docs butuh rebuild image.

Response `200 OK`: HTML / JSON / JS statis.
