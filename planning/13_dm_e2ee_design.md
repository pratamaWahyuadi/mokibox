# MokiBox Direct Messages — E2EE v1

**Status:** Backend relay plus a reference Go SDK/CLI are in scope for this
repository. A production client UI, distribution/license decision for the
Olm implementation, and an independent security audit remain out of scope.

## Goal and boundaries

Implement the Go backend component for 1:1 encrypted messaging. This backend
alone is not an end-user-ready E2EE chat; a separately implemented client must
encrypt and decrypt messages:

- The Go API authenticates MokiBox accounts and relays opaque ciphertext.
- Clients outside this repository own Olm accounts, sessions, key verification,
  message encryption/decryption, and encrypted local state.
- No server component receives private keys or plaintext.
- `clients/dm` is the reference Go SDK/CLI in this repository. It is not a
  production chat UI; a client UI and its distribution license still need a
  product decision.

The supplied `chat-e2ee-master.zip` is a behavior/architecture reference only.
Its shared invitation-secret + HKDF/AES-GCM protocol is not mixed with Olm, and
its source is not copied. This design uses account-bound device keys instead.

## Product scope

### Backend included

- 1:1 text messages between active MokiBox users who mutually follow each other.
- One active DM device per account. Registering a replacement revokes the old
  device and deletes its server-held keys and queued messages.
- HTTP key-bundle fetch, ciphertext send, inbox polling, and acknowledgement.
- Offline delivery via a bounded server inbox; messages and idempotency records
  expire after 168 hours.
- Per-device message ordering, idempotent retry, account-deletion cleanup,
  device binding, rate limits, and a feature flag that defaults off.

### Reference Go SDK/CLI included

- A client must compare a peer fingerprint out of band before sending. A
  changed identity key must block sends and require re-verification.
- A client must encrypt before sending and decrypt only after receiving. The
  backend must never add a plaintext fallback.

### Not included in v1

Message requests, block/report, read receipts, typing/presence, WebSocket,
push notifications, media/voice/video, replies/reactions, groups, history
transfer, and multi-device fanout. DM does not inherit a global offline mode;
only its bounded ciphertext queue supports offline recipients.

## Client cryptographic contract

- Olm v1 is the intended client protocol. The reference SDK uses
  `maunium.net/go/mautrix` (MPL-2.0) and its pure-Go `crypto/goolm`. Upstream
  has stated that goolm still needs an audit; no independent goolm audit
  report was found during this review. Production client distribution requires
  a license/audit decision before enablement.
- Each device has Curve25519 and Ed25519 identity keys. Private account and
  Olm session state must be stored using the selected crypto library's
  encrypted state format; its random pickle key must use OS-backed secure
  storage. Never log keys, pickles, plaintext, or ciphertext.
- The client publishes its public identity keys, signed Olm one-time keys, and
  an optional signed fallback key. The server stores and returns their opaque
  encodings; the client verifies each signature against the peer Ed25519
  identity key before establishing a
  session. The signed UTF-8 string is
  `mokibox-dm-one-time-key-v1\n{key_id}\n{canonical_base64_public_key}`.
- Fingerprint bytes are
  `UTF8("mokibox-dm-fingerprint-v1\\0") || user_uuid_16_bytes ||
  device_uuid_16_bytes || curve25519_key_32_bytes || ed25519_key_32_bytes`.
  The display value is uppercase SHA-256 hex grouped in blocks of four. Trust
  is stored locally against the peer account ID and device ID. Sending is
  rejected until that exact fingerprint is explicitly verified locally. If a
  previously trusted fingerprint changes, the client blocks sending and
  reports a key-change state; it never silently replaces trust.
- The client uses Olm sessions for message encryption and decryption. Message
  envelopes carry the protocol identifier/version and Olm message type; the
  server treats the ciphertext bytes as opaque.
- Before the first network send, the client persists the exact request (including
  ciphertext) in an outbox keyed by client message ID. A retry with the same ID
  resends that exact request; a different plaintext or recipient is rejected
  locally as an outbox conflict instead of creating an idempotency conflict.
