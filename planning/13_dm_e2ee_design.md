# MokiBox Direct Messages — E2EE v1

**Status:** Backend-only scope for `mokibox-main`. Client-side crypto and UI
are outside this repository.

## Goal and boundaries

Implement the Go backend component for 1:1 encrypted messaging. This backend
alone is not an end-user-ready E2EE chat; a separately implemented client must
encrypt and decrypt messages:

- The Go API authenticates MokiBox accounts and relays opaque ciphertext.
- Clients outside this repository own Olm accounts, sessions, key verification,
  message encryption/decryption, and encrypted local state.
- No server component receives private keys or plaintext.
- Client implementation and chat UI are both out of scope for this backend
  repository.

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

### Client requirements, not implemented here

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

- Olm v1 is the intended client protocol. Client library, platform, and
  distribution license selection are outside this backend repository and must
  be decided before implementing or distributing a client.
- Each device has Curve25519 and Ed25519 identity keys. Private account and
  Olm session state must be stored using the selected crypto library's
  encrypted state format; its random pickle key must use OS-backed secure
  storage. Never log keys, pickles, plaintext, or ciphertext.
- The client publishes its public identity keys and signed Olm one-time keys.
  The server stores and returns their opaque encodings; the client verifies
  each signature against the peer Ed25519 identity key before establishing a
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
  sequence and enforce the queue cap in one transaction.
- A device may have at most 1,000 pending, unexpired messages. Queue-full and
  dedicated rate-limit failures are explicit responses.
- Send idempotency is keyed by sender device + client message ID and stores only
  a SHA-256 digest and response/routing metadata. Acknowledgement removes
  ciphertext but retains the digest record until expiry.
- Poll requires an explicit `after` cursor. Sequence numbers can have gaps.
  Acknowledgement is idempotent and returns 204.
- Expired inbox and idempotency records are removed by a janitor, not by poll.
  Account deletion removes device keys, inbox rows, and idempotency records in
  its existing transaction.
- The server may observe account/device IDs, routing, timestamps, sequence,
  envelope size, and delivery state. It cannot recall copies already decrypted
  or copied by a recipient.

## Verification seams

1. Go handler tests exercise the public HTTP contract with a test verifier and
   consumer-side store doubles.
2. HTTP/PostgreSQL smoke verifies the backend relay contract and ciphertext-only
   storage; E2EE interoperability requires a separately maintained client.
3. The owner-run evidence in `.smoke/REPORT.md` records a local Olm end-to-end
   run using temporary Flutter test helpers. Those client helpers are not part
   of this backend repository.

Smoke evidence is not a security audit. Production enablement still requires
an independently reviewed, interoperable client and an independent security
review.

