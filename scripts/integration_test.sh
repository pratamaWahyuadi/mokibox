#!/usr/bin/env bash
# =====================================================
# integration_test.sh - MokiBox end-to-end integration
# smoke test (Fase 10, issue #30).
#
# Runs the full user journey against the LOCAL *.localhost
# environment (see deploy/nginx/local.conf +
# docker-compose.override.yml, commit phase-10.1):
#
#   auth.localhost  -> Zitadel (via Traefik, cross-network)
#   api.localhost   -> MokiBox api-gateway
#
# Prerequisites (documented in HANDOFF.md "Local E2E
# environment" section):
#
#   1. MokiBox .env has real values for:
#      ZITADEL_CLIENT_ID, ZITADEL_API_CLIENT_ID,
#      ZITADEL_CLIENT_SECRET, ZITADEL_TARGET_SIGNING_KEY,
#      ZITADEL_ISSUER_URL=http://auth.localhost,
#      API_BASE_URL=http://api.localhost
#   2. zitadel-compose is UP with the domain override
#      (ZITADEL_DOMAIN=auth.localhost) and the
#      docker-compose.override.yml HTTPClient deny-list
#      override ( zitadel-compose/ is gitignored; the
#      override file content is reproduced in this
#      repo at scripts/zitadel-override.example.yml ).
#   3. Zitadel has: Web app (authorization_code, BASIC
#      auth, JWT access token type), API app, 2 human
#      users (test1/test2) with passwords set, and an
#      Actions V2 target + executions
#      (user.deactivated, user.removed) pointing at
#      http://api.localhost/api/webhooks/zitadel.
#   4. The login-client PAT is available from the
#      zitadel-bootstrap volume (used for the headless
#      session login; see login() below).
#   5. ffmpeg is installed (generates the test video).
#
# Usage:
#   bash scripts/integration_test.sh
#
# Exit code 0 = all steps PASS. Non-zero = at least one
# step FAILED (see the per-step report at the end).
# =====================================================
set -uo pipefail

# ----------------------------- config -----------------------------
API="http://api.localhost"
AUTH="http://auth.localhost"
REDIRECT_URI="http://api.localhost/callback"
ENV_FILE="${ENV_FILE:-.env}"
VIDEO_OUT="/tmp/mokibox-itest.mp4"
POLL_TIMEOUT="${POLL_TIMEOUT:-300}"     # 5 min for transcode
WEBHOOK_USER="test2"                    # user deactivated in step 12

PASS=0; FAIL=0; FAILED_STEPS=()

step()  { printf '\n== STEP %s: %s ==\n' "$1" "$2"; }
pass()  { PASS=$((PASS+1)); printf '   PASS: %s\n' "$1"; }
fail()  { FAIL=$((FAIL+1)); FAILED_STEPS+=("$1"); printf '   FAIL: %s\n' "$1"; }
note()  { printf '   note: %s\n' "$1"; }

req()   { # method url [token] [json-body]
  local m="$1" u="$2" t="${3:-}" b="${4:-}"
  if [[ -n "$b" ]]; then
    if [[ -n "$t" ]]; then curl -sS -X "$m" "$u" -H "Authorization: Bearer $t" -H "Content-Type: application/json" -d "$b"
    else curl -sS -X "$m" "$u" -H "Content-Type: application/json" -d "$b"; fi
  else
    if [[ -n "$t" ]]; then curl -sS -X "$m" "$u" -H "Authorization: Bearer $t"; else curl -sS -X "$m" "$u"; fi
  fi
}
code()  { # same args as req, prints http code only
  local m="$1" u="$2" t="${3:-}" b="${4:-}"
  if [[ -n "$b" ]]; then
    if [[ -n "$t" ]]; then curl -sS -o /dev/null -w '%{http_code}' -X "$m" "$u" -H "Authorization: Bearer $t" -H "Content-Type: application/json" -d "$b"
    else curl -sS -o /dev/null -w '%{http_code}' -X "$m" "$u" -H "Content-Type: application/json" -d "$b"; fi
  else
    if [[ -n "$t" ]]; then curl -sS -o /dev/null -w '%{http_code}' -X "$m" "$u" -H "Authorization: Bearer $t"; else curl -sS -o /dev/null -w '%{http_code}' -X "$m" "$u"; fi
  fi
}
envget(){ grep -E "^$1=" "$ENV_FILE" | head -1 | cut -d= -f2-; }

