# MokiBox Direct Messages — E2EE Design Proposal

**Status:** Design and threat-model proposal only. No protocol, library, API,
or production scope is approved by this document.

**Purpose:** Design and threat-model an E2EE chat for MokiBox with a product
target of feature coverage comparable to TikTok/Instagram DM. This document
starts from the current `main` baseline; it does not continue or depend on any
earlier DM pull request or issue.

## Product and planning context

The current PRD does not assign a priority to direct messages. The separate
`planning/ROADMAP_TIKTOK_PARITY.md` lists DM under P3 ("eval dulu, jangan
dijadwalkan"), but that roadmap is marked Draft and is awaiting user approval.
Treat this as an unapproved evaluation note, not a committed product priority.
Promoting DM to a committed product phase requires an explicit product
decision and corresponding planning/issue updates.

The product direction is to keep E2EE as a requirement while targeting a
TikTok/Instagram-like set of user-facing chat capabilities. This is a product
target, not a claim that every reference-app behavior is compatible with E2EE
or will ship at once. The design must make capability gaps and trade-offs
explicit rather than silently dropping parity features.

- 1:1 text messages and first-contact message requests.
- Share MokiBox videos, media, and voice notes; support reactions, replies,
  unsend, and group chat as parity capabilities to design and sequence.
- Multiple devices per account.
- No server-side recovery of private encryption keys.
- Delivery/read receipts and online/typing indicators, subject to a documented
  metadata and privacy review.
- Preserve useful search and new-device history experiences where possible,
  while documenting E2EE limitations and recovery trade-offs.
- Blocking and user-initiated reporting, with clear limits on moderation
  without plaintext access.
- Best-effort account-deletion propagation; deletion cannot erase offline,
  backed-up, exported, or copied plaintext.

These are product targets for evaluation, not a release commitment, finalized
protocol, or client contract.

## Feature coverage and boundaries

This is a status of the design work, not implemented product functionality.
Parity features remain targets to design and prioritize; the table does not
promise they are all included in the first release.

| Capability | Current design status | Boundary or open point |
|---|---|---|
| 1:1 text and message requests | Parity target; design incomplete | Define request limits and acceptance behavior; no implementation exists. |
| Read receipts, typing, and online status | Parity target; design incomplete | Requires realtime delivery and discloses activity metadata. |
| Push notifications | Privacy requirement documented | No plaintext or preview in FCM/APNs payloads; the client decrypts locally. |
| Block and report | Parity target; design incomplete | Define blocking effects and a user-controlled report flow; reporting may disclose selected plaintext to moderators. |
| Share a MokiBox video in DM | Parity target; design incomplete | Keep the video ID/reference inside encrypted message content. Define recipient authorization, deleted/private video behavior, previews, and metadata when the recipient opens it. |
| Media and voice notes | Parity target; design incomplete | Define client-side encryption, ciphertext storage/delivery, size/type limits, preview behavior, and abuse reporting. |
| Reactions, replies, and unsend | Parity target; design incomplete | Define encrypted event semantics, edits/deletion propagation, offline behavior, and what recipients may already have copied. |
| Group chat | Parity target; design incomplete | Requires a multi-member key-management/protocol evaluation and membership-change behavior. |
| Message search | Parity target with an E2EE constraint | The server cannot search opaque message content. Evaluate local search and explicitly decide whether that satisfies product expectations; do not imply server-side search works. |
| History on a new device | Parity target with a recovery constraint | Evaluate authenticated device-to-device transfer without server-side private-key recovery. If no approved device or transfer is available, local history may be lost. |

If a reference-app behavior fundamentally requires server access to plaintext,
record the difference as an explicit E2EE trade-off and product decision. Do
not silently weaken encryption or call an unavailable server-side behavior
"feature parity".

## Security goals

- Message plaintext and private protocol keys exist only on user devices.
- The service stores and routes opaque, versioned ciphertext envelopes and
  only the metadata needed to operate the service.
- A database or API operator cannot decrypt message content or recover device
  private keys.
- Messages are authenticated between devices under an explicit identity-trust
  model; a server must not silently replace a trusted identity key.
- The selected construction must provide appropriate forward-secrecy and
  post-compromise properties for asynchronous multi-device messaging.
- Push notifications must not expose plaintext message content; any message
  preview shown by the client must be decrypted locally on the device.
- No custom cryptographic primitives or unreviewed protocol variants.

## Threat model

### Assets

- Message plaintext, media/voice attachments, and local history.
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

The protocol review must define, before implementation:

1. Per-device identity-key generation and secure local storage.
2. Device enrollment and authorization of a new device, including what
   existing devices must approve and what happens when none are available.
3. Signed-prekey rotation, one-time-prekey publication/claiming, expiry, and
   recovery from prekey exhaustion.
4. Session establishment and per-device message fanout, including the
   sender's other devices.
5. Identity-key change, device revocation, session replacement, and user
   notification behavior.
6. User-visible identity verification (for example, an out-of-band
   fingerprint or QR flow) and the security limitations of an unverified
   first contact.
7. Local history migration, authenticated device-to-device transfer, device
   loss, and explicit no-server-private-key-recovery behavior.

Do not treat an authenticated account or HTTPS connection as proof that a
device identity key is trustworthy.

## Client/server responsibility boundary

### Client

- Owns all message encryption/decryption and private protocol state.
- Uses platform-appropriate secure storage and does not expose keys through
  logs, analytics, crash reports, or backups unless a separately reviewed
  recovery design is approved.
- Verifies device identities according to the agreed trust model.
- Encrypts separately for each authorized recipient device and the sender's
  linked devices as required by the protocol.
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
- Delivers messages and ephemeral events over an approved transport; records
  only the minimum metadata required for delivery and abuse controls.
- If FCM/APNs is used for offline delivery, sends no plaintext or message
  preview in the push payload. Prefer a generic wake-up signal; any encrypted
  envelope used for a client-side preview remains opaque to the provider.
- Purges server-held ciphertext and device directory entries on account
  deletion, subject to documented retention and backup behavior.

The available repository is the Go/Echo backend. No mobile/client repository
or client source was present during this design pass, so client-side
cryptography cannot be implemented or validated here yet.

## Protocol and library decision gate

No protocol or library is selected. Evaluate established asynchronous
multi-device messaging designs and their client bindings. Candidate families
may include the Signal protocol suite (for example, PQXDH/X3DH, Double Ratchet,
and multi-device session management) and MLS where its group model is
appropriate. These are investigation leads, not recommendations or approvals.

For each candidate, review:

- Protocol coverage for 1:1 asynchronous messaging and multiple devices.
- Current maintenance, security policy, and independently reviewed audit
  scope.
- Client-platform support, secure-storage integration, interoperability, and
  test-vector coverage.
- License compatibility, including copyleft obligations and distribution
  model.
- Build provenance, dependency update process, vulnerability response, and
  migration/rollback behavior.

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

## Open decisions before implementation

- Confirm the client repository, framework, supported platforms, and owner.
- Approve the product scope and whether DM should be prioritized; the P3 entry
  in the Draft parity roadmap is not approved.
- Select and review a protocol/library and its license.
- Approve device enrollment, identity verification, and key-change UX.
- Define the ciphertext envelope, message ordering/synchronization cursor,
  retention, and idempotency contract.
- Define the parity target and delivery sequence for media, voice notes,
  reactions, replies, unsend, and group chat; specify which behaviors are
  E2EE-compatible and which require explicit product trade-offs.
- Decide whether local-only message search meets user expectations given that
  the server cannot search opaque ciphertext.
- Decide how new-device history transfer can work without server-side private
  key recovery, and state what happens if no trusted device is available.
- Decide transport and fanout architecture for delivery, receipts, and
  ephemeral presence.
- Specify encrypted video-reference authorization, deleted/private-video
  behavior, and client rendering.
- Decide the offline-delivery and push-notification design, including payload
  contents and which metadata platform push providers can observe.
- Define abuse limits and the account-deletion contract for other participants'
  history.
- Agree on security-review ownership and release gates.

Until these decisions are recorded, this proposal does not authorize
cryptographic implementation, a production schema, or a claim that MokiBox DM
is end-to-end encrypted.