- A message that fails validation, Olm decryption, session lookup, or fingerprint
  rotation is quarantined locally and skipped, not acknowledged and not
  allowed to block later sequences. The inbox cursor advances past quarantined
  messages; decrypt failures reload persisted state so a partially consumed
  account/OTK mutation cannot corrupt the ratchet.
- No custom cryptographic primitive or AES shared-room key is introduced.
- Any client implementation must use disposable smoke identities and must not
  reuse production credentials or pickle keys.

## Server invariants

- Every DM route is authenticated and is registered only when
  `DM_E2EE_ENABLED=true`; the default is false. When enabled,
  `DM_INBOX_TTL=168h` is required.
- Key-bundle fetch and send require mutual follows. Missing and ineligible
  recipients have the same not-found response.
- Key-bundle fetch supports identity-only lookup; the client uses that form to
  inspect a peer fingerprint without consuming a one-time key.
- Device-sensitive requests include `device_id`; stale IDs return
  `409 DM_DEVICE_STALE`.
- One-time-key claims are atomic. Sends allocate the recipient's per-device
  sequence and enforce the queue cap in one transaction. When one-time keys are
  exhausted the bundle may return the device's current signed fallback key.
- DM control endpoints (device, key upload, key bundle) and delivery endpoints
  (send, poll, ack) use separate per-user buckets. Delivery is not capped by
  the generic 60/min API limiter.
- A device may have at most 1,000 pending, unexpired messages. Queue-full and
  dedicated rate-limit failures are explicit responses.
- Send idempotency is keyed by sender device + client message ID and stores only
  a SHA-256 digest and response/routing metadata. Acknowledgement removes
  ciphertext but retains the digest record until expiry.
- Poll requires an explicit `after` cursor. Sequence numbers can have gaps.
  Acknowledgement is idempotent and returns 204.
- Expired inbox and idempotency records are removed by a janitor, not by poll.
  Account deletion removes all DM inbox rows where the user is sender or
  recipient, plus device keys, fallback keys and idempotency records, in its
  existing transaction. This runs even when the DM feature flag is off, so the
  009-011 migrations are mandatory for every deployment and must be applied
  before   the new binary takes traffic. Migration 015 adds two-way `dm_blocks`; a block
  denies key-bundle and send eligibility in both directions and purges pair
  state. Sent inbox rows reference the sender

  user (not the short-lived device), so re-registering a device does not delete
  delivery history. The v1 privacy decision is one-sided: deleting an account
  also removes messages that account sent but whose recipients have not polled
  them yet; the alternative would retain deleted-user ciphertext in the relay.
- `GET /api/dm/users/:id/key-bundle` defaults to fingerprint-only inspection and
  only claims a one-time key with `include_one_time_key=true`. The key-bundle
  limiter is scoped to `(caller, target)` so one client cannot drain a target's
  key pool by cycling callers.
- `fk_dm_inbox_sender_user` uses `ON DELETE RESTRICT`. The deployment only
  tombstones users, so this is safe. A future hard-delete flow must purge DM
  inbox rows first.
- The server may observe account/device IDs, routing, timestamps, sequence,
  envelope size, and delivery state. It cannot recall copies already decrypted
  or copied by a recipient.

## Verification seams

1. Go handler tests exercise the public HTTP contract with a test verifier and
   consumer-side store doubles.
2. `scripts/smoketest/dm` runs against real HTTP + PostgreSQL and covers the
   009-011 + 015 schema, `FOR UPDATE SKIP LOCKED`, idempotency upsert, register,
   upload, send, poll and ack. Its recorded results are in
   `scripts/smoketest/dm/README.md`.
3. The Go SDK integration tests in `clients/dm` cover a local Olm end-to-end run
   with an in-memory relay; those tests are not a network interoperability test.
4. `TestUnknownAPIRouteSkipsAuthAndRateLimit` proves unknown `/api/...` and
   `/api/dm/...` paths return 404 without entering the auth or rate-limit
   middleware. Echo adds group middleware to its synthetic not-found catch-all,
   so `NewRouter` re-registers those two not-found paths without middleware.

Smoke evidence is not a security audit. Production enablement still requires
an independently reviewed, interoperable client and an independent security
review.

