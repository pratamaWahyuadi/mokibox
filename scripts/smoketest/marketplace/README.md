# Marketplace HTTP + PostgreSQL smoke

This smoke exercises the real Marketplace HTTP routes and verifies their
persistent state in PostgreSQL. It does not upload objects to R2.

## Prerequisites

- PostgreSQL with migrations `001`-`014` applied.
- A running API Gateway connected to that database with
  `MARKETPLACE_ENABLED=true`.
- Two existing active users: one seller and one buyer, with valid bearer tokens.

## Run

From the repository root:

```sh
go run ./scripts/smoketest/marketplace
```

Required environment:

- `DATABASE_URL` — same database used by the API Gateway.
- `MARKETPLACE_SMOKE_SELLER_USER` — seller user UUID.
- `MARKETPLACE_SMOKE_SELLER_TOKEN` — seller bearer token.
- `MARKETPLACE_SMOKE_BUYER_USER` — buyer user UUID.
- `MARKETPLACE_SMOKE_BUYER_TOKEN` — buyer bearer token.

Optional:

- `MARKETPLACE_API_URL` — defaults to `http://localhost:8080`.
- `MARKETPLACE_SWEEP_INTERVAL` / `MARKETPLACE_SWEEP_BATCH` — consumed by the
  stub-auth gateway used for simulated live smoke, not by this script; defaults
  are `1m` and `100`.
- `MARKETPLACE_STUB_SERVER_ADDR` — bind address for `make
  smoke-marketplace-stub-server`; defaults to `127.0.0.1:18081`. The target sets
  `MARKETPLACE_STUB_SERVER_ENABLED=1` so the blocking stub test is skipped by
  ordinary tagged suites.

`make smoke-marketplace-live` sources the gitignored `.env.live.local` and
requires the six `MARKETPLACE_*` variables above. Set
`MARKETPLACE_SMOKE_MODE=local-simulation` only when pointing it at the local
stub gateway; the target prints a warning for that mode.

The script checks categories, listing creation and filtered browse (including a
near radius hit with `distance_km` and a far radius miss), favorites,
idempotent reports, seller contact grants, expiry/renewal, revocation, and the
corresponding SQL state. It does not call the E2EE DM endpoints; contact
authorization is verified through the persisted grant, expiry, and revocation
columns, and DM routes also require `DM_E2EE_ENABLED=true`. It creates a uniquely titled listing and
deletes that exact listing in a deferred cleanup; related favorites, reports,
and contact grants are removed by foreign-key cascades. Use only a disposable
local environment.

## Smoke gates

- Local: run `go test -tags=integration ./shared/db -count=1` with `DATABASE_URL`
  against migrations `001`-`014`, then run this script against a gateway with
  `MARKETPLACE_ENABLED=true`. Confirm expired image-intent keys land in
  `marketplace_cleanup_outbox`.
- Live/staging: repeat against a disposable staging database with real R2 and
  Redis. Confirm `cleanup:objects` removes the temporary/final object keys and
  that failed enqueues back off instead of leaking objects. Keep
  `MARKETPLACE_ENABLED=false` until this smoke and the security review pass.
