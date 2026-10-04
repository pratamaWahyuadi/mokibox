# Fase 2 Implementation Notes — MokiBox `shared/` package

Concrete design decisions dan code patterns dari fase 2 yang dipakai sebagai
referensi fase 3+ saat consume `shared/errors.go`, `shared/response.go`,
`shared/r2.go`, `shared/redis.go`, dan `shared/models.go`.

## Sentinel error table (final, dari `shared/errors.go`)

| Sentinel               | ErrorCode                  | HTTP |
|------------------------|----------------------------|------|
| `ErrValidation`        | `VALIDATION_ERROR`         | 400  |
| `ErrUnauthorized`      | `UNAUTHORIZED`             | 401  |
| `ErrForbidden`         | `FORBIDDEN`                | 403  |
| `ErrNotFound`          | `NOT_FOUND`                | 404  |
| `ErrVideoStatusConflict` | `VIDEO_STATUS_CONFLICT` | 409  |
| `ErrVideoNotReady`     | `VIDEO_NOT_READY`          | 409  |
| `ErrUploadMissing`     | `UPLOAD_MISSING`           | 409  |
| `ErrUploadSizeInvalid` | `UPLOAD_SIZE_INVALID`      | 400  |
| `ErrSelfFollow`        | `SELF_FOLLOW_NOT_ALLOWED`  | 400  |
| `ErrWebhookSignature`  | `WEBHOOK_INVALID_SIGNATURE`| 401  |
| `ErrWebhookEvent`      | `WEBHOOK_EVENT_UNSUPPORTED`| 400  |
| `ErrInternal`          | `INTERNAL_ERROR`           | 500  |
| `ErrRateLimited`       | `RATE_LIMITED`             | 429  |

`ErrRateLimited` ditambah walau tidak di-list issue phase 2 — untuk forward
compatibility dengan phase 9 rate-limit middleware. Dokumentasi ada di
`## Deviations` section PR #34.

## `classifyError` pattern (`shared/response.go`)

```go
func classifyError(err error) (int, ErrorCode, string, []FieldError) {
    if err == nil {
        return 500, CodeInternalError, "internal server error", nil
    }
    var api *APIError
    if errors.As(err, &api) {
        code := api.Code
        if code == "" { code = codeFor(err) }
        status := api.Status
        if status == 0 { status = httpStatusFor(err) }
        msg := api.Message
        if msg == "" { msg = "request failed" }
        return status, code, msg, api.Details
    }
    return httpStatusFor(err), codeFor(err), err.Error(), nil
}
```

Penting: `httpStatusFor` dan `codeFor` di `errors.go` adalah **satu-satunya**
sumber mapping error → HTTP. Handler tidak boleh pilih status sendiri.

## Asynq marshalTask (`shared/redis.go`)

```go
func marshalTask(typename string, payload any) (*asynq.Task, error) {
    body, err := json.Marshal(payload)
    if err != nil {
        return nil, fmt.Errorf("marshal task %s: %w", typename, err)
    }
    return asynq.NewTask(typename, body), nil
}
```

`asynq.NewTask` di v0.26.0 return `*Task` (bukan `(*Task, error)`) dan payload
harus `[]byte`. Pattern ini dipakai 3× (transcode/cleanup-objects/cleanup-video).

## R2 error mapping (`shared/r2.go`)

```go
func mapR2Error(op, key string, err error) error {
    if err == nil { return nil }
    var apiErr smithy.APIError
    if errors.As(err, &apiErr) {
        if isNotFoundErr(apiErr.ErrorCode()) {
            return fmt.Errorf("%s: %s: %w", op, key, ErrNotFound)
        }
        return fmt.Errorf("%s: %s: code=%s: %w", op, key, apiErr.ErrorCode(), err)
    }
    return fmt.Errorf("%s: %s: %w", op, key, err)
}

func isNotFoundErr(code string) bool {
    switch code {
    case "NoSuchKey", "NotFound", "404":
        return true
    }
    return false
}
```

`HeadObject` adalah satu-satunya path yang surface `ErrNotFound` (untuk 409
UPLOAD_MISSING). `DeleteObjects` skip `NoSuchKey` di per-key error (idempotent
cleanup). `Download` pakai wrapped generic error karena worker retry on
transient — only `/confirm` cares about NotFound specifically.

