---
name: mokibox-go-shared
description: "MokiBox Go shared patterns. Sentinel, DI, pitfalls."
---

# MokiBox Go Shared Package Patterns

Pola dan pitfall Go yang muncul spesifik di MokiBox
(`github.com/pratamaWahyuadi/mokibox`) di bawah `shared/`, dipakai oleh
`api-gateway` dan `transcoder-worker`. Pakai ini **bersamaan** dengan
`hermes-go-idiomatic` untuk setiap file di `shared/` atau handler/service
yang consume tipe dari `shared/`.

## Kapan pakai

- Menulis file baru di `shared/`.
- Mereview PR yang menyentuh `shared/`.
- Implementasi handler/service di `api-gateway` atau `transcoder-worker`
  yang consume sentinel errors, envelope helpers, Asynq client, R2 client,
  atau MediaToken dari `shared/`.
- Mulai fase baru (fase 3+) yang butuh tahu konvensi existing di `shared/`.
- **Setup / debug deployment topology** (docker compose, Zitadel split,
  Makefile targets `up-zitadel` / `up-all` / `down-zitadel`) — lihat
  section "Deployment topology" di bawah.

## Pola error (dual-layer, final sejak fase 2)

1. **Sentinel errors** di `shared/errors.go` (ErrValidation, ErrUnauthorized,
   ErrNotFound, dst — 13 total termasuk ErrRateLimited).
2. **APIError struct** untuk custom message/field details:
   `NewAPIError(code, message).WithCause(err).WithDetails(...)`.
3. **Central `ClassifyError(err)`** di `shared/response.go` adalah
   **satu-satunya** tempat yang map error → HTTP status + wire code.
   Handler cukup `shared.RespondError(c, err)` — tidak boleh
   `c.JSON(status, gin.H{...})` manual.

