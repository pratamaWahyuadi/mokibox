#!/usr/bin/env bash
set -uo pipefail

source /home/pratama/MokiBox/.env

CLIENT_ID="$ZITADEL_CLIENT_ID"
CLIENT_SECRET="$ZITADEL_CLIENT_SECRET"
API_BASE_URL="$API_BASE_URL"
AUTH="$ZITADEL_ISSUER_URL"
REDIRECT_URI="$API_BASE_URL/callback"

echo "AUTH: $AUTH"
echo "API_BASE_URL: $API_BASE_URL"
echo "CLIENT_ID: $CLIENT_ID"
echo "REDIRECT_URI: $REDIRECT_URI"

# login-client PAT from the zitadel bootstrap volume
LCPAT=$(docker run --rm -v zitadel_zitadel-bootstrap:/bs alpine cat /bs/login-client.pat 2>/dev/null)
[[ -z "$LCPAT" ]] && { echo "FATAL: cannot read login-client PAT"; exit 1; }
echo "LCPAT: ${LCPAT:0:30}..."

# headless login for test1
ENCODED_REDIRECT=$(printf %s "$REDIRECT_URI" | sed 's|/|%2F|g;s|:|%3A|g')
AUTH_URL="$AUTH/oauth/v2/authorize?client_id=$CLIENT_ID&response_type=code&scope=openid+profile&redirect_uri=$ENCODED_REDIRECT&state=itest"
echo "AUTH_URL: $AUTH_URL"

loc=$(curl -sS -o /dev/null -w '%{redirect_url}' "$AUTH_URL")
echo "loc: $loc"

arid=$(printf %s "$loc" | grep -oE 'authRequest=[A-Za-z0-9_]+' | cut -d= -f2)
[[ -z "$arid" ]] && { echo "FATAL: no authRequest ID"; exit 1; }
echo "arid: $arid"