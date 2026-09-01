# MokiBox Conventions

Lessons and rules that apply across phases, that are NOT in any skill (git-phase-workflow, hermes-go-idiomatic, mokibox-go-shared) and NOT in PRDGEN output (`planning/PRD.md`, `03_schema.md`, `04_api_contracts.md`, `LLD_PLAN.md`, `ISSUES.json`).

## Repo-Specific Lessons

### Single-pool architecture (`*sql.DB` via pgx stdlib)

Both `api-gateway` and `transcoder-worker` open a single `*sql.DB` pool against the same database, backed by the `pgx/v5/stdlib` driver. The pool is built by `shared.NewSQLDB(ctx, dsn, maxConns, maxIdle)` in `shared/db.go`.

- sqlc `Queries` (read path) and `Queries.WithTx` (transaction path) both bind to the same `*sql.DB`.
- The `*pgxpool.Pool` previously used by `api-gateway` and `transcoder-worker` has been removed entirely (pre-fase-7 refactor `refactor/pool-consolidation`).
- Trade-off (intentional, do not silently re-introduce pgxpool without re-reading `shared/db.go`): the pgx stdlib adapter keeps pgx wire-level type mapping and per-connection prepared-statement cache, but loses pgxpool's granular acquire/release semantics. For MokiBox workload (max 10 conn API + max 5 conn worker) the difference is negligible. If a future phase needs pgxpool-only ergonomics, rewrite `shared/db/*.sql.go` to use `pgx.Tx` instead of `*sql.Tx` (~1900 LOC of generated code) — do NOT silently bring back a second pool.

**ErrNoRows sentinel: hanya `sql.ErrNoRows`.** Because every query in `api-gateway/` and `transcoder-worker/` runs through the `*sql.DB` (pgx stdlib adapter), a missing row surfaces as `sql.ErrNoRows` from `QueryRowContext().Scan()`. `pgx.ErrNoRows` will NEVER match in this code path — using it returns 500 for every legitimate not-found branch. Rule: `grep -rn "pgx.ErrNoRows" api-gateway transcoder-worker --include="*.go"` MUST return 0 matches.

Reference: `shared/db.go` (rationale + driver registration) and `api-gateway/main.go` (single-pool wiring).

### Smoke test = in-network `go run` from a Docker container

The `mokibox-api-gateway` container is a stub (`sleep 3600` until fase 9 wires the real binary). `denyAllVerifier` is active because Zitadel is not in the dev stack, so every `/api/*` endpoint returns 401.

The viable smoke pattern is: `docker run --rm --network mokibox_backend -v $PWD:/repo -w /repo golang:1.25.5-alpine go run ./scripts/smoketest/phaseN_X`. The `mokibox_backend` network lets the smoke reach Postgres/Redis without host port mapping. Each smoke embeds a superuser DSN fallback (`postgres://postgres:change-me-postgres@postgres:5432/tiktok?sslmode=disable`) so the smoke runs without reading `.env` (which is blocked by Hermes terminal policy).

**Why pure DB-level smoke, not HTTP curl**: HTTP smoke would require bypassing the auth middleware (test middleware reading `X-Test-User-Id`, or a dummy verifier) which is invasive and prone to leakage. Pure DB-level smoke (insert rows, call queries, verify state) covers the same handler logic without the auth cost.

Reference smoke: `scripts/smoketest/phase6_feed/main.go`.

### Commit format: `phase-N.M` (dot, 1-digit)

The repo uses `phase-N.M` with a dot and 1 digit, e.g. `feat: [phase-6.1] home feed`. This is NOT what `scripts/filter_issues.py` produces (which emits `phase-4-01` with dash, 2-digit). The repo convention wins. Always check `git log --oneline main -10` to confirm the pattern before the first commit of a new phase.

### `migrations/001_init.sql` is forward-only

The single migration file `migrations/001_init.sql` defines the entire schema. It is NEVER edited after fase 1 lands. New columns, tables, or constraints go in a NEW migration file (`migrations/002_*.sql`) — but for fase 6 we did not need any schema change. If a future phase needs one, follow the existing pattern: `cat migrations/002_xxx.sql | docker compose exec -T postgres psql -U $POSTGRES_USER -d $DB_NAME -v ON_ERROR_STOP=1`.

### Spec gap = fix inline (fase-4 lesson)

When LLD or the API contract describes a behavior that the existing sqlc queries don't directly support (e.g. `DecrementCommentsCount` decrements by 1, but DeleteComment with subtree needs to decrement by N), do NOT carry it as a "DEVIATION" in the PR. The right move is:

1. Add the new sqlc query (e.g. `DecrementCommentsCountBy(ctx, videoID, n int64)`) to `sqlc/queries/videos.sql`.
2. Run `make sqlc-gen` to regenerate `shared/db/`.
3. Wire the new query into the handler in the same atomic commit.

The PR checklist should NOT strike through any acceptance criterion for this — the criterion is met, just via a new helper query instead of an existing one. This applies when the spec gap is small (1 query, 1 call site, no design discussion needed).

Reserve "DEVIATION" for things that need design discussion, depend on external systems, or are blocked. Anything that can be fixed with `1 query sqlc + 1 line handler change` is in-scope.

### Anti-enumeration: 404 (not 403) for unauthorized reads

For ANY read-side endpoint that touches a video or comment, the rules are:

