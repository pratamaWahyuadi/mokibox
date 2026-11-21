# E2EE DM v1 backend review guide

This guide is review assistance for PR #77, not approval of any D1-D10
proposal. PR #77 delivers a backend relay only; it does not implement E2EE.
Keep the production feature flag off until the client and independent security
review checklist in PR #77 is complete.

## Three review commands

Run these against a disposable database initialized through migration 008, not
against production:

1. Apply the forward-only DM migration:

   ```sh
   psql "$DM_REVIEW_DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/009_dm_e2ee_relay.sql
   ```

2. Run static checks and the full Go test suite:

   ```sh
   go mod tidy && make sqlc-gen && go vet ./... && go test ./... && go build ./api-gateway ./transcoder-worker
   ```

3. Exercise the real HTTP handlers and PostgreSQL twice consecutively. The
   program performs two runs and uses random opaque bytes; this is not a
   cryptography test:

   ```sh
   DATABASE_URL="$DM_REVIEW_DATABASE_URL" DM_INBOX_TTL=168h go run ./scripts/smoketest/phase13_dm_e2ee
   ```

The `168h` value is the selected v1 TTL and is used by this smoke test. Keep it
explicit in every environment where the feature is enabled.

## Evidence and limits

- Unit tests cover device authorization, mutual-follow anti-enumeration,
  opaque protocol/version validation, one-time-key exhaustion, inbox cap,
  idempotency, sequence ordering, expiry, acknowledgements, deletion, and
  exclusion of key/ciphertext bytes from logs.
- Observed verification: migrations 001, 002, 008, and 009 applied in order to
  disposable PostgreSQL 16; the full Go tests, vet, and builds passed; the
  PostgreSQL + HTTP harness passed two consecutive runs.
- The HTTP harness uses the Echo routes with a test verifier and covers
  concurrent one-time-key claims, concurrent sends, sequence uniqueness/order,
  idempotent retry after acknowledgement, digest cleanup and ID reuse after
  expiry, stale-device rejection, queue-cap rollback, and repeatability.
- Acknowledgements delete ciphertext immediately but preserve the digest and
  routing/response metadata until TTL cleanup. Review this additional metadata
  retention alongside the selected TTL. The 1,000 pending-inbox cap does not
  cap tombstones; their count follows the send rate and TTL.
- These checks prove relay behavior and database transaction behavior only.
  They do not prove encryption, key safety on client devices, or protocol
  security.
- The server sees routing metadata and cannot detect a client that labels
  plaintext as ciphertext. Device replacement relies on a JWT; a stolen
  session can replace a device. Copied messages cannot be recalled.
- The per-process rate limiter is suitable only for one gateway instance.
  Production remains disabled pending the client and independent security
  review; the selected v1 TTL is 168h.

## Suggested reviewer dispositions (all remain Proposed until recorded)

| Decision | Suggested disposition |
|---|---|
| D1 — v1 scope | Accept the narrow 1:1, mutual-follow, single-device backend scope; keep parity features deferred. |
| D2 — protocol/library | Keep protocol and license selection unapproved until the owner/reviewer explicitly decides; do not assume AGPL-3.0 acceptance. |
| D3 — device model | Accept one active device, with no server recovery/history transfer; require the client key-change warning. |
| D4 — message flow | Accept opaque key-bundle claim and ciphertext relay only after the client protocol decision and review. |
| D5 — ordering/retention | Accept the transactional per-device sequence, acknowledgement flow, and selected 168h TTL. |
| D6 — receipts | Accept delivery acknowledgement only; keep read receipts deferred. |
| D7 — eligibility/abuse | Accept mutual-follow eligibility, documented per-process limits, and the 1,000-message cap; use a shared limiter before scaling beyond one gateway. |
| D8 — identity verification | Require fingerprint comparison and identity-key-change warnings in the client before production. |
| D9 — deletion | Accept server-side key/queue purge while documenting that copies on other devices cannot be erased. |
| D10 — release gates | Keep the flag off in production until real client encryption/decryption and independent security review pass. |

Record the reviewer's actual decision for every D1-D10 row in the decision
table in `planning/13_dm_e2ee_design.md`; these suggestions are not approvals.
For each decision, the reviewer should record Accept, Change, or Reject with a
short rationale in the PR review or a comment on #76. The author then updates
the Approver and Status columns (and proposal text when changed) in a final PR
commit before merge. Leave every row Proposed until that explicit decision is
recorded.
