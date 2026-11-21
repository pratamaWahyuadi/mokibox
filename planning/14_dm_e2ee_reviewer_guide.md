# E2EE DM v1 backend review guide

PR #77 delivers only an opaque backend relay, not E2EE. D1 and D3-D10 are
accepted as written only when the repository owner's reviewer merges the PR.
D2 stays open and is decided in #78. Keep the production flag off until the
Flutter client and independent security review are complete.

## Three review commands

Use a disposable database already migrated through 008, never production:

1. Apply forward-only migration 009:
   ```sh
   psql "$DM_REVIEW_DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/009_dm_e2ee_relay.sql
   ```
2. Run code generation, static checks, uncached tests, and builds:
   ```sh
   go mod tidy && make sqlc-gen && go vet ./... && go test -count=1 ./... && go build ./api-gateway ./transcoder-worker
   ```
3. Run the HTTP/PostgreSQL relay smoke (it performs two runs; random bytes are
   not a cryptography test):
   ```sh
   DATABASE_URL="$DM_REVIEW_DATABASE_URL" DM_INBOX_TTL=168h go run ./scripts/smoketest/phase13_dm_e2ee
   ```

`DM_INBOX_TTL=168h` (7 days) is the selected v1 value and must be set wherever
the feature is enabled.

## Evidence and limits

- Tests cover device binding, mutual-follow anti-enumeration, key exhaustion,
  queue cap, idempotency, sequence ordering, ack, expiry, deletion, validation,
  and exclusion of key/ciphertext bytes from logs.
- Migrations 001, 002, 008, and 009 applied in order on disposable PostgreSQL
  16. The uncached Go suite passed. The PostgreSQL + HTTP smoke passed twice
  using `tiktok_api`, including concurrent key claims/sends, duplicate retry
  after ack, TTL cleanup/reuse, stale devices, and queue-cap rollback.
- These checks prove relay/database behavior only, not encryption or protocol
  security. The server sees routing metadata; a stolen JWT can replace a
  device; copied messages cannot be recalled. Rate limits are per-process,
  suitable only for one gateway.
- Ack deletes ciphertext but retains a digest and routing/response metadata
  until the selected TTL. Tombstone volume depends on send rate and TTL, not
  the 1,000-message inbox cap.

## Suggested D1-D10 dispositions

| ID | Suggested disposition |
|---|---|
| D1 | Accept narrow 1:1 mutual-follow backend scope; defer parity features. |
| D2 | Keep protocol/license open for decision in #78; merge does not accept AGPL. |
| D3 | Accept one active device; require client key-change warning. |
| D4 | Accept opaque key-bundle claim and relay only after client protocol review. |
| D5 | Accept transactional per-device ordering, ack, and 168h TTL. |
| D6 | Defer read receipts; ack means delivered/handled only. |
| D7 | Accept mutual-follow, 1,000 cap, and single-instance process-local limits. |
| D8 | Require client fingerprint verification and key-change warning. |
| D9 | Accept server purge; copies on other devices cannot be recalled. |
| D10 | Keep production disabled until client crypto and independent review pass. |

Merging PR #77 as the repository owner's reviewer records acceptance of D1
and D3-D10 as written. D2 remains open and is decided in #78; the merge does
not approve a protocol or license and does not implicitly accept AGPL-3.0.