# ----------------------------- env -----------------------------
CLIENT_ID="$(envget ZITADEL_CLIENT_ID)"
CLIENT_SECRET="$(envget ZITADEL_CLIENT_SECRET)"
[[ -z "$CLIENT_ID" || -z "$CLIENT_SECRET" ]] && { echo "FATAL: ZITADEL_CLIENT_ID/ZITADEL_CLIENT_SECRET missing in $ENV_FILE"; exit 2; }

# login-client PAT from the zitadel bootstrap volume
LCPAT="$(docker run --rm -v zitadel_zitadel-bootstrap:/bs alpine cat /bs/login-client.pat 2>/dev/null)"
[[ -z "$LCPAT" ]] && { echo "FATAL: cannot read login-client PAT from zitadel_zitadel-bootstrap volume"; exit 2; }

# ----------------------------- helpers -----------------------------
headless_login() { # $1=loginName $2=password -> prints access token
  local loc arid sresp sid stok cb code tres
  loc=$(curl -sS -o /dev/null -w '%{redirect_url}' \
    "$AUTH/oauth/v2/authorize?client_id=$CLIENT_ID&response_type=code&scope=openid+profile&redirect_uri=$(printf %s "$REDIRECT_URI" | sed 's|/|%2F|g;s|:|%3A|g')&state=itest")
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
  code=$(printf %s "$cb" | jq -r .callbackUrl | grep -oE 'code=[^&]+' | cut -d= -f2)
  [[ -z "$code" || "$code" == "null" ]] && return 1
  tres=$(curl -sS -X POST "$AUTH/oauth/v2/token" -u "$CLIENT_ID:$CLIENT_SECRET" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "grant_type=authorization_code" \
    --data-urlencode "code=$code" \
    --data-urlencode "redirect_uri=$REDIRECT_URI")
  printf %s "$tres" | jq -r .access_token
}

wait_healthz() {
  for i in $(seq 1 30); do
    [[ "$(code GET "$API/healthz")" == "200" ]] && return 0
    sleep 2
  done
  return 1
}

# =====================================================
# STEP 0: environment sanity
# =====================================================
step 0 "environment sanity (healthz + OIDC discovery)"
wait_healthz && pass "GET /healthz = 200" || fail "healthz not reachable"
[[ "$(code GET "$AUTH/.well-known/openid-configuration")" == "200" ]] \
  && pass "OIDC discovery reachable via auth.localhost" \
  || fail "OIDC discovery unreachable"

# =====================================================
# STEP 1: headless login test1 -> real JWT
# =====================================================
step 1 "login (headless authorization_code flow) -> JWT"
T1=$(headless_login "test1" "MokiTest1-A")
if [[ -n "$T1" && "$T1" != "null" ]] && [[ "$(printf %s "$T1" | cut -d. -f2 | wc -c)" -gt 100 ]]; then
  pass "access token obtained ($(printf %s "$T1" | wc -c) bytes, JWT)"
else
  fail "login test1"; T1=""
fi

# =====================================================
# STEP 2: GET /api/users/me
# =====================================================
step 2 "GET /api/users/me"
if [[ -n "$T1" ]] && [[ "$(code GET "$API/api/users/me" "$T1")" == "200" ]]; then
  UID1=$(req GET "$API/api/users/me" "$T1" | jq -r .data.id)
  pass "200, user id $UID1"
else fail "GET /api/users/me"; UID1=""; fi

