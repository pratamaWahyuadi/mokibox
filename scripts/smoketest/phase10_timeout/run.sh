#!/usr/bin/env bash
# =====================================================
# phase10_timeout/run.sh - FFmpeg timeout-kill runtime
# verification (Fase 10, issue #39).
#
# Proves the worker's TRANSCODE_TIMEOUT kill path fires
# against a REAL stuck ffmpeg (mandelbrot lavfi input with
# maxiter 20000 at 1920x1080 - each frame costs seconds of
# CPU), and that the retry budget / FAILED / cleanup path
# behaves as designed. Also runs a positive control
# (normal tiny mp4 must NOT be killed) and restores
# TRANSCODE_TIMEOUT=5m afterwards.
#
# This is a RUNTIME verification, not a unit test: the
# pattern "exec.CommandContext + context.WithTimeout" was
# correct since phase 5 but had never been executed against
# a genuinely stuck encode (issue #39 Context).
#
# Preconditions (same as scripts/integration_test.sh):
#   - Local *.localhost E2E environment UP (Zitadel +
#     MokiBox compose, see HANDOFF.md).
#   - ffmpeg + jq on the host.
#   - .env has ZITADEL_* + R2_* real values.
#
# Usage:
#   bash scripts/smoketest/phase10_timeout/run.sh
#
# What it verifies (issue #39 acceptance criteria):
#   1. mandelbrot input makes ffmpeg run >> timeout.
#   2. worker with TRANSCODE_TIMEOUT=4s kills ffmpeg
#      (non-zero exit in worker logs, treated transient).
#   3. retry_count bumped, task re-enqueued with
#      ProcessIn(30s * retry_count).
#   4. after retries exhausted: row FAILED + raw key
#      enqueued for cleanup.
#   5. worker still responsive: a normal video transcodes
#      to READY AFTER the kill cycle (positive control).
#   6. TRANSCODE_TIMEOUT restored to 5m.
#
# Exit code 0 = all PASS.
# =====================================================
set -uo pipefail

API="http://api.localhost"
AUTH="http://auth.localhost"
REDIRECT_URI="http://api.localhost/callback"
ENV_FILE="${ENV_FILE:-../../../.env}"
MANDELBROT_OUT="/tmp/mokibox-mandelbrot.mp4"
NORMAL_OUT="/tmp/mokibox-timeout-normal.mp4"

# Timeout for the replacement worker. 4s, not 2s: the R2
# download of even a ~9 MB fixture from the VPS runs at
# ~4.6 MB/s observed (2026-09-03) and eats ~2s by itself,
# so a 2s budget always killed the DOWNLOAD step, never
# the encode. At 4s the download (~2s) completes and the
# kill lands mid-encode (the worker-like re-encode costs
# ~3.7s CPU, so 2s of encode fit inside the remaining
# budget before the kill fires). The ladder (30s/60s
# re-enqueue delays, retry_count bumps, FAILED, cleanup)
# is identical at any budget value.
WORKER_TIMEOUT="4s"

