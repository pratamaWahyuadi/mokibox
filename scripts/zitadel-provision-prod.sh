#!/bin/bash
# =====================================================
# zitadel-provision-prod.sh - Tahap 5 provisioning (v2)
# Zitadel v4.16.0 fresh instance @ auth.binery.my.id
# (prompt 2, Opsi W webhook).
#
# Contract (integration_test.sh): mokibox_web
# (confidential) + mokibox_spa (public PKCE) + mokibox_api
# (JWT audience), test1/test2 users, Actions V2
# target+execution (Opsi W).
#
# Search shapes verified live 2026-09-10:
#  - projects/apps _search: {"queries":[{"nameQuery":{"name":X,"method":"TEXT_QUERY_METHOD_EQUALS"}}]}
#  - users search: POST /v2/users/search userNameQuery STARTS_WITH
#  - targets search: POST /v2/actions/targets/_search {}
#
# Non-secret IDs printed; secrets written ONLY to
# /root/mokibox-secrets/. Run as root on the VPS.
# =====================================================
set -euo pipefail

ZITADEL_INTERNAL="http://127.0.0.1:8181"
H_HOST="Host: auth.binery.my.id"
SECDIR=/root/mokibox-secrets
mkdir -p "$SECDIR" && chmod 700 "$SECDIR"
IDS="$SECDIR/zitadel-ids.env"
TMPD=$(mktemp -d); trap 'rm -rf "$TMPD"' EXIT

jqr() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1" 2>/dev/null; return 0; }

PAT=$(docker run --rm -v zitadel_zitadel-bootstrap:/bs alpine cat /bs/login-client.pat)
echo "[1] login-client PAT loaded (${#PAT} chars)"
AUTH="Authorization: Bearer ${PAT}"

api() { # method path [json-file]
  local m=$1 p=$2 data=${3:--}
  if [ "$data" = "-" ]; then
    curl -s -X "$m" -H "$H_HOST" -H "$AUTH" -H 'Content-Type: application/json' "$ZITADEL_INTERNAL$p"
  else
    curl -s -X "$m" -H "$H_HOST" -H "$AUTH" -H 'Content-Type: application/json' -d "@$data" "$ZITADEL_INTERNAL$p"
  fi
}
wjson() { printf '%s' "$1" > "$TMPD/body.json"; echo "$TMPD/body.json"; }
qname() { printf '{"queries":[{"nameQuery":{"name":"%s","method":"TEXT_QUERY_METHOD_EQUALS"}}]}' "$1"; }

echo "[2] find-or-create project mokibox"
PROJ=$(api POST /management/v1/projects/_search "$(wjson "$(qname mokibox)")")
PROJ_ID=$(echo "$PROJ" | jqr "d['result'][0]['id']")
if [ -z "${PROJ_ID:-}" ] || [ "$PROJ_ID" = "None" ]; then
  PROJ=$(api POST /management/v1/projects "$(wjson '{"name":"mokibox"}')")
  PROJ_ID=$(echo "$PROJ" | jqr "d['id']")
  echo "  created project $PROJ_ID"
else
  echo "  project exists: $PROJ_ID"
fi
echo "PROJECT_ID=$PROJ_ID" > "$IDS"

app_id() { # name -> appId or empty
  api POST "/management/v1/projects/$PROJ_ID/apps/_search" "$(wjson "$(qname "$1")")" | jqr "d['result'][0]['id']" 2>/dev/null || true
}
app_clientid() { # appId -> clientId
  api GET "/management/v1/projects/$PROJ_ID/apps/$1" | jqr "d['app']['oidcConfig']['clientId']"
}

# --- mokibox_web (confidential Web app) -------------------
echo "[3] app mokibox_web (Web/confidential, BASIC secret)"
WEB_ID=$(app_id mokibox_web)
if [ -z "${WEB_ID:-}" ] || [ "$WEB_ID" = "None" ]; then
  WEB=$(api POST "/management/v1/projects/$PROJ_ID/apps/oidc" "$(wjson '{
    "name": "mokibox_web",
    "redirectUris": ["https://mokiboxapi.binery.my.id/callback"],
    "grantTypesList": [0], "responseTypesList": [0],
    "appType": 0, "authMethodType": 0, "accessTokenType": 1, "devMode": false}')")
  WEB_ID=$(echo "$WEB" | jqr "d['appId']")
  WEB_CID=$(echo "$WEB" | jqr "d['clientId']")
  WEB_SEC=$(echo "$WEB" | jqr "d.get('clientSecret','')")
  echo "  created: appId=$WEB_ID clientId=$WEB_CID"
else
  echo "  exists: appId=$WEB_ID"
  WEB_CID=$(app_clientid "$WEB_ID")
  WEB_SEC=$(api POST "/management/v1/projects/$PROJ_ID/apps/$WEB_ID/oidc_config/_generate_client_secret" | jqr "d.get('clientSecret','')")
fi
printf 'WEB_APP_ID=%s\nWEB_CLIENT_ID=%s\n' "$WEB_ID" "$WEB_CID" >> "$IDS"
echo "$WEB_SEC" > "$SECDIR/zitadel-web-client-secret"; chmod 600 "$SECDIR/zitadel-web-client-secret"