- Owner: always allowed, regardless of status or visibility.
- Non-owner: must satisfy `status='READY'` + `u.is_active=true` + (if `u.is_private=true`, the viewer must be a follower via `IsFollowing`).
- Any failure of those conditions → return 404, NOT 403. This prevents attackers from probing which video/comment IDs exist and whether accounts are private/deactivated.

The only legitimate 400 in this pattern is `SELF_FOLLOW_NOT_ALLOWED` (self-follow) which is an explicit denial, not an anti-enumeration case.

Reference: `api-gateway/handlers/video_detail.go:GetVideoDetail` and `user_follow.go:FollowUser`.

### Notification insert: in-tx for like/comment, out-of-tx for follow

The notification side-effect has different atomicity requirements:

- **Follow notification** (`user_follow.go:FollowUser`): inserted OUTSIDE the follow transaction. The follow is the primary action; the notification is a best-effort side effect. If notification insert fails, the follow still succeeds (logged as warn).
- **Like + comment notifications** (fase 7): inserted INSIDE the same transaction as the counter update. If the notification insert fails, the like/comment rolls back. Rationale: the counter increment and the notification are part of the same user-visible action — partial success would leave counts out of sync with inbox state.

This split is a design decision documented in fase 6 PR #40 Deviations.

### Visibility check pattern: do it ONCE per handler, factor the helper if needed

`GetVideoDetail` (fase 6) does the full visibility check inline. Fase 7 will need the same check in Like, Unlike, View, CreateComment, ReplyComment, ListComments — six places. The pattern is to either:

- Inline the check in each handler (verbose but explicit), OR
- Extract a private helper like `h.assertCanSeeVideo(ctx, viewerID, videoID) error` that returns the appropriate sentinel error or nil. Used by `user_follow.go:assertCanSeeFollowList` for the follow-list case in fase 6.

Pick ONE and stay consistent across handlers. Mixing both styles is hard to read.

## Anti-Patterns (Project-Specific)

### Don't reuse an old `feature/phase-N` branch as a base for fase N+1

All `feature/phase-N` branches from fases 0-5 are still alive in remote as historic references. They are NOT the source of truth — `main` is. Creating `feature/phase-7` from `feature/phase-6` instead of `main` causes PR chaining and conflicts.

The branch is named `feature/phase-N` (lowercase, hyphen, no `fase` prefix) per the `git-phase-workflow` skill.

### Don't `git commit --amend` to "fix" the wrong commit

`--amend` modifies HEAD, which after a rebase or `git reset --soft` may not be the commit you think it is. If you need to move a file to a previous commit, use `git commit --fixup=<target-sha>` + interactive rebase (squash the fixup into the target). See `git-phase-workflow` Aturan Ketat #8 sub-pitfall for the full story and the exact rebase command.

### Don't add a dependency to `go.mod`/`go.sum` without justification

For fases 4-6, all needed functionality was already in `shared/` (sentinels, response helpers, R2 client, media token, sqlc). If fase 7 (or later) needs a new external package, justify it in the PR body — what does it do that the existing `shared/` can't? If the answer is "nothing", write it in `shared/`.

### Don't write raw SQL in handlers

All queries go through sqlc-generated `*db.Queries` methods. The migration files define the schema, `sqlc/queries/*.sql` define the queries, `shared/db/` is generated. Writing a raw SQL string in a handler breaks the auditability of the schema and creates inconsistency with the rest of the codebase.

Exception: smoke test programs in `scripts/smoketest/` may use raw SQL for seeding/cleanup, but the production code path always goes through `*db.Queries`.

### Don't write a `healthz` handler that touches the DB

`/healthz` returns 200 unconditionally. The orchestrator's `docker compose ps` healthcheck is the authoritative liveness check, and DB/Redis liveness are their own concern (not the API gateway's). Fase 3 set this in `api-gateway/handlers/user.go:HealthHandler`. Don't extend it.

### Don't expose `403` for unauthorized reads

See "Anti-enumeration" above. The only 403-equivalent in this codebase is `SELF_FOLLOW_NOT_ALLOWED` (400, actually, not 403 — see `shared/errors.go`).

### Don't trust `LikedByMe` / `FollowerCount` from a non-READY video for non-owner

A non-owner requesting `GET /api/videos/:id` for a non-READY video gets 404 — no VideoObject returned. Don't try to return a "preview" VideoObject with `status='PROCESSING'` for the owner's benefit; the LLD/contract say READY-only for non-owners. Owner-only status reads go through `GET /api/videos/:id/status` which returns a different (status-only) shape.

## When to Add Here

Add a section to this file when:

- You notice a pattern repeated across 2+ phases that would have saved time if written down earlier.
- You hit a pitfall that took >30 minutes to debug and was NOT covered by `git-phase-workflow`, `hermes-go-idiomatic`, `mokibox-go-shared`, or any file in `planning/`.
- The user explicitly says "tambahin ke conventions" or similar.
- A design decision is made that contradicts or extends what LLD/contract say, and is not already in HANDOFF.md (which is per-phase, not cross-phase).

Do NOT add:

- Anything already in a skill (those are the skills' jobs).
- Anything already in `planning/` PRDGEN (those are the spec's jobs).
- Generic Go best practice (those are `hermes-go-idiomatic`'s job).
- Per-phase state (use HANDOFF.md instead).
- One-off design decisions for a single phase (use HANDOFF.md or the phase's PR body).
