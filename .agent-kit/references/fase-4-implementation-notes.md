# Fase 4 Implementation Notes — Upload Intent & Confirm

Concrete design decisions dan code patterns dari fase 4 (PR #37) yang
jadi referensi fase 5+. Melengkapi `fase-2-implementation-notes.md` +
`fase-3-implementation-notes.md`.

> **Status post-PR-41 (`refactor/pool-consolidation`, commit `69bac5c`,
> 2026-08-31)**: Section "Dual-pool architecture" di bawah ini
> **sudah obsolete**. MokiBox sekarang single `*sql.DB` pool — `*pgxpool.Pool`
> dihapus total, `RouterDeps.SQLDB` di-rename ke `RouterDeps.DB`, dan
> 23 sentinel sites diseragamkan ke `sql.ErrNoRows` (tidak ada lagi
> dual-check `pgx.ErrNoRows || sql.ErrNoRows`).
>
> **Pertahankan section ini sebagai historical reference**: pola
> `tx, err := h.DB.BeginTx(ctx, nil)` + `defer tx.Rollback()` +
> `qtx := h.Queries.WithTx(tx)` masih dipakai fase 7+ — itu yang
> ditiru. Yang obsolete: `h.SQLDB.BeginTx` (ganti `h.DB.BeginTx`),
> dual-check `pgx.ErrNoRows` (hapus `pgx.ErrNoRows` half, pakai
> `sql.ErrNoRows` saja). Untuk ground truth lihat `CONVENTIONS.md`
> section "Single-pool architecture" + `shared/db.go`.

## Dual-pool architecture: `*pgxpool.Pool` + `*sql.DB`

Phase 4 introduce pola baru di `RouterDeps`: **dua field pool untuk
database yang sama**.

```go
type RouterDeps struct {
    DB      *pgxpool.Pool   // existing — dipakai user handler
    Queries *db.Queries
    SQLDB   *sql.DB         // NEW — dipakai video handler confirm tx
    // ...
}
```

**Kenapa dua-duanya?** sqlc-generated `Queries.WithTx(tx *sql.Tx)`
cuma terima `*sql.Tx` dari `database/sql`. `*pgxpool.Pool.Begin(ctx)`
return `pgx.Tx` yang **bukan** `*sql.Tx` — tidak bisa di-bridge
dengan type assertion sederhana.

Pilihan yang diambil fase 4: tambah `SQLDB *sql.DB` (di-open via
`pgx/v5/stdlib`, `sql.Open("pgx", databaseURL)`) terpisah dari
existing `DB *pgxpool.Pool`. Alasan:

- **Minimum change** — handler read-only lain (user, feed, video
  detail) tidak ter-refactor; cukup tambah wiring di main.go dan
  satu field di `RouterDeps`. Bandingkan dengan alternatif: switch
  sqlc ke driver `pgx/v5` (regenerate `shared/db/`, semua handler
  yang baca `sql.NullString`/`sql.NullTime` harus adapt ke pgtype
  atau pointer). Itu multi-PR yang bukan net value untuk MVP.
- **Surgical addition** — video confirm transaction adalah satu
  path; dual-pool lokal di file itu lebih aman dari migrasi global.

**Catatan rasionalisasi yang setengah benar di CONVENTIONS.md**:

CONVENTIONS.md section "Dual-pool architecture" menyatakan:

> A separate pool is intentional so the user handler's ErrNoRows
> semantics (pgx.ErrNoRows) stay correct.

Klausa itu **tidak akurat**. Cek `api-gateway/main.go`:
`queries := db.New(sqlDB)` (line 121) — `Queries` selalu di-construct
dari `*sql.DB`, jadi **semua** `*db.Queries` method (tx maupun
non-tx) di MokiBox saat ini surface `sql.ErrNoRows` dari
`QueryRowContext().Scan()`. pgxpool (`DB` di RouterDeps) di-pass
ke `UserHandler.DB` tapi tidak dipakai untuk query (lihat
`user.go:48` — field exists tapi handler baca via `h.Queries.*`).

Kalau rasionalisasinya "ErrNoRows stay correct", yang sebenarnya
stay correct adalah: `sql.ErrNoRows` (untuk `Queries`), bukan
`pgx.ErrNoRows`. Pola dual-check yang CONVENTIONS.md rekomendasikan
(`errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows)`)
adalah defence-in-depth yang benar — bukan karena pgxpool dipakai
untuk query, tapi supaya handler tetap benar kalau wiring pool
berubah di refactor berikutnya.

**Tradeoff**: dua pool = dua koneksi physical ke Postgres yang
sebenarnya sama. Untuk single-VPS MVP, ini tidak masalah (max
conns 10 untuk API). Phase 9 akan konsolidasi kalau scale berubah.

**Bug fase 6 yang follow-up-nya fase 7+**: `user_follow.go:FollowUser`
line 95 hanya check `pgx.ErrNoRows`, tidak dual-check. Polanya
identik dengan fase 3 webhook bug yang sudah difix. Saat target
user tidak ada, handler return 500 (salah) bukan 404. Fix: tambah
`|| errors.Is(err, sql.ErrNoRows)`. Lihat SKILL.md section
"`database/sql` + sqlc" untuk detail dan audit pattern.

Pattern membuka `*sql.DB` di main.go (lihat PR #37):

```go
import _ "github.com/jackc/pgx/v5/stdlib"  // driver

sqlDB, err := sql.Open("pgx", cfg.DatabaseURL)
if err != nil { log.Fatalf("...") }
sqlDB.SetMaxOpenConns(int(shared.APIPoolMaxConns))
sqlDB.SetMaxIdleConns(2)
sqlDB.SetConnMaxLifetime(30 * time.Minute)
defer sqlDB.Close()
pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
if err := sqlDB.PingContext(pingCtx); err != nil { /* fatal */ }

queries := db.New(sqlDB)  // *db.Queries sekarang surface sql.ErrNoRows
```

## Confirm transaction pattern

`api-gateway/handlers/video.go:ConfirmUpload` adalah handler
transaksional pertama MokiBox. Pattern lengkap:

```go
tx, err := h.SQLDB.BeginTx(ctx, nil)
if err != nil { /* ErrInternal */ }
defer func() { _ = tx.Rollback() }()  // no-op setelah Commit

qtx := h.Queries.WithTx(tx)
video, err := qtx.GetVideoByIDForUpdate(ctx, videoID)
if err != nil {
    if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
        return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "..."))
    }
    /* ErrInternal */
}
// validasi: ownership, status, r2_key match
size, _ := h.R2.HeadObject(ctx, video.R2Key)
// validasi: size 1KB..200MB
confirmed, _ := qtx.ConfirmVideoProcessing(ctx, db.ConfirmVideoProcessingParams{...})
info, qerr := shared.EnqueueTranscode(h.Queue, ...)
if qerr != nil {
    _ = tx.Rollback()           // explicit: stay PENDING_UPLOAD
    return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "enqueue"))
}
if err := tx.Commit(); err != nil { /* ErrInternal */ }
return shared.RespondOK(c, ...)
```

**Critical decisions**:

1. **Check both `sql.ErrNoRows` dan `pgx.ErrNoRows`** — `WithTx(tx)`
   bikin sqlc pakai `database/sql.Tx`/`*sql.DB`, tapi defence-in-depth
   butuh dua-duanya. Pattern ini akan reuse di fase 5+ untuk setiap
   handler yang read dalam tx.

2. **`defer tx.Rollback()` aman dipanggil kapan saja** — `sql.Tx.Rollback`
   return `ErrTxDone` kalau sudah di-commit, tapi `defer` pattern
   standar adalah `_ = tx.Rollback()` (ignore error). Kalau enqueue
   gagal sebelum Commit, Rollback eksplisit sebelum return untuk
   kejelasan intent.

3. **Enqueue di LUAR tx commit**, TAPI masih di dalam defer-rollback
   scope — kalau enqueue gagal, rollback eksplisit sebelum return.
   Jangan pakai tx-rollback untuk silent-rollback enqueue failure.

4. **Tx tetap di-commit kalau size invalid** (sengaja, beda dari
   kebanyakan pattern). Alasan: status tetap `PENDING_UPLOAD` (karena
   `ConfirmVideoProcessing` tidak dipanggil), user bisa retry upload
   tanpa harus upload-intent ulang. Cleanup enqueue best-effort.

5. **`ConfirmVideoProcessing`** query sudah include guard
   `AND status = 'PENDING_UPLOAD' AND r2_key = $3` di SQL — kalau
   ada race antara `GetVideoByIDForUpdate` dan update ini, query
   return `sql.ErrNoRows` dan handler map ke `VIDEO_STATUS_CONFLICT`
   (409). Pattern: defence-in-depth di SQL layer + validasi di Go
   layer.

## UploadIntent: r2_key rotation + best-effort cleanup

Per LLD section 7, `UploadIntent` harus handle dua kasus:
- Tidak ada `PENDING_UPLOAD` → INSERT baru, return 201.
- Ada `PENDING_UPLOAD` → reuse ID existing, rotate r2_key, return 200.

Pattern yang dipakai:

```go
existing, err := h.Queries.GetPendingVideoByUser(ctx, user.ID)
if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
    // Branch 1: create new
    videoID = uuid.New()
    r2Key = uploadKey(user.ID, videoID)
    inserted, _ := h.Queries.InsertVideo(ctx, db.InsertVideoParams{
        UserID: user.ID, R2Key: r2Key,
        Title: nullString(title), Description: nullString(desc),
    })
    created = true
} else if err != nil {
    return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load pending"))
} else {
    // Branch 2: rotate
    oldKey = existing.R2Key
    videoID = existing.ID
    r2Key = uploadKey(user.ID, existing.ID)
    if oldKey != r2Key {
        updated, _ := h.Queries.UpdatePendingVideoR2Key(ctx, db.UpdatePendingVideoR2KeyParams{
            ID: existing.ID, R2Key: r2Key,
        })
        videoID = updated.ID
    }
    // (deferred cleanup best-effort)

// Refresh metadata (title/description) on the existing
// PENDING_UPLOAD row. Both UpdatePendingVideoR2Key and
// UpdatePendingVideoMetadata are guarded by
// status = 'PENDING_UPLOAD' so a concurrent /confirm
// in another tab surfaces as sql.ErrNoRows — the
// handler maps that to VIDEO_STATUS_CONFLICT (409)
// instead of silently mutating a PROCESSING row.
if _, merr := h.Queries.UpdatePendingVideoMetadata(ctx, db.UpdatePendingVideoMetadataParams{
    ID: existing.ID,
    Title: nullString(title), Description: nullString(desc),
}); merr != nil {
    if errors.Is(merr, sql.ErrNoRows) || errors.Is(merr, pgx.ErrNoRows) {
        return shared.RespondError(c, shared.Wrap(shared.ErrVideoStatusConflict, "..."))
    }
    /* ErrInternal */
}
}

// Presign + RespondCreated/RespondOK
url, _ := h.R2.PresignPut(ctx, r2Key, uploadContentType, expiry)
if !created && oldKey != "" && oldKey != r2Key {
    if _, qerr := shared.EnqueueCleanupObjects(h.Queue, shared.CleanupObjectsPayload{
        Keys: []string{oldKey},
    }); qerr != nil {
        slog.Warn("enqueue cleanup old upload key failed", ...)
        // TIDAK fail handler — orphan akan di-pickup path lain.
    }
}
```

**Note**: as of commit `f82ee02` (PR #37 followup) title and
description ARE refreshed on the reuse path. The original
deviation was closed by adding a new sqlc query
`UpdatePendingVideoMetadata` in `sqlc/queries/videos.sql` +
regenerate + call both in handler. This was the right call —
LLD section 7 explicitly says "UpdatePendingVideoR2Key + update
title/description", so the deviation was a spec mismatch that
should have been caught at the original commit. Lesson: when
the LLD mentions an action the ISSUES.json acceptance criteria
omit, treat the LLD as the source of truth and fix the gap
inline rather than carrying it as a deviation.

## Constructor nil-check pattern (extended)

Phase 4 extend pattern fase 3 (defence-in-depth nil check di method)
ke **constructor return error**. `NewVideoHandler` validate semua
dependency di awal dan return error kalau ada yang nil:

```go
func NewVideoHandler(queries *db.Queries, sqldb *sql.DB, r2 *shared.R2Client, queue *asynq.Client, cfg *shared.APIConfig) (*VideoHandler, error) {
    if queries == nil { return nil, fmt.Errorf("queries is nil") }
    if sqldb == nil { return nil, fmt.Errorf("sqldb is nil") }
    // ... dst
    return &VideoHandler{...}, nil
}
```

Di `routes.go:NewRouter`, error dari constructor di-handle dengan
`panic(fmt.Sprintf(...))` — karena `NewRouter` signature tidak bisa
return error (phase 3 constraint). Trade-off: panik di startup lebih
baik daripada handler silently run dengan nil dependency yang akan
panic di tengah request (atau worse, oracle ke attacker via differential
response time/error message). Phase 9 akan refactor `NewRouter` signature
kalau perlu.

Pattern ini akan reuse di setiap handler baru di fase 5+ (worker
handler, follow handler, like handler, dll).

## Smoke test pattern: hermetic, in-network, no Dockerfile

Phase 4 butuh verifikasi runtime tanpa `api-gateway` Dockerfile
production (yang masih stub fase 9). Solusi: Go program terpisah di
`scripts/smoketest/phase4/main.go` yang di-run via `go run` di dalam
docker network compose — supaya bisa reach Postgres tanpa host port
mapping.

**Recipe** (in-network smoke, untuk program Go):

```bash
docker run --rm \
    --network mokibox_backend \
    -v $PWD:/repo -w /repo \
    golang:1.25.5-alpine \
    sh -c "DATABASE_URL='postgres://tiktok_api:CHANGEME@postgres:5432/tiktok?sslmode=disable' \
          R2_ACCOUNT_ID=... R2_ACCESS_KEY_ID=... R2_SECRET_ACCESS_KEY=... \
          R2_BUCKET=... R2_ENDPOINT=... \
          go run ./scripts/smoketest/phase4"
```

Program smoke test:
1. Open `*pgxpool.Pool` + `*sql.DB` via `pgx/v5/stdlib` (sama dengan
   main.go production).
2. Seed smoke-test user kalau belum ada (idempotent INSERT).
3. `InsertVideo` + `GetPendingVideoByUser` roundtrip.
4. `GetVideoByID(uuid.New())` → expect `sql.ErrNoRows` (verifies ErrNoRows
   contract dipakai handler).
5. `R2Client.PresignPut("uploads/smoke-test/<uuid>/source.mp4", ...)`
   → expect URL contain `X-Amz-Signature=`.
6. Cleanup row.
7. Print `PASS` di akhir — caller `echo $?` atau grep.

**Recipe alternatif (host-side binary smoke)** — kalau mau verify
bahwa listener + routes + envelope wire-format jalan end-to-end, bukan
cuma unit-test query + R2 presign. Pakai `socat` port-forward karena
Postgres/Redis defaultnya tidak expose host port. Detail lengkap +
pitfall di SKILL.md section "Binary smoke: `go run` di host + `socat`
port-forward". Phase 4 PR #37 pakai dua-duanya: in-network unit smoke
untuk verify query contracts + host binary smoke untuk verify listener
+ auth + envelope wire.

**Catatan penting**:

- Image `golang:1.25.5-alpine` punya glibc-compatible Go compiler, tapi
  `go run` recompile tiap kali → lambat (~30s download deps pertama
  kali, lalu cached di image layer). Trade-off vs benefit: zero new
  binary, zero commit artifact.
- **JANGAN build binary host lalu copy ke alpine container** — binary
  glibc (Ubuntu host) tidak akan jalan di alpine (musl). Symptom:
  `/usr/local/bin/api-gateway: not found` (padahal file ada). Pattern
  ini aman karena `go run` re-compile di target image dengan target
  libc, BUKAN karena file binary static.
- `scripts/smoketest/<phase>/` adalah layout yang akan reuse di fase
  5+ (transcoder), 6 (feed), 7 (notifications), 8 (delete). Phase 5
  mungkin perlu smoke test untuk worker handler yang consume dari
  Redis queue — recipe sama, tambahan: seed asynq task di queue, run
  worker handler function inline.
- `.env` di-mount via `-v $PWD:/repo -w /repo` supaya smoke test
  baca `R2_*` credentials yang sama dengan main.go production. **JANGAN
  hardcode credentials di scripts/smoketest/** — selalu baca dari env.
- R2 key untuk smoke test selalu prefix `uploads/smoke-test/` supaya
  tidak touch production data walau accidentally di-PUT.
- Tidak boleh commit program smoke test + leave di repo tanpa cleanup
  — tapi folder `scripts/smoketest/phase4/` **di-commit** karena ini
  bagian dari PR deliverable (regression suite).

## Routes pattern: route in handler-owning issue

Phase 4 punya 2 issue per task spec (A: handler, B: routes delta),
tetapi **di-merge jadi 1 atomic commit** karena Issue B adalah delta
dari Issue A (gak ada handler tanpa route, dan route tanpa handler
gak bisa di-test). Sesuai dengan `git-phase-issue-workflow` rule
#3 "1 issue = 1 commit, tapi issue yang logically dependent boleh
di-merge asalkan tetap atomic + self-contained".

**Rule praktis**: kalau 2 issue di fase yang sama touch `routes.go`
secara berurutan (issue B tambah field, issue C add route pakai
field), lebih baik **satu commit dengan 2 file** daripada 2 commit
dengan force-push rebase. Alasan: rebase PR yang sudah di-review
membuang effort reviewer.

## Pitfall yang tertangkap smoke test

### `git reset --hard` di session interactive = blocked

Pattern `git reset --hard origin/main` di tengah session interactive
di-block oleh Hermes karena destructive (kalau ada perubahan
uncommitted yang terlewat, hilang permanent). Solusi aman:

```bash
git fetch origin main
git log --oneline origin/main -3
git log --oneline main -3           # bandingkan HEAD
git checkout main
git pull --ff-only origin main      # safe, refuse kalau diverge
```

`--ff-only` cukup untuk sinkron `main` lokal dengan `origin/main`
kalau branch lokal belum dimajukan. Kalau ada divergen, tanya user
dulu — JANGAN `reset --hard` paksa.

### Issue-id format: `phase-N.M` (dengan dot), bukan `phase-N` saja

Task prompt fase 4 menulis "feat: [phase-4.M] deskripsi" tapi saya
sempat commit dengan `[phase-4]` (tanpa `.1`). Pattern dari
phase-3 commits di repo: `[phase-3.1]`, `[phase-3.2]`, `[phase-3.3]`
— **harus pakai `.M`**. Issue-id untuk fase N dengan M issue adalah
`phase-N.M`. Karena filter_issues.py auto-assign `{phase_slug}-{seq:02d}`
(`phase-4-01`), format repo sebenarnya pakai **dash**, bukan dot.

**Conventions clash**:

- `filter_issues.py` (skill helper): `phase-4-01` (dash, 2-digit).
- Commit message actual di repo (phase-3 reference): `phase-3.1`, `phase-3.2`, `phase-3.3` (dot, 1-digit).

Repo pakai dot + 1-digit. Ikutin yang repo pakai, **bukan** helper.
Untuk fase 4 dengan 1 issue, id-nya `phase-4.1` (sesuai amend yang
sudah dilakukan di PR #37). Phase 5+ cek `git log --oneline main -10`
untuk konfirmasi pattern saat itu.