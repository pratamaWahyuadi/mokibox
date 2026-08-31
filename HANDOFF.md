# Handoff — Fase 6 → Fase 7

State at end of fase 6, captured 2026-08-30. Regenerate per session via `git fetch + gh pr list + gh issue list` at startup.

## Latest Merged

- **Fase 6**: PR #40 — MERGED, merge commit `0f49a8e` (2026-08-30 13:16 UTC).
  - `6ee0862` — issue A: `GET /api/feed/home` + `VideoObject` mapper + modified `ListFeedVideos` (JOIN users + EXISTS likes)
  - `81cc62f` — issue B: `GET /api/videos/:id` + `/status` + `/playlist.m3u8` + `R2Client.GetObject` helper
  - `339efe4` — issue C: `POST/DELETE /api/users/:id/follow` + `GET /api/users/:id/{followers,following}` + new sqlc `GetFollow` query
  - Files: 13 changed, +2548 / -10. Smoke: 3/3 PASS (`phase6_feed`, `phase6_video`, `phase6_follow`).
  - Force-push history: branch was opened with SHAs `a1ee7b3/e4865c9/b27763e`, then re-pushed with `6ee0862/81cc62f/339efe4` after the `phase6_feed` smoke was discovered to be missing from issue A's commit. The `Post-submit note` in PR #40 body documents this. If reviewing the original SHAs, fetch latest.

## Alive Branches (JANGAN reuse sebagai basis)

