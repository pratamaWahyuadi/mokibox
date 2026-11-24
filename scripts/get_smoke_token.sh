#!/usr/bin/env bash
set -uo pipefail

source /home/pratama/MokiBox/.env

CLIENT_ID="$ZITADEL_CLIENT_ID"
CLIENT_SECRET="$ZITADEL_CLIENT_SECRET"
API_BASE_URL="$API_BASE_URL"
AUTH="$ZITADEL_ISSUER_URL"
REDIRECT_URI="$API_BASE_URL/callback"

# login-client PAT from the zitadel bootstrap volume
LCPAT=$(docker run --rm -v zitadel_zitadel-bootstrap:/bs alpine cat /bs/login-client.pat 2>/dev/null)
[[ -z "$LCPAT" ]] && { echo "FATAL: cannot read login-client PAT"; exit 1; }

# headless login for test1
loc=$(curl -sS -o /dev/null -w '%{redirect_url}' \
  "$AUTH/oauth/v2/authorize?client_id=$CLIENT_ID&response_type=code&scope=openid+profile&redirect_uri=$(printf %s "$REDIRECT_URI" | sed 's|/|%2F|g;s|:|%3A|g')&state=itest")
arid=$(printf %s "$loc" | grep -oE 'authRequest=[A-Za-z0-9_]+' | cut -d= -f2)
[[ -z "$arid" ]] && { echo "FATAL: no authRequest ID"; exit 1; }

sresp=$(curl -sS -X POST "$AUTH/v2/sessions" -H "Content-Type: application/json" -H "Accept: application/json" \
  -H "Authorization: Bearer $LCPAT" \
  -d '{"checks":{"user":{"loginName":"test1"},"password":{"password":"MokiTest1-A"}}}')
sid=$(printf %s "$sresp" | jq -r .sessionId)
stok=$(printf %s "$sresp" | jq -r .sessionToken)
[[ "$sid" == "null" || -z "$sid" ]] && { echo "FATAL: session creation failed"; exit 1; }

cb=$(curl -sS -X POST "$AUTH/v2/oidc/auth_requests/$arid" -H "Content-Type: application/json" -H "Accept: application/json" \
  -H "Authorization: Bearer $LCPAT" \
  -d "{\"session\":{\"sessionId\":\"$sid\",\"sessionToken\":\"$stok\"}}")
code=$(printf %s "$cb" | jq -r .callbackUrl | grep -oE 'code=[^&]+' | cut -d= -f2)
[[ -z "$code" || "$code" == "null" ]] && { echo "FATAL: no auth code"; exit 1; }

tres=$(curl -sS -X POST "$AUTH/oauth/v2/token" -u "$CLIENT_ID:$CLIENT_SECRET" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "grant_type=authorization_code" \
  --data-urlencode "code=$code" \
  --data-urlencode "redirect_uri=$REDIRECT_URI")
access_token=$(printf %s "$tres" | jq -r .access_token)
[[ -z "$access_token" || "$access_token" == "null" ]] && { echo "FATAL: token exchange failed"; exit 1; }

echo "$access_token"