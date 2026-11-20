# Fase 5 Implementation Notes — Transcoder Worker

Concrete design decisions dan code patterns dari fase 5 (PR #38) yang
jadi referensi fase 6+ (feed, follow, like, dll — semuanya akan
reuse pola DI + asynq + R2 yang didefine di sini). Melengkapi
`fase-2-implementation-notes.md` + `fase-3-implementation-notes.md` +
`fase-4-implementation-notes.md`.

> **Status post-PR-41 (`refactor/pool-consolidation`)**: `Worker`
> struct di bawah (`DB *pgxpool.Pool` + `SQLDB *sql.DB`) **sudah
> obsolete**. Sekarang `Worker.DB *sql.DB` saja (sebelumnya
> `Worker.SQLDB`), `*pgxpool.Pool` sudah dihapus total. Pola
> `tx, err := w.DB.BeginTx(ctx, nil)` + `defer tx.Rollback()` +
> `qtx := w.Queries.WithTx(tx)` masih dipakai fase 7+ — itu yang
> ditiru. Untuk ground truth lihat `CONVENTIONS.md` section
> "Single-pool architecture" + `transcoder-worker/main.go`.

## Worker struct + dependency injection

`transcoder-worker/main.go` mendefinisikan satu struct besar yang
inject SEMUA dependency ke handler. Pattern sama dengan handler
di `api-gateway`, tapi tidak ada HTTP request — worker ambil job
dari Redis queue dan return `error` ke asynq.

```go
type Worker struct {
    DB      *pgxpool.Pool       // pool pgx (saat ini gak dipakai langsung, simetris dgn api-gateway)
    SQLDB   *sql.DB             // untuk sqlc Queries.WithTx kalau ada handler yg butuh tx
    Queries *db.Queries         // sqlc-generated, source of truth untuk semua DB read/write
    R2      *shared.R2Client    // download raw, upload HLS, delete objects
    Asynq   *asynq.Client       // untuk enqueue retry + cleanup (worker juga producer, bukan cuma consumer)
    Cfg     *shared.WorkerConfig
    Logger  *slog.Logger
}

func NewWorker(ctx context.Context, cfg *shared.WorkerConfig, logger *slog.Logger) (*Worker, error) {
    // Validasi cfg + logger di awal constructor (fail-fast pattern, sama dengan
    // api-gateway/handlers/video.go).
    // shared.NewDB(ctx, url, maxConns) → pgxpool.
    // sql.Open("pgx", url)            → *sql.DB.
    // shared.NewR2Client + shared.NewAsynqClient → concrete structs.
    return &Worker{...}, nil
}
```

**Kenapa `*pgxpool.Pool` disimpan kalau gak dipakai?** Saat ini
handler tidak pakai langsung — semua lewat `Queries`. Tapi simetris
dengan `api-gateway` yang sudah punya pola dual-pool, dan `*sql.DB`
saja tidak cukup untuk handler yang butuh raw `*pgxpool.Pool.Query`
atau `Begin(ctx)`. Biarkan untuk forward-compat, daripada refactor
nanti saat fase 6 butuh.

**Kenapa worker juga `*asynq.Client` (producer), bukan cuma
consumer?** HandleTranscode saat fail transient perlu enqueue ulang
task yang sama (`ProcessIn(30s * retry_count)`). HandleTranscode
juga enqueue `cleanup:objects` setelah sukses sebagai fire-and-forget.
HandleCleanupObjects/Video mungkin perlu enqueue follow-up
(sub-cleanup task). Tanpa `*asynq.Client`, worker ini read-only dan
tidak bisa self-heal atau cleanup — fatal.

## Handler-stub strategy untuk atomic-commit per issue

Fase 5 punya 4 issue dengan dependency order: Worker struct (issue A)
→ HandleTranscode (C) + HandleCleanupObjects/Video (D). Tapi atomic-
commit rule: 1 issue = 1 commit, dan tiap commit harus build standalone.

Solusi yang dipakai: **stub handler bodies di main.go saat issue A,
replace dengan real body di file masing-masing (transcode.go /
cleanup.go) di issue C / D**. Stub body cuma log + return nil:

```go
// main.go, selama issue A belum punya transcode.go / cleanup.go:
func (w *Worker) HandleTranscode(ctx context.Context, t *asynq.Task) error {
    w.Logger.Warn("HandleTranscode stub: pipeline not yet wired (issue C)",
        "task_type", t.Type())
    return nil
}
```

Saat issue C commit, file `transcode.go` define method
`HandleTranscode` (real body), dan stub di main.go di-remove. Method
signature identik, jadi `mux.HandleFunc(...)` di `registerHandlers`
tidak berubah. Idem untuk issue D dengan `cleanup.go`.

**Catatan penting**: stub tetap di main.go (bukan file terpisah)
supaya tiap commit benar-benar self-contained — kalau handler
dipindah ke cleanup.go saat issue C, issue C tiba-tiba punya
cleanup.go (file yang logically milik issue D). Itu pelanggaran
atomic-commit karena commit C menyentuh file yang bukan miliknya.
Stub di main.go = 1 file per issue = atomic.

Alternatif yang ditolak: taruh 3 stub di `cleanup.go` dari awal.
Itu memaksa issue C menyentuh file D, dan `cleanup.go` di-commit
sebagai bagian issue C walaupun logic-nya masih stub. Lebih buruk
daripada stub di main.go karena混淆 ownership.

## HandleTranscode: full pipeline + retry budget

Pipeline ada di `transcoder-worker/transcode.go`. Flow lengkap
(LLD section 8 + PRD FR-VIDEO-04..09):

```
1. unmarshal payload (defence-in-depth nil checks w.Queries/w.R2/w.Asynq)
2. GetVideoByID — sql.ErrNoRows OR pgx.ErrNoRows OR status != PROCESSING → return nil (no retry)
3. IncrementVideoRetry (atomic bump); retry_count > 3 → MarkVideoFailed + cleanup raw + return nil
4. Mkdir /tmp/transcode/<videoID>-<random> (random suffix = parallel-safety, jangan bocorin UUID)
5. R2.Download raw
6. ffprobe + ValidateMedia. ErrInvalidMedia → MarkVideoFailed + cleanup raw + return nil (permanent).
   Other ffprobe errors → transient.
7. ffmpeg 480p + 720p sequential (single-VPS, parallel = CPU peg). Upload per variant sebelum lanjut,
   sehingga partial failure punya recoverable state.
8. ffmpeg thumbnail -ss 00:00:01 -frames:v 1.
9. Build master.m3u8 lokal (writeMasterPlaylist helper) → upload application/vnd.apple.mpegurl.
10. Re-check status: kalau status != PROCESSING atau row hilang, cleanup partial uploads + return nil.
    Race window protection: antara MarkVideoReady dan delete-video / delete-account, ada window
    singkat di mana R2 sudah punya orphan files. Cleanup di sini bikin R2 storage tight selama race.
11. MarkVideoReady (WHERE status='PROCESSING' guard, idempotent).
12. Enqueue cleanup:objects untuk raw R2 key.
```

**Retry budget model**:

```go
const MaxRetries = 3
func retryDelayFor(retryCount int32) time.Duration {
    return time.Duration(retryCount) * 30 * time.Second
}
```

`IncrementVideoRetry` jalan di step 3, SEBELUM kerja apapun. Jadi
`retry_count` di akhir attempt ke-N adalah N. Cek `retry_count >= 3`
artinya "sudah attempt ke-3, jangan ulangi lagi". Sama dengan PRD
FR-VIDEO-08: "retry maksimal 3 kali".

**Two-layer retry** (sama dengan fase 4 handler):

- **asynq.MaxRetry(1) di producer** = queue-level safety net untuk
  Redis blip saat pickup. Setelah 1 retry, asynq drop ke dead queue.
- **App-level retry budget 3x** (di worker, code di atas) = handle
  transient ffmpeg / R2 failure yang real. IncrementVideoRetry +
  enqueue ulang `transcode:video` dengan `ProcessIn(30s * retry_count)`.

Handler return `nil` di SEMUA branch (sukses / permanent fail /
budget exhausted) supaya asynq tidak double-count dengan retry-nya
sendiri. Kalau return error → asynq retry sekali lagi (MaxRetry(1))
yang akan trigger IncrementVideoRetry ke-2 → MarkVideoFailed pada
attempt 2 (bukan 3). Untuk handler yang pure transient retry,
return `error` ke asynq TIDAK dipakai — handler selalu return nil
dan manage retry budget sendiri.

## Cleanup task pipelines

### HandleCleanupObjects (simple)

Cuma marshal payload + `R2Client.DeleteObjects`. Idempotent karena
DeleteObjects swallow NotFound. Empty keys = no-op (return nil,
bukan error). Non-NotFound R2 error → return error → asynq retry.

### HandleCleanupVideo (DELETED + 24h grace)

```go
const cleanupGrace = 24 * time.Hour
```

Logic:

1. Load row. `sql.ErrNoRows` / `pgx.ErrNoRows` → skip (sudah dihapus
   attempt sebelumnya).
2. `status != "DELETED"` → skip (api-gateway adalah source of truth,
   worker jangan overwrite).
3. `!deletedAt.Valid` → skip dengan warn (chk_videos_deleted_at
   violation, defence-in-depth).
4. `elapsed < cleanupGrace` → re-enqueue dengan `ProcessIn(remaining)`
   supaya next attempt pas pas 24h elapsed.
5. `elapsed >= cleanupGrace` → collect keys, delete, hard-delete row.

**Key collection** (`R2Client.DeletePrefix`):

- `r2_key` (raw upload) → DeleteObjects.
- `hls_prefix + "/"` → `R2Client.DeletePrefix(ctx, hlsPrefix+"/")`.
- `thumbnail_key` → DeleteObjects.

`hls_prefix` di DB disimpan tanpa trailing slash (mis.
`hls/<uid>/<vid>`). Trailing slash ditambah di sini karena
DeletePrefix HARUS berakhir dengan `/` (lihat `shared/r2.go`
constructor).

## `shared.R2Client.DeletePrefix`

Method baru di fase 5 yang reusable untuk cleanup path apapun yang
perlu hapus semua key di bawah prefix. Tanpa ini, cleanup path
hardcode segment filenames (`segment_0000.ts`, `segment_0001.ts`,
...) — fragile kalau ffmpeg naming berubah atau jumlah segment
berubah.

```go
func (c *R2Client) DeletePrefix(ctx context.Context, prefix string) error {
    if !strings.HasSuffix(prefix, "/") {
        return fmt.Errorf("DeletePrefix: prefix %q must end with '/'", prefix)
    }
    // ListObjectsV2 paginator + DeleteObjects batch (max 1000/request per AWS spec)
    // NotFound keys silently skipped (idempotent).
}
```

**Kenapa trailing slash wajib**: ListObjectsV2 prefix matching is
substring match, bukan boundary-aware. Tanpa `/`, `hls/u/v1/` akan
match `hls/u/v11/...` (sibling prefix). Slash = explicit boundary.

**Reuse potensial fase 6+**: delete-account (fase 8) juga butuh
hapus HLS per-user — bisa pakai `DeletePrefix("hls/<uid>/")` bukan
loop per video.

## Smoke test pattern: Docker build + in-network enqueue

Worker tidak punya HTTP listener (SEC-02: tidak expose port). Cara
verify end-to-end:

1. `docker compose build transcoder-worker` — verify multi-stage build jalan.
2. `docker compose up -d transcoder-worker` — verify container start, subscribe Redis, connect Postgres.
3. `docker inspect mokibox-transcoder-worker --format 'User=... CapDrop=... TmpFs=...'` — verify SEC-02 hardening.
4. `docker run --rm --entrypoint /bin/sh mokibox-transcoder-worker:dev -c "ffmpeg -version"` — verify ffmpeg ada di image.
5. **In-network enqueue via socat** (Redis/Postgres tidak expose host port):
   ```bash
   docker run -d --name socat-redis --network mokibox_backend -p 16379:6379 \
       alpine:3.19 sh -c "apk add --no-cache socat && socat TCP-LISTEN:6379,fork,reuseaddr TCP:redis:6379"
   REDIS_ADDR=localhost:16379 REDIS_PASSWORD=... \
       go run ./scripts/smoketest/phase5_enqueue -op transcode -videoID <uuid>
   ```
6. Tunggu, `docker logs mokibox-transcoder-worker --tail 20` — verify
   handler log + DB row update + R2 object count.

Recipe lengkap smoke + verification ada di `scripts/smoketest/phase5_enqueue/`
dan `scripts/smoketest/r2_list/` (commit terpisah, dev-only — tidak
masuk PR review scope).

## Pitfall yang tertangkap fase 5

### Docker build context salah

Lihat SKILL.md section "Docker build context untuk multi-package Go
module". Symptom: `ERROR: failed to calculate checksum of ref ...
"/go.sum": not found`. Fix: `context: .` di compose, bukan
`context: ./transcoder-worker`. Reusable untuk fase 9.

### `go build ./transcoder-worker` di host: binary name clash dengan directory

`go build ./transcoder-worker` tanpa `-o` error:

```
go: build output "transcoder-worker" already exists and is a directory
```

Fix: `go build -o /tmp/worker ./transcoder-worker`. Pattern ini aman
untuk semua service yang foldernya sama dengan output binary name.
Makefile `build-worker` target pakai `-o bin/transcoder-worker` jadi
aman.

### `docker exec mokibox-transcoder-worker` tanpa `-T` / `--env` flag

Container `transcoder-worker` jalan sebagai `app` (UID 10001) yang
non-interactive. Default `docker exec` interactive prompt jadi bingung
kalau pipe. Pakai `docker exec -T` untuk non-TTY, atau panggil via
`docker exec mokibox-transcoder-worker /bin/sh -c "command"`.

### Race antara MarkVideoReady dan delete-video

Pipeline punya `re-check status` (step 10) yang catch race dengan
delete-video / delete-account. Penting: race window ada antara
`GetVideoByID` di step 2 dan `MarkVideoReady` di step 11 — bisa
ada `MarkVideoDeleted` di tengah. Solusi:

- Step 10 re-check setelah upload — kalau sudah DELETED, cleanup
  uploads + return nil tanpa flip status.
- `MarkVideoReady` query `WHERE id=$1 AND status='PROCESSING'` guard
  supaya DELETE yang landing setelah step 10 juga di-handle
  (return sql.ErrNoRows → handler log "already moved" + return nil).

Tanpa dua lapis ini, orphan HLS bisa accumulate kalau user spam
delete di tengah transcode. Race window detection + cleanup = bagus
untuk R2 storage hygiene.

### `ffprobe` exit non-zero = bukan selalu transient

`ProbeFile` exit non-zero bisa karena:
- File tidak ada (mount issue, race dengan download) → transient.
- File malformed container / no streams (security reject) → PERMANENT.
- Parser crash (ffmpeg CVE-level) → permanent, jangan retry.

Pattern yang dipakai: **semua non-zero exit di-wrap sebagai
`ErrInvalidMedia`** (sentinel), lalu handler cek `errors.Is(err,
ErrInvalidMedia)` → branch permanent (`MarkVideoFailed` + cleanup
raw + return nil). Non-`ErrInvalidMedia` error dari ProbeFile
(subprocess tidak bisa di-execute, ctx cancelled, I/O) → branch
transient (`handleTransient` → retry budget check).

Trade-off: ini berarti file yang "bukan video tapi ffprobe
crash-nya aneh" bisa di-mark failed bukan retried. Untuk MVP ini OK
karena worker tidak boleh blast ffmpeg pada file yang sama 3x
(cost CPU +95% per attempt). Failure fast + MarkFailed → user
lihat FAILED status → re-upload kalau mau.

## Per-file count untuk self-report di PR #38

Total `R2Client` methods (count untuk verifikasi self-report):

- `Bucket` (1 accessor)
- `PresignPut`, `PresignGet`, `HeadObject`, `DeleteObjects`,
  `Download`, `UploadFile` (6 ops existing dari fase 2)
- `DeletePrefix` (1 op baru fase 5)
- = 8 total

PR #38 body bilang "R2Client methods: 8" — diverifikasi via
`grep -E '^func \(c \*R2Client\)' shared/r2.go | wc -l`.

`MaxRetries = 3` adalah konstanta yang sama dengan PRD FR-VIDEO-08.
Handler cek `retry_count >= MaxRetries` (LLD literal: cek SEBELUM
increment). Setelah post-review fix `6cf0640`, urutan di `transcode.go`:
load row → cek `video.RetryCount >= 3` → kalau lewat budget, MarkVideoFailed
+ cleanup raw + return nil (no increment). Kalau masih di bawah
budget, baru `IncrementVideoRetry` lalu proses. Lihat
"Post-review fixes" section di bawah untuk rationale.

## Reuse potensial fase 6+

- **Worker DI struct** — pola `Worker` (DB, Queries, R2, Asynq, Logger)
  akan reuse untuk handler sosial fase 7 (like, comment, notification).
  Bedanya handler sosial dipanggil via HTTP request (melalui api-gateway),
  bukan dari asynq. Tapi DI-nya sama.
- **`R2Client.DeletePrefix`** — fase 8 delete-account butuh hapus
  semua HLS + thumbnails untuk satu user. Pakai `DeletePrefix("hls/<uid>/")`
  + `DeletePrefix("thumbs/<uid>/")` daripada loop per video.
- **Cleanup-via-enqueue pattern** — handler apapun yang mau hapus R2
  enqueue `cleanup:objects` task, jangan panggil `DeleteObjects`
  langsung. Konsisten dengan fase 4 confirm handler (size invalid)
  dan fase 5 transcode handler (raw cleanup post-success).
- **Worker smoke recipe** — fase 6+ yang tambah task type baru
  (mis. fase 7 butuh `notification:push`) cukup copy-paste pattern
  `mux.HandleFunc(taskType, w.Handler)` + stub handler dulu, lalu
  real handler di issue berikutnya.

## Post-review fixes (commit `6cf0640`, di luar 4 issue utama)

PR #38 review menemukan tiga hal yang tidak terkait scope utama
tapi harus di-fix sebelum merge. Semuanya dimuat dalam satu commit
additive (non-destructive, per preferensi user terhadap history rewrite
setelah PR open).

### 1. Bucket-wipe defense di `shared.R2Client.DeletePrefix`

Constructor guard original hanya cek `!HasSuffix(prefix, "/")`,
yang menerima prefix `"/"` atau `"//"` lolos. ListObjectsV2 dengan
`Prefix="/"` atau `Prefix="//"` mengembalikan **setiap object di
bucket** (S3 list-keys behavior: empty prefix = bucket root). Kalau
suatu caller malformed — mis. admin edit row DB dengan `hls_prefix="/"`
atau concatenation `String+"/"` dimana `String=""` menghasilkan
`"/"` — `DeletePrefix("/")` akan match semua key dan batch-delete
mereka. **Bucket-wipe bug kelas produksi.**

Fix berlapis:
- `DeletePrefix` sendiri reject: `strings.Trim(prefix, "/") == ""`
  → tolak `"/"`, `"//"`, `"///"`, dst dengan error eksplisit
  (`"prefix %q is too broad (would match bucket root)"`).
- `HandleCleanupVideo` caller-side: tambah `strings.HasPrefix(prefix,
  "hls/")` → tolak prefix yang tidak dimulai `hls/`. Belt-and-braces
  kalau future refactor memindahkan call site.

Verified runtime:
- Unit test `shared/r2_delete_prefix_test.go` (3 cases): empty
  prefix, no-trailing-slash, slash-only prefixes semua rejected.
- Runtime smoke dengan R2Client langsung: `DeletePrefix("/")`,
  `DeletePrefix("//")`, `DeletePrefix("///")` return error
  eksplisit. Bucket contents sebelum / sesudah: identik (7
  unrelated objects intact).
- End-to-end smoke: DELETED row dengan `hls_prefix=NULL` →
  `HandleCleanupVideo` skip DeletePrefix branch (NULL valid=false),
  cleanup r2_key biasa, bucket contents intact.

### 2. Retry-budget check ordering (LLD literal compliance)

Original implementation increment dulu, baru cek `> MaxRetries`
(strict greater-than). LLD section 8 line 726-727 literal:
"retry_count >= 3 → set FAILED, return nil" — **cek SEBELUM**
increment. Reorder agar konsisten dengan spec.

Matematika attempt sama (3 attempt max, lalu FAILED di attempt 4)
tapi sekarang DB column tidak menerima bump ke-4 yang sia-sia
kalau task re-delivered setelah budget habis.

Test pin: `TestMaxRetriesConstant = 3` (gagal kalau future edit
silent-ubah budget) + `TestRetryDelayFor` (formula `30s * retry_count`).

### 3. Failure-path smoke (carry-over dari review)

Review PR #38: smoke yang dilaporkan cuma happy path (32 KB file
selesai 6 detik). Failure paths (timeout kill, invalid media, retry
exhausted) ditandai ✅ tanpa pernah dieksekusi end-to-end.

Di-rerun di session yang sama, setelah fix commit:

- **Invalid-media path**: 0.8s mp4 (below 1s minimum) → worker logs
  `"validate media failed, marked FAILED err=\"invalid media file:
  duration 0.800s outside allowed [1, 180]\""`, status=FAILED,
  retry_count=1, raw key cleaned.
- **Retry-exhausted path**: row dengan `retry_count=3` → handler
  logs `"retry budget exhausted, marked FAILED retry_count=3"`,
  status=FAILED, retry_count stays at 3 (no spurious 4th bump).
- **Timeout-kill path**: TIDAK dieksekusi (butuh restart worker
  dengan `TRANSCODE_TIMEOUT=2s` yang akan invalidate smoke lain).
  Didokumentasikan di commit + GH issue #39 carry ke fase 10.

### PR body integrity lesson

Saat patch PR body via `gh api -X PATCH ... -f body=@file`, body TIDAK
terbaca dari file — `-f` adalah form-field literal dan Git menyimpan
`@file` sebagai string body. Yang benar: `--input /tmp/payload.json`
dimana payload.json adalah `{"body": "..."}`. Lihat
`git-phase-issue-workflow` "Mengirim file sebagai body JSON via
gh api" pitfall.