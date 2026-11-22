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

## Direct-message backend relay: client wire contract

This is the backend wire contract for the proposed v1 relay, not approval of a
cryptographic protocol or client library. PR #77 stores and forwards opaque
bytes only; it does not implement E2EE. Every route is authenticated and is
registered only when `DM_E2EE_ENABLED=true`. The flag defaults off. When
enabled, `DM_INBOX_TTL` is required; `.env.example` currently documents
`168h` (7 days), the selected v1 ciphertext-retention value.

All payloads use the standard `{ "data": ... }` success envelope and standard
error envelope. JSON `[]byte` fields use standard padded Base64. A device
registers a protocol token and positive integer version; that pair identifies
its opaque key bundle and one-time/fallback key material. Protocol identifiers
are not allow-listed. Key bundles are 1-65,536 bytes; individual one-time and
fallback key material is 1-4,096 bytes. Maximum one-time-key upload is 100
keys per request and 1,000 stored keys per device. Ciphertext is 1-65,536
bytes. These are encoding and resource limits only: the backend does not parse
protocol-specific structures, validate cryptographic keys, or decrypt.

Eligibility requires a follow row in both directions. A missing recipient and
an ineligible recipient produce the same `404 NOT_FOUND` response. The
mutual-follow check is performed when a message is sent; already queued
messages remain available until acknowledged or expired if the users later
unfollow. The current backend has no block mechanism; if one exists before
merge, it must be included in eligibility checks. The existing per-user
limiter is 60 requests per minute with burst 60. DM key-bundle fetches have a
separate 30/minute, burst-10 limiter; sends have a separate 60/minute,
burst-20 limiter. All limiters are per-process and suitable only for one
gateway instance; multiple gateways do not share counters.

Each recipient device may have at most 1,000 unacknowledged, unexpired inbox
messages. A full inbox rejects a new send with `429 DM_INBOX_FULL`. At the
maximum 64 KiB payload size, the payload-only upper bound is about 62.5 MiB
per device, excluding database overhead.

### `POST /api/dm/device`

Register or replace the authenticated user's active device. Replacement
revokes the prior device, its opaque key material, and its queued recipient
inbox. Private keys are never sent. Registration/replacement currently relies
on the authenticated JWT alone: a stolen valid session can replace the device.
Client-side key-change warnings are required; step-up authentication is not
implemented in this backend.

Request:

```json
{
  "protocol": "<protocol-token>",
  "version": 1,
  "key_bundle": "<base64 opaque public key bundle>",
  "fallback_key_id": "fallback-1",
  "fallback_key_material": "<base64 opaque key bytes>",
  "one_time_keys": [
    {"key_id": "otk-1", "key_material": "<base64 opaque key bytes>"}
  ]
}
```

`fallback_key_id` and `fallback_key_material` must be supplied together. The
`one_time_keys` array may be empty; duplicate IDs within the batch are invalid.
All key material is opaque and uses the device's registered protocol/version.
Success is `201 Created`:

```json
{"data":{"device_id":"<uuid>","protocol":"<protocol-token>","version":1,"registered_at":"<RFC3339 UTC>"}}
```

### `POST /api/dm/device/keys`

Add opaque one-time key material to the current active device:

```json
{
  "device_id": "<active device uuid>",
  "one_time_keys": [
    {"key_id": "otk-2", "key_material": "<base64 opaque key bytes>"}
  ]
}
```

Success is `200 OK` with `{"data":{"device_id":"<uuid>","uploaded":1}}`.
`device_id` must match the authenticated user's active device. A valid but
revoked/replaced ID returns `409 DM_DEVICE_STALE`. Keys inherit the
protocol/version of the registered device. Invalid encoding, sizes, duplicate
IDs, and exceeded count limits return `400 VALIDATION_ERROR`.

### `GET /api/dm/users/:id/key-bundle?device_id=<uuid>`

The authenticated sender must supply its active `device_id`. A missing or
malformed ID returns `400 VALIDATION_ERROR`; an ID that is no longer active
returns `409 DM_DEVICE_STALE`. Fetching a bundle claims and removes one
recipient one-time key atomically. A fetch consumes that key even if the
response is lost, so clients must persist the resulting session and avoid
unnecessary retries. When no one-time keys remain, the optional fallback key
is returned instead. If neither exists, the response is `409 DM_KEYS_EXHAUSTED`.

Success:

```json
{
  "data": {
    "user_id": "<uuid>",
    "device_id": "<uuid>",
    "protocol": "<protocol-token>",
    "version": 1,
    "key_bundle": "<base64 opaque bytes>",
    "one_time_key": {"key_id":"otk-1","key_material":"<base64 opaque bytes>"}
  }
}
```

`one_time_key` and `fallback_key` are mutually exclusive and omitted when
absent. Missing and ineligible recipients both return `404 NOT_FOUND`. Parallel
fetches claim distinct one-time keys; fallback material may be returned after
the one-time-key pool is exhausted.

### `POST /api/dm/messages`

Send an opaque envelope tagged with its protocol and version. The sender user
is derived from authentication. Both device IDs are required and checked
against the active sender and eligible recipient device; stale IDs return
`409 DM_DEVICE_STALE`. The protocol/version must match both registered devices;
the backend does not allow-list a protocol or inspect its payload.

Request:

```json
{
  "sender_device_id": "<active sender device uuid>",
  "recipient_user_id": "<uuid>",
  "recipient_device_id": "<active recipient device uuid from the key bundle>",
  "protocol": "<protocol-token>",
  "version": 1,
  "client_message_id": "<uuid>",
  "ciphertext": "<base64 opaque bytes>"
}
```

The client message ID is unique per sender device. Repeating an identical
request returns the original message ID and sequence, including after the
inbox ciphertext has been acknowledged and deleted. The server retains a
SHA-256 ciphertext digest and routing/response metadata until the original
message expires; it does not retain ciphertext in this idempotency record.
After the janitor deletes the expired record, the ID may be treated as a new
send. Reusing the ID with a different recipient, recipient device, protocol,
version, or ciphertext returns `409 DM_CONFLICT`.
The 1,000 pending-inbox cap does not cap retained idempotency records; their
count is bounded by the send rate and the selected 168h TTL.
An envelope whose protocol/version does not match the registered devices
returns `409 DM_PROTOCOL_MISMATCH`.
If the recipient's active device changed after its bundle was fetched, the
send returns `409 DM_DEVICE_STALE` rather than queueing ciphertext for a key
the active client no longer has.
Success is `201 Created`:

```json
{
  "data": {
    "message_id": "<uuid>",
    "sequence": 1,
    "client_message_id": "<uuid>",
    "created_at": "<RFC3339 UTC>",
    "expires_at": "<RFC3339 UTC>"
  }
}
```

Unknown and ineligible recipients both return `404 NOT_FOUND`. Invalid
protocol-token encoding, non-positive versions, and invalid sizes return
`400 VALIDATION_ERROR`.

### `GET /api/dm/inbox?device_id=<uuid>&after=<sequence>&limit=<n>`

Poll the authenticated active device's inbox in ascending, per-device
sequence order. `device_id` and `after` are required; missing/malformed values
return `400 VALIDATION_ERROR`, while a valid but stale device ID returns
`409 DM_DEVICE_STALE`. `after` is a non-negative integer; `limit` is optional
(default 50, maximum 100). Sequence numbers may have gaps when earlier entries
were acknowledged or expired. Expired ciphertext is never returned; a
background janitor deletes expired rows. Polling does not acknowledge or delete
messages.

Success:

```json
{
  "data": {
    "messages": [
      {
        "message_id": "<uuid>",
        "sequence": 1,
        "sender_user_id": "<uuid>",
        "sender_device_id": "<uuid>",
        "protocol": "<protocol-token>",
        "version": 1,
        "ciphertext": "<base64 opaque bytes>",
        "client_message_id": "<uuid>",
        "created_at": "<RFC3339 UTC>",
        "expires_at": "<RFC3339 UTC>"
      }
    ],
    "next_after": 1
  }
}
```

After successfully handling and acknowledging every message in a response, the
client may persist `next_after` as its cursor. Gaps are normal; the client
should use the returned sequence values and `next_after`, not assume a
contiguous range. If handling any message fails, keep the prior cursor and
retry; acknowledged messages will no longer be returned, while unacknowledged
messages remain available until expiry.

### `POST /api/dm/inbox/:id/ack`

Request body:

```json
{"device_id":"<active device uuid>"}
```

The supplied device must match the authenticated user's active device;
otherwise return `409 DM_DEVICE_STALE`. Delete the identified message only if
it belongs to that device. Success is always `204 No Content`, including
repeated acknowledgements and unknown message IDs. The acknowledgement means
delivered/handled by the client, not read by a person.

### Device-related and capacity errors

| HTTP | Code | Meaning |
|---|---|---|
| 409 | `DM_DEVICE_STALE` | Supplied device ID is not the authenticated user's active device, or the recipient device changed after bundle fetch |
| 429 | `DM_INBOX_FULL` | Recipient has reached 1,000 pending, unexpired messages |

### Client and deployment contract

- Clients must not send plaintext or private keys. The backend treats
  `ciphertext` as opaque bytes and cannot determine whether a caller mislabeled
  plaintext as ciphertext.
- The client owns encryption/decryption, private-key storage, session state,
  and identity verification. None is implemented by this PR.
- The client must persist a newly established session after fetching a
  key-bundle response; that request permanently claims its one-time key.
- The client should persist each `client_message_id` and its send result. The
  server retains only a digest and routing/response metadata for idempotent
  retries after acknowledgement, until the original message TTL is cleaned.
- Deployed migration `009_dm_e2ee_relay.sql` before deploying a gateway build
  with these routes. Keep the feature flag false until the Flutter client and
  independent security review are complete.
- The relay smoke test uses random opaque bytes and verifies HTTP/storage
  behavior only; it is not a cryptography test.

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
| POST | `/api/dm/device` | Register/replace device (feature flag) | JWT |
| POST | `/api/dm/device/keys` | Upload opaque one-time key material (feature flag) | JWT |
| GET | `/api/dm/users/:id/key-bundle` | Claim eligible recipient bundle (feature flag) | JWT |
| POST | `/api/dm/messages` | Send opaque ciphertext (feature flag) | JWT |
| GET | `/api/dm/inbox` | Poll per-device ciphertext inbox (feature flag) | JWT |
| POST | `/api/dm/inbox/:id/ack` | Acknowledge and delete inbox entry (feature flag) | JWT |
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
