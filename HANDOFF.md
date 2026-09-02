# Handoff — Fase 9 → Fase 10

State captured 2026-09-02 (post fase 9 PR #45, pre-merge).
Regenerate per session via `git fetch && gh pr list && gh issue list --state open`.

## Latest

- **PR #45 `feature/phase-9` OPEN** — 5 commit atomic per issue (A..E):
  - `155b44c` phase-9.1: api-gateway main.go — slog + fail-fast verifier + 30s shutdown
  - `d2dad34` phase-9.2: central HTTPErrorHandler as safety net + `shared.ClassifyError` exported
  - `af9f94b` phase-9.3: rate limit middleware (per-user auth 60/min + per-IP webhook 30/min)
  - `2073a09` phase-9.4: production Dockerfile (distroless) + compose context fix
  - `3b6a438` phase-9.5: graceful shutdown phase visibility (slog per step)
- **8 file changes**, +765/-106 (ground truth from `git diff --shortstat main..HEAD`).
- **Verification**: `go build`/`vet`/`test` exit 0, `docker compose build api-gateway` succeeds,
  image identity nonroot:nonroot, binary fail-fast verified.
- **Verification gap (carried to fase 10)**: live HTTP smoke for `/healthz`,
  `/api/videos/00000000-...` (404 envelope), and rate limit 429 require real
  Zitadel OIDC issuer. Static checks cover wire contract; runtime path
  verified by code reading.

## Merged (state of main)

- PR #43 (`65e0e12`) phase-8 merged — DELETE /api/users/me + DELETE /api/videos/:id + cleanup:video
- PR #42 phase-7, #41 refactor pool-consolidation, #40 phase-6, dst

## Alive Branches (JANGAN dijadikan basis)

- `feature/phase-9` — menunggu review/merge PR #45.
- `feature/phase-0..8` — history; tip-nya merge commit, jangan branch dari sini.
- `refactor/pool-consolidation` — sudah merged.

## Open Items

### Issues GH

- PR #45 body includes checklist issue #28 dengan `Closes #28` (auto-close saat merge).
- Belum disentuh:
  - #29 Fase 10: Unit tests shared utils dan worker
  - #30 Fase 10: Integration smoke test script
  - #31 Fase 10: Security hardening checklist
  - #39 Fase 10: Verify FFmpeg timeout-kill path in worker
  - #44 Fase 10: R2 orphan reconciliation sweeper untuk delete-account dual-write

### Risiko yang ditunda

- **Counter race** (carry-over fase 5-8 + fase 9): tx tidak pakai `SELECT ... FOR UPDATE`
  di decrement. `GREATEST(..., 0)` absorbs. Fase 10 reconciliation job.
  Fase 9 TIDAK fix ini.
- **Live HTTP smoke verification gap** (NEW dari fase 9): fail-fast verifier di Issue A
  + distroless image di Issue D = binary tidak bisa boot tanpa Zitadel infra yang up.
  Issue #30 (integration smoke) menutup gap ini dengan Zitadel real di zitadel-compose.
- **mokibox-nginx restart-loop** (pre-existing): cert belum di-populate di `deploy/nginx/certs/`.
  Fase 9 BUKAN touchpoint nginx. Tinggalkan sebagai known issue di HANDOFF.

## Decisions & gotchas for fase 10+

- **Counter race** (carry-over fase 5-9): tidak ada `FOR UPDATE` di decrement.
  Defer ke fase 10 reconciliation.
- **Orphan R2 risk** (carry-over fase 8): tracked issue #44, fase 10 reconciliation sweep.
- **Verification gap on live HTTP smoke** (NEW fase 9): binary fail-fast tanpa Zitadel
  infra up. PR body section "Verification gap" explains why integration smoke deferred
  to issue #30. Static checks + grep assertions + Docker build verify wire contract.
- **Fail-fast verifier** (NEW fase 9): `denyAllVerifier` removed dari `main.go`. Misconfigured
  ZITADEL_ISSUER_URL → container restart-loop sampai fixed. Doc trade-off di PR body.
- **Distroless image** (NEW fase 9): `gcr.io/distroless/static-debian12:nonroot`. Tidak punya
  shell — `docker exec ... sh` tidak akan jalan. Untuk debug live, harus buat debug image
  terpisah atau pakai `kubectl debug`/`docker debug` (kalau Docker version support).
- **Zero handler refactor** (NEW fase 9): handler fase 3-8 tetap pakai `shared.RespondError`.
  Central HTTPErrorHandler safety net untuk framework errors saja. Kalau fase 10 mau adopt
  `return err` idiom secara konsisten, refactor eksplisit dengan migration plan — jangan
  half-half.
- **`shared.ClassifyError`** (NEW fase 9): exported (dulu `classifyError` unexported).
  Internal call site `RespondError` updated. Kalau fase 10 bikin wrapper error baru,
  tambahkan mapping di `shared/errors.go` `httpStatusFor` + `codeFor`, JANGAN duplicate
  logic di tempat lain.
- **Rate limit per-process `sync.Map`** (NEW fase 9): janitor 5 menit sweep TTL 10 menit.
  Multi-instance deployment (kalau fase 10 atau production scale): per-instance counter,
  bukan global. Untuk global rate limit butuh Redis-backed counter — di luar scope fase 9.
- **Dockerfile `context: .`** (NEW fase 9): pitfall `mokibox-go-shared` SKILL.md section
  "Docker build context untuk multi-package Go module" menjelaskan kenapa. Kalau ada service
  baru di compose yang juga butuh `go.mod`/`go.sum` dari root, ULANGI pattern `context: .`.
- **`golang.org/x/time` direct dependency** (NEW fase 9): dipromote dari indirect di fase 9.
  Tetap di track sebagai "production runtime dep", bukan "test-only" — kalau di-unrequire
  di fase 10, rate limit akan break.
- **Pre-commit verification** (Aturan #11): expected file list per issue ditulis sebelum
  `git add`, dicocokkan dengan `git status --porcelain`, dan `git log --stat -1` diverifikasi
  setelah commit. Fase 9 mengikuti pattern ini: 5 commit, semua expected files match.
- **PR body post-submit edit** (carry-over): `gh api -X PATCH .../pulls/<num> --input <json>`
  reliable; `gh pr edit --body-file` deprecated + exit 1 silent. Fase 9 tidak butuh edit.
- **PR body angka self-check** (per git-phase-workflow rule #10): angka breakdown touchpoint
  di PR body diverifikasi dengan `git diff --shortstat main..HEAD`. Fase 9 PR body section
  "Commits" table juga catat perbedaan `git log --stat` per-commit sum (785/126) vs
  `git diff --stat` (765/106) dengan jelas, supaya reviewer tidak bingung.
- **Live HTTP smoke recipe** (NEW fase 9): butuh `make up-zitadel` dulu + cert populate
  di `deploy/nginx/certs/` + binary boot dengan env lengkap. Issue #30 fase 10 menulis
  integration smoke script yang ngebukti /healthz 200 + /api/videos/.../404 envelope
  + rate limit 429 — semua tiga skenario harus jalan end-to-end dengan Zitadel up.

## Files touched fase 9 (for future reference)

```
api-gateway/Dockerfile              (+97 / -13)
api-gateway/error_handler.go        (+185 / -0, new)
api-gateway/main.go                 (+221 / -102)  // A + E
api-gateway/middleware/ratelimit.go (+290 / -0, new)
api-gateway/routes.go               (+47 / -0)
docker-compose.yml                  (+12 / -2)
go.mod                              (+2 / -2)  // promote golang.org/x/time
shared/response.go                  (+17 / -1)  // classifyError -> ClassifyError
```

## Handoff prompt

`prompts/PHASE-10-PROMPT.md` (untracked) akan ditulis setelah fase 9 PR #45 merged,
sebelum fase 10 agent mulai kerja. Pattern carry-over dari fase 9 → fase 10 yang
WAJIB di-promote ke prompt berikutnya:
- Verification gap on live HTTP smoke (binary fail-fast tanpa Zitadel infra)
- Distroless image no-shell caveat
- Per-process rate limit (bukan global) — kalau production scale, butuh Redis-backed
- Single-pool `*sql.DB` + `sql.ErrNoRows` only (carry dari fase 7-9)
- Counter race (carry dari fase 5-9)
- Orphan R2 reconciliation (issue #44)