# =====================================================
# STEP 3: PUT /api/users/me (update profile)
# =====================================================
step 3 "PUT /api/users/me (update display_name)"
if [[ -n "$T1" ]] && [[ "$(code PUT "$API/api/users/me" "$T1" '{"display_name":"Integration Test One"}')" == "200" ]]; then
  pass "200, display_name updated"
else fail "PUT /api/users/me"; fi

# =====================================================
# STEP 4: POST /api/videos/upload-intent
# =====================================================
step 4 "POST /api/videos/upload-intent"
INTENT=$(req POST "$API/api/videos/upload-intent" "$T1" '{"title":"Integration smoke test video","description":"uploaded by integration_test.sh"}')
VID=$(printf %s "$INTENT" | jq -r .data.video_id 2>/dev/null)
R2KEY=$(printf %s "$INTENT" | jq -r .data.r2_key 2>/dev/null)
UPL=$(printf %s "$INTENT" | jq -r .data.upload_url 2>/dev/null)
if [[ -n "$VID" && "$VID" != "null" && -n "$UPL" && "$UPL" != "null" ]]; then
  pass "video_id=$VID, presigned PUT url obtained"
else fail "upload-intent (resp: $(printf %s "$INTENT" | head -c 200))"; VID=""; fi

# =====================================================
# STEP 5: generate + PUT dummy H.264 video to R2
# =====================================================
step 5 "PUT dummy H.264 video to presigned R2 url"
if [[ -n "$UPL" && "$UPL" != "null" ]]; then
  ffmpeg -y -loglevel error -f lavfi -i testsrc=duration=4:size=640x480:rate=24 \
    -f lavfi -i sine=frequency=440:duration=4 \
    -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -shortest \
    "$VIDEO_OUT" 2>/dev/null
  if [[ ! -s "$VIDEO_OUT" ]]; then
    ffmpeg -y -loglevel error -f lavfi -i testsrc=duration=4:size=640x480:rate=24 \
      -c:v libx264 -preset ultrafast -pix_fmt yuv420p "$VIDEO_OUT"
  fi
  PUTCODE=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary "@$VIDEO_OUT" \
    -H "Content-Type: application/octet-stream" "$UPL")
  [[ "$PUTCODE" == "200" ]] && pass "R2 PUT = 200 ($(stat -c%s "$VIDEO_OUT") bytes)" \
    || fail "R2 PUT = $PUTCODE"
else fail "no upload url (skipped)"; fi

# =====================================================
# STEP 6: POST /api/videos/confirm
# =====================================================
step 6 "POST /api/videos/confirm"
CONF=$(req POST "$API/api/videos/confirm" "$T1" "{\"video_id\":\"$VID\",\"r2_key\":\"$R2KEY\"}")
CSTATUS=$(printf %s "$CONF" | jq -r .data.status 2>/dev/null)
if [[ "$CSTATUS" == "PROCESSING" ]]; then
  pass "200, status=PROCESSING"
else fail "confirm (resp: $(printf %s "$CONF" | head -c 200))"; fi

# =====================================================
# STEP 7: poll GET /api/videos/:id/status until READY
# =====================================================
step 7 "poll video status until READY (max ${POLL_TIMEOUT}s)"
FINAL_STATUS="UNKNOWN"
if [[ -n "$VID" ]]; then
  deadline=$(( $(date +%s) + POLL_TIMEOUT ))
  while [[ $(date +%s) -lt $deadline ]]; do
    S=$(req GET "$API/api/videos/$VID/status" "$T1" | jq -r '.data.status // empty' 2>/dev/null)
    [[ "$S" == "READY" || "$S" == "FAILED" || "$S" == "DELETED" ]] && { FINAL_STATUS="$S"; break; }
    sleep 5
  done