## Cursor wire format (`shared/cursor.go`)

```
base64url( <RFC3339Nano> | <uuid> )
```

Contoh: `2025-01-01T12:00:00.123456789Z|3f7d3c86-a1b2-4e5f-9c0d-1234567890ab`
→ base64url encode.

SQL pattern (sesuai PRD note A9 + API contract):
```sql
WHERE (created_at, id) < ($1, $2)
ORDER BY created_at DESC, id DESC
LIMIT $3
```

Empty cursor string (first page) → return zero time + nil UUID, tidak error.

## Media token wire format (`shared/mediatoken.go`)

```
base64url( <expiry_unix> . <hex-hmac-sha256> )
```

HMAC computed over `video_id:<id>:<expiry_unix>`. Verifikasi pakai
`hmac.Equal` (constant-time). Semua failure (format, expiry, signature) collapse
ke `ErrMediaTokenInvalid` — tidak ada oracle leak.

`NewMediaToken` panggil `time.Now().Add(ttl)` — `SignMediaToken` expose
deterministic version untuk testing tanpa mock clock.

## Response helpers signature (untuk handler phase 3+)

```go
shared.RespondOK(c, data)        // 200 + {data: ...}
shared.RespondCreated(c, data)   // 201 + {data: ...}
shared.RespondList(c, items, *string) // 200 + {data, pagination: {next_cursor}}
shared.RespondNoContent(c)       // 204
shared.RespondError(c, err)      // error envelope, status dari classifyError
```

`RespondList` ambil `*string` (pointer) untuk nextCursor — pass `nil` kalau
last page supaya JSON field jadi `null`, bukan `""`.

## Constructor signature patterns

Semua constructor di `shared/` return concrete struct, ambil config kecil:

```go
NewSQLDB(ctx, dsn, maxConns, maxIdle) (*sql.DB, error)        // post-PR-41
NewR2Client(ctx, R2Config) (*R2Client, error)
NewAsynqClient(RedisConfig) (*asynq.Client, error)
NewAsynqServer(RedisConfig, concurrency int) (*asynq.Server, error)
```

> Pre-PR-41 (`refactor/pool-consolidation`): constructor lama
> `NewDB(ctx, databaseURL, maxConns) (*pgxpool.Pool, error)` sudah
> dihapus. Single `*sql.DB` pool adalah satu-satunya opsi.

Service/repo yang butuh mock declare interface kecil di package mereka
sendiri, bukan di `shared/`. Contoh (akan dibuat di fase 3+):

```go
// di api-gateway/handlers/user.go
type UserService interface {
    GetProfile(ctx, id) (*db.User, error)
}
type UserRepository interface {
    GetByID(ctx, id) (*db.User, error)
}
```

## R2 method recap (6 + 1)

- `PresignPut(ctx, key, contentType, expiry) (string, error)` — untuk
  /api/videos/upload-intent
- `PresignGet(ctx, key, expiry) (string, error)` — untuk thumbnail + .ts
  segments di playlist rewrite (phase 6)
- `HeadObject(ctx, key) (int64, error)` — untuk /confirm size validation
- `DeleteObjects(ctx, keys []string) error` — untuk cleanup tasks (phase 5+)
- `Download(ctx, key, destPath) error` — untuk worker download raw (phase 5)
- `UploadFile(ctx, key, filePath, contentType) error` — untuk worker upload
  HLS + thumbnail (phase 5)
- `Bucket() string` — small accessor untuk test + cleanup worker key building

## Asynq task types & payloads (`shared/models.go`)

```go
const (
    TypeTranscodeVideo = "transcode:video"
    TypeCleanupObjects = "cleanup:objects"
    TypeCleanupVideo   = "cleanup:video"
)

type TranscodeVideoPayload struct {
    VideoID string `json:"video_id"`
}
type CleanupObjectsPayload struct {
    Keys []string `json:"keys"`
}
type CleanupVideoPayload struct {
    VideoID string `json:"video_id"`
}
```

Producer helper di `shared/redis.go`: `EnqueueTranscode`, `EnqueueCleanupObjects`,
`EnqueueCleanupVideo`, `EnqueueWithDelay`. Worker register handler di `mux`
yang di-pass ke `asynq.NewServer` (phase 5 main.go).
