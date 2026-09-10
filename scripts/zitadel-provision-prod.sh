#!/bin/bash
# =====================================================
# zitadel-provision-prod.sh - Tahap 5 provisioning (v3)
# AUTH PATH (live-verified pattern from fase 10):
#   login-client PAT (bootstrap volume) -> ONLY for session
#   create + auth_requests. Management API requires the
#   FIRST-INSTANCE ADMIN session: create session with
#   loginName zitadel-admin@zitadel.auth.binery.my.id +
#   password, then use sessionToken as Bearer (same
#   mechanism as integration_test.sh step 10/12).
# Idempotent: search-before-create for everything.
# =====================================================
set -euo pipefail

ZITADEL_INTERNAL="http://127.0.0.1:8181"
H_HOST="Host: auth.binery.my.id"
SECDIR=/root/mokibox-secrets
mkdir -p "$SECDIR" && chmod 700 "$SECDIR"
IDS="$SECDIR/zitadel-ids.env"
ADMIN_LOGIN="zitadel-admin@zitadel.auth.binery.my.id"
ADMIN_PASS="${ADMIN_PASS:-Password1!}"
TMPD=$(mktemp -d); trap 'rm -rf "$TMPD"' EXIT

jqr() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1" 2>/dev/null; return 0; }

LCPAT=$(docker run --rm -v zitadel_zitadel-bootstrap:/bs alpine cat /bs/login-client.pat)
echo "[1] login-client PAT loaded (${#LCPAT} chars) - for session create only"

wjson() { printf '%s' "$1" > "$TMPD/body.json"; echo "$TMPD/body.json"; }

