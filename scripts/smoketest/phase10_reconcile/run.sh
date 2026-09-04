#!/usr/bin/env bash
# =====================================================
# phase10_reconcile/run.sh - R2 orphan reconciliation
# sweeper verification (Fase 10, issue #44).
#
# Exercises the FULL Detection -> Action -> Safety ->
# Verification criteria from the issue against live
# Postgres + R2 + Redis (the local E2E stack):
#
#   1. NEGATIVE CONTROL (the actual bug the sweeper
#      exists for): a tombstoned user whose R2 objects
#      survived because the post-commit cleanup:objects
#      enqueue was "lost". Simulated by seeding a user
#      row (tombstoned >24h) + R2 objects under
#      uploads/<uid>/ and hls/<uid>/ with NO videos rows
#      pointing at them - exactly the state a
#      crash-between-commit-and-enqueue leaves behind.
#      -> tick must detect the orphans, enqueue
#      cleanup:objects, and the worker must delete them
#      from R2.
#
#   2. POSITIVE CONTROL: an active user with a live R2
#      object must NOT be touched (query only returns
#      tombstoned users; sweeper cannot operate on
#      actives even if it wanted to).
#
#   3. IDEMPOTENCY: a second tick against the same
#      (now-cleaned) user must find 0 orphans and enqueue
#      nothing.
#
#   4. DRY RUN: with RECONCILE_DRY_RUN=true the tick logs
#      the orphan candidate but does NOT enqueue cleanup
#      (object still present afterwards).
#
# Preconditions: local E2E stack up (see HANDOFF), worker
# rebuilt from current source with the reconcile handler.
#
# Usage:
#   bash scripts/smoketest/phase10_reconcile/run.sh
#
# Exit 0 = all PASS.
# =====================================================
set -uo pipefail

