# Handoff — Fase 7 → Fase 8

State captured 2026-09-01 (post fase 7, pre-merge PR #42). Regenerate per session via `git fetch && gh pr list && gh issue list --state open`.

## Latest

- **PR #42 `feature/phase-7` OPEN** (menunggu review/merge) — 3 commit:
  - `f5f0842` phase-7.1: like/unlike + view tracking — `SocialHandler` baru di `api-gateway/handlers/social.go`
  - `76e5571` phase-7.2: comment + list + delete + reply — 4 method di social.go, query sqlc baru `DecrementCommentsCountBy(videoID, n int32)` dengan `GREATEST(..., 0)` guard
  - `3761f64` phase-7.3: notifications list + mark-all-read — `NotificationHandler` (Queries only, tanpa tx)
- **Smoke fase 7 PASS** semua 3 (`scripts/smoketest/phase7_{like,comment,notification}`) via in-network Docker pattern yang sama dengan fase 6.
- Files total: 8 changed, +1790 (lihat `git diff --stat main..HEAD` saat branch di-checkout).

## Merged

- PR #41 `69bac5c` refactor/pool-consolidation — single `*sql.DB` pool via pgx stdlib; `RouterDeps.SQLDB` → `RouterDeps.DB`; sentinel tunggal `sql.ErrNoRows`. Fase 7 dibangun di atas ini.
- PR #40 `0f49a8e` phase-6 — feed, video detail/status/playlist, follow + notif follow (best-effort out-of-tx).
- PR #38 phase-5 (worker + HLS), #37 phase-4 (upload-intent + confirm), #36 zitadel split.

## Alive Branches (JANGAN dijadikan basis)

- `feature/phase-7` — menunggu merge PR #42.
- `feature/phase-0..6` — history; tip-nya merge commit, jangan branch dari sini.
- `refactor/pool-consolidation` — sudah merged; boleh dihapus lokal/remote.

## Open Items

### Issues GH

- Terbuka & relevan untuk fase 7 (akan auto-close kalau PR #42 body diberi `Closes #23, #24, #25`):
  - #23 Fase 7 like/unlike/view
  - #24 Fase 7 comment/list/delete/reply
  - #25 Fase 7 notifikasi list/mark-all-read
- Belum disentuh:
  - #39 FFmpeg timeout-kill verification — fase 10.
  - #26/#27 (fase 8), #28 (fase 9), #29/#30/#31 (fase 10).

### Risiko yang ditunda

- **Counter race** di likes/comments/views: tx tidak pakai `SELECT ... FOR UPDATE`. `GREATEST(..., 0)` mencegah negatif; drift kecil ditoleransi. Kalau mau strict: ambil `qtx.GetVideoByIDForUpdate(ctx, videoID)` di awal tx like/unlike/comment/reply/delete. Masukkan ke fase hardening/test (10).

## Carry-over untuk Fase 8 (account/video deletion)

- Query sqlc yang sudah tersedia untuk cleanup: `DeleteCommentsByUser`, `DeleteLikesByUser`, `DeleteNotificationsForUser`, `DeleteNotificationsByActor`, `DecrementLikesForUser`, `DecrementCommentsForUser`, `DeleteVideosByUser`, `MarkVideoDeleted`, `ListVideoKeysByUser`. Tinggal wire ke handler/service.
- Untuk DeleteVideo: cek juga `CountCommentSubtree` tidak diperlukan (cascade handles row); tapi counter `comments_count` perlu dikoreksi kalau delete individual comment.
- Pattern tx yang established di fase 7: `h.DB.BeginTx` → `qtx := h.Queries.WithTx(tx)` → `defer tx.Rollback()` → commit → refetch fresh state untuk response. Ikuti ini untuk fase 8 tx (user tombstone, video delete + enqueue cleanup).
- Notif payload: like={username, video_id}; comment/reply={username, video_id, comment_id}; follow={username}. Kalau fase 8 menambah notif type baru (`video_deleted`?), tentukan payload-nya terlebih dahulu.
- Coast pattern: `assertVideoVisible(ctx, viewerID, videoID)` di `handlers/social.go` — owner bypass, non-owner READY+active+follower-if-private, `404` untuk semua unauthorized. Reusable untuk read/write video lain di fase 8.

## Decisions & gotchas for fase 8+

- **sqlc regen**: `make sqlc-gen` (cd ke `sqlc/` dir via Makefile). Jangan edit `shared/db/*.sql.go` manual.
- **`sql.ErrNoRows` tunggal**: jangan reintroduce `pgx.ErrNoRows` ataupun dual-check (grep assertion: `grep -rn "pgx\\.ErrNoRows" api-gateway transcoder-worker --include="*.go"` harus 0).
- **Pre-commit verification** (Aturan #11): tulis expected file list per issue SEBELUM `git add`, cocokkan dengan `git status --porcelain`, verifikasi `git log --stat -1` SETELAH commit. Fase 6 kehilangan smoke file karena skip step ini (sudah di-fix dengan force-push, dokumentasikan di PR).
- **Smoke DSN**: smokes pakai superuser via `docker exec mokibox-postgres printenv POSTGRES_PASSWORD` karena role `tiktok_api` password-nya placeholder `***` di `.env`. Recipe sudah ada di `scripts/smoketest/phase7_*/main.go`.
- **Pattern `Closes #N` di PR body**: issue closure via GitHub akan auto-close issue #23-#25 saat PR #42 merge — tambahkan ke body PR kalau missed.