**Diekspor sejak fase 9 (PR #45, commit `d2dad34`)**: `ClassifyError`
adalah exported name (dulu `classifyError` unexported). Export-nya
diperlukan supaya `api-gateway/error_handler.go` (central
HTTPErrorHandler) bisa reuse mapping table yang sama untuk
Echo framework errors (`*echo.HTTPError` 404 / 405 / body-bind)
yang tidak melalui `RespondError`. Aturan pakai:

- **Handler production code**: tetap `shared.RespondError(c, err)`
  sebagai primary path. ClassifyError tidak dipanggil langsung
  dari handler.
- **Central HTTPErrorHandler** (di package yang berbeda, mis.
  `api-gateway/error_handler.go`): boleh panggil `shared.ClassifyError`
  untuk reuse mapping table ketika handler tidak bisa (framework
  errors, panic recovery, dll). Tetap serialize envelope dengan
  shape yang sama (`{error:{code,message,details}}`).
- **Tidak boleh** duplikasi mapping logic di package lain —
  mapping table di `shared/errors.go` (`httpStatusFor` + `codeFor`)
  adalah single source of truth.

Aturan:
- Jangan `errors.Is(err, &someStruct{})` kalau ada sentinel cocok. Pakai
  `errors.Is(err, ErrXxx)` langsung.
- Wrap dengan `fmt.Errorf("konteks: %w", sentinel)` di service, jangan bikin
  error baru. `errors.Is` harus chain sampai handler.
- Log error asli (lengkap, bukan `err.Error()`) di `RespondError` sebelum
  response di-serialize. Pattern: `slog.Error("request failed", "err", err,
  "path", c.Path())`.

Wire envelope (sesuai `planning/04_api_contracts.md`):

```json
{"data": {}}                              // sukses tunggal
{"data": [...], "pagination": {"next_cursor": null}}   // list
{"error": {"code": "NOT_FOUND", "message": "...", "details": [...]}}   // error
```

`details` hanya muncul untuk `VALIDATION_ERROR` — untuk kode lain field
omit, bukan null.

## Constructor & DI

Konvensi MokiBox (lihat `shared/config.go`, `shared/db.go`, `shared/r2.go`,
`shared/redis.go` dari fase 1-2):

- Return concrete struct, bukan interface.
  `NewR2Client(ctx, cfg) (*R2Client, error)`, bukan `NewR2Client(...) (Client, error)`.
- Dependency via parameter, **tidak pernah** global var.
  `EnqueueTranscode(client *asynq.Client, ...)` — bukan package-level
  `var defaultClient`.
- Field validasi di awal constructor. Gagal cepat, jangan return zero value.
- Config dirangkum dalam struct kecil (`R2Config`, `RedisConfig`) supaya
  constructor tidak terikat struct config lebih besar. Test cukup bikin
  literal.

## Pitfall library yang muncul berulang

### `github.com/hibiken/asynq` v0.26.0

- `asynq.NewTask(typename, payload []byte) *Task` — **return 1 value (bukan
  2)**, payload **harus `[]byte`**. Struct harus di-`json.Marshal` manual.
  Pattern di `shared/redis.go` `marshalTask`.
- `asynq.RedisClientOpt` butuh `Password` non-empty (SEC-03). DB 0 aman.
- `asynq.Config.Queues` wajib di-set eksplisit, default kosong = no-op.
- `ErrorHandler` di config adalah safety net — wajib di-set default yang log
  task failures, karena task gagal tanpa handler = silent drop.
- **Two-layer retry model — JANGAN keliru baca `MaxRetry(N)`**. asynq
  punya retry policy sendiri (`asynq.MaxRetry`) yang **independen** dari
  retry policy application-level yang kita manage manual. Saat producer
  enqueue task:
  - `MaxRetry(1)` = asynq boleh retry task 1x kalau Redis/network blip
    saat pickup. Setelah itu asynq drop ke dead queue (atau call
    ErrorHandler).
  - Ini **BUKAN** retry budget total. Total attempt = 1 initial + 1 retry
    = 2, tapi untuk failure yang non-transient (mis. transcode fail),
    retry asynq tetap di-trigger kalau handler return error ke asynq.
  - Producer-side `MaxRetry(1)` artinya: "aku producer, kalau task
    gagal aku mau fail-fast, app-level retry aku yang handle".

  Retry logic **3x** yang PRD sebut (FR-VIDEO-08 / NFR) ada di
  **worker (fase 5)**, bukan producer. Pattern worker (lihat
  `LLD_PLAN.md` section 8): catch error → cek `retry_count < 3` di
  videos row → kalau iya, `IncrementVideoRetry` + enqueue ulang
  `transcode:video` dengan `ProcessIn(30s * retry_count)`. Kalau
  sudah 3, `MarkVideoFailed` + cleanup raw R2. Worker-owned.

  Doc-only check: kalau handler enqueue pakai `MaxRetry(1)` dan
  commit message bilang "retry 3x", reviewer akan bingung. Selalu
  tulis di commit message + docstring: "asynq.MaxRetry(1) = safety
  net queue-level; retry 3x policy ada di worker (fase 5) via
  IncrementVideoRetry + enqueue ulang ProcessIn(30s * retry_count)".

### Binary smoke: `go run` di host + `socat` port-forward

Phase 4 Dockerfile masih stub (`echo 'binary not yet built'` sleep 3600).
Itu **BUKAN** alasan untuk skip binary smoke — `go run ./api-gateway`
di host bisa jalan selama host punya env lengkap DAN bisa reach
Postgres/Redis. Postgres/Redis di compose project `mokibox` defaultnya
**tidak expose host port** (lihat docker-compose.yml — gak ada
`ports: - "5432:5432"`).

Recipe:

```bash
# 1. Start socat port-forward untuk postgres + redis (binding ke host port)
docker run -d --name socat-pg --network mokibox_backend \
    -p 15432:5432 alpine:3.19 \
    sh -c "apk add --no-cache socat >/dev/null && socat TCP-LISTEN:5432,fork,reuseaddr TCP:postgres:5432"
docker run -d --name socat-redis --network mokibox_backend \
    -p 16379:6379 alpine:3.19 \
    sh -c "apk add --no-cache socat >/dev/null && socat TCP-LISTEN:6379,fork,reuseaddr TCP:redis:6379"

# 2. Build + run binary di host, env mengarah ke socat port
go build -o /tmp/api-gateway-host ./api-gateway
API_GATEWAY_ADDR=:18090 \
DATABASE_URL='postgres://tiktok_api:CHANGE@localhost:15432/tiktok?sslmode=disable' \
REDIS_ADDR=localhost:16379 \
R2_ACCOUNT_ID=... R2_ACCESS_KEY_ID=... R2_SECRET_ACCESS_KEY=... \
R2_BUCKET=... R2_ENDPOINT=... \
ZITADEL_ISSUER_URL=http://localhost:8080 \
ZITADEL_CLIENT_ID=test-web ZITADEL_API_CLIENT_ID=test-api \
ZITADEL_TARGET_SIGNING_KEY=test \
API_BASE_URL=https://api.example.com MEDIA_TOKEN_SECRET=test \
/tmp/api-gateway-host &

# 3. Smoke test dengan curl — expect 401 untuk /api/* (denyAllVerifier)
curl -sS -i http://localhost:18090/healthz                        # 200
curl -sS -i -X POST http://localhost:18090/api/videos/upload-intent \
    -H "Content-Type: application/json" -d '{"title":"x"}'        # 401

# 4. Cleanup
docker rm -f socat-pg socat-redis
kill %1
```

Pitfall:
- **JANGAN build binary host lalu copy ke alpine container** — binary
  glibc (Ubuntu host) tidak akan jalan di alpine (musl). Symptom:
  `/usr/local/bin/api-gateway: not found` (padahal file ada). Solusi:
  pakai `go run` di dalam container (lihat fase-4 reference), atau
  build binary di alpine (`docker run --rm -v /tmp:/out golang:1.25.5-alpine
  go build -o /out/api-gateway ./api-gateway`), atau cukup test di
  host langsung.
- Socat alpine perlu `apk add --no-cache socat` setiap kali (alpine
  base tidak include socat). Image-nya tetap kecil (~5MB).
- Zitadel URL di `.env` untuk binary smoke di host: pakai
  `http://localhost:8080` atau dummy value, denyAllVerifier akan
  refuse anyway (lihat `main.go` phase 3 wiring).
- Setelah smoke selesai, **`docker rm -f socat-pg socat-redis`**
  — jangan biarkan orphan container yang occupy port 15432/16379.

### `github.com/aws/aws-sdk-go-v2/...`

- R2 butuh `o.BaseEndpoint = aws.String(endpoint)` DAN `o.UsePathStyle = true`
  di `s3.NewFromConfig`. Lupa salah satu = 307 redirect atau
  SignatureDoesNotMatch.
- `Region: "auto"` literal untuk R2.
- `s3types.Error` di v2 adalah **struct** (bukan pointer), field
  `Code`/`Key`/`Message` `*string`. Iterasi pakai `aws.ToString(e.Code)`,
  **bukan** `e.Code` atau `if e == nil`.
- Translate SDK error ke sentinel lokal via `smithy.APIError` + `errors.As`,
  bukan string matching. Pattern di `shared/r2.go` `mapR2Error` +
  `isNotFoundErr`.

### `github.com/labstack/echo/v4`

- `c.JSON(status, body)` otomatis set Content-Type yang benar di v4.7+.
- `c.NoContent(http.StatusNoContent)` untuk 204, bukan `c.JSON(204, nil)`.
- `c.Request().Context()` untuk `context.Context` standar, bukan
  `c.Context()` (internal).
- Central `HTTPErrorHandler` dipasang sekali di `e.HTTPErrorHandler = ...`.
  Handler cukup `return err` atau `h.respondError(c, err)` — jangan campur
  dua pola di codebase yang sama.

### `github.com/jackc/pgx/v5/pgxpool`

> **MokiBox post-PR-41 tidak lagi pakai `*pgxpool.Pool` di production
> code.** Section ini dipertahankan sebagai referensi jika fase
> berikutnya butuh regenerate sqlc ke `pgx.Tx` native (saat itu
> pgxpool akan muncul lagi). Untuk pembahasan kenapa pgxpool
> dihapus, lihat `### database/sql + sqlc` di atas.

- `pgxpool.NewWithConfig` butuh `MaxConnLifetime`/`MaxConnIdleTime`/
  `HealthCheckPeriod` set supaya pool tidak menyimpan dead conns dari
  Postgres restart atau NAT timeout.
- Pool returned as concrete `*pgxpool.Pool`, bukan interface. Downstream
  consumer yang butuh interface (untuk mock) definisikan sendiri di
  package mereka.

### Docker build context untuk multi-package Go module (single `go.mod` at repo root)

MokiBox adalah **satu Go module** rooted di repo root — bukan satu
module per service. Setiap service binary hidup di subdirectory
(`./api-gateway`, `./transcoder-worker`) tapi di-build sebagai
`./<subdir>` dari context repo-root. Konsekuensi penting untuk
Dockerfile multi-stage.

**Symptom**: build gagal dengan:

```
ERROR: failed to calculate checksum of ref ... "/go.sum": not found
```

**Cause**: `docker-compose.yml` punya `context: ./transcoder-worker`
(subdir saja) tapi Dockerfile di subdir itu `COPY go.mod go.sum ./`
+ `COPY . .` — subdir tidak punya `go.sum` (modul ada di parent).

**Fix**: di compose, **set context ke repo root** dan `dockerfile`
ke path relatif:

```yaml
transcoder-worker:
  build:
    context: .                           # repo root, BUKAN ./transcoder-worker
    dockerfile: transcoder-worker/Dockerfile
```

Pattern ini akan reuse di fase 9 (api-gateway Dockerfile production),
jadi biasakan dari sekarang. Cek compose `context:` setiap kali
Dockerfile perlu file dari repo root (`go.mod`, `go.sum`, `shared/`,
`shared/db/`, dll).

**Caveat**: `api-gateway/Dockerfile` fase 0-8 masih stub
(`sleep 3600`) yang tidak butuh `go.mod`, jadi context-nya
`./api-gateway` masih jalan. Begitu fase 9 replace stub dengan
multi-stage build, context HARUS pindah ke `.` — kalau lupa, build
fase 9 akan pecah dengan error yang sama.

**Runtime consequence: distroless = no shell, no package manager**.
Fase 9 (PR #45) memindahkan `mokibox-api-gateway` ke
`gcr.io/distroless/static-debian12:nonroot`. Image ini tidak punya
shell, apk, glibc, atau apapun selain binary + nonroot user
(UID 65532). Konsekuensi operasional:

  - **`docker exec ... sh` tidak jalan** — error "executable file
    not found in $PATH". Untuk debug live, build image debug
    terpisah (`FROM golang:1.25.5-alpine` + `apk add ... gosu`),
    atau pakai `docker debug` kalau Docker version support.
  - **Binary HARUS static** — `CGO_ENABLED=0` di builder stage.
    Library apapun yang pakai cgo akan fail to load di distroless.
    Echo, pgx stdlib, asynq semuanya pure-Go, aman.
  - **SIGTERM hits PID 1 directly** karena tidak ada shell wrapper
    (`ENTRYPOINT` not `CMD`). Inilah yang membuat 30s graceful
    shutdown di `api-gateway/main.go` benar-benar fires saat
    `docker compose stop` atau restart.
  - **Verify container identity post-build**:
    `docker inspect <image> --format '{{.Config.User}}'` →
    `nonroot:nonroot`. Compose yang punya `user: "10001:10001"`
    di service block akan conflict — drop `user:` atau ganti ke
    UID 65532.

Detail lebih panjang di `CONVENTIONS.md` section "Distroless
runtime image".

### `database/sql` + sqlc (single-pool, post `refactor/pool-consolidation`)

- `shared.NewSQLDB(ctx, dsn, maxConns, maxIdle) (*sql.DB, error)` di
  `shared/db.go` membuka `*sql.DB` lewat pgx stdlib driver — driver
  ter-register via side-effect import `_ "github.com/jackc/pgx/v5/stdlib"`
  di `shared/db.go`. `db.New(sqlDB)` di api-gateway & worker pakai
  handle yang sama.
- Sejak refactor `refactor/pool-consolidation` (PR #41, commit
  `69bac5c`) ada **satu `*sql.DB` pool saja** di MokiBox
  (`*pgxpool.Pool` sudah dihapus total). sqlc `Queries` (read path)
  dan `Queries.WithTx(tx *sql.Tx)` (transaction path) keduanya
  bind ke pool yang sama.
- **Missing-row error type: SELALU `sql.ErrNoRows`** dari
  `QueryRowContext().Scan()`. `pgx.ErrNoRows` TIDAK akan pernah match
  di handler — pgxpool sudah tidak ada di codebase. **DILARANG** pakai
  dual-check atau `pgx.ErrNoRows` — akan return 500 untuk not-found
  branch. Aturan (verifiable):
  `grep -rn "pgx\.ErrNoRows" api-gateway transcoder-worker --include="*.go"`
  HARUS return 0 matches.
- **BUKAN** ada wiring `*pgxpool.Pool` di production code. Kalau
  reader referensi menemukan `pgxpool` di `mokibox-go-shared`
  references, itu historical (fase 4–6) dan sudah di-refactor ke
  single `*sql.DB`. Trade-off: pgxpool's granular acquire/release
  hilang, tapi workload kecil (max 10 conn API + 5 conn worker)
  tidak signifikan. Trade-off documented di `shared/db.go` untuk
  fase 7+ tidak re-introduce pgxpool tanpa sadar `WithTx` butuh
  `*sql.Tx`.

- **sqlc `:one` query returns 2 values, bukan 1.** Generated
  signature: `func (q *Queries) SomeOneQuery(ctx, arg) (Row, error)`.
  Kode yang tulis `if err := qtx.SomeOneQuery(...); ...` akan
  fail compile dengan `assignment mismatch: 1 variable but
  qtx.X returns 2 values`. Pattern yang benar (untuk query
  yang row-nya tidak perlu di-respond):

  ```go
  if _, err := qtx.TombstoneUser(ctx, userID); err != nil { ... }
  ```

  Berlaku untuk `MarkVideoDeleted` (returns Video), `GetUserByID`
  (returns User), `TombstoneUser` (returns User), dst. Query
  `:exec` (no rows returned) tetap return 1 value (`error`),
  dan `:many` return `([]Row, error)`. Cek signature generated
  di `shared/db/*.sql.go` sebelum pakai di handler baru.

### Defence-in-depth nil check di handler constructor field

Pattern fase 3: setiap handler yang baca dependency wajib cek nil di
awal method (bukan constructor) dan return `ErrInternal` dengan
pesan eksplisit. Contoh di `WebhookHandler.Handle`:

```go
if h.SigningKey == "" {  // env loader juga sudah refuse, tapi
                          // kalau handler dipakai di test/future path
                          // tanpa env, JANGAN panic - return 500
    return shared.RespondError(c, shared.Wrap(shared.ErrInternal,
        "webhook signing key not configured"))
}
if h.Queries == nil {  // sama: defence in depth
    return shared.RespondError(c, shared.Wrap(shared.ErrInternal,
        "webhook queries not configured"))
}
```

Penalti skip: nil-pointer panic = `EOF` di client, server log
membanjiri stack trace, attacker bisa pakai sebagai oracle (kalau
panic itu di branch tertentu). Selalu return 500 envelope
terstruktur.

## Deployment topology

> Ringkasan — detail lengkap di `references/deployment-topology.md`.

MokiBox **bukan** satu compose project. Ada 2 compose yang
di-manage terpisah, di-host yang sama:

| Compose project | Direktori | Isi | Makefile target |
|---|---|---|---|
| `mokibox` | repo ini (`./docker-compose.yml`) | `mokibox-postgres`, `mokibox-redis`, `mokibox-api-gateway`, `mokibox-transcoder-worker`, `mokibox-nginx` | `make up` / `make down` / `make logs` |
| `zitadel` | sibling (`./zitadel-compose/docker-compose.yml`, default) | `zitadel-proxy` (Traefik), `zitadel-zitadel-api-1`, `zitadel-zitadel-login-1`, `zitadel-postgres-1` | `make up-zitadel` / `make down-zitadel` / `make logs-zitadel` |
| (keduanya) | — | — | `make up-all` / `make down-all` |

### Pitfall yang harus dihindari

- **JANGAN tambahin service `zitadel` ke `MokiBox/docker-compose.yml`.**
  Zitadel v3+ adalah multi-service (Traefik + zitadel-api +
  zitadel-login + Postgres sendiri) yang secara fisik tidak fit
  di satu slot container dalam MokiBox compose. Attempt yang
  kita coba di fase 0 (single `zitadel: v2.54.0` container)
  restart-loop indefinite karena env v2 conflict dengan layout
  v4 yang sebenarnya jalan. Symptom: `docker ps` shows
  `tiktok-zitadel Restarting (1) X seconds ago`. Fix: Zitadel
  HARUS di sibling compose project. LLD asumsi A11 + PR #36.
- **Container name clash**: kalau pernah ada compose project
  lama `tiktok-backend` (sebelum rename ke `mokibox`), orphan
  containers tetap hidup walau `docker compose down` di project
  baru. Cleanup manual:
  `docker compose -p tiktok-backend down --remove-orphans`.
- **`make up` tidak bring up Zitadel.** Default `up` target
  cuma MokiBox app. Untuk Zitadel, pakai `make up-zitadel`
  atau `make up-all`. Kebalikannya: `make down` tidak sentuh
  Zitadel — pakai `make down-zitadel` atau `make down-all`.
- **`ZITADEL_ISSUER_URL` di `.env` adalah HTTP host URL
  yang Traefik di `zitadel-compose` publish ke host** (dev:
  `http://localhost:8080`). Production: HTTPS domain yang
  DNS-point ke deployment Zitadel. MokiBox `api-gateway`
  baca env ini dan call out ke Zitadel via network host,
  TIDAK via docker network internal.
- **Nginx MokiBox tidak reverse-proxy Zitadel** sejak
  fix branch `fix/zitadel-separate-deployment`. Kalau lihat
  config lama yang punya `location /zitadel/` atau
  `server_name auth.example.com`, itu pre-fix.
- **Nginx resolve upstream hostname SEKALI saat config-load** (default
  `proxy_pass http://hostname:`): kalau container upstream restart dan
  dapat IP baru (mis. post-VPS-reboot, api-gateway restart 1 detik
  SETELAH nginx start), nginx tetap mem-proxy ke IP lama → **502
  permanen** padahal `wget` dari dalam container nginx ke hostname
  yang sama sukses (DNS resolve fresh tiap call). Symptom fase 10:
  healthz 200 dari dalam network, 502 massal dari host; integration
  smoke jatuh ke 3 PASS. Fix cepat: `docker compose restart nginx`
  setelah upstream restart. Fix permanen: `resolver 127.0.0.11
  valid=10s;` + variable di `proxy_pass http://$upstream;` supaya
  DNS di-resolve ulang berkala.
- **Pre-existing nginx restart loop on missing dev cert**
  (`/etc/nginx/certs/live/api.example.com/fullchain.pem`).
  Ini TIDAK disebabkan Zitadel split. Folder
  `deploy/nginx/certs/` harus dipopulate dengan self-signed
  dev cert atau certbot real cert sebelum `mokibox-nginx`
  start. Production: mount dari secret manager atau
  certbot. Phase 9 own.

### Production VPS multi-tenant (aaPanel edge) — LIVE-VERIFIED 2026-09-24

Deploy ke VPS `43.157.227.130` multi-tenant (9 container tenant lain + aaPanel + host processes) dengan aaPanel nginx sebagai edge:

**Topologi:**
- Internet → aaPanel nginx host (:80/:443, TLS di sini)
  - `mokiboxapi.binery.my.id` → `proxy_pass http://127.0.0.1:8180` (mokibox-api-gateway publish host loopback)
  - `auth.mokibox.my.id` → `proxy_pass http://127.0.0.1:8181` (Zitadel Traefik publish host loopback)
  - `mokibox-nginx` container TIDAK jalan (nginx `profiles:[disabled]` di `docker-compose.prod.yml`)

**Compose production override** (`docker-compose.prod.yml`):
```yaml
services:
  nginx:
    profiles: ["disabled"]
  api-gateway:
    ports:
      - "127.0.0.1:8180:8080"  # loopback only
```

**Jalankan di VPS:**
```bash
cd /www/wwwroot/mokibox
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
```

**aaPanel vhost untuk Zitadel OIDC discovery (KRITIS):**
`/.well-known/openid-configuration` HARUS di-proxy ke Zitadel Traefik (127.0.0.1:8181), BUKAN diserve static. Tanpa ini, discovery 404/301, api-gateway verifier crash-loop.

```nginx
location = /.well-known/openid-configuration {
    proxy_pass http://127.0.0.1:8181/.well-known/openid-configuration;
    proxy_http_version 1.1;
    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Connection        "";
}
location /.well-known/acme-challenge/ {
    root /www/wwwroot/auth.mokibox.my.id;
}
location /.well-known/ {
    return 404;
}
```

**Container env frozen**: setelah update `.env` (R2 credentials, Zitadel client IDs/secrets), WAJIB:
```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --force-recreate api-gateway transcoder-worker
```
Restart saja (`docker restart`) TIDAK reload env — root cause integration test 403 massal run-2.

**Anggaran memori**: VPS 7.4Gi RAM, tenant lain ~4.1Gi. MokiBox stack ≈3-3.5Gi (gateway 512m + worker 1g + postgres ~300m + redis ~50m + Zitadel ~1-1.5g). Mitigasi: monitor `docker stats` minggu pertama; worker `mem_limit` 1g→768m duluan; Zitadel single instance; JANGAN tambah swap host (MT5/wine thrashing).

**Zitadel domain migration (auth.binery.my.id → auth.mokibox.my.id, 2026-09-24):**
1. `docker compose -f zitadel-compose/docker-compose.yml down -v`
2. `docker volume rm zitadel_zitadel-bootstrap zitadel_postgres-data`
3. Update `.env` Zitadel: `ZITADEL_DOMAIN=auth.mokibox.my.id`, `ZITADEL_EXTERNALDOMAIN=auth.mokibox.my.id`, `ZITADEL_EXTERNALSECURE=true`, `ZITADEL_EXTERNALPORT=443`, `ZITADEL_PUBLIC_SCHEME=https`
4. `docker compose -f zitadel-compose/docker-compose.yml up -d`
5. Re-provision penuh (project, apps, Actions V2, test users)
6. Sync downstream: MokiBox `.env` + `deploy/demo/config.js` + integration_test.sh `ADMIN_LOGIN`
7. `docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --force-recreate`

### Verifikasi cepat (setiap setup dev baru)

```bash
cd /home/pratama/MokiBox
make -n up-zitadel    # dry-run; cek ZITADEL_COMPOSE_DIR resolved benar
make -n up            # dry-run; harus docker compose up -d --build
docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Label \"com.docker.compose.project\"}}"
# Expected: mokibox-* (project: mokibox) + zitadel-* (project: zitadel)
# Kalau lihat tiktok-* atau Restarting (1) di kolom STATUS, cleanup dulu.
```

`ZITADEL_COMPOSE_DIR` overridable per-invocation:
`make up-zitadel ZITADEL_COMPOSE_DIR=/srv/zitadel`.

## Konvensi response & cursor (fase 2, final)

- `shared.EncodeCursor(createdAt, id)` → `base64url("<RFC3339Nano>|<uuid>")`.
  SQL paging: `WHERE (created_at, id) < ($1, $2) ORDER BY created_at DESC,
  id DESC LIMIT n`.
- `RespondList(c, items, *string)` — pass `nil` untuk `nextCursor` kalau
  last page, **bukan** empty string. Field jadi `null` di JSON.
- `shared.NewMediaToken(videoID, secret, ttl)` — payload internal
  `video_id:<id>:<expiry_unix>`, HMAC-SHA256, `hmac.Equal` untuk compare.
  `VerifyMediaToken` collapse semua failure ke `ErrMediaTokenInvalid`
  sentinel — jangan split "expired" vs "bad signature" (oracle leak).

## `sqlc.yaml` cuma list `001_init.sql` — migration baru = sqlc diam-diam stale

`sqlc/sqlc.yaml` sekarang hanya list satu file di `schema:`:

```yaml
sql:
  - engine: "postgresql"
    schema: "../migrations/001_init.sql"
  queries: "queries"
```

File baru di `migrations/` **tidak** otomatis masuk ke parse schema sqlc.
Kalau tidak ditambah eksplisit ke list itu, `make sqlc-gen` tetap exit 0
tanpa error tapi tidak pernah tahu tabel/kolom baru — generated code stale,
dan compile error muncul jauh dari sumber masalahnya (handler, bukan migration).

Rule: PR yang menambah `migrations/0NN_*.sql` **wajib** menambahkan file itu
ke list `schema:` di commit yang sama. Bukti yang benar bukan "sqlc-gen jalan"
tapi "struct untuk tabel baru benar-benar ada dengan field yang diharapkan":

```bash
make sqlc-gen && grep -n "type Hashtag struct" shared/db/*.go
```

Mulai fase 11 ada beberapa migration baru berurutan, jadi mudah lupa di PR
yang isinya cuma handler. Hanya `001_init.sql` yang boleh immutable; `sqlc.yaml`
justru harus nyusul.

## Self-check output sendiri sebelum commit (angka DAN karakter)

Setiap total yang ditulis di commit message atau PR body **wajib** diverifikasi
dengan `grep`/`wc`/`find` aktual. Pola cek untuk fase MokiBox:

```bash
# method count di struct
grep -E '^func \(c \*R2Client\)' shared/r2.go | wc -l

# sentinel + code count
grep -E '^\s*Err[A-Z]' shared/errors.go | wc -l
grep -E '^\s*Code[A-Z]' shared/errors.go | wc -l

# task type constant
grep -E 'Type[A-Z][a-zA-Z]+ = "' shared/models.go | wc -l

# file count
git diff --stat main..HEAD
```

Agent self-report yang inconsistent (mis. tulis "13 method" tapi aktualnya
"6 method + 1 accessor + 6 helper = 13") lebih buruk dari tidak punya angka
sama sekali — reviewer akan curiga seluruh self-report. Breakdown dulu, baru
tulis total.

**Karakter punya kelas self-report yang sama.** Menulis dokumen panjang
(issue body, roadmap, reference) dalam satu `write_file` bisa menghasilkan
token dari script lain nyangkut di tengah kata — CJK, Arab, Cyrillic di
dokumen Indonesia/Inggris. Gejalanya sama: output yang terlihat benar ke mata
sebagian, tapi salah di beberapa tempat. Cek sebelum lanjut:

```bash
grep -nP '[\x{4e00}-\x{9fff}\x{0600}-\x{06ff}\x{0400}-\x{04ff}\x{FF00}-\x{FFEF}]' <file>
```

Tanda lain dari kelas ini: kata yang kehilangan spasi (`tidak/tasku`,
`untuk offset.Buat`). Gejalanya mirip — token terpotong — dan grep
`\w\.\w` menangkapnya. Dua-duanya wasting satu putaran review per file kalau
tidak dicegat sebelum commit.

## Dual-write trap: tombstone di DB → enqueue cleanup task ke Redis non-transactional

Pattern yang muncul di fase 8 (DeleteUserData + DeleteVideo): "hapus
data lokal, lalu enqueue task ke Redis/asynq supaya worker yang
hapus file R2". Implementasi naif:

```go
// 1. Tx: tombstone + hapus row + hapus relational
tx, qtx := h.DB.BeginTx(ctx, nil); ... ; tx.Commit()

// 2. Enqueue cleanup task ke Redis
shared.EnqueueCleanupObjects(h.Queue, shared.CleanupObjectsPayload{Keys: ...})
```

Risiko: proses crash / Redis down antara langkah 1 dan 2 →
**data di DB sudah tidak ada, tapi file R2 belum dihapus** dan
tidak ada lagi row DB yang menunjuk ke R2 key. Object R2 jadi
**orphan permanen** sampai manual cleanup atau sampai tagihan R2
membengkak.

Trade-off matrix per tipe delete:

| Tipe delete | Aman dari dual-write trap? | Mekanisme recover |
|---|---|---|
| Tombstone user (row tetap ada, `is_active=false`) | Aman | Row bisa di-scan; reconciliation sweep bandingkan `uploads/<userID>/` di R2 vs users aktif |
| Delete video (row tetap sebagai `status=DELETED` + `deleted_at`) | Aman | `ListVideosEligibleForCleanup` (sudah ada) sweep row `DELETED` >24 jam |
| Hard delete row (`DeleteVideosByUser` di dalam tx delete-account) | **TIDAK aman** | Tidak ada row DB referensi → orphan permanen. Perlu dedicated reconciliation sweep (R2 list prefix `uploads/<userID>/` vs users active/tombstoned) |

Mitigasi yang dipakai di fase 8 (dan boleh dipakai di fase
berikutnya untuk pattern serupa):

1. **For delete-account**: hls prefix TIDAK di-enqueue sebagai
   individual keys (tidak ada ListObjects di dalam tx). Worker
   yang handle `cleanup:video` per video akan call
   `R2Client.DeletePrefix` saat grace period lewat. Itu
   berfungsi selama row `videos` belum di-hard-delete.
2. **Trade-off eksplisit di commit message + PR body**:
   "Enqueue failure is logged warn, not surfaced, so a queue
   blip does not undo the tombstone" — tapi ini HARUS
   disertai carry-over eksplisit untuk fase berikutnya
   (reconciliation job). Jangan cuma taruh di commit message
   dan lupa; reviewer tidak akan connect the dots.
3. **Defence-in-depth**: kalau memungkinkan, enqueue task
   dengan payload yang carry SEMUA info yang worker butuhkan
   (R2 keys + userID), supaya worker bisa idempotent re-run
   kalau task duplicate.

Pattern yang BENAR untuk delete + async cleanup (recommended
untuk fase 9+ jika menambah delete pattern lain):

```go
// Step 1: tx (commit atomically)
// - Tombstone / soft delete (row tetap ada sebagai referensi)
// - Kumpulkan R2 keys
// Step 2: enqueue SETELAH commit (di luar tx)
// - Idempotent payload (re-run aman)
// - Jika enqueue gagal: log warn, BUKAN fail request
// Step 3: dokumentasikan trade-off + reconciliation carry-over
//   di PR body section "Trade-off & known limitation"
//   DAN di HANDOFF.md "Decisions & gotchas" supaya fase
//   berikutnya tahu orphan perlu di-sweep
```

JANGAN hard-delete row di tx kalau async cleanup butuh
referensi ke row. Soft delete (tombstone dengan `is_active=false`
atau `status=DELETED`) lebih aman karena row tetap bisa di-scan
oleh reconciliation job.

## LLD > literal ISSUES.json acceptance — fix spec gap inline, bukan jadi deviasi

ISSUES.json acceptance criteria untuk satu fase bisa lebih ringkas dari
LLD_PLAN. Contoh fase 4: ISSUES.json bilang "reuse record, ganti r2_key"
(5 kata), LLD section 7 bilang "UpdatePendingVideoR2Key + update
title/description". Fase 4 awalnya carry "title/description TIDAK
di-refresh" sebagai deviasi — salah. Yang benar: tambah query sqlc
baru (`UpdatePendingVideoMetadata`) + regenerate + panggil di handler
satu atomic commit. Spec source of truth untuk behavior detail = LLD;
ISSUES.json cuma checklist minimum.

Rule praktis:
- Kalau LLD menyebut action yang acceptance criteria omit → cek dulu
  apakah action itu achievable dengan 1 query sqlc + 1 line handler
  change. Kalau ya, fix inline (close deviasi di PR yang sama), bukan
  carry sebagai future work.
- Deviasi yang legitimate (perlu design discussion, >1 file, atau
  blocked by dependency external) tetap ditulis di PR body + commit
  message. Yang **bukan** legitimate: "spec minta X tapi aku skip
  karena query sqlc-nya belum ada" — itu solvable dalam 1 commit.

## PR-review finding = "kode benar tapi tidak terbukti" → carry ke fase-N+1, jangan diem-diem mark ✅

Ini **bukan** soal bug kode — itu fix inline. Yang dimaksud: review
menemukan safety net yang **pola penulisannya benar tapi belum pernah
dieksekusi** di end-to-end smoke. Contoh fase 5: `exec.CommandContext`
+ `context.WithTimeout` adalah pattern stdlib untuk kill ffmpeg
setelah `TRANSCODE_TIMEOUT`, tapi smoke cuma memproses file 32 KB
selesai dalam 6 detik — tidak ada bukti kill-switch benar-benar
aktif kalau ffmpeg hang.

Tanda khas:
- Pola kode = textbook idiom, tidak ada code bug yang bisa di-patch.
- Static check (`go build`, `go vet`, unit test untuk pure function)
  lulus tanpa effort tambahan.
- Yang **belum** ada = bukti runtime bahwa jalur tertentu benar-benar
  fires di kondisi yang seharusnya trigger.

Tindakan yang salah: tulis "✅ implemented" di checklist PR body
untuk acceptance yang sebenarnya cuma di-code-read, bukan
dieksekusi. Reviewer fase 5 (user) koreksi keras: "smoke cuma
verifikasi happy path, klaim ✅ itu overclaim untuk bagian yang
sebenarnya cuma diverifikasi lewat code-reading".

Tindakan yang benar:
1. Acknowledge di commit message PR (jangan dipingitin di "Notes"
   bawah) — tulis eksplisit: "X path not exercised at PR time;
   carried to fase-N+1 as a verification issue".
2. Tambah entry baru di `planning/ISSUES.json` fase yang sesuai
   (biasanya fase testing yang akan datang, mis. fase 10). Body
   acceptance criteria harus berisi: cara membikin input yang
   trigger kondisi, positive control (jalur yang TIDAK boleh
   ter-trigger), transisi state lengkap (retry exhausted, FAILED,
   cleanup raw, dst), cara restore env ke nilai production.
3. Tambah GitHub issue yang matching dengan label `phase-N`
   + `testing` (atau `security` kalau jalur protektif).
4. Setelah PR fase-N merged → cherry-pick commit planning-doc
   langsung ke `main` (per `git-phase-issue-workflow` post-merge
   planning-doc update).

Bedanya dengan "fix spec gap inline": spec gap = kode yang kurang
sesuai spec; verification gap = kode yang sesuai spec tapi tidak
terbukti benar di runtime. Inline fix cocok untuk spec gap karena
kode perlu diubah; verification gap TIDAK butuh kode diubah, dia
butuh **smoke baru di fase berikutnya**.

Konsistensi dengan `git-phase-issue-workflow` rule #9 (deviasi
wajib eksplisit): rule #9 melarang silently-drop acceptance criteria
dari checklist. Aturan ini melarang silently-claim ✅ untuk jalur
yang tidak terbukti. Keduanya tentang kejujuran PR body, bukan
kode.

**Outcome fase 10 (issue #39, live-verified 2026-09-03)**: menjalankan
verification gap ini benar-benar menemukan **bug #6** — bukan di ffmpeg,
tapi di `MarkVideoFailed`: semua 4 call site mewarisi task-ctx yang
sudah deadline-exceeded saat timeout-kill aktif → UPDATE gagal → row
stuck `PROCESSING:3` selamanya (invisible ke status-poll user DAN ke
24h cleanup job). Fix: `markFailedDetached` (fresh ctx 30s), commit
`fix: [phase-10.10]` di PR #47. Lesson: menjalankan verification issue
yang lama BUKAN formalitas — pattern "textbook correct" yang tidak
pernah dieksekusi bisa menyembunyikan bug terminal-state bertahap.
Lihat juga §"Terminal-state DB writes" di bawah.

## Integration smoke vs helper smoke: the 5-bug lesson (fase 10, live-verified)

Fase 10 (issue #30, local *.localhost E2E) menemukan **5 bug produksi pre-existing dalam satu sesi** — semua ter-masked bertahun-tahap karena setiap smoke fase sebelumnya memverifikasi lewat helper (baca R2 langsung, stub verifier, DB query) dan TIDAK PERNAH lewat endpoint HTTP asli dengan dependensi live. Kelas bug yang hanya muncul di jalur "asli":

| Bug | Fase asal | Kenapa ter-masked | Ter-expose oleh
|---|---|---|---|
| NewZitadelVerifier kirim full URL ke zitadel.New(domain) → origin `https://http//host` | phase-3.1 | Semua smoke pakai denyAll stub; binary smoke bilang "OIDC discovery failed" dan dibaca "Zitadel belum nyala", padahal URL malformed | Boot pertama vs Zitadel live |
| CheckToken strip prefix "Bearer " padahal SDK menuntut prefix ada (ErrMissingToken) | phase-3.1 | Tidak ada smoke yang pernah kirim JWT Zitadel asli lewat middleware | JWT live pertama |
| Webhook dispatch di `userID` (aktor) bukan `aggregateID` (target) — admin men-deactivate user → **akun admin yang ke-tombstone** | fase-8 | Semua contoh payload self-actored (userID==aggregateID); smoke fase-8 simulasikan webhook manual | Webhook asli dari event admin-triggered |
| Playlist key `hlsPrefix + "master.m3u8"` tanpa separator → `<videoID>master.m3u8` → 404 permanen | fase-6 | Smoke fase-6 verifikasi output transcode via r2_list (list key langsung), bukan GET playlist endpoint | GET playlist.m3u8 pertama |
| RewriteMasterPlaylist ambil segment PERTAMA URI absolut (="hls") → semua variant `?variant=hls`; RewriteVariantPlaylist menuntut trailing-slash yang tidak pernah ada | fase-6 | Idem — isi file diverifikasi, bukan hasil rewrite endpoint | Konten playlist hasil rewrite pertama |

**Aturan turunan (WAJIB untuk fase berikutnya):**
1. Smoke yang meng-claim endpoint bekerja HARUS memanggil endpoint itu (HTTP call), bukan membaca efek sampingnya (file R2/row DB).
2. Dependency eksternal (verifier JWT, webhook, presign) harus diuji dengan **artefak asli** — JWT yang benar-benar di-sign Zitadel, webhook yang benar-benar dikirim Zitadel (bukan curl manual dengan body buatan sendiri).
3. Payload contract library eksternal = capture live (lihat §"Cara mengintip payload webhook" di skill zitadel) sebelum menulis handler yang membaca field-nya.
4. "Sudah PASS di fase N" bukan bukti — 5 bug ini lolos fase 3-9 review + smoke.

Referensi: `scripts/integration_test.sh` (MokiBox) = pola smoke endpoint-asli; PR #46 commit phase-10.2..10.6 untuk detail per bug.

## Terminal-state DB writes: jangan warisi task-ctx yang sudah expired (fase 10, live-verified)

`HandleTranscode` membungkus seluruh pipeline dalam
`context.WithTimeout(ctx, w.Cfg.TranscodeTimeout)`. Saat timeout membunuh
pipeline (kill path aktif), semua operasi DB yang berjalan SETELAH kill
dengan ctx yang sama pasti gagal (`context deadline exceeded`). Ini
menghantui `MarkVideoFailed` di 4 call site (post-budget di
HandleTranscode, invalid-media, validate, post-budget di handleTransient):
row stuck `status=PROCESSING retry_count=3` SELAMANYA — invisible ke
status-poll user, invisible ke 24h cleanup job (dia butuh status=DELETED),
dan retry budget habis permanen.

Fix (PR #47, commit `9385f16`): `markFailedDetached(videoID)` membungkus
`MarkVideoFailed` dalam `context.WithTimeout(context.Background(), 30s)`.
Semua 4 call site dimigrasi. `MarkVideoReady` SENGAJA TIDAK dimigrasi:
sudah dijaga `WHERE status='PROCESSING'` + recoverable oleh re-check
dan 24h cleanup; migrasi justru melebarkan window unbounded work.

Aturan umum (lihat juga `hermes-go-idiomatic` §terminal-state writes):
terminal-state write yang berjalan setelah kill path WAJIB ctx fresh;
pipeline step tetap ctx timeout. Sinyal: write dipanggil di branch
"budget exhausted" / "give up" SETELAH error yang dikirim ctx timeout
itu sendiri.

## Fixture sizing untuk kill-verification smoke (fase 10, live-measured)

Membuat input yang "men-trigger timeout di step tertentu" itu constrained
fisik. Data live dari tuning issue #39 (VPS dev, R2 throughput ~2.3 MB/s,
CPU encode cepat):

| Fixture | Size | Re-encode (worker-like) | Verdict |
|---|---|---|---|
| mandelbrot 960x540 maxiter90k, 20s | 19 MB | 0.5s | lossy compression MENGHANCURKAN per-frame cost — mahal saat generate, murah saat re-encode |
| mandelbrot 1280x720 maxiter120k, 60s | 174 MB | 4.4s | file terlalu besar — download makan seluruh budget |
| noise 720p crf35 15s | 60 MB | 1.75s | downscale meng-cheap-kan encode |
| noise 480p crf35 40s | 65 MB | 2.91s | re-encode bagus, tapi download 65MB >2s di 2.3MB/s |
| noise 480p crf40 40s | 3 MB | 1.2s | terlalu murah |
| noise 480p crf40 120s | 9.2 MB | 3.62s | compromise terbaik — tapi download 9MB TETAP ~2-4s di VPS ini |

Pola umum: konten yang mahal re-encode juga besar filenya
(incompressible), dan di bandwidth VPS→R2, besar = lama download.
Konsekuensi: **kill mendarat di step mana pun yang memegang budget saat
deadline fires** — di VPS ini selalu step download. Itu BUKAN bug;
kill mechanism-nya identik (satu ctx deadline shared lintas semua step).

**Struktur verifikasi 2-layer yang work** (user-approved, PR #47):
1. Runtime smoke: buktikan LADDER lengkap (kill → retry bump →
   re-enqueue delay 30s/60s → FAILED → cleanup:objects → restore →
   positive control di worker yang sudah di-restore). Step kill
   dilaporkan APA ADANYA (grep nama step dari log), tidak overclaim.
2. Unit test step-specific: panggil `runFFmpeg` LANGSUNG dengan ctx 1s
   + encode nyata yang butuh menit → bukti `exec.CommandContext`
   membunuh ffmpeg (evidence "signal: killed"), tanpa network.

Dua trap yang nyasar di sesi yang sama:
- **Rebuild image SEBELUM swap container**: replacement worker yang
  jalan dari stale `:dev` image diam-diam mengetest binary LAMA — fix
  tidak terbukti, assertion FAILED dengan row `PROCESSING:3` padahal
  fix sudah di-commit. Persis kelas masked-bug yang mau dicegah.
  Urutan: `docker compose build <service>` dulu, baru `docker run`
  replacement, baru assertions.
- **Assertion hanya-PASS-tanpa-FAIL bukan assertion**: grep-PASS di
  dalam polling loop yang tidak punya cabang FAIL eksplisit
  menghasilkan smoke "21 PASS" padahal kriteria inti tidak match
  (terjadi nyata: PASS "kill evidence" generik menutupi FAIL-nya
  "kill di step ffmpeg"). Setiap kriteria WAJIB punya cabang FAIL
  yang benar-benar terpanggil kalau kondisinya tidak terpenuhi.

Detail lengkap (repro timeline, timing per attempt, commands):
`references/fase-10-verification-notes.md`.

## Lessons fase-10 tests/reconcile (2026-09-04, PR #47/#49)

Empat kelas pitfall baru, semuanya live-verified:

1. **Terminal-state writes JANGAN mewarisi ctx yang bisa expired**
   (bug nyata, `fix: [phase-10.10]`): saat `context.WithTimeout` kill pipeline,
   SEMUA write DB setelahnya (MarkVideoFailed post-budget, invalid-media
   branches) jalan dengan ctx deadline-exceeded → UPDATE gagal → row stuck
   PROCESSING:3 selamanya (invisible ke user DAN 24h cleanup job). Fix:
   fresh `context.WithTimeout(context.Background(), 30s)` untuk terminal
   writes. General: "error/ctx asli harus survive sampai log" — kelas yang
   sama dengan ProbeFile yang menelan error cmd.Run() (kill evidence hilang,
   log operator tidak bisa bedakan killed-probe vs corrupt-file).

2. **Budget fixed vs network variable**: di uplink yang bervariasi (WiFi
   rumah, throughput R2 1.5-9MB/s), budget hardcoded untuk pipeline yang
   menyentuh network tidak pernah stabil — 2s/4s selalu salah arah. Pola:
   ukur dulu jalur yang sama (in-network container), lalu budget =
   measured + margin. Dan jangan percaya measurement host vs container
   identik — worker container bisa bandwidth-starved relatif ke golang
   container (gap ~7x teramati).

3. **Task periodic idempotent TETAP bisa lintas-run** (reconcile smoke):
   task yang gagal (mis. permission denied) duduk di asynq retry-set dan
   dieksekusi kapan pun syarat gagalnya hilang (grant applied) — meracing
   assertion smoke run berikutnya (stale non-dry tick menghapus seed
   sebelum dry-run assert). Smoke state-mutating harus settle queue
   (enqueue 1 tick + wait complete) SEBELUM seeding. Lihat phase
   `DRAIN` di `scripts/smoketest/phase10_reconcile/run.sh`.

4. **Column-level GRANT untuk role restricted** (`migrations/002`): sweeper
   yang butuh baca tabel di luar scope role-nya (tiktok_worker = videos
   only per SEC-03) → `GRANT SELECT (kolom_yang_dibaca) ON tabel TO role`,
   bukan full-table. Verifikasi: `has_column_privilege(role, tbl, kolom,
   'SELECT')` — kolom PII tetap `f`.

5. **psql di script**: INSERT..RETURNING via `docker exec psql -At -c`
   mengeluarkan command status (`INSERT 0 1`) MENYATU dengan RETURNING
   value → pakai `-q` + `tr -d '[:space:]'` (CR docker exec).

6. **Nginx upstream DNS di docker resolve sekali saat config load** —
   setelah gateway restart dapat IP baru (termasuk post-VPS-reboot),
   nginx pakai IP stale → semua route 502 sampai `docker compose restart
   nginx`. Symptom: curl dari DALAM container nginx 200, dari host 502.

## hls_prefix contract: TANPA trailing slash (fase 6 worker)

Worker menyimpan `videos.hls_prefix` sebagai `hls/<userID>/<videoID>` — **tanpa trailing slash**. Semua compose key R2 HARUS menambahkan separator eksplisit: `hlsPrefix + "/master.m3u8"`, `hlsPrefix + "/" + variant + "/index.m3u8"`. Dua bug fase-6 (phase-10.5/10.6) berasal dari asumsi salah soal ini (sekali lupa separator, sekali salah mengira ada trailing slash). Jangan pernah concat tanpa `"/"` eksplisit, dan jangan tambahkan check yang menuntut trailing slash.

## Ketergantungan prompt fase pada PR YA BELUM merged — verify dulu, jangan asumsikan

Prompt fase `prompts/PHASE-N-PROMPT.md` sering claim prerequisite × sudah
merged. **Selalu verify sebelum checkout branch fase** — prompt ditulis saat
fase N-1 berakhir, tapi reviewer/user mungkin belum merge refactor dependency:

```bash
gh pr list --state all --limit 5          # cek status PR prerequisite
git fetch origin && git log --oneline origin/main -3
git merge-base --is-ancestor <sha-prereq> origin/main && echo OK || echo MISSING
```

Jika prerequisite belum merged — **STOP, jangan branch.** Tanya user:
merge sekarang (kalau mereka yakin refactor sudah benar), atau tunggu. Fase 7
hampir start dengan pola `SQLDB` + `pgx.ErrNoRows` obsolete karena prompt berasumsi
PR #41 sudah merged padahal masih OPEN; user merge manual dulu baru branch dibuat.

## Smoke DSN: .env pakai placeholder `***`, ambil password live dari container

`.env` repo menyimpan `postgres://tiktok_api:***@postgres:5432/...` — literal
`***` BUKAN password. Smoke yang connect ke postgres TIDAK bisa pakai `.env`
verbatim. Recipe yang bekerja (fase 7):

```bash
PGPASS=$(docker exec mokibox-postgres printenv POSTGRES_PASSWORD)
docker run --rm --network mokibox_backend -v $PWD:/repo -w /repo \
  -e DATABASE_URL="postgres://postgres:${PGPASS}@postgres:5432/tiktok?sslmode=disable" \
  golang:1.25.5-alpine go run ./scripts/smoketest/<phase_x>
```

Superuser boleh untuk smoke karena test butuh INSERT/DELETE lintas tabel
(users + videos + likes + comments + notifications). Production code tetap
pakai role restricted.

## Notifikasi placement: DI DALAM tx untuk mutation, best-effort untuk non-tx

Pattern yang established fase 7 (like/comment/reply) vs fase 6 (follow):

- **Mutation dengan counter update** (like, comment, reply): notif HARUS
  di dalam tx yang sama dengan counter update. Atomic: kalau notif insert
  gagal, seluruh like/comment fail (roll back). Ini sesuai LLD best-practice
  "insert notif di transaction yang sama dengan operasi utama".
- **Non-mutation / independent event** (follow): notifikasi BEST-EFFORT,
  dipanggil setelah operasi utama sukses, error di-log tapi response sukses
  tetap dikembalikan. Follow tidak butuh tx (query tunggal ON CONFLICT DO
  NOTHING), jadi notif tidak mengikat apa-apa.

Jangan campur dua pola di satu aksi. Kalau unsure, tanya user sebelum implement.

## Visibility helper reuse (post testability-refactor, 2026-09-07)

`SocialHandler.assertVideoVisible` (method) sudah tidak ada —
refactor `refactor/handler-testability` (PR #52) memindahkannya ke
package-level helpers di `api-gateway/handlers/visibility.go` dengan
DUA mode eksplisit yang share sub-step owner-active + private-following:

- `assertVideoOwnerOrVisible(ctx, store, viewerID, videoID)` — pola
  social/detail: **owner bypass SEMUA check** (termasuk non-READY);
  non-owner butuh status=READY + owner aktif + following kalau private.
- `assertVideoReadyVisible(ctx, store, ownerID, viewerID)` — pola
  playlist: **NO owner bypass** pada sub-step (private owner via JWT
  gagal IsFollowing(self,self) → 404; owner konsumsi via ?token= path).
  Gate status=READY tetap di caller GetPlaylist (butuh row dulu untuk
  cek hls_prefix).

TRAP: JANGAN pernah dedup dua mode ini jadi satu fungsi — playlist
anti-enumeration (404 non-READY bahkan untuk owner) akan diam-diam
pecah. Kedua behavior di-pin oleh test
`TestGetPlaylist_NonReady404EvenForOwner` dan
`TestVisibility_PlaylistMode_PrivateOwnerJWTNoBypass`.
Semua unauthorized → 404 (anti-enumeration).
Fase berikutnya yang menyentuh video HARUS pakai helper ini (pilih
mode sesuai endpoint), jangan tulis ulang inline.

## Write-side idempotency pada tombstoned rows (fase 8+)

Setelah `MarkVideoDeleted` atau `TombstoneUser`, row tetap ada di DB
(status='DELETED' / is_active=false, deleted_at set). Endpoint write
seperti `DELETE /api/videos/:id`, `DELETE /api/comments/:id` (sudah
di fase 7), `DELETE /api/users/me` WAJIB idempotent:

- **Pre-load row, check tombstone flag, return success early.** Pola:

  ```go
  if video.Status == "DELETED" {
      // Already tombstoned. Skip UPDATE + skip
      // re-enqueue of cleanup:video (prior one
      // is still in flight for 24h).
      return c.NoContent(204)
  }
  ```

  Untuk account-delete, `TombstoneUser` returns `sql.ErrNoRows` kalau
  user sudah di-tombstone — wrap dengan `errors.Is(err, sql.ErrNoRows)`
  dan treat as 204/200 (bukan 500).

- **Race-safe** dengan handling `sql.ErrNoRows` setelah UPDATE/DELETE:
  kalau dua request bersamaan keduanya pass pre-check dan keduanya
  fire UPDATE, yang kedua dapat `sql.ErrNoRows` (0 rows affected).
  Jangan return 500 — treat as success.

- **Counter preservation**: write-side tombstone JANGAN zero counter
  columns (`likes_count`, `comments_count`). Row masih serves purpose
  selama 24h grace (status check elsewhere blocks visibility).
  Worker yang hard-delete via `DeleteVideoRow` akan cascade like +
  comment rows. Counter zero-ing belongs to the worker, not the
  tombstone handler.

## File pendukung
- `references/fase-2-implementation-notes.md` — concrete code snippets dan
  design decision dari fase 2 (sentinel table, classifyError, asynq
  marshalTask, R2 error mapping, cursor/media token wire format) untuk
  referensi fase 3+.
- `references/fase-3-implementation-notes.md` — auth middleware
  (`TokenVerifier` interface, `getOrCreateUser` race), user profile
  handler (`VideoObject` subset, 404-not-403 visibility), webhook
  handler (raw body before parse, `actions.ValidateRequestPayload`),
  plus the docker-network smoke test recipe untuk verifikasi
  signed-webhook E2E di fase 3. **Dual-check `pgx.ErrNoRows ||
  sql.ErrNoRows` di file ini historical (sudah obsolete post-PR-41)
  — lihat preamble file untuk status penuh.**
- `references/fase-4-implementation-notes.md` — confirm transaction
  pattern (`Queries.WithTx` + `BeginTx` — note: `h.SQLDB.BeginTx`
  sekarang `h.DB.BeginTx` post-PR-41), upload-intent r2_key rotation
  + best-effort cleanup, hermetic smoke test recipe via
  `golang:1.25.5-alpine --network mokibox_backend`, dan pitfall
  `git reset --hard` di session interactive yang di-block Hermes.
  **Section "Dual-pool architecture" di file ini historical (sudah
  obsolete post-PR-41) — lihat preamble file untuk status penuh.**
- `references/fase-5-implementation-notes.md` — worker struct + DI
  pattern, HandleTranscode pipeline (ffprobe → ffmpeg 480p/720p →
  thumbnail → master playlist → MarkVideoReady) + retry budget
  model, `shared.R2Client.DeletePrefix` untuk cleanup prefix tanpa
  hardcode segment filenames, worker smoke recipe (Docker build
  context ke repo root, asynq enqueue via socat port-forward), dan
  catatan inter-issue stub strategy agar tiap atomic commit bisa
  build standalone. **`Worker` struct dengan `DB *pgxpool.Pool` di
  file ini historical (sudah obsolete post-PR-41) — lihat preamble
  file untuk status penuh.**
- `references/deployment-topology.md` — kenapa Zitadel HARUS di
  sibling compose project (bukan service di `MokiBox/docker-compose.yml`),
  symptom yang muncul kalau dipaksakan (`tiktok-zitadel Restarting
  (1)`), Makefile target untuk manage 2 compose, dan env-var
  contract yang harus konsisten di kedua stack. Juga topologi
  production VPS multi-tenant (aaPanel edge, publish loopback,
  branch deploy, aturan `.env` prod). Touchpoint kalau
  ada pertanyaan "kenapa Zitadel gak satu compose", "container
  apa saja yang harus jalan di VPS production", atau eksekusi
  deploy VPS multi-tenant.
- `references/fase-10-verification-notes.md` — bukti & eksperimen
  issue #29/#39 (PR #47): bug ctx-expired `MarkVideoFailed`
  (repro timeline + root cause + fix `markFailedDetached`), tabel
  eksperimen fixture sizing (mandelbrot vs noise, throughput R2
  live), pattern worker-swap untuk timeout verification (rebuild
  image dulu), gap assertion honesty di smoke script, dan nginx
  stale-DNS 502 post-reboot.
- `references/demo-ui.md` — pattern single-page browser UI untuk
  manual E2E demo semua endpoint (fase-10 deliverable):
  PKCE S256 SPA flow + nginx /callback 302 ke UI + **HLS player
  auth via `?token=` URL, BUKAN Authorization header di
  hls.js (xhrSetup meneruskan header ke segment R2 presigned
  → 400 SignatureDoesNotMatch yang tampak seperti CORS error;
  live-verified 2026-09-06, lihat skill `presigned-object-urls`)**
  + error handling yang tunjukkan pesan konkret (bukan silent
  fail) + asumsi live yang harus diverifikasi (redirect_uri,
  SPA client_id via config.js di .gitignore, port conflict
  dengan reverse proxy host).

## Maintenance: pattern setelah library/wiring dihapus dari production

Skill ini punya banyak pitfall section yang ditulis untuk
**ground truth saat section itu ditulis**. Pola yang sudah terjadi
di MokiBox: refactor cross-cutting (mis. PR #41 `refactor/pool-consolidation`)
menghapus library/wiring (`*pgxpool.Pool` dari production) → pitfall
section di SKILL.md + `references/*.md` yang membahas library
tersebut jadi misleading. Tanpa maintenance, fase agent berikutnya
yang load skill akan lihat pattern obsolete sebagai best practice.

**Pola maintenance yang dipakai MokiBox saat library/wiring dihapus**:

1. **SKILL.md pitfall section yang TIDAK LAGI relevan** → dua pilihan:
   - (a) Rewrite section dengan ground truth baru (mis. §"database/sql
     + sqlc" di-rewrite dari dual-check jadi `sql.ErrNoRows` tunggal).
   - (b) Keep section + tambah blockquote note di paling atas:
     "MokiBox post-PR-N tidak lagi pakai X di production code.
     Section ini dipertahankan untuk referensi jika fase berikutnya
     butuh reintroduce." (mis. §"pgxpool" yang masih dipakai sebagai
     referensi API untuk potential future pgx.Tx migration).
   - Pilih (a) kalau section mengajarkan pattern yang masih
     dipakai dalam bentuk berbeda (mis. db pattern masih relevan
     walaupun pgxpool diganti sql.DB).
   - Pilih (b) kalau section mengajarkan API library yang masih
     valid sebagai konsep, hanya saja MokiBox tidak pakai
     sementara.

2. **`references/*.md` file** → tambah preamble di paling atas
   (di bawah `# Title` + deskripsi 1-paragraph) yang menyatakan
   status obsolete + rujukan ke ground truth. Contoh (dari
   PR #41 post-merge):
   ```
   > **Status post-PR-41 (`refactor/pool-consolidation`, commit `69bac5c`)**:
   > Section "Dual-pool architecture" di bawah ini **sudah obsolete**.
   > MokiBox sekarang single `*sql.DB` pool — `*pgxpool.Pool` dihapus
   > total, `RouterDeps.SQLDB` di-rename ke `RouterDeps.DB`. Untuk
   > ground truth lihat `CONVENTIONS.md` section "Single-pool
   > architecture".
   ```
   - Pertahankan body section sebagai historical reference (jangan
     hapus) — fase agent akan baca warning dulu sebelum content,
     sehingga tidak akan tertipu copy-paste pola obsolete.
   - Update SKILL.md "File pendukung" description untuk masing-masing
     reference file dengan note: "**[obsolete content] lihat preamble**".

3. **Pre-merge planning doc + skill maintenance** — refactor yang
   triggered maintenance ini biasanya juga menghasilkan planning
   doc untracked (`PLAN_<TOPIC>.md`). Planning doc itu yang
   mendokumentasikan "kenapa library dihapus" + "ground truth
   baru". Reference ke planning doc harus muncul di skill update
   (mis. CONVENTIONS.md note tentang refactor, atau skill section
   yang baru).

**Kapan pakai pola ini**: setiap kali refactor cross-cutting
(`refactor/<topic>` branch, di luar fase) menghapus/mengganti
library atau wiring struct field. Cek apakah SKILL.md punya pitfall
section yang membahas library tsb → patch dengan pola (a) atau (b)
+ cek `references/*.md` → tambah preamble obsolete note.

**Kapan TIDAK pakai**: kalau perubahan confined ke satu fase saja
(mis. tambah query sqlc baru di fase 7), tidak perlu maintenance
skill karena pitfall section yang affected belum tentu ada.
