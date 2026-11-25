# DM relay HTTP + PostgreSQL smoke

This smoke exercises the real SQL and the real HTTP relay. It is not an E2EE
interoperability test and not a security audit; the client SDK tests cover Olm
session behavior separately.

## Prerequisites

- PostgreSQL with migrations `001`, `002`, `008`, `009`, `010`, `011` applied.
  `make db-migrate-dm` applies `009`-`011`; it must run for every deployment,
  even when `DM_E2EE_ENABLED=false`, because account deletion always removes
  DM rows.
- A running API Gateway with `DM_E2EE_ENABLED=true`, `DM_INBOX_TTL=168h`,
  `DATABASE_URL` pointing at that PostgreSQL, and a test identity provider.
- Two existing active users and bearer tokens for them.

## Run

From the repository root:

```sh
go run ./scripts/smoketest/dm
```

Required environment:

- `DATABASE_URL` — same DSN used by the gateway.
- `DM_SMOKE_USER_A`, `DM_SMOKE_USER_B` — UUIDs of two active users.
- `DM_SMOKE_TOKEN_A`, `DM_SMOKE_TOKEN_B` — bearer tokens for those users.

Optional:

- `DM_API_URL` — default `http://localhost:8080`.

## What it checks

- `dm_devices` and `dm_fallback_keys` exist; `fk_dm_inbox_sender_user` exists and
  the old `fk_dm_inbox_sender_device` is gone. This covers migrations `009`-`011`.
- Two devices register through HTTP; one uploads two one-time keys and a fallback
  key; the fallback row is visible in PostgreSQL.
- Two concurrent transactions call `ClaimDMOneTimeKey` and receive different
  keys, exercising `FOR UPDATE SKIP LOCKED`.
- An expired idempotency row is replaced through `ON CONFLICT ... WHERE`, and a
  second conflicting write on the refreshed row returns `sql.ErrNoRows` without
  overwriting it.
- `POST /api/dm/messages` twice with the same client message ID returns the same
  message ID and sequence.
- `GET /api/dm/inbox` sees the message; `POST /api/dm/inbox/:message_id/ack`
  sets `acknowledged_at` and NULLs `ciphertext`.
- No row contains the smoke plaintext substring `smoke-ciphertext`.
- Test rows, follows, devices, keys, and idempotency records are removed at the
  end with the same queries used by account deletion.

## Evidence

Recorded 2026-11-25 against the local smoke stack (`postgres:16-alpine`,
Go 1.25.5, gateway built from this branch):

- Fresh database: migrations `001`, `002`, `008`, `009`, `010`, `011` applied in
  order with `psql -v ON_ERROR_STOP=1`; no schema errors.
- Existing database with `009` already applied: `010` and `011` applied in order;
  `dm_fallback_keys` present, `fk_dm_inbox_sender_user` present,
  `fk_dm_inbox_sender_device` absent.
- `go run ./scripts/smoketest/dm` → `PASS scripts/smoketest/dm` on both the
  fresh database and the existing-`009` database; the fresh run was repeated
  back-to-back for rerun safety.
- Separate two-user end-to-end client run → `LIVE_DM_E2EE_SMOKE_OK` and
  `LIVE_DM_OUTBOX_RETRY_OK`.
- Live `DELETE /api/users/me` on a seeded user with an active DM device,
  one-time key, fallback key, idempotency row, and pending inbox message →
  all DM rows removed and the user tombstoned in the same database.
  `encode(ciphertext, 'escape')` checks found `0` plaintext rows.
