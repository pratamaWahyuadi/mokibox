---
name: presigned-object-urls
description: Use when presigning S3/R2 URLs or debugging player CORS.
---

# Presigned Object URLs (S3-compatible storage)

Presigned URLs look like plain URLs but are cryptographically bound to an
exact request shape. Violating that shape produces errors that browsers
render as "CORS errors", sending you to debug bucket config when the real
bug is in the request. Read this before wiring any player or fetcher to
object storage.

## The signed-request contract

A SigV4 presigned URL signs: **method + path + query string + ONLY the
headers listed in `X-Amz-SignedHeaders`** (usually just `host`).

- ANY extra header the client attaches (`Authorization`, custom headers)
  → **400 SignatureDoesNotMatch**. Read `X-Amz-SignedHeaders` in the URL
  first — if it says `host`, the request must carry nothing else.
- The method is signed too: a bare `curl -I` (HEAD) against a GET-presigned
  URL returns **403** even when the URL is valid. Probe with
  `curl -X GET -o /dev/null -D -` (see headers, discard body).
- `X-Amz-Date` + `X-Amz-Expires` bound validity (often 15 min). An expired
  presign → 403. A 403 can also mean the signing key/secret changed since
  the URL was generated — regenerate, don't reuse old URLs.

## CORS errors lie: find the real status code first

Object-storage error responses (400/403/404) do NOT attach CORS headers, so
the browser console shows **any** storage-origin failure as
"CORS header 'Access-Control-Allow-Origin' missing". The real signal is
the status code printed in the same console/Network line:

- **400** = extra header broke the signature → audit client-side wrappers
  (player loaders, fetch interceptors) for globally-attached Authorization.
- **403** = expired, HEAD-on-GET, or signing-key mismatch.
- **404** = key path wrong (leading slash, bucket prefix, separator
  mismatch — see the project's key-path contract if one exists).
- True preflight failure = the OPTIONS request itself fails while a plain
  GET succeeds.

Verify server-side BEFORE touching bucket config:

```sh
curl -X GET -o /dev/null -D - -H 'Origin: <page-origin>' "<presigned-url>"
```

This shows the true status AND whether CORS headers are present. Only when
this returns 2xx-with-CORS and the browser still fails, suspect browser
state (preflight cache up to MaxAge, stale presigned URLs cached in the
page, hard-reload or fresh private window).

## Rule: two-tier player auth

Presigned media URLs must be fetched with NO custom headers. Therefore a
player's auth splits into two tiers:

- **Tier 1 — manifest/playlist (your API origin):** auth via
  `Authorization` header OR a short-TTL query token bound to the resource
  id (`?token=`). The query-token path exists BECAUSE players (hls.js,
  `<video>`, MSE) cannot attach headers to media requests without breaking
  presigned signatures.
- **Tier 2 — media objects (object-storage origin):** presigned GETs,
  fetched headerless straight to storage.

Backend shape for tier 1: register the manifest route OUTSIDE the strict
auth group with an optional-auth middleware — header absent → run
anonymous, handler validates the query token; header PRESENT but invalid →
fail closed 401 (never silently downgrade to anonymous, or an attacker
turns a bad token into a token-path bypass).

## Rule: never forward Authorization in player loaders

hls.js `xhrSetup`/`fetchSetup`, axios interceptors, service-worker fetch
handlers: if they attach `Authorization` globally, every media segment 400s
and the page shows nothing but misleading CORS errors. Auth headers
belong ONLY on requests to your own API origin. Before shipping a player
page, check every loader/interceptor for a host allowlist.

## Bucket-side CORS (only when it really is CORS)

- CORS policy lives on the bucket (R2 dashboard → bucket → Settings →
  CORS policy; AWS S3 bucket CORS), never in app code — code cannot inject
  CORS headers onto presigned responses.
- `AllowedOrigins` must list the page origin(s); `AllowedMethods` GET/HEAD;
  `ExposeHeaders` ETag/Content-Length/Content-Range for range/streaming.
- After changing bucket CORS, browser preflight cache can serve stale
  failures for up to MaxAge — retest in a fresh private window or with the
curl probe above.

## Probe recipe (in this order)

1. Real status code from the browser console/Network line — ignore the
   "CORS" wording.
2. `curl -X GET` with Origin header from a host that can reach the storage.
3. Chain the whole flow server-side exactly as the player does: token →
   manifest → variant → segment, with NO headers on the segment requests.
4. All green server-side + browser still failing → browser cache, not
   infrastructure.

Source lesson: MokiBox 2026-09-06 — an hls.js `xhrSetup` forwarded
`Authorization: Bearer` to R2 presigned segments (SignedHeaders=host) → 400
SignatureDoesNotMatch rendered as "CORS error" across multiple sessions;
the manifest route needed an optional-auth + `?token=` path so segments
could stay headerless.