else FINAL_STATUS="SKIPPED"; fi
if [[ "$FINAL_STATUS" == "READY" ]]; then pass "transcode finished, status=READY"
elif [[ "$FINAL_STATUS" == "FAILED" ]]; then
  note "status=FAILED - retry info: $(req GET "$API/api/videos/$VID/status" "$T1" | jq -c . 2>/dev/null | head -c 300)"
  fail "transcode FAILED (reported honestly, not forced)"
else fail "status poll ended with: $FINAL_STATUS"; fi

# =====================================================
# STEP 8: GET /api/videos/:id (full detail)
# =====================================================
step 8 "GET /api/videos/:id (full VideoObject)"
if [[ "$FINAL_STATUS" == "READY" ]]; then
  DET=$(req GET "$API/api/videos/$VID" "$T1")
  DTITLE=$(printf %s "$DET" | jq -r .data.title 2>/dev/null)
  DSTATUS=$(printf %s "$DET" | jq -r .data.status 2>/dev/null)
  if [[ "$DTITLE" == "Integration smoke test video" && "$DSTATUS" == "READY" ]]; then
    pass "full detail OK (title + status READY)"
  else fail "detail mismatch (title=$DTITLE status=$DSTATUS)"; fi
else note "skipped (video not READY)"; fail "step 8 skipped"; fi

# =====================================================
# STEP 9: GET /api/videos/:id/playlist.m3u8 (master)
# =====================================================
step 9 "GET playlist.m3u8 (master playlist)"
if [[ "$FINAL_STATUS" == "READY" ]]; then
  # The route lives inside the JWT auth group, so the
  # playlist must be fetched with the Bearer JWT header
  # (the ?token= media-token path in the handler is for
  # browser clients whose middleware skip is not wired;
  # see routes.go phase 6 registration).
  PLCODE=$(curl -sS -o /tmp/playlist.m3u8 -w '%{http_code}' \
    -H "Authorization: Bearer $T1" "$API/api/videos/$VID/playlist.m3u8")
  if [[ "$PLCODE" == "200" ]] && grep -q "480p" /tmp/playlist.m3u8 2>/dev/null; then
    pass "master playlist OK ($(wc -l < /tmp/playlist.m3u8) lines, contains 480p variant)"
  else
    head -3 /tmp/playlist.m3u8 >/dev/null 2>&1
    fail "playlist http=$PLCODE (or missing 480p variant)"
  fi
else note "skipped (video not READY)"; fail "step 9 skipped"; fi

# =====================================================
# STEP 10: test1's OWN feed must NOT contain their own
#          video (feed excludes viewer's own videos per
#          FR-FEED-01), even though it is READY + public.
#          test2 (no follow yet) DOES see it - feed shows
#          public accounts' videos too, not only follows.
# =====================================================
step 10 "own-video exclusion: test1 feed must NOT contain it"
T2=$(headless_login "test2" "MokiTest2-B")
if [[ -z "$T2" || "$T2" == "null" ]]; then
  fail "login test2 (needed for step 11)"; T2=""
fi
if [[ -n "$T1" ]]; then
  OWNFEED=$(req GET "$API/api/feed/home" "$T1")
  NOWN=$(printf %s "$OWNFEED" | jq --arg v "$VID" '[.data[] | select(.id==$v)] | length' 2>/dev/null)
  if [[ "$NOWN" == "0" ]]; then pass "own video correctly excluded from own feed"
  else fail "own video leaked into own feed (FR-FEED-01 violation)"; fi
else fail "skipped (no t1 token)"; fi

# =====================================================
# STEP 11: test2 follows test1 -> video in follower feed
# =====================================================
step 11 "follow test1 -> video in test2 feed"
if [[ -n "$T2" && -n "$UID1" ]]; then
  FC=$(code POST "$API/api/users/$UID1/follow" "$T2")
  if [[ "$FC" == "201" || "$FC" == "200" ]]; then
    FEED2=$(req GET "$API/api/feed/home" "$T2")
    N2=$(printf %s "$FEED2" | jq --arg v "$VID" '[.data[] | select(.id==$v)] | length' 2>/dev/null)
    if [[ "$N2" -ge 1 ]]; then pass "video appears in test2 feed (public account)"
    else fail "video missing from test2 feed"; fi
  else fail "follow request http=$FC"; fi