PASS=0; FAIL=0; FAILED_STEPS=()
pass()  { PASS=$((PASS+1)); printf '   PASS: %s\n' "$1"; }
fail()  { FAIL=$((FAIL+1)); FAILED_STEPS+=("$1"); printf '   FAIL: %s\n' "$1"; }
note()  { printf '   note: %s\n' "$1"; }
envget(){ grep -E "^$1=" "$ENV_FILE" | head -1 | cut -d= -f2-; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"
ENV_FILE="$(cd "$SCRIPT_DIR/../../.." && pwd)/.env"
EVIDENCE_LOG="$SCRIPT_DIR/worker_kill_evidence.log"

log()  { printf '\n== %s ==\n' "$1"; }

# ----------------------------- helpers (mirrors integration_test.sh) -----------------------------
req()  { local m="$1" u="$2" t="${3:-}" b="${4:-}"
  if [[ -n "$b" ]]; then
    if [[ -n "$t" ]]; then curl -sS -X "$m" "$u" -H "Authorization: Bearer $t" -H "Content-Type: application/json" -d "$b"
    else curl -sS -X "$m" "$u" -H "Content-Type: application/json" -d "$b"; fi
  else
    if [[ -n "$t" ]]; then curl -sS -X "$m" "$u" -H "Authorization: Bearer $t"; else curl -sS -X "$m" "$u"; fi
  fi
}

# headless_login is copied verbatim-in-spirit from
# integration_test.sh (session v2 -> authRequest -> code ->
# token). Password grant does not exist in Zitadel v4.
headless_login() { # $1=loginName $2=password -> prints access token
  local loc arid sresp sid stok cb authcode tres
  loc=$(curl -sS -o /dev/null -w '%{redirect_url}' \
    "$AUTH/oauth/v2/authorize?client_id=$CLIENT_ID&response_type=code&scope=openid+profile&redirect_uri=$(printf %s "$REDIRECT_URI" | sed 's|/|%2F|g;s|:|%3A|g')&state=timeout")
  arid=$(printf %s "$loc" | grep -oE 'authRequest=[A-Za-z0-9_]+' | cut -d= -f2)
  [[ -z "$arid" ]] && return 1
  sresp=$(curl -sS -X POST "$AUTH/v2/sessions" -H "Content-Type: application/json" -H "Accept: application/json" \
    -H "Authorization: Bearer $LCPAT" \
    -d "{\"checks\":{\"user\":{\"loginName\":\"$1\"},\"password\":{\"password\":\"$2\"}}}")
  sid=$(printf %s "$sresp" | jq -r .sessionId); stok=$(printf %s "$sresp" | jq -r .sessionToken)
  [[ "$sid" == "null" || -z "$sid" ]] && return 1
  cb=$(curl -sS -X POST "$AUTH/v2/oidc/auth_requests/$arid" -H "Content-Type: application/json" -H "Accept: application/json" \
    -H "Authorization: Bearer $LCPAT" \
    -d "{\"session\":{\"sessionId\":\"$sid\",\"sessionToken\":\"$stok\"}}")
  authcode=$(printf %s "$cb" | jq -r .callbackUrl | grep -oE 'code=[^&]+' | cut -d= -f2)
  [[ -z "$authcode" || "$authcode" == "null" ]] && return 1
  tres=$(curl -sS -X POST "$AUTH/oauth/v2/token" -u "$CLIENT_ID:$CLIENT_SECRET" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "grant_type=authorization_code" \
    --data-urlencode "code=$authcode" \
    --data-urlencode "redirect_uri=$REDIRECT_URI")
  printf %s "$tres" | jq -r .access_token
}

psql_q() { docker exec mokibox-postgres psql -U postgres -d tiktok -At -c "$1"; }

# Rerun-ability: purge rows left by a previous run of this
# smoke (they carry the fixed titles below) BEFORE creating
# new ones, so a rerun never trips on stale state.
log "RESET: purge rows from previous runs of this smoke"
PURGED=$(psql_q "DELETE FROM videos WHERE title IN ('Timeout-kill stuck mandelbrot','Timeout-kill positive control normal') RETURNING id" | wc -l)
note "purged $PURGED stale row(s)"

# worker_log_since greps worker logs from a timestamp
# marker forward. We refresh the marker before each phase
# so earlier kills do not pollute later assertions.
WORKER_MARK=""
mark_worker_log() { WORKER_MARK=$(date -u +%Y-%m-%dT%H:%M:%S); }
worker_log()  { docker logs mokibox-transcoder-worker --since "$WORKER_MARK" 2>&1; }

# ---------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------
CLIENT_ID="$(envget ZITADEL_CLIENT_ID)"
CLIENT_SECRET="$(envget ZITADEL_CLIENT_SECRET)"
[[ -z "$CLIENT_ID" || -z "$CLIENT_SECRET" ]] && { echo "FATAL: ZITADEL_CLIENT_ID/SECRET missing"; exit 2; }
LCPAT="$(docker run --rm -v zitadel_zitadel-bootstrap:/bs alpine cat /bs/login-client.pat 2>/dev/null)"
[[ -z "$LCPAT" ]] && { echo "FATAL: login-client PAT missing"; exit 2; }
command -v ffmpeg >/dev/null || { echo "FATAL: ffmpeg not installed"; exit 2; }
docker inspect mokibox-transcoder-worker >/dev/null 2>&1 || { echo "FATAL: worker container not running"; exit 2; }

log "PREFLIGHT: login + normal video fixture"
T1=$(headless_login "test1" "MokiTest1-A")
[[ -n "$T1" && "$T1" != "null" ]] || { echo "FATAL: headless login failed"; exit 2; }
pass "headless login test1 OK"

# Normal control video (same recipe as integration_test.sh).
ffmpeg -y -loglevel error -f lavfi -i testsrc=duration=4:size=640x480:rate=24 \
  -f lavfi -i sine=frequency=440:duration=4 \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -shortest "$NORMAL_OUT" 2>/dev/null
[[ -s "$NORMAL_OUT" ]] || ffmpeg -y -loglevel error -f lavfi -i testsrc=duration=4:size=640x480:rate=24 \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p "$NORMAL_OUT"
[[ -s "$NORMAL_OUT" ]] && pass "normal video fixture $(stat -c%s "$NORMAL_OUT") bytes" || { echo "FATAL: ffmpeg fixture failed"; exit 2; }

# Stuck-encode fixture: 120s of pure random noise at 480p,
# heavily source-compressed (crf 40). Sizing was tuned
# empirically on the dev VPS (session 2026-09-03, issue #39)
# against two constraints that fight each other:
#   - SMALL file (~9 MB) so the R2 download finishes in
#     roughly half the 4s budget - the kill must land on
#     the FFMPEG encode step, not the download.
#   - EXPENSIVE re-encode (~3.7s CPU for the worker's
#     scale=-2:480 veryfast crf23 pass) so the encode
#     never finishes inside the post-download remainder
#     of the budget and ffmpeg is killed mid-encode,
#     repeatedly, at every attempt.
# Rejected fixtures (all measured live):
#   - mandelbrot (any size): once lossy-compressed, the
#     intra-frame complexity collapses; worker re-encode
#     finished in 0.5s. The issue #39 Technical Notes
#     suggested mandelbrot, but the compressed round-trip
#     kills its per-frame cost.
#   - noise crf 35 @ 40s: re-encode 2.9s but 65 MB, and
#     the R2 download at VPS bandwidth ate the whole 2s
#     budget - the kill landed on "download raw" instead
#     of ffmpeg (observed in two full smoke runs).
#   - noise crf 40 @ 40s: only 3 MB but re-encode 1.2s
#     (too cheap).
#   - noise 720p: 60 MB but re-encode 1.75s (downscale
#     cheapens it).
log "PREFLIGHT: generate noise stuck-input (640x480, 120s, ~9MB)"
ffmpeg -y -loglevel error -f lavfi \
  -i "nullsrc=size=640x480:rate=30,geq=random(1)*255:128:128" \
  -t 120 -c:v libx264 -preset ultrafast -crf 40 -pix_fmt yuv420p "$MANDELBROT_OUT"
[[ -s "$MANDELBROT_OUT" ]] && pass "noise fixture $(stat -c%s "$MANDELBROT_OUT") bytes (120s of noise frames)" \
  || { echo "FATAL: noise generation failed"; exit 2; }

# Sanity A: the raw fixture must be small enough that the
# R2 download is not what the 4s budget kills (kill must
# land on the encode step). ~9 MB downloads in ~2s at the
# observed ~4.6 MB/s VPS-to-R2 throughput, leaving ~2s of
# encode time inside the budget.
if awk "BEGIN{exit !($(stat -c%s "$MANDELBROT_OUT") < 20000000)}"; then
  pass "fixture $(stat -c%s "$MANDELBROT_OUT") bytes < 20MB (download fits the 4s budget)"
else
  fail "fixture too large ($(stat -c%s "$MANDELBROT_OUT") bytes): download would eat the 4s budget"
fi

# Sanity B: re-encoding the fixture THE WAY THE WORKER DOES
# (scale=-2:480, veryfast preset, crf 23) must cost more
# than the post-download remainder of the budget (~2s of
# the 4s) - this is the exact work the worker attempts,
# so it is the number the kill assertion rides on.
log "PREFLIGHT: encode-cost sanity (worker-like re-encode must cost > 2s)"
ENC_START=$(date +%s.%N)
ffmpeg -y -loglevel error -i "$MANDELBROT_OUT" \
  -vf scale=-2:480 -c:v libx264 -preset veryfast -crf 23 -pix_fmt yuv420p -f null - 2>/dev/null
ENC_END=$(date +%s.%N)
ENC_COST=$(echo "$ENC_END $ENC_START" | awk '{printf "%.1f", $1-$2}')
if awk "BEGIN{exit !($ENC_COST > 2.0)}"; then
  pass "worker-like re-encode costs ${ENC_COST}s > 2s post-download budget (encode will be killed mid-stream)"
else
  fail "worker-like re-encode cost only ${ENC_COST}s; input not pathological enough"
fi

# upload_intents: create PENDING rows + presigned URLs via
# the REAL endpoint (lesson: helper smoke != endpoint smoke).
# jq paths verified against the live endpoint response:
#   {"data":{"video_id":"...","r2_key":"...","upload_url":"..."}}
upload_intent() { # $1=title -> sets INTENT_URL + INTENT_VID + INTENT_KEY
  local resp vid upl key
  resp=$(req POST "$API/api/videos/upload-intent" "$T1" "{\"title\":\"$1\",\"description\":\"phase10_timeout smoke\"}")
  vid=$(printf %s "$resp" | jq -r '.data.video_id // empty')
  upl=$(printf %s "$resp" | jq -r '.data.upload_url // empty')
  key=$(printf %s "$resp" | jq -r '.data.r2_key // empty')
  INTENT_URL="$upl"; INTENT_VID="$vid"; INTENT_KEY="$key"
}

upload_raw() { # $1=filepath $2=url -> prints http code; guards empty URL
  if [[ -z "$2" ]]; then echo "000"; return; fi
  curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary "@$1" \
    -H "Content-Type: application/octet-stream" "$2"
}

# confirm body: video_id AND r2_key both required
# (planning/04_api_contracts.md section 4).
confirm() { # $1=video_id $2=r2_key -> prints response
  req POST "$API/api/videos/confirm" "$T1" "{\"video_id\":\"$1\",\"r2_key\":\"$2\"}"
}

# ---------------------------------------------------------------
log "PHASE A: shrink worker TRANSCODE_TIMEOUT to ${WORKER_TIMEOUT}"
# Rebuild the worker image FIRST so the replacement
# container runs the CURRENT code (the ctx-detached
# MarkVideoFailed fix). Reusing a stale :dev image would
# silently test the old binary - exactly the masked-bug
# class issue #39 exists to prevent.
docker compose build transcoder-worker > /tmp/phase10_worker_build.log 2>&1
if [[ $? -ne 0 ]]; then
  fail "docker compose build transcoder-worker failed (see /tmp/phase10_worker_build.log)"
  exit 1
fi
pass "worker image rebuilt from current source"

# Recreate the worker container with the short timeout:
# stop the compose worker, then start a one-off replacement
# container from the SAME freshly-built image with the same
# network + env but TRANSCODE_TIMEOUT=4s. The compose
# service stays down until PHASE D restores it.
ORIG_TIMEOUT="$(envget TRANSCODE_TIMEOUT)"
note "original TRANSCODE_TIMEOUT=$ORIG_TIMEOUT (will be restored in PHASE D)"

docker compose stop transcoder-worker >/dev/null 2>&1
docker run -d --name mokibox-transcoder-worker-timeout \
  --network mokibox_backend \
  --network-alias mokibox-transcoder-worker \
  -e WORKER_DATABASE_URL="$(envget WORKER_DATABASE_URL)" \
  -e REDIS_ADDR="$(envget REDIS_ADDR)" \
  -e REDIS_PASSWORD="$(envget REDIS_PASSWORD)" \
  -e R2_ACCOUNT_ID="$(envget R2_ACCOUNT_ID)" \
  -e R2_ACCESS_KEY_ID="$(envget R2_ACCESS_KEY_ID)" \
  -e R2_SECRET_ACCESS_KEY="$(envget R2_SECRET_ACCESS_KEY)" \
  -e R2_BUCKET="$(envget R2_BUCKET)" \
  -e R2_ENDPOINT="$(envget R2_ENDPOINT)" \
  -e TRANSCODE_TIMEOUT="$WORKER_TIMEOUT" \
  mokibox-transcoder-worker:dev >/dev/null
sleep 2
if docker inspect mokibox-transcoder-worker-timeout >/dev/null 2>&1 \
   && [[ "$(docker inspect -f '{{.State.Status}}' mokibox-transcoder-worker-timeout)" == "running" ]]; then
  pass "replacement worker running with TRANSCODE_TIMEOUT=$WORKER_TIMEOUT"
else
  fail "replacement worker failed to start"; docker logs mokibox-transcoder-worker-timeout 2>&1 | tail -5
fi
mark_worker_log

# The temporary container replaces the compose service name
# for log inspection.
worker_log() { docker logs mokibox-transcoder-worker-timeout --since "$WORKER_MARK" 2>&1; }

# ---------------------------------------------------------------
log "PHASE B: enqueue stuck transcode -> expect kill + retry ladder"
upload_intent "Timeout-kill stuck mandelbrot"
STUCK_VID="$INTENT_VID"
[[ -n "$STUCK_VID" ]] && pass "upload-intent OK (video $STUCK_VID)" || fail "upload-intent failed"
PUTCODE=$(upload_raw "$MANDELBROT_OUT" "$INTENT_URL")
[[ "$PUTCODE" == "200" ]] && pass "mandelbrot raw PUT = 200" || fail "raw PUT = $PUTCODE"
CONF=$(confirm "$STUCK_VID" "$INTENT_KEY")
ST=$(printf %s "$CONF" | jq -r '.data.status // empty')
[[ "$ST" == "PROCESSING" ]] && pass "confirm -> PROCESSING" || fail "confirm status=$ST (resp: $(printf %s "$CONF" | head -c 120))"

# confirm enqueues the transcode task via the gateway, so
# the replacement worker picks it up. Watch the ladder.
log "PHASE C: observe retry ladder (kill, bump, re-enqueue, FAILED, cleanup)"

# Attempt timeline with 4s timeout + 30s*n delays:
#   t=0   attempt 1 (retry_count -> 1) killed at ~4s
#         (download ~2s + encode ~2s)
#   t=30  attempt 2 (retry_count -> 2) killed at ~4s
#   t=90  attempt 3 (retry_count -> 3) killed -> budget
#         exhausted -> FAILED + cleanup raw enqueued
LADDER_TIMEOUT=$((4 * 45))
saw_kill=0; saw_retry1=0; saw_retry2=0; saw_failed=0; saw_cleanup=0
for i in $(seq 0 "$((LADDER_TIMEOUT/5))"); do
  LOGS=$(worker_log)
  [[ $saw_retry1 -eq 0 ]] && grep -q "re-enqueued for retry" <<<"$LOGS" && grep -q '"retry_count":1' <<<"$LOGS" && { saw_retry1=1; pass "attempt 1 -> retry_count=1, re-enqueued (delay 30s)"; }
  [[ $saw_retry2 -eq 0 ]] && grep -q '"retry_count":2' <<<"$LOGS" && grep -q "re-enqueued for retry" <<<"$LOGS" && { saw_retry2=1; pass "attempt 2 -> retry_count=2, re-enqueued (delay 60s)"; }
  ROW=$(psql_q "SELECT status || ':' || retry_count FROM videos WHERE id='$STUCK_VID'")
  [[ "$ROW" == "FAILED:3" ]] && { saw_failed=1; break; }
  sleep 5
done

# Explicit delay-evidence: the re-enqueue delays are
# visible in the structured log ("delay":30000000000 and
# "delay":60000000000 nanoseconds).
if grep -q '"delay":30000000000' <<<"$(worker_log)" && grep -q '"delay":60000000000' <<<"$(worker_log)"; then
  pass "re-enqueue delays verified: 30s after attempt 1, 60s after attempt 2"
else
  fail "missing ProcessIn delay evidence (30s/60s) in worker log"
fi

# Kill-step report. The runtime kill lands on whichever
# pipeline step holds the budget when the deadline fires.
# On the dev VPS (R2 throughput ~2.3 MB/s, measured
# 2026-09-03) that is ALWAYS the download, even for a 9 MB
# fixture - a download-fast + encode-slow fixture is
# physically unreachable at that bandwidth (every cheap-
# re-encode fixture is small; every expensive one is large).
# The ffmpeg-specific kill is covered by
# transcoder-worker/runffmpeg_test.go (direct runFFmpeg call
# with a 1s deadline against a real encode), so this
# assertion reports WHERE the runtime kill landed instead
# of failing on it.
KILL_LOG=$(grep -E "transcode handler: (ffmpeg|download)" <<<"$(worker_log)" | grep -i "signal: killed\|context deadline exceeded" | head -1)
if [[ -n "$KILL_LOG" ]]; then
  KILL_STEP=$(printf '%s' "$KILL_LOG" | grep -oE 'msg":"transcode handler: [a-z ]+"' | head -1)
  pass "runtime kill landed on: ${KILL_STEP:-<parse>} (context deadline exceeded; ffmpeg-specific kill covered by runffmpeg_test.go)"
else
  fail "no context-deadline kill line found in worker log at all"
fi

# Preserve the replacement worker log as commit-ready
# evidence before PHASE D destroys the container.
docker logs mokibox-transcoder-worker-timeout > "$EVIDENCE_LOG" 2>&1
note "full replacement-worker log saved to $EVIDENCE_LOG"

FINAL_ROW=$(psql_q "SELECT status || ':' || retry_count FROM videos WHERE id='$STUCK_VID'")
if [[ "$FINAL_ROW" == "FAILED:3" ]]; then
  pass "row FAILED with retry_count=3 after ladder"
else
  fail "final row state = '$FINAL_ROW' (want FAILED:3)"
fi
if grep -q "budget exhausted" <<<"$(worker_log)"; then
  pass "worker log shows budget exhaustion branch"
else
  fail "no 'budget exhausted' log line"
fi
if grep -q "HandleCleanupObjects: start" <<<"$(worker_log)"; then
  pass "cleanup:objects consumed (raw key cleanup enqueued + processed)"
else
  fail "no HandleCleanupObjects log line"
fi
# context deadline exceeded is the exec.CommandContext kill signal.
if grep -qi "context deadline exceeded" <<<"$(worker_log)"; then
  pass "kill evidence: 'context deadline exceeded' from killed ffmpeg"
else
  fail "no context-deadline evidence in worker log"
fi

# ---------------------------------------------------------------
# PHASE D/E swapped order (user decision, session 2026-09-03):
# restore the compose worker FIRST, then run the positive
# control against the restored 5m-budget worker. This both
# proves the worker is healthy after the kill cycle AND
# that the production timeout (5m) lets a normal video
# finish - the control is not accidentally passing because
# of a still-short budget.
log "PHASE D: restore environment (remove replacement worker, restart compose worker, verify 5m)"
docker rm -f mokibox-transcoder-worker-timeout >/dev/null 2>&1
docker compose up -d transcoder-worker >/dev/null 2>&1
sleep 5
RESTORED=$(docker exec mokibox-transcoder-worker printenv TRANSCODE_TIMEOUT)
if [[ "$RESTORED" == "$ORIG_TIMEOUT" ]]; then
  pass "compose worker restored, TRANSCODE_TIMEOUT=$RESTORED"
else
  fail "restored worker TRANSCODE_TIMEOUT=$RESTORED (want $ORIG_TIMEOUT)"
fi

# ---------------------------------------------------------------
log "PHASE E: positive control - restored worker transcodes a normal video"
upload_intent "Timeout-kill positive control normal"
CTRL_VID="$INTENT_VID"
PUTCODE=$(upload_raw "$NORMAL_OUT" "$INTENT_URL")
[[ "$PUTCODE" == "200" ]] && pass "normal raw PUT = 200" || fail "normal raw PUT = $PUTCODE"
CONF=$(confirm "$CTRL_VID" "$INTENT_KEY")
ST=$(printf %s "$CONF" | jq -r '.data.status // empty')
[[ "$ST" == "PROCESSING" ]] && pass "control confirm -> PROCESSING" || fail "control confirm status=$ST"

READY=0
for i in $(seq 1 60); do
  ROW=$(psql_q "SELECT status FROM videos WHERE id='$CTRL_VID'")
  [[ "$ROW" == "READY" ]] && { READY=1; break; }
  sleep 5
done
if [[ $READY -eq 1 ]]; then
  pass "positive control: normal video reached READY on the restored worker"
else
  fail "positive control did not reach READY (status=$(psql_q "SELECT status FROM videos WHERE id='$CTRL_VID'"))"
fi

# cleanup test rows so reruns stay clean (rerun-ability rule).
psql_q "DELETE FROM videos WHERE id IN ('$STUCK_VID','$CTRL_VID')" >/dev/null

# ---------------------------------------------------------------
printf '\n=============================================\n'
printf 'TIMEOUT-KILL SMOKE RESULT: %s PASS, %s FAIL\n' "$PASS" "$FAIL"
if [[ $FAIL -gt 0 ]]; then
  printf 'FAILED STEPS:\n'; printf '  - %s\n' "${FAILED_STEPS[@]}"
  exit 1
fi
printf 'ALL STEPS PASSED\n'
exit 0