# --- mokibox_spa (public PKCE) -----------------------------
echo "[4] app mokibox_spa (UserAgent/SPA, PKCE public)"
SPA_ID=$(app_id mokibox_spa)
if [ -z "${SPA_ID:-}" ] || [ "$SPA_ID" = "None" ]; then
  SPA=$(api POST "/management/v1/projects/$PROJ_ID/apps/oidc" "$(wjson '{
    "name": "mokibox_spa",
    "redirectUris": ["https://mokiboxapi.binery.my.id/callback"],
    "grantTypesList": [0], "responseTypesList": [0],
    "appType": 1, "authMethodType": 2, "accessTokenType": 1, "devMode": false}')")
  SPA_ID=$(echo "$SPA" | jqr "d['appId']")
  SPA_CID=$(echo "$SPA" | jqr "d['clientId']")
  echo "  created: appId=$SPA_ID clientId=$SPA_CID"
else
  echo "  exists: appId=$SPA_ID"
  SPA_CID=$(app_clientid "$SPA_ID")
fi
printf 'SPA_APP_ID=%s\nSPA_CLIENT_ID=%s\n' "$SPA_ID" "$SPA_CID" >> "$IDS"

# --- mokibox_api (JWT audience resource server) ------------
echo "[5] app mokibox_api (API resource server)"
APIA_ID=$(app_id mokibox_api)
if [ -z "${APIA_ID:-}" ] || [ "$APIA_ID" = "None" ]; then
  APIA=$(api POST "/management/v1/projects/$PROJ_ID/apps/api" "$(wjson '{
    "name": "mokibox_api",
    "authTokenRoleAssertion": true, "accessTokenRoleAssertion": true}')")
  APIA_ID=$(echo "$APIA" | jqr "d['appId']")
  APIA_CID=$(echo "$APIA" | jqr "d['clientId']")
  echo "  created: appId=$APIA_ID clientId=$APIA_CID"
else
  echo "  exists: appId=$APIA_ID"
  APIA_CID=$(api GET "/management/v1/projects/$PROJ_ID/apps/$APIA_ID" | jqr "d['app']['apiConfig']['clientId']")
fi
printf 'API_APP_ID=%s\nAPI_CLIENT_ID=%s\n' "$APIA_ID" "$APIA_CID" >> "$IDS"

# --- Actions V2 (Opsi W) -----------------------------------
echo "[6] Actions V2 target (Opsi W: public https URL)"
TGT=$(api POST /v2/actions/targets "$(wjson '{
  "name": "mokibox-webhook",
  "restWebhook": {"interruptOnError": false},
  "endpoint": "https://mokiboxapi.binery.my.id/api/webhooks/zitadel",
  "timeout": "10s'}')")
echo "$TGT" | head -c 200; echo
TGT_ID=$(echo "$TGT" | jqr "d['id']")
SIGN_KEY=$(echo "$TGT" | jqr "d.get('signingKey','')")
echo "TARGET_ID=$TGT_ID" >> "$IDS"
if [ -n "${SIGN_KEY:-}" ] && [ "$SIGN_KEY" != "None" ]; then
  echo "$SIGN_KEY" > "$SECDIR/zitadel-target-signing-key"; chmod 600 "$SECDIR/zitadel-target-signing-key"
  echo "  signingKey saved"
else
  echo "  WARN: signingKey not in response"
fi

echo "[7] Execution user.deactivated -> target"
EXEC=$(api PUT /v2/actions/executions "$(wjson "{
  \"condition\": {\"event\": {\"event\": \"user.deactivated\"}},
  \"targets\": [\"$TGT_ID\"]}")")
echo "$EXEC" | head -c 200; echo

# --- test users -------------------------------------------
mk_user() { # username
  local pw; pw=$(openssl rand -base64 18 | tr '+/' '-_')
  local resp; resp=$(api POST /v2beta/users/human "$(wjson "{
    \"username\": \"$1\",
    \"profile\": {\"given_name\": \"$1\", \"family_name\": \"MokiBox\"},
    \"email\": {\"email\": \"$1@example.local\", \"is_verified\": true},
    \"password\": {\"password\": \"$pw\", \"change_required\": false}}")")
  echo "$1=$pw" >> "$SECDIR/test-user-passwords"
  chmod 600 "$SECDIR/test-user-passwords"
  echo "  $1 -> userId=$(echo "$resp" | jqr "d.get('userId', d.get('code','ERR'))")"
}
echo "[8] test users (test1 login, test2 webhook)"
USERS=$(api POST /v2/users/search "$(wjson '{"userNameQuery":{"userName":"test","method":"TEXT_QUERY_METHOD_STARTS_WITH"}}')")
HAVE_T1=$(echo "$USERS" | jqr "any(u.get('username')=='test1' for u in d.get('result',[]))")
HAVE_T2=$(echo "$USERS" | jqr "any(u.get('username')=='test2' for u in d.get('result',[]))")
if [ "$HAVE_T1" != "True" ]; then mk_user test1; else echo "  test1 exists"; fi
if [ "$HAVE_T2" != "True" ]; then mk_user test2; else echo "  test2 exists"; fi

echo "[9] SUMMARY:"
cat "$IDS"
echo "Secrets: $SECDIR/{zitadel-web-client-secret, zitadel-target-signing-key, test-user-passwords}"
