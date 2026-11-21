# MokiBox Direct Messages — E2EE Design Proposal

**Status:** Design proposal with proposed v1 decisions. Nothing in the
"Proposed v1 decisions" section is approved until the reviewer records approval
in the decision table. No implementation is authorized by this document.

**Purpose:** Define a threat model and a reviewable proposal for a minimal,
end-to-end encrypted 1:1 text chat for MokiBox. This document starts from the
current `main` baseline and does not continue the closed plaintext prototype.

## Product and planning context

The current PRD does not assign a priority to direct messages. The separate
`planning/ROADMAP_TIKTOK_PARITY.md` lists DM under P3 ("eval dulu, jangan
dijadwalkan"), but that roadmap is marked Draft and is awaiting user approval.
Treat this as an unapproved evaluation note, not a committed product priority.
Promoting DM to a committed product phase requires an explicit product
decision and corresponding planning/issue updates.

The proposed v1 goals, pending reviewer approval, are:

- 1:1 text messages with client-side E2EE.
- One active device per account, with no server-side private-key recovery.
- Simple first-contact message requests and encrypted read receipts.
- HTTP polling for inbox delivery.
- A production feature flag that remains off until the security review passes.
- Best-effort account-deletion propagation; deletion cannot erase offline,
  backed-up, exported, or copied plaintext.

WebSocket, typing/online status, push notifications, video sharing, media,
voice notes, reactions, replies, unsend, groups, client-side search, and
history transfer to new devices are proposed as deferred. Block/report scope
is not decided in the proposal and needs reviewer confirmation.

## Feature coverage and boundaries

This is a status of the design work, not implemented product functionality:

| Capability | Current design status | Boundary or open point |
|---|---|---|
| 1:1 text | Proposed for v1 | Client performs encryption/decryption; no implementation exists in this proposal. |
| First-contact message requests | Proposed for v1 | Recipient must accept before replying; proposed pre-accept cap is 1 message, pending approval. |
| Read receipts | Proposed for v1 | Sent as encrypted control messages; server sees envelopes, not read state. |
| HTTP polling | Proposed for v1 | Per-device inbox and sequence cursor; no WebSocket in v1. |
| One active device per account | Proposed for v1 | New-device login revokes the prior registration; no history transfer or server recovery. |
| Fingerprint verification | Proposed for v1 | Manual comparison and identity-key-change warning; trust-on-first-use is vulnerable to server substitution until comparison. |
| Feature flag | Proposed for v1 | Off in production; dev/staging may enable after merge; production waits for security review. |
| Typing/online, WebSocket, push, video share, media, voice notes, reactions, replies, unsend, groups | Proposed deferred scope | Follow-up designs; not implemented or promised by this proposal. |
| Block and report | Not decided | Reviewer must confirm whether these are deferred or required in v1; report content must be user-selected. |
| Server-side message search | Not available over opaque E2EE content | Server cannot search ciphertext; local search is deferred. |
| History on a new device | Not in proposed v1 | No history transfer or server-side key recovery; losing the active device may lose history. |

## Security goals

- Message plaintext and private protocol keys exist only on user devices.
- The service stores and routes opaque, versioned ciphertext envelopes and
  only the metadata needed to operate the service.
- A database or API operator cannot decrypt message content or recover device
  private keys.
- Messages are authenticated between devices under an explicit identity-trust
  model; a server must not silently replace a trusted identity key.
- The proposed v1 protocol must provide forward secrecy and clearly document
  any post-compromise and post-quantum limitations.
- Push notifications must not expose plaintext message content; any message
  preview shown by the client must be decrypted locally on the device.
- No custom cryptographic primitives or unreviewed protocol variants.

## Threat model

### Assets

- Message plaintext, attachments (out of initial scope), and local history.
- Device identity private keys, prekeys, session/ratchet state, and backups.
- Public identity keys, signed prekeys, and one-time prekeys.
- Device membership, enrollment approvals, revocations, and identity changes.
- Message-request state and anti-abuse limits.
- Delivery/read receipts, online/typing events, and routing metadata.
- Authentication tokens and active WebSocket sessions.

### Adversaries and trust boundaries

- A network attacker can observe, delay, replay, reorder, or modify traffic.
- A curious or compromised API/database operator can read or alter stored
  records and attempt to substitute public-key bundles.
- An attacker with a compromised account can try to add a device, drain
  prekeys, impersonate the account, or send unsolicited messages.
- A lost, stolen, or compromised endpoint can expose plaintext and local keys.
- A recipient can copy, photograph, export, or back up decrypted messages.
- Other MokiBox services and operators may observe metadata even when message
  content is encrypted.

### Out of scope for the E2EE guarantee

- Compromised endpoints, malicious OS/platform components, screenshots, and
  recipient disclosure.
- Hiding account identities, IP addresses, device identifiers, message
  timing/size, delivery patterns, or presence from the service.
- Hiding a video-view request from the service when a recipient opens a shared
  video through the normal MokiBox video APIs; the encrypted message reference
  itself must not be exposed as server-readable DM content.
- Guaranteed deletion from devices that are offline, backups, exports, or
  copies.
- Availability, traffic-analysis resistance, and sender anonymity.

## Trust and key lifecycle requirements

The proposed v1 device lifecycle, pending reviewer approval, is one active
device per account:

1. Per-device identity-key generation and secure local storage.
2. Device registration, key-bundle publication, one-time-key claiming, expiry,
   and recovery from prekey exhaustion.
3. New-device login revokes the previous device registration. The new device
   starts without prior message history; server-side private-key recovery and
   history transfer are not in proposed v1.
4. Identity-key changes, session replacement, and a user-visible warning.
5. Manual fingerprint comparison and the security limitations of
   trust-on-first-use until users compare fingerprints.

Do not treat an authenticated account or HTTPS connection as proof that a
device identity key is trustworthy.

## Client/server responsibility boundary

### Client

- Owns all message encryption/decryption and private protocol state.
- Uses platform-appropriate secure storage and does not expose keys through
  logs, analytics, crash reports, or backups unless a separately reviewed
  recovery design is approved.
- Verifies device identities according to the agreed trust model.
- For proposed v1, encrypts for the single active recipient device; adding
  multi-device fanout requires a follow-up protocol and product decision.
- Handles offline queues, duplicate delivery, replay protection, and session
  recovery according to the selected library.
- If an OS notification displays a message preview, the client decrypts the
  content locally; push providers must never receive plaintext or previews.

### Backend

- Authenticates accounts using the existing MokiBox JWT middleware.
- Stores and serves public device/key material only; claims one-time prekeys
  atomically and enforces quotas/rate limits.
- Relays opaque ciphertext envelopes without plaintext fallback, content
  indexing, or payload logging.
- Enforces conversation membership, request state, and device authorization.
- For proposed v1, provides an authenticated HTTP polling inbox and read
  receipt relay; WebSocket and ephemeral presence are deferred.
- If FCM/APNs is used for offline delivery, sends no plaintext or message
  preview in the push payload. Prefer a generic wake-up signal; any encrypted
  envelope used for a client-side preview remains opaque to the provider.
- Purges server-held ciphertext and device directory entries on account
  deletion, subject to documented retention and backup behavior.

The available repository is the Go/Echo backend. No mobile/client repository
or client source is available in this workspace; the Flutter client is owned
by the repository owner's friend. Client-side cryptography must be implemented
and reviewed in that client repository before the feature can be called
end-to-end usable.

## Protocol and library decision gate

The provisional candidate and its due-diligence notes are recorded under D2
below. They are not an approved dependency. Verify package facts, protocol fit,
license obligations, current advisories, and actual Flutter integration with
the repository owner and an independent security reviewer before adoption.

Before approval, the reviewer must verify:

- The candidate's current package maintenance, changelog, security advisories,
  and the exact independent audit scope and date.
- Android/iOS builds and Flutter API suitability in the actual client repo,
  plus secure storage and test-vector coverage.
- License compatibility and distribution obligations.
- Build provenance, dependency update process, vulnerability response, and
  migration/rollback behavior.
- The protocol's properties for v1's one-device-per-account model and its
  later multi-device migration path.

Passing tests, popularity, a verified package publisher, or use by another
product is not by itself evidence of a security audit. Security review is
required before production rollout.

## MokiBox integration constraints

- The backend is a single Go module with an Echo API gateway, PostgreSQL via
  `database/sql` and the pgx stdlib adapter, sqlc-generated queries, Redis /
  Asynq, and a separate transcoder worker.
- Schema changes are forward-only migrations. Add each new migration to
  `sqlc/sqlc.yaml`; do not edit the initial migration or generated SQL by hand.
- Authentication runs before the existing per-user in-process rate limiter.
  That limiter is not distributed across gateway instances.
- The current Nginx `/api/` proxy does not configure WebSocket upgrade
  headers/timeouts. The current gateway has no WebSocket hub or cross-instance
  fanout mechanism.
- Account deletion tombstones the account and removes associated relational
  data in a transaction. The new design must explicitly include device
  registrations and server-held ciphertext in that lifecycle.

These are constraints observed in the current backend, not decisions that
preselect the final service architecture.

## Metadata, abuse, and operational review

Before production scope is approved, document:

- Which identifiers, IP/session details, timestamps, envelope sizes, receipt
  states, and presence events the server can observe and retain.
- Whether offline delivery uses FCM/APNs, what the push payload contains, and
  what additional metadata push providers can observe (such as device tokens,
  app identifiers, routing data, and delivery timing). Push payloads must not
  include plaintext or previews; message decryption and display happen on the
  client.
- For video sharing, how the client resolves an encrypted video reference,
  checks the recipient's access under existing video visibility rules, and
  handles deleted or newly private videos without exposing message content to
  the server.
- Retention and deletion rules for ciphertext, public keys, prekeys, delivery
  state, logs, metrics, and backups.
- Message-request limits, recipient blocking/reporting, prekey-exhaustion
  protection, and WebSocket connection/message limits.
- WebSocket authentication at connection upgrade, token expiry/revocation,
  origin policy, reconnect behavior, backpressure, and delivery across more
  than one gateway instance.
- What abuse reporting/moderation can and cannot do without access to
  plaintext.

## Proposed v1 decisions

Facts below were checked on 2026-11-21. These are proposals for the reviewer to
approve, change, or reject; they narrow v1 so that it can be finished end to
end.

### D1. v1 scope

- In: 1:1 text, E2EE, first-contact message requests, read receipts, HTTP
  polling, and a feature flag that stays off in production until security
  review.
- Deferred: WebSocket, typing/online status, push notifications, video sharing,
  media, voice notes, reactions, replies, unsend, groups, client-side search,
  and history transfer to new devices.
- Block/report is not assigned to v1 in this proposal; the reviewer must
  confirm whether it is deferred or required before implementation starts.

### D2. Protocol and library

- Proposed: Olm via Matrix's Rust [`vodozemac`](https://github.com/matrix-org/vodozemac),
  using the Flutter [`flutter_vodozemac`](https://pub.dev/packages/flutter_vodozemac)
  binding.
- Checked: `flutter_vodozemac` 0.9.1, published by the verified
  `famedly.com` publisher, supports Android and iOS, is licensed AGPL-3.0, and
  wraps the Rust `vodozemac` crate.
- The upstream vodozemac [README](https://github.com/matrix-org/vodozemac#security-notes)
  links a public [Least Authority audit report](https://matrix.org/media/Least%20Authority%20-%20Matrix%20vodozemac%20Final%20Audit%20Report.pdf)
  with no significant findings. The audit is from 2022 and predates current
  releases; verify its scope, the changelog, and current advisories before
  pinning a version. A verified publisher or audit history does not itself
  approve release.
- Olm is Double Ratchet-based and is proposed for forward secrecy. It provides
  no post-quantum protection in v1. The reviewer must verify the exact
  post-compromise properties and threat-model fit against the selected
  version/specification before implementation.
- The Go backend needs no cryptographic library: it stores public key material
  and opaque ciphertext and may validate encoding and length only.
- AGPL-3.0 has copyleft obligations for the distributed client. The owner and
  reviewer must confirm the client's license and distribution model before
  adoption. This is not legal advice.
- Alternatives considered: Signal [`libsignal`](https://github.com/signalapp/libsignal)
  (AGPL-3.0; official repo documents Java, Swift, and TypeScript APIs, says
  use outside Signal's own clients is unsupported, and has no documented
  official Dart binding) and [OpenMLS](https://github.com/openmls/openmls)
  (MIT; better suited to MLS group key management).

### D3. Device model

- Proposed: one active device per account in v1. The schema addresses every
  envelope to a `device_id` so multi-device can be added later without a
  breaking change.
- Logging in on a new device revokes the previous device's registration. The
  new device cannot read earlier messages: no server recovery and no history
  transfer in v1.

### D4. Message flow

1. Registration: the client generates device keys (Ed25519 signing key and
   Curve25519 identity key), a batch of one-time keys, and an optional fallback
   key, then uploads public parts only.
2. First message: the sender fetches the recipient's key bundle. The server
   claims one one-time key atomically, or returns the fallback key when none
   remain. Bundle fetches are rate limited per sender.
3. The sender creates an outbound Olm session, encrypts the payload (type,
   text, client message ID, and client timestamp), and posts an envelope with
   sender and recipient user/device IDs, message kind (pre-key or normal),
   ciphertext, and a client message ID for idempotency.
4. The recipient polls its inbox, decrypts, creates the inbound session, and
   replenishes one-time keys when the server reports a low count.

This is a proposed flow, not an approved wire/API contract. The client
cryptography details must be checked against the selected library API and
reviewed before implementation.

### D5. Ordering, delivery, and retention

- Each recipient device has an inbox with a server-assigned sequence number.
  The number comes from a per-device counter row incremented inside the
  message-insert transaction. Do not use `created_at` or a global sequence:
  concurrent transactions can commit out of allocation order, allowing a
  polling cursor to skip messages.
- The server is a store-and-forward queue: ciphertext is deleted after the
  recipient device acknowledges it, and undelivered ciphertext expires after
  a TTL whose value remains to be set. Conversation history lives only on
  devices.

### D6. Receipts

- Read receipts are sent as encrypted control messages from recipient to
  sender, so the server does not learn read state. The server sees only that
  an envelope exists.

### D7. Message requests and abuse limits

- A sender without an accepted relationship creates a pending request; the
  recipient accepts or declines. Proposed cap before acceptance: one message;
  reviewer approval is required.
- Per-user rate limits apply to sends and bundle fetches. The current limiter
  is per process: acceptable only for a single gateway instance; otherwise use
  a Redis-backed limit before scaling out.

### D8. Identity verification

- v1 shows a key fingerprint (safety number) for manual comparison and warns
  the user when a contact's identity key changes. An unverified first contact
  is trust-on-first-use; a malicious server could substitute keys until users
  compare fingerprints.

### D9. Account deletion

- Deleting an account removes its device keys, prekeys, and queued ciphertext
  in the account-deletion transaction. Copies on other users' devices cannot
  be erased; this remains a documented limit.

### D10. Release gates

- Feature flag is off in production by default; dev/staging may enable it after
  merge.
- Live smoke test (PostgreSQL + HTTP, two users and two devices total: one
  device per user) passes twice consecutively.
- A test verifies no plaintext in endpoints, database, logs, or telemetry.
- Independent security review is completed and findings are addressed before
  the production flag is enabled.

### Decision table

| ID | Proposal | Approver | Status |
|---|---|---|---|
| D1 | v1 scope | `<reviewer>` | Proposed |
| D2 | Olm / `flutter_vodozemac`, license accepted | `<reviewer>` | Proposed |
| D3 | One active device in v1 | `<reviewer>` | Proposed |
| D4 | Message flow | `<reviewer>` | Proposed |
| D5 | Per-device sequence and store-and-forward | `<reviewer>` | Proposed |
| D6 | Encrypted read receipts | `<reviewer>` | Proposed |
| D7 | Request cap and rate limits | `<reviewer>` | Proposed |
| D8 | Fingerprint verification and key-change warning | `<reviewer>` | Proposed |
| D9 | Account-deletion contract | `<reviewer>` | Proposed |
| D10 | Release gates | `<reviewer>` | Proposed |

## Open decisions before implementation

- Confirm the Flutter client repository, framework, supported platforms, and
  owner; do not fill these in without the repository owner's confirmation.
- Approve, change, or reject proposed decisions D1-D10, including explicit
  acceptance or rejection of the AGPL-3.0 license.
- Set the remaining values: message-request cap and undelivered-ciphertext TTL.
- Decide push-notification and WebSocket design for a later version.
- Decide whether encrypted video references enter a later version.
- Confirm block/report sequencing.
- Agree on security-review ownership and schedule.

Until the decision table records approval, this proposal does not authorize
cryptographic implementation, a production schema, or a claim that MokiBox DM
is end-to-end encrypted.
