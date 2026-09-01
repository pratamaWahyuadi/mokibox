# Handoff — Fase 8 → Fase 9

State captured 2026-09-02 (post fase 8, pre-merge PR #43). Regenerate per session via `git fetch && gh pr list && gh issue list --state open`.

## Latest

- **PR #43 `feature/phase-8` OPEN** (menunggu review/merge) — 2 commit:
  - `b9eebce` phase-8.1: DELETE /api/users/me + DeleteUserData tx + user.removed webhook wiring — `AccountHandler` baru di `api-gateway/handlers/account.go`, `WebhookHandler` extended dengan `DB`+`Queue`
  - `551dab6` phase-8.2: DELETE /api/videos/:id owner-only + `MarkVideoDeleted` + `cleanup:video` `ProcessIn(24h)` — method baru di `VideoHandler` (existing `video.go`)
- **Smoke fase 8 PASS** keduanya (`scripts/smoketest/phase8_{delete_user,delete_video}`) via in-network Docker pattern yang sama dengan fase 6-7.
- **20 routes** di `api-gateway/routes.go` (counter `grep -c "^\\s*api\\." api-gateway/routes.go`).

## Merged

- PR #42 `6f51d7d` phase-7 (3 commit: like/unlike/view, comment/reply, notification list/mark-all-read).
- PR #41 `69bac5c` refactor/pool-consolidation — single `*sql.DB` pool via pgx stdlib; `RouterDeps.SQLDB` → `RouterDeps.DB`; sentinel tunggal `sql.ErrNoRows`. Fase 7+8 dibangun di atas ini.
- PR #40 phase-6, #38 phase-5, #37 phase-4, #36 zitadel split.

## Alive Branches (JANGAN dijadikan basis)

- `feature/phase-8` — menunggu merge PR #43.
- `feature/phase-0..7` — history; tip-nya merge commit, jangan branch dari sini.
- `refactor/pool-consolidation` — sudah merged; boleh dihapus lokal/remote.

## Open Items

### Issues GH

- PR #43 body sudah include checklist issue #26 + #27 dengan `Closes #26, #27` (auto-close saat merge).
- Belum disentuh:
  - #28 Fase 9 (api-gateway main, error handler, rate limit, Dockerfile)
  - #29, #30, #31 (fase 10: hardening + integration smoke + unit tests)
  - #39 FFmpeg timeout-kill verification (fase 10)

### Risiko yang ditunda

- **Counter race** di likes/comments/views: tx tidak pakai `SELECT ... FOR UPDATE`. `GREATEST(..., 0)` mencegah negatif; drift kecil ditoleransi. Fase 10 punya reconciliation job. **Tetap tidak fix di fase 8** — delete-user decrement self-locks target video rows; drift window kecil.

## Decisions & gotchas for fase 9+

- **Counter race** (carry-over fase 5-8): tidak ada `FOR UPDATE` di decrement. Defer ke fase 10 reconciliation.
- **Session invalidation post-tombstone**: BUKAN concern fase 8. Tombstone + Zitadel sign-out adalah tanggung jawab client. Konfirmasi user via clarify.
- **Worker `HandleCleanupVideo` sudah lengkap** (fase 5) — tidak perlu perubahan worker di fase 8. Sudah handle re-enqueue jika grace belum elapsed + hls prefix DeletePrefix + idempotent R2 delete.
- **Spec gap LLD vs acceptance criteria**: tidak ada di fase 8 (semua spec sudah covered oleh query sqlc yang ada). `MarkVideoDeleted`, `ListVideoKeysByUser`, `DeleteVideosByUser`, `DeleteFollowsByFollower`, `DeleteFollowsByFollowee`, `DeleteLikesByUser`, `DeleteCommentsByUser`, `DeleteNotificationsForUser`, `DeleteNotificationsByActor`, `TombstoneUser`, `DecrementLikesForUser`, `DecrementCommentsForUser` semua sudah ada di `shared/db/*.sql.go` dari fase 1-7.
- **Anti-enumeration untuk non-owner delete video**: API contract membolehkan 403/404, CONVENTIONS.md mandate 404. Konfirmasi user via clarify — pakai 404. Konsisten dengan GetVideoDetail, LikeVideo, dsb.
- **Dockerfile production + nginx cert**: pre-existing. `mokibox-nginx` restart-loop karena `deploy/nginx/certs/` belum di-populate (sudah ada caveat di CONVENTIONS.md). Fase 9 (Dockerfile api-gateway) bukan touchpoint nginx cert. Tinggalkan sebagai known issue.
- **Single-pool assertion**: `grep -rn "pgx\\.ErrNoRows" api-gateway transcoder-worker --include="*.go"` HARUS 0. Verified post-fase-8: 0 matches.
- **Pre-commit verification** (Aturan #11): expected file list per issue ditulis sebelum `git add`, dicocokkan dengan `git status --porcelain`, dan `git log --stat -1` diverifikasi setelah commit. Fase 8 clean (4 + 3 files, semua match expected).
- **Smoke DSN**: smokes pakai superuser via `docker exec mokibox-postgres printenv POSTGRES_PASSWORD` karena role `tiktok_api` password-nya placeholder `***` di `.env`. Recipe sudah ada di `scripts/smoketest/phase8_*/main.go`. Redis password = `change-me-redis` (hard-coded di `docker-compose.yml`, BUKAN di `.env`).
- **PR review pattern `Closes #N`**: pakai `--body-file` via `gh pr create` (md file), dengan `Closes #26, #27` per baris agar GitHub auto-link.
- **Handoff prompt**: `prompts/PHASE-9-PROMPT.md` (untracked) sudah ditulis dengan carry-over + scope + 5 pertanyaan klarifikasi untuk di-`clarify` sebelum eksekusi.