PASS=0; FAIL=0; FAILED_STEPS=()
pass()  { PASS=$((PASS+1)); printf '   PASS: %s\n' "$1"; }
fail()  { FAIL=$((FAIL+1)); FAILED_STEPS+=("$1"); printf '   FAIL: %s\n' "$1"; }
note()  { printf '   note: %s\n' "$1"; }
log()   { printf '\n== %s ==\n' "$1"; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
ENV_FILE="$REPO_ROOT/.env"
cd "$SCRIPT_DIR"
envget(){ grep -E "^$1=" "$ENV_FILE" | head -1 | cut -d= -f2-; }

# -q suppresses psql's command-status line ("INSERT 0 1")
# so INSERT..RETURNING yields exactly the returned column.
# tr removes any stray whitespace/CR from docker exec.
psql_q() { docker exec mokibox-postgres psql -q -U postgres -d tiktok -At -c "$1" | tr -d '[:space:]'; }

worker_log_mark() { WORKER_MARK=$(date -u +%Y-%m-%dT%H:%M:%S); }
worker_log()      { docker logs mokibox-transcoder-worker --since "${WORKER_MARK:-1h}" 2>&1; }

# ---------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------
docker inspect mokibox-transcoder-worker >/dev/null 2>&1 || { echo "FATAL: worker not running"; exit 2; }
command -v jq >/dev/null || { echo "FATAL: jq missing"; exit 2; }

R2_ENDPOINT="$(envget R2_ENDPOINT)"
R2_ACCESS_KEY_ID="$(envget R2_ACCESS_KEY_ID)"
R2_SECRET_ACCESS_KEY="$(envget R2_SECRET_ACCESS_KEY)"
R2_BUCKET="$(envget R2_BUCKET)"
[[ -n "$R2_ENDPOINT" ]] || { echo "FATAL: R2 env missing"; exit 2; }

# The worker image must contain the reconcile handler; a
# stale image would silently test nothing (masked-bug
# lesson).
log "RESET+BUILD: rebuild worker image, restart worker"
docker compose build transcoder-worker > /tmp/phase10_reconcile_build.log 2>&1 \
  || { echo "FATAL: worker build failed (see /tmp/phase10_reconcile_build.log)"; exit 2; }
docker compose up -d transcoder-worker > /dev/null 2>&1
sleep 3
docker exec mokibox-transcoder-worker printenv RECONCILE_INTERVAL >/dev/null 2>&1 \
  && pass "worker running with reconcile env" \
  || fail "worker missing RECONCILE_* env (compose not applied?)"

# ---------------------------------------------------------------
# Rerun-ability: purge smoke users + their R2 objects from
# any previous run before seeding fresh state.
# ---------------------------------------------------------------
log "RESET: purge previous smoke state"
ORPHAN_UID=$(psql_q "SELECT id FROM users WHERE username='reconcile_orphan_user' LIMIT 1")
ACTIVE_UID=$(psql_q "SELECT id FROM users WHERE username='reconcile_active_user' LIMIT 1")
if [[ -n "$ORPHAN_UID" ]]; then
  docker run --rm --network mokibox_backend \
    -e R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" -e R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
    -e R2_ENDPOINT="$R2_ENDPOINT" -e R2_BUCKET="$R2_BUCKET" \
    golang:1.25.5-alpine true 2>/dev/null
  # best-effort prefix wipe via existing r2 helpers is not
  # available; direct SQL purge + R2 delete via psql-driven
  # key list is overkill - cleanup:objects task handles it:
  echo "$ORPHAN_UID" | grep -qE '^[0-9a-f-]{36}$' && \
    psql_q "DELETE FROM users WHERE id='$ORPHAN_UID'" >/dev/null && note "purged orphan user row"
fi
[[ -n "$ACTIVE_UID" ]] && psql_q "DELETE FROM users WHERE id='$ACTIVE_UID'" >/dev/null && note "purged active user row"
pass "reset done (stale rows removed if any)"

# ---------------------------------------------------------------
# DRAIN: a failed reconcile:tick from an earlier smoke run
# can still sit in asynq's retry set (asynq retries returned
# errors); when it eventually runs it would delete freshly
# seeded orphans out from under this run's dry-run phase
# (observed live in run 4: a stale non-dry tick from run 3
# executed right after the grant landed). Ticks are
# idempotent, so the safe drain is: enqueue one real tick
# now (no eligible users exist post-reset) and wait for it
# to complete - any stale retries also settle here.
# ---------------------------------------------------------------
log "DRAIN: settle pending reconcile tasks before seeding"
worker_log_mark
docker run --rm --network mokibox_backend \
  -e REDIS_ADDR="$(envget REDIS_ADDR)" -e REDIS_PASSWORD="$(envget REDIS_PASSWORD)" \
  -v "$REPO_ROOT:/repo" -w /repo \
  golang:1.25.5-alpine go run ./scripts/smoketest/reconcile_enqueue -batch 10 -dry-run=false >/dev/null 2>&1
DRAINED=0
for i in $(seq 1 24); do
  grep -q "tick complete" <<<"$(worker_log)" && { DRAINED=1; break; }
  sleep 5
done
[[ $DRAINED -eq 1 ]] && pass "reconcile queue drained (tick completed, 0 eligible users)" \
  || fail "drain tick did not complete within 120s - stale tasks may interfere"

# ---------------------------------------------------------------
# Seed: tombstoned orphan user (deleted_at > 24h ago) with
# orphan R2 objects; active user with a live object.
# ---------------------------------------------------------------
log "SEED: tombstoned orphan user + R2 objects (no videos rows)"
ORPHAN_UID=$(psql_q "INSERT INTO users (zitadel_id, username, is_active, deleted_at) \
  VALUES ('reconcile-orphan-zid', 'reconcile_orphan_user', FALSE, NOW() - INTERVAL '25 hours') \
  RETURNING id")
[[ "$ORPHAN_UID" =~ ^[0-9a-f-]{36}$ ]] && pass "orphan user seeded ($ORPHAN_UID, tombstoned 25h ago)" \
  || { fail "seed orphan user: got '$ORPHAN_UID'"; exit 1; }

ACTIVE_UID=$(psql_q "INSERT INTO users (zitadel_id, username, is_active) \
  VALUES ('reconcile-active-zid', 'reconcile_active_user', TRUE) \
  RETURNING id")
[[ "$ACTIVE_UID" =~ ^[0-9a-f-]{36}$ ]] && pass "active control user seeded ($ACTIVE_UID)" \
  || { fail "seed active user: got '$ACTIVE_UID'"; exit 1; }

# Upload orphan objects to R2 via presign from the host
# (raw aws sdk not available; reuse the api-gateway's own
# presign is impossible for arbitrary users - instead use
# the s3-compatible PUT through the r2_put-style helper via
# docker golang + minio client? Simpler: use curl with a
# presigned URL generated by a tiny go run).
put_r2() { # $1=key $2=content
  URL=$(docker run --rm --network mokibox_backend \
    -v "$REPO_ROOT:/repo" -w /repo \
    -e R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" \
    -e R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
    -e R2_ENDPOINT="$R2_ENDPOINT" -e R2_BUCKET="$R2_BUCKET" \
    golang:1.25.5-alpine go run ./scripts/smoketest/reconcile_presign -put -key "$1" 2>/dev/null)
  CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary "$2" \
    -H "Content-Type: application/octet-stream" "$URL")
  echo "$CODE"
}