SESS=$(curl -s -X POST -H "$H_HOST" -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $LCPAT" \
  -d "{\"checks\":{\"user\":{\"loginName\":\"$ADMIN_LOGIN\"},\"password\":{\"password\":\"$ADMIN_PASS\"}}}" \
  "$ZITADEL_INTERNAL/v2/sessions")
ADMTOK=$(echo "$SESS" | jqr "d['sessionToken']")
if [ -z "${ADMTOK:-}" ] || [ "$ADMTOK" = "None" ]; then
  echo "FATAL: admin session create failed:"; echo "$SESS" | head -c 300; echo; exit 1
fi
echo "[2] admin session OK -> sessionToken bearer (${#ADMTOK} chars)"
AUTH="Authorization: Bearer $ADMTOK"

api() {
  local m=$1 p=$2 data=${3:--}
  if [ "$data" = "-" ]; then
    curl -s -X "$m" -H "$H_HOST" -H "$AUTH" -H 'Content-Type: application/json' "$ZITADEL_INTERNAL$p"
  else
    curl -s -X "$m" -H "$H_HOST" -H "$AUTH" -H 'Content-Type: application/json' -d "@$data" "$ZITADEL_INTERNAL$p"
  fi
}
qname() { printf '{"queries":[{"nameQuery":{"name":"%s","method":"TEXT_QUERY_METHOD_EQUALS"}}]}' "$1"; }

echo "[3] find-or-create project mokibox"
PROJ_ID=$(api POST /management/v1/projects/_search "$(wjson "$(qname mokibox)")" | jqr "d['result'][0]['id']")
if [ -z "${PROJ_ID:-}" ] || [ "$PROJ_ID" = "None" ]; then
  PROJ=$(api POST /management/v1/projects "$(wjson '{"name":"mokibox"}')")
  PROJ_ID=$(echo "$PROJ" | jqr "d['id']")
  if [ -z "${PROJ_ID:-}" ] || [ "$PROJ_ID" = "None" ]; then
    echo "FATAL: project create failed:"; echo "$PROJ" | head -c 300; echo; exit 1
  fi
  echo "  created project $PROJ_ID"
else
  echo "  project exists: $PROJ_ID"
fi
echo "PROJECT_ID=$PROJ_ID" > "$IDS"

app_id() { api POST "/management/v1/projects/$PROJ_ID/apps/_search" "$(wjson "$(qname "$1")")" | jqr "d['result'][0]['id']"; }
app_clientid() { api GET "/management/v1/projects/$PROJ_ID/apps/$1" | jqr "d['app']['oidcConfig']['clientId']"; }

echo "[4] app mokibox_web (Web/confidential, BASIC secret)"
WEB_ID=$(app_id mokibox_web)
if [ -z "${WEB_ID:-}" ] || [ "$WEB_ID" = "None" ]; then
  WEB=$(api POST "/management/v1/projects/$PROJ_ID/apps/oidc" "$(wjson '{"name":"mokibox_web","redirectUris":["https://mokiboxapi.binery.my.id/callback"],"grantTypesList":[0],"responseTypesList":[0],"appType":0,"authMethodType":0,"accessTokenType":1,"devMode":false}')")
  WEB_ID=$(echo "$WEB" | jqr "d['appId']")
  WEB_CID=$(echo "$WEB" | jqr "d['clientId']")
  WEB_SEC=$(echo "$WEB" | jqr "d.get('clientSecret','')")
  if [ -z "${WEB_ID:-}" ] || [ "$WEB_ID" = "None" ]; then
    echo "FATAL web create:"; echo "$WEB" | head -c 300; echo; exit 1
  fi
  echo "  created: appId=$WEB_ID clientId=$WEB_CID"
else
  echo "  exists: appId=$WEB_ID"
  WEB_CID=$(app_clientid "$WEB_ID")
  WEB_SEC=$(api POST "/management/v1/projects/$PROJ_ID/apps/$WEB_ID/oidc_config/_generate_client_secret" | jqr "d.get('clientSecret','')")
fi
printf 'WEB_APP_ID=%s\nWEB_CLIENT_ID=%s\n' "$WEB_ID" "$WEB_CID" >> "$IDS"
echo "$WEB_SEC" > "$SECDIR/zitadel-web-client-secret"; chmod 600 "$SECDIR/zitadel-web-client-secret"

echo "[5] app mokibox_spa (UserAgent/SPA, PKCE public)"
SPA_ID=$(app_id mokibox_spa)
if [ -z "${SPA_ID:-}" ] || [ "$SPA_ID" = "None" ]; then
  SPA=$(api POST "/management/v1/projects/$PROJ_ID/apps/oidc" "$(wjson '{"name":"mokibox_spa","redirectUris":["https://mokiboxapi.binery.my.id/callback"],"grantTypesList":[0],"responseTypesList":[0],"appType":1,"authMethodType":2,"accessTokenType":1,"devMode":false}')")
  SPA_ID=$(echo "$SPA" | jqr "d['appId']")
  SPA_CID=$(echo "$SPA" | jqr "d['clientId']")
  if [ -z "${SPA_ID:-}" ] || [ "$SPA_ID" = "None" ]; then
    echo "FATAL spa create:"; echo "$SPA" | head -c 300; echo; exit 1
  fi
  echo "  created: appId=$SPA_ID clientId=$SPA_CID"
else
  echo "  exists: appId=$SPA_ID"
  SPA_CID=$(app_clientid "$SPA_ID")
fi
printf 'SPA_APP_ID=%s\nSPA_CLIENT_ID=%s\n' "$SPA_ID" "$SPA_CID" >> "$IDS"

echo "[6] app mokibox_api (API resource server)"
APIA_ID=$(app_id mokibox_api)
if [ -z "${APIA_ID:-}" ] || [ "$APIA_ID" = "None" ]; then
  APIA=$(api POST "/management/v1/projects/$PROJ_ID/apps/api" "$(wjson '{"name":"mokibox_api","authTokenRoleAssertion":true,"accessTokenRoleAssertion":true}')")
  APIA_ID=$(echo "$APIA" | jqr "d['appId']")
  APIA_CID=$(echo "$APIA" | jqr "d['clientId']")
  if [ -z "${APIA_ID:-}" ] || [ "$APIA_ID" = "None" ]; then
    echo "FATAL api create:"; echo "$APIA" | head -c 300; echo; exit 1
  fi
  echo "  created: appId=$APIA_ID clientId=$APIA_CID"
else
  echo "  exists: appId=$APIA_ID"
  APIA_CID=$(api GET "/management/v1/projects/$PROJ_ID/apps/$APIA_ID" | jqr "d['app']['apiConfig']['clientId']")
fi
printf 'API_APP_ID=%s\nAPI_CLIENT_ID=%s\n' "$APIA_ID" "$APIA_CID" >> "$IDS"

echo "[7] Actions V2 target (Opsi W: public https URL)"
TGT=$(api POST /v2/actions/targets "$(wjson '{"name":"mokibox-webhook","restWebhook":{"interruptOnError":false},"endpoint":"https://mokiboxapi.binery.my.id/api/webhooks/zitadel","timeout":"10s"}')")
TGT_ID=$(echo "$TGT" | jqr "d['id']")
if [ -z "${TGT_ID:-}" ] || [ "$TGT_ID" = "None" ]; then
  echo "FATAL target create:"; echo "$TGT" | head -c 300; echo; exit 1
fi
echo "  created: targetId=$TGT_ID"
SIGN_KEY=$(echo "$TGT" | jqr "d.get('signingKey','')")
echo "TARGET_ID=$TGT_ID" >> "$IDS"
if [ -n "${SIGN_KEY:-}" ] && [ "$SIGN_KEY" != "None" ]; then
  echo "$SIGN_KEY" > "$SECDIR/zitadel-target-signing-key"; chmod 600 "$SECDIR/zitadel-target-signing-key"
  echo "  signingKey saved"
else
  echo "  WARN: signingKey not in create response"
fi

echo "[8] Execution user.deactivated -> target"
EXEC_BODY=$(printf '{"condition":{"event":{"event":"user.deactivated"}},"targets":["%s"]}' "$TGT_ID")
EXEC=$(api PUT /v2/actions/executions "$(wjson "$EXEC_BODY")")
echo "  done: $(echo "$EXEC" | head -c 120)"

mk_user() {
  local pw; pw=$(openssl rand -base64 18 | tr '+/' '-_')
  local body; body="{\"username\":\"$1\",\"profile\":{\"given_name\":\"$1\",\"family_name\":\"MokiBox\"},\"email\":{\"email\":\"$1@example.local\",\"is_verified\":true},\"password\":{\"password\":\"$pw\",\"change_required\":false}}"
  local resp; resp=$(api POST /v2beta/users/human "$(wjson "$body")")
  local uid; uid=$(echo "$resp" | jqr "d.get('userId','')")
  if [ -n "$uid" ] && [ "$uid" != "None" ]; then
    echo "$1=$pw" >> "$SECDIR/test-user-passwords"
    echo "  $1 created userId=$uid"
  else
    echo "  $1 FAILED: $(echo "$resp" | head -c 200)"
  fi
  chmod 600 "$SECDIR/test-user-passwords"
}
echo "[9] test users (test1 login, test2 webhook)"
USERS=$(api POST /v2/users/search "$(wjson '{"userNameQuery":{"userName":"test","method":"TEXT_QUERY_METHOD_STARTS_WITH"}}')")
HAVE_T1=$(echo "$USERS" | jqr "any(u.get('username')=='test1' for u in d.get('result',[]))")
HAVE_T2=$(echo "$USERS" | jqr "any(u.get('username')=='test2' for u in d.get('result',[]))")
if [ "$HAVE_T1" != "True" ]; then mk_user test1; else echo "  test1 exists"; fi
if [ "$HAVE_T2" != "True" ]; then mk_user test2; else echo "  test2 exists"; fi

echo "[10] SUMMARY:"
cat "$IDS"
echo "Secrets: $SECDIR/{zitadel-web-client-secret, zitadel-target-signing-key, test-user-passwords}"
