# Direct Messages E2EE — Design and Threat Model (Draft)

**Status:** Initial draft; protocol/library selection and security review are
pending. Do not treat the current PR #74 implementation or migration 009 as
E2EE-ready or production-ready.

**Tracking:** GitHub issue #75 records acceptance criteria. PR #74 is the
linked implementation draft. PR #72 is still open; resolve its overlapping
changes and migration ordering before implementation resumes.

## Product decisions

- 1:1 text messaging; no media.
- Multiple devices per account.
- No server-side private-key recovery.
- Keep first-contact message requests; a recipient must accept before replying.
- Support delivery/read receipts and online/typing status.
- Use WebSocket for bidirectional delivery and ephemeral presence/typing events;
  use HTTP for history and pagination.
- Account deletion removes server-held ciphertext and requests best-effort
  deletion from clients when they reconnect. It cannot erase offline,
  backed-up, exported, or copied messages.
- A friend will provide or prepare the Flutter/mobile client repository.

## Security goals and threat model

### Assets

- Plaintext messages and local message history.
- Per-device identity private keys, signed prekeys, one-time prekeys, and
  ratchet/session state.
- Public identity keys and prekey bundles used to start sessions.
- Account/device membership and first-contact request state.
- Delivery/read receipts and online/typing events.

### Adversaries and trust boundaries

- A network attacker may observe, delay, replay, reorder, or modify traffic.
- A curious or compromised API/database operator may read or alter server-side
  records and may attempt to substitute public key bundles.
- An attacker with access to one user's account may attempt to register a
  device, consume prekeys, or send abusive requests.
- A lost or stolen device may expose its locally available keys and history.
- A recipient may copy, export, photograph, or back up plaintext after
  decryption.

Client devices are the only places where message plaintext and private protocol
keys may exist. The server is a ciphertext relay and public-key directory, not
a trusted plaintext processor.

### Intended guarantees

- Message content is confidential and authenticated between verified devices.
- The server cannot decrypt message content or recover device private keys.
- Forward secrecy and post-compromise recovery are required properties of the
  selected protocol and implementation, not properties inferred from HTTPS.
- Identity-key changes and new devices are visible to users; a server must not
  silently replace a trusted identity key.
- No server-side key recovery. Loss of all trusted devices may make prior
  history unrecoverable.

### Explicit limitations

- E2EE does not hide routing metadata. The service may observe account and
  device IDs, connection/IP metadata, message timing and size, request state,
  receipt timing, and presence/typing events.
- E2EE cannot protect plaintext on a compromised endpoint or prevent a
  recipient from copying content.
- Account deletion cannot guarantee erasure from offline devices, user
  backups, or copies.
- Availability, traffic-analysis resistance, and sender anonymity are not
  promised by this design.

## Protocol and library investigation

The protocol is **not selected**. The design review should evaluate an
established asynchronous messaging construction, including:

- [PQXDH](https://signal.org/docs/specifications/pqxdh/) or
  [X3DH](https://signal.org/docs/specifications/x3dh/) for initial
  asynchronous session setup.
- [Double Ratchet](https://signal.org/docs/specifications/doubleratchet/) for
  per-message key evolution.
- [Sesame](https://signal.org/docs/specifications/sesame/) or an equivalent
  reviewed multi-device session-management design.

These are candidate specifications, not an implementation decision. No custom
cryptographic primitives or protocol variants should be introduced.

Initial Flutter/Dart package review on 2026-11-20 found candidates but no
verified independent audit suitable to approve one:

- [`libsignal_protocol_dart`](https://pub.dev/packages/libsignal_protocol_dart)
  0.8.2 is a community pure-Dart implementation, GPL-3.0, published by the
  verified `mixin.dev` publisher. A verified publisher and recent release do
  not establish an independent security audit or complete multi-device
  protocol support.
- [`libsignal`](https://pub.dev/packages/libsignal) 9.0.1 describes itself as
  Dart bindings over upstream libsignal, but is a new AGPL-3.0 wrapper
  published on 2026-11-11 with no verified pub.dev publisher. Its build,
  native bindings, maintenance, security process, and license compatibility
  need review.
- [`double_ratchet`](https://pub.dev/packages/double_ratchet) 0.2.1 is an MIT
  pure-Dart implementation. Its package documentation says Sesame
  multi-device management and PQXDH are not included, so it is insufficient
  by itself for the selected requirements.
- The [upstream `signalapp/libsignal`](https://github.com/signalapp/libsignal)
  documents official Java, Swift, and TypeScript APIs; it does not document an
  official Dart API. A community bridge to upstream code still requires
  independent review.

Before choosing a library, verify upstream maintenance and security policy,
independent audit scope, platform support, license compatibility, protocol
version and test-vector coverage, native build provenance, secure key-store
interfaces, migration behavior, and incident-response history. A package
search result, popularity score, or passing tests is not a security audit.

## Candidate data flow

1. A device creates its private identity and session material locally using the
   approved library and platform secure storage.
2. The device registers only public identity material, signed prekeys, and
   one-time prekeys. The backend must atomically claim a one-time prekey.
3. Before sending, the client obtains bundles for every recipient device and
   the sender's other linked devices. It verifies identity changes according
   to the approved trust model, establishes per-device sessions, and encrypts
   a separate envelope for each target device.
4. The backend stores and routes opaque, versioned ciphertext envelopes. It
   must not accept a plaintext fallback, log payloads, or persist private key
   or ratchet state.
5. WebSocket carries authenticated ciphertext-delivery and ephemeral
   presence/typing events. HTTP remains available for inbox/history queries
   and cursor pagination. Receipts and presence are metadata and must be
   documented as server-visible.
6. Message request acceptance is server-managed state. Request policy and
   abuse limits must stop unsolicited-message spam without decrypting content.
7. Account deletion removes server-held ciphertext and public device
   registrations, and sends best-effort client purge requests. This does not
   revoke copies already downloaded by another device.

The final envelope, endpoint contract, device-enrollment approval flow,
identity-verification UX, retry semantics, retention, and key-rotation policy
remain open until the protocol and client repository are reviewed.

## Implementation and review gates

1. Confirm the client repository and responsible maintainer.
2. Complete and review the threat model, protocol choice, library due
   diligence, identity verification, and multi-device enrollment design.
3. Split implementation into small dependent changes: design; client key
   storage/device registration; public key directory and prekey operations;
   ciphertext relay; WebSocket delivery/receipts/presence; abuse limits and
   account deletion; then integrated two-user/two-device validation.
4. Rebase against PR #72 after it merges. Reconcile migrations 008/009, SQLC
   schema inputs, account deletion, and shared routes before regenerating SQL.
5. Require focused unit/integration tests and a live PostgreSQL + HTTP +
   WebSocket smoke test for two users and two devices, run twice consecutively.
6. Obtain security review before a production rollout.

The plaintext handler, `dm_messages.content` schema, and migration 009 in PR
#74 are only a prototype. Reuse individual state-machine or pagination ideas
only after review; replace the plaintext persistence and do not merge the
prototype as the E2EE feature.