ORPHAN_KEY_1="uploads/$ORPHAN_UID/seed-orphan-1.bin"
ORPHAN_KEY_2="hls/$ORPHAN_UID/seed-orphan-2.bin"
ACTIVE_KEY="uploads/$ACTIVE_UID/seed-active.bin"

C1=$(put_r2 "$ORPHAN_KEY_1" "orphan-payload-1")
[[ "$C1" == "200" ]] && pass "R2 orphan object 1 uploaded" || { fail "R2 upload 1 = $C1"; }
C2=$(put_r2 "$ORPHAN_KEY_2" "orphan-payload-2")
[[ "$C2" == "200" ]] && pass "R2 orphan object 2 uploaded" || { fail "R2 upload 2 = $C2"; }
C3=$(put_r2 "$ACTIVE_KEY" "active-user-live-object")
[[ "$C3" == "200" ]] && pass "R2 active-user object uploaded (control)" || { fail "R2 upload 3 = $C3"; }

# ---------------------------------------------------------------
# Dry-run first: candidates logged, nothing deleted.
# ---------------------------------------------------------------
log "DRY RUN: tick with RECONCILE_DRY_RUN semantics (payload-driven)"
worker_log_mark
docker run --rm --network mokibox_backend \
  -e REDIS_ADDR="$(envget REDIS_ADDR)" -e REDIS_PASSWORD="$(envget REDIS_PASSWORD)" \
  -v "$REPO_ROOT:/repo" -w /repo \
  golang:1.25.5-alpine go run ./scripts/smoketest/reconcile_enqueue -batch 10 -dry-run=true 2>&1 | tail -1
sleep 4
DRYLOG=$(worker_log)
if grep -q "DRY RUN - orphan candidate" <<<"$DRYLOG"; then
  pass "dry-run tick logged the orphan candidate"
else
  fail "dry-run did not log an orphan candidate"
fi

r2_has() { # $1=key -> exit 0 if object exists
  docker run --rm --network mokibox_backend \
    -e R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" -e R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
    -e R2_ENDPOINT="$R2_ENDPOINT" -e R2_BUCKET="$R2_BUCKET" \
    -v "$REPO_ROOT:/repo" -w /repo \
    golang:1.25.5-alpine go run ./scripts/smoketest/r2_head -key "$1" 2>/dev/null
}

if r2_has "$ORPHAN_KEY_1"; then
  pass "dry-run left the orphan object in place (no cleanup enqueued)"
else
  fail "dry-run deleted the orphan - dry-run semantics broken"
fi

