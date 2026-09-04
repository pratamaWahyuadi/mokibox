# MokiBox Security Policy

Single-VPS TikTok-clone backend (Go, Docker Compose). This
document maps the PRD section 6.5 security requirements
(SEC-01..08) to their implementations and the evidence that
verifies them, plus operational security notes and the
vulnerability-reporting policy.

Audit status below reflects the phase-10 security hardening
review (issue #31, 2026-09-04). Every ✅ carries concrete
evidence - config that exists in this repo, live `docker
inspect` output, or a test that runs - never "it passed in
a previous phase" (the 5-bug lesson from issue #30).

## SEC-01 — Media validation before FFmpeg

Every upload is probed with ffprobe and validated against
allowlists BEFORE ffmpeg ever sees the file.

| Control | Implementation | Evidence |
|---|---|---|
| ffprobe pre-validation | `transcoder-worker/ffprobe.go` `ProbeFile` + `ValidateMedia` runs before the transcode loop | `TestValidateMedia_*` (codec/duration/dimension/bitrate allowlists, table-driven) |
| Video codec allowlist | h264, hevc, vp9, av1 | `ffprobe_test.go` `TestValidateMedia_AcceptsAllWhitelistedCodecs` |
| Audio codec allowlist | aac, opus, mp3 | idem |
| Duration 1s..180s, dimensions 16..4096, bitrate ≤ 25 Mbps | `ValidateMedia` rules | `TestValidateMedia_Rejects` (13 rejection cases) |
| Size re-check in worker (last line of defence) | `MinUploadBytes`/`MaxUploadBytes` re-validated even if /confirm missed an edge case | `ValidateMedia` size branch + worker smoke |
| Probe cannot hang forever | `ProbeFile` uses `exec.CommandContext` under the task deadline | `probe_kill_test.go` `TestProbeFile_KillsOnContextDeadline` (FIFO hang, killed at ~1s) |

## SEC-02 — Sandboxed services (worker / gateway / nginx)

| Control | Implementation | Evidence (live `docker inspect`) |
|---|---|---|
| Worker: non-root, read-only FS, cap_drop ALL, no-new-privileges, pids_limit 100, mem 2g, cpus 1.5, tmpfs /tmp | `docker-compose.yml` transcoder-worker | `readonly_rootfs=true cap_drop=[ALL] security_opt=[no-new-privileges:true] pids_limit=100 mem=2g user=10001:10001` |
| Gateway: read-only FS, cap_drop ALL, no-new-privileges, pids_limit 200, mem 512m, cpus 1.0, distroless nonroot (UID 65532) | `docker-compose.yml` api-gateway (phase-10 hardening) | `readonly_rootfs=true cap_drop=[ALL] ... user=nonroot:nonroot` |
| Nginx: cap_drop all-except-bind, no-new-privileges, read-only FS + tmpfs runtime dirs, pids/mem limits | `docker-compose.yml` nginx (phase-10 hardening) | live inspect after `docker compose up -d` |
| FFmpeg killed at TRANSCODE_TIMEOUT | `exec.CommandContext` + `context.WithTimeout` in `HandleTranscode`/`runFFmpeg` | `runffmpeg_test.go` kill tests + runtime smoke `scripts/smoketest/phase10_timeout/run.sh` (24 PASS, rerun 2x) |
| Redis / Postgres internal-only | no `ports:` on either service | `docker port` = empty |

Nginx cannot `cap_drop: ALL` (root master needs
CAP_NET_BIND_SERVICE + setgid/setuid for the worker user),
so it drops the full docker-recommended list except those.

## SEC-03 — Redis internal-only, password, payload validation

| Control | Implementation | Evidence |
|---|---|---|
| No host port | compose: no `ports:` on redis | `docker port mokibox-redis` → empty |
| Password required | `--requirepass ${REDIS_PASSWORD}` + clients send it | compose healthcheck uses `-a` |
| Job payload validated in worker | every handler unmarshals + nil-checks deps before acting | `HandleTranscode`/`HandleCleanupObjects`/`HandleCleanupVideo` preambles |

## SEC-04 — Object-level authorization

| Control | Implementation | Evidence |
|---|---|---|
| Video/comment/social reads gated by visibility | `SocialHandler.assertVideoVisible` (owner bypass; non-owner: READY + owner active + following-if-private) reused by Like/Unlike/View/Comment/Reply/DeleteComment | 7 call sites in `social.go` |
| Detail + playlist inline visibility | `video_detail.go` GetVideoDetail + GetPlaylist apply the same rules | visibility blocks in both handlers |
| Anti-enumeration | unauthorized reads return 404, never 403 | `shared/errors.go` mapping + `CONVENTIONS.md` rule |
| Inactive user rejected at auth | `Authenticate` middleware checks `is_active` | `auth_test.go` `TestAuthenticate_InactiveUserRejected` (401) |

## SEC-05 — Webhook signature + rate limit

| Control | Implementation | Evidence |
|---|---|---|
| HMAC verification (dual header) | `actions.ValidateRequestPayload` from zitadel-go on raw body + headers | `webhook.go` verify step; live-verified with real Zitadel events (PR #46, phase-10.4 fix) |
| Webhook rate limit | `RateLimitWebhook` 30/min burst 30 per IP | `routes.go` wiring; integration smoke drives real webhooks |
| Wrong-signature → 401 | `CodeWebhookInvalidSignature` | errors mapping table + `TestErrorMapping` |

## SEC-06 — R2 bucket private, presigned access only

| Control | Implementation | Evidence |
|---|---|---|
| Bucket private; no static/public URLs | all client access via presigned PUT/GET with expiry | `shared/r2.go` `PresignPut`/`PresignGet`; handlers return `upload_url` only |
| Server-side ops use credentials, never presigned | `Download`/`UploadFile`/`DeleteObjects`/`DeletePrefix` | `shared/r2.go` |
| HLS access via short-lived media token | `NewMediaToken`/`VerifyMediaToken` (HMAC, binds video_id + expiry) | `mediatoken_test.go` (valid/expired/tampered/rebinding) |
| Prefix-delete safety | `DeletePrefix` refuses empty/slash-only/non-terminated prefixes | `r2_delete_prefix_test.go` (3 guards) |

## SEC-07 — JWT validation + HTTPS/HSTS

| Control | Implementation | Evidence |
|---|---|---|
| JWT via zitadel-go (JWKS + issuer + audience + expiry, never hand-rolled) | `NewZitadelVerifier` + `oauth.DefaultJWTAuthorization` | live-verified end-to-end in the *.localhost E2E (PR #46: 16-assertion integration smoke incl. real Zitadel JWTs) |
| Bearer prefix contract | re-attached before `CheckAuthorization` | phase-10.3 fix + `auth_test.go` raw-token passthrough assertion |
| HTTPS + HSTS + security headers | `deploy/nginx/default.conf`: TLS 1.2/1.3, HSTS max-age=2y, nosniff, DENY, no-referrer | config in repo |
| Local dev exception (HTTP-only) | `deploy/nginx/local.conf` + `docker-compose.override.yml` — local-only, never production | files marked DO-NOT-USE-IN-PROD |

## SEC-08 — Upload size validated at confirm; invalid objects cleaned

| Control | Implementation | Evidence |
|---|---|---|
| HeadObject size check 1 KB..200 MB at /confirm | `ConfirmUpload` → `R2.HeadObject` | `video.go` confirm path (400 UPLOAD_SIZE_INVALID) |
| Wrong-size / invalid media → cleanup:objects enqueue | best-effort enqueue of the raw key | confirm + worker FAILED branches |
| Confirm requires exact r2_key match | `GetVideoByIDForUpdate` + key rotation | `video.go` (r2_key rotation, phase-4) |

## Secrets hygiene

- `.env` is gitignored; only `.env.example` is tracked (no
  real values).
- All dev-local default passwords (`change-me-*`) were
  rotated on 2026-09-04 (issue #31): postgres superuser,
  tiktok_api + tiktok_worker roles, redis, media-token
  secret - generated via `openssl rand`, altered live
  (`ALTER USER/ROLE`), stack recreated, and the full
  integration smoke re-run: **16 PASS / 0 FAIL** with the
  new credentials.
- Production deploys MUST regenerate every secret (see the
  prod-domain migration runbook) - never reuse dev values.
- `ZITADEL_TARGET_SIGNING_KEY` is the Actions V2 webhook
  HMAC key returned once at target creation; treat as a
  secret.

## Known limitations (tracked, not silent)

- **Per-process rate limits**: webhook + auth rate limiters
  are in-process `sync.Map` counters (single-instance only).
  A second gateway replica would need Redis-backed
  counters.
- **/healthz during verifier retry**: on cold start with
  Zitadel slow to boot, the gateway retries the verifier
  build for up to 60s; /healthz is unreachable during that
  window (liveness vs readiness split proposed as a
  follow-up, not silently fixed - see HANDOFF).
- **Local E2E nginx DNS**: after a VPS reboot the nginx
  container can hold a stale IP for api-gateway until
  restarted (`docker compose restart nginx`). Dev-env
  quirk; production nginx uses a fixed upstream + systemd
  ordering.
- **No automated security scanning** (trivy/dependabot) in
  CI yet - candidate for a follow-up issue.

## Reporting a vulnerability

Please report privately to the repository owner (GitHub
security advisory or the contact in the org profile). Do
not open a public issue for security bugs. Include: affected
service/endpoint, reproduction steps, and impact
assessment. We aim to respond within 72 hours.