else fail "skipped (no t2/uid1)"; fi

# =====================================================
# STEP 12: webhook user.deactivated with REAL Zitadel
#          signature (deactivate test2 via admin API ->
#          Zitadel fires the Actions V2 event)
# =====================================================
step 12 "webhook user.deactivated (real Zitadel-triggered)"
# deactivate the Zitadel user via the admin session (same
# headless mechanism), Zitadel emits user.deactivated, the
# Actions V2 execution POSTs to our webhook.
WUID=$(docker exec zitadel-postgres-1 psql -U postgres -d zitadel -At -c \
  "SELECT id FROM projections.login_names3_users WHERE user_name='$WEBHOOK_USER';" 2>/dev/null)
if [[ -n "$WUID" ]]; then
  ADMINSESS=$(curl -sS -X POST "$AUTH/v2/sessions" -H "Content-Type: application/json" -H "Accept: application/json" \
    -H "Authorization: Bearer $LCPAT" \
    -d '{"checks":{"user":{"loginName":"zitadel-admin@zitadel.auth.localhost"},"password":{"password":"Password1!"}}}' \
    | jq -r .sessionToken)
  DEACT=$(curl -sS -X POST "$AUTH/management/v1/users/$WUID/_deactivate" \
    -H "Authorization: Bearer $ADMINSESS" -H "Accept: application/json")
  if [[ "$(printf %s "$DEACT" | jq -r '.details.sequence // empty')" != "" ]]; then
    sleep 3  # allow the async webhook to fire
    ISACT=$(docker exec mokibox-postgres psql -U postgres -d tiktok -At -c \
      "SELECT is_active FROM users WHERE zitadel_id='$WUID';" 2>/dev/null)
    if [[ "$ISACT" == "f" ]]; then pass "user.deactivated webhook processed (is_active=false in DB)"
    else fail "webhook did not flip is_active (is_active='$ISACT')"; fi
  else fail "deactivate API call failed: $(printf %s "$DEACT" | head -c 200)"; fi
else fail "cannot resolve $WEBHOOK_USER id in Zitadel DB"; fi

# =====================================================
# STEP 13: DELETE /api/videos/:id (soft delete +
#          cleanup:video enqueue)
# =====================================================
step 13 "DELETE /api/videos/:id (soft delete + cleanup enqueue)"
if [[ -n "$T1" && -n "$VID" ]]; then
  DC=$(code DELETE "$API/api/videos/$VID" "$T1")
  if [[ "$DC" == "204" ]]; then
    ROWSTAT=$(docker exec mokibox-postgres psql -U postgres -d tiktok -At -c \
      "SELECT status FROM videos WHERE id='$VID';" 2>/dev/null)
    QLEN=$(docker exec mokibox-redis redis-cli -a change-me-redis --no-auth-warning \
      zcard asynq:pending 2>/dev/null || echo "?")
    pass "204; videos.status='$ROWSTAT' (expect DELETED); asynq pending=$QLEN"
    [[ "$ROWSTAT" == "DELETED" ]] || fail "videos.status is '$ROWSTAT' (expected DELETED)"
  else fail "DELETE http=$DC"; fi
else fail "skipped (no token/video)"; fi

# =====================================================
# report
# =====================================================
printf '\n=============================================\n'
printf 'INTEGRATION SMOKE RESULT: %d PASS, %d FAIL\n' "$PASS" "$FAIL"
if [[ "$FAIL" -gt 0 ]]; then
  printf 'Failed steps: %s\n' "${FAILED_STEPS[*]}"
  exit 1
fi
printf 'ALL STEPS PASSED\n'
exit 0