# ---------------------------------------------------------------
# Real tick: orphan detected -> cleanup enqueued -> worker
# deletes from R2.
# ---------------------------------------------------------------
log "SWEEP: real tick -> detect -> enqueue cleanup:objects -> R2 delete"
worker_log_mark
docker run --rm --network mokibox_backend \
  -e REDIS_ADDR="$(envget REDIS_ADDR)" -e REDIS_PASSWORD="$(envget REDIS_PASSWORD)" \
  -v "$REPO_ROOT:/repo" -w /repo \
  golang:1.25.5-alpine go run ./scripts/smoketest/reconcile_enqueue -batch 10 -dry-run=false 2>&1 | tail -1

# Wait for the tick + cleanup:objects to run.
DELETED=0
for i in $(seq 1 24); do
  LOGS=$(worker_log)
  grep -q "tick complete" <<<"$LOGS" || { sleep 5; continue; }
  grep -q "HandleCleanupObjects: success" <<<"$LOGS" || { sleep 5; continue; }
  DELETED=1; break
done
[[ $DELETED -eq 1 ]] && pass "tick completed + cleanup:objects consumed" \
  || fail "tick/cleanup did not complete within 120s"

if grep -q '"users_with_orphans":1' <<<"$(worker_log)"; then
  pass "exactly 1 user with orphans detected (the seeded one)"
else
  fail "users_with_orphans != 1 in tick summary"
fi

# Orphan keys gone?
if r2_has "$ORPHAN_KEY_1"; then fail "orphan key 1 still in R2 after sweep"; else pass "orphan key 1 deleted from R2"; fi
if r2_has "$ORPHAN_KEY_2"; then fail "orphan key 2 still in R2 after sweep"; else pass "orphan key 2 deleted from R2"; fi

# Active user object untouched?
if r2_has "$ACTIVE_KEY"; then
  pass "active user's object NOT touched by the sweep"
else
  fail "active user's object was deleted - sweeper violated the safety rule"
fi

# ---------------------------------------------------------------
# Idempotency: second tick finds 0 orphans.
# ---------------------------------------------------------------
log "IDEMPOTENCY: second tick must find 0 orphans"
worker_log_mark
docker run --rm --network mokibox_backend \
  -e REDIS_ADDR="$(envget REDIS_ADDR)" -e REDIS_PASSWORD="$(envget REDIS_PASSWORD)" \
  -v "$REPO_ROOT:/repo" -w /repo \
  golang:1.25.5-alpine go run ./scripts/smoketest/reconcile_enqueue -batch 10 -dry-run=false 2>&1 | tail -1
IDEMP=0
for i in $(seq 1 12); do
  L=$(worker_log)
  grep -q "tick complete" <<<"$L" && { IDEM=1; break; }
  sleep 5
done
[[ $IDEM -eq 1 ]] || fail "second tick did not complete"
if grep -q '"users_with_orphans":0' <<<"$(worker_log)"; then
  pass "second tick found 0 orphans (idempotent)"
else
  fail "second tick still reports orphans - not idempotent"
fi

# ---------------------------------------------------------------
# Cleanup: remove seed rows + active-user object (leave env
# clean for reruns).
# ---------------------------------------------------------------
log "CLEANUP: remove seed state"
psql_q "DELETE FROM users WHERE id IN ('$ORPHAN_UID','$ACTIVE_UID')" >/dev/null
worker_log_mark
docker run --rm --network mokibox_backend \
  -e REDIS_ADDR="$(envget REDIS_ADDR)" -e REDIS_PASSWORD="$(envget REDIS_PASSWORD)" \
  -v "$REPO_ROOT:/repo" -w /repo \
  golang:1.25.5-alpine go run ./scripts/smoketest/phase5_enqueue -op cleanup-objects -keys "$ACTIVE_KEY" 2>&1 | tail -1
sleep 3
note "active-user seed object removed; users rows purged"

# ---------------------------------------------------------------
printf '\n=============================================\n'
printf 'RECONCILE SMOKE RESULT: %s PASS, %s FAIL\n' "$PASS" "$FAIL"
if [[ $FAIL -gt 0 ]]; then
  printf 'FAILED STEPS:\n'; printf '  - %s\n' "${FAILED_STEPS[@]}"
  exit 1
fi
printf 'ALL STEPS PASSED\n'
exit 0