- `feature/phase-0`, `feature/phase-1`, `feature/phase-2`, `feature/phase-3`, `feature/phase-4`, `feature/phase-5`, `feature/phase-6` — historic fase branches, all merged via PRs. The tip of each is the merge commit, not the last fase commit. Don't branch from these.
- `fix/zitadel-separate-deployment` — historic fix branch, merged via PR #36.
- `master` — old name, kept for legacy. Use `main`.
- For fase 7, create `feature/phase-7` from `main` directly (per `git-phase-workflow` Aturan #2).

## Open Items

### GH issues that are referenced in planning but out of scope for fase 7

- **#39 — Verify FFmpeg timeout-kill path in worker (TRANSCODE_TIMEOUT)** — OPEN. Belongs to fase 10. The code path is implemented in `transcoder-worker/handlers/transcode.go` but the smoke to actually prove `exec.CommandContext` kills ffmpeg after the timeout was not run. Do NOT address in fase 7.

### GH issues that ARE fase 7 scope (carry from ISSUES.json → GitHub)

- **#23 — Fase 7: Like/unlike dan view tracking** (issue A)
- **#24 — Fase 7: Comment, list comment, delete comment, reply** (issue B)
- **#25 — Fase 7: Notifikasi list dan mark all read** (issue C)

All three are currently OPEN. If fase 7 work completes, they can be closed via the PR body with `Closes #23, #24, #25` per GitHub auto-close convention.

### GH issues that belong to later phases (do NOT touch in fase 7)

- **#26 — Fase 8: Delete account + DeleteUserData + integrasi webhook user.removed**
- **#27 — Fase 8: Delete video endpoint + enqueue cleanup**
- **#28 — Fase 9: Wiring API gateway main, routes, error handler, rate limit, Dockerfile**
- **#29 — Fase 10: Unit tests shared utils dan worker**
- **#30 — Fase 10: Integration smoke test script**
- **#31 — Fase 10: Security hardening checklist**

## Notes Penting (carry to fase 7)

### Pre-fase-7: dual-pool consolidation (cross-cutting refactor)

Branch `refactor/pool-consolidation` (PR pending at start of fase 7) replaced the dual-pool wiring (`*pgxpool.Pool` + `*sql.DB`) with a single `*sql.DB` via `pgx/v5/stdlib` adapter. Both sqlc read paths (`*db.Queries`) and `Queries.WithTx` now bind to the same pool.

Key changes:

- `shared.NewDB` (pgxpool) removed → replaced by `shared.NewSQLDB` (`*sql.DB`).
- `RouterDeps.SQLDB` renamed to `RouterDeps.DB`; `Worker.SQLDB` renamed to `Worker.DB`.
- Dead-weight field `UserHandler.DB` and `Worker.DB` (both `*pgxpool.Pool`) removed — verified unused via grep before deletion.
- 23 sentinel sites unified to `sql.ErrNoRows` only (10 dual-check reduced to single-check + 13 single-check `pgx.ErrNoRows` → `sql.ErrNoRows`).
- No API, schema, or sqlc-generated code change. Trade-off documented in `shared/db.go` (pgxpool granular acquire/release lost; workload small enough that it doesn't matter).

This refactor MUST be merged before fase 7 begins. Fase 7 will reference `d.DB` in `routes.go` and add 8+ routes that may use `Queries.WithTx` for atomic like/comment/notification operations. Doing the pool consolidation first prevents fase 7 from referencing fields that will be renamed and prevents copy-paste of the obsolete dual-check pattern.

For full inventory (32 touchpoint breakdown) see `PLAN_DUAL_POOL_CONSOLIDATION.md` (untracked planning doc at repo root).

### Decisions from fase 6 that fase 7 must respect

- **`DecrementCommentsCount` decrements by 1, but `DeleteComment` with subtree needs to decrement by N.** Add a new sqlc query `DecrementCommentsCountBy(ctx, videoID, n int64)` to `sqlc/queries/videos.sql` and regenerate. Do NOT carry this as a deviation in fase 7's PR — it is a spec gap fix per the fase-4 lesson documented in CONVENTIONS.md. Put the new query + handler change in ONE atomic commit (fase 7 issue B).

- **Notification payload for follow = `{"username": "<actor>"}`** (per fase 6 user confirmation). Fase 7 like/comment payloads are open questions — see `prompts/PHASE-7-PROMPT.md` questions 1-2.

- **`GetVideoDetail` visibility check pattern** is the reference for all fase 7 video-accessing handlers (Like, Unlike, View, CreateComment, ReplyComment, ListComments). Use the SAME inline-or-helper choice across all six handlers — don't mix. Fase 6 used inline for `GetVideoDetail` and a private helper (`assertCanSeeFollowList`) for the follow-list case. Fase 7 can pick either; pick ONE.

- **Token path in `GetPlaylist` skips visibility check** (per fase 6 user clarification). A valid media token = pre-authorized. Fase 7 has no token-path concept — all endpoints are JWT-only.

- **Visibility = 404 (not 403) for non-owner** on any read-side endpoint. Fase 7 like/comment/view all return 404 if non-owner can't see the video.

### Spec gaps to watch for in fase 7

- `CountCommentSubtree` returns subtree count (parent + all replies). `DeleteComment` must use that count, not 1, when decrementing `comments_count`.

- Composite FK `(parent_id, video_id)` in `comments` table prevents cross-video reply at SQL level (FK violation → 500). The fase 7 prompt asks whether to validate this in Go layer (return 400) or let it fail (500). User decision pending.

- Like and comment notifications are inserted INSIDE the transaction (unlike follow which is best-effort out-of-tx). Pattern established in CONVENTIONS.md.

- `comments_count` tracks total comments on a video (parent + replies). Reply also increments this counter (not a separate `replies_count`).

### Pitfalls from fase 6 that fase 7 must avoid

- **Pre-commit file list verification**: write the expected file paths for each issue BEFORE `git add`, then run `git status --porcelain` to verify. This is what `git-phase-workflow` Aturan #11 was added for. Fase 6 lost a smoke file because of skipping this step.

- **Don't `git commit --amend` to "fix" the wrong commit.** Use `git commit --fixup=<target-sha>` + rebase. See CONVENTIONS.md "Anti-Patterns" section.

- **Don't write a smoke that requires a feature you haven't built yet.** Fase 6 `phase6_video` smoke tested master/variant playlist rewrite with mocked R2, not the full HTTP path — because the HTTP path needs auth. Same approach for fase 7: pure DB-level smoke against `*db.Queries` is sufficient.

- **`UpdateVideoToProcessing` is required before `MarkVideoReady`** because the schema defaults status to `PENDING_UPLOAD` and `MarkVideoReady` requires `status='PROCESSING'`. Fase 6 smoke helpers document this; reuse the pattern in fase 7 smoke (any smoke that needs a READY video).

### File scope expected for fase 7

Based on fase 6 → fase 7 transition:

- New: `api-gateway/handlers/social.go` (7 methods: Like, Unlike, TrackView, CreateComment, ListComments, DeleteComment, ReplyComment)
- New: `api-gateway/handlers/notification.go` (2 methods: List, MarkAllRead)
- Modified: `api-gateway/routes.go` (9 new routes: 3 like/view + 4 comment + 2 notif)
- Modified: `sqlc/queries/videos.sql` (add `DecrementCommentsCountBy`)
- Regenerated: `shared/db/videos.sql.go`
- New smoke: `scripts/smoketest/phase7_{like,comment,notification}/main.go` (3 programs)

This is a 3-commit, 3-issue PR (like/view → comment → notification), per the same pattern as fase 6.
