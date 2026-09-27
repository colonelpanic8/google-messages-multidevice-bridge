# 0003: WhatsApp linked-device storage and identity

Status: accepted

## Decision

Each process selects one network at startup. The Google connection adapter retains
libgm's pairing and event behavior. WhatsApp uses the pinned whatsmeow module. The
store, HTTP API, supervisor, outbox, history jobs and push delivery remain shared.
The database records its network; opening an existing Google database as WhatsApp,
or a tagged database as the other network, fails before connecting.

Implement whatsmeow's store interfaces directly over an encrypted bbolt bucket.
There is no SQLite database, plaintext sidecar, decrypted temporary database or
second database lock. Every value uses the existing storage key, AES-256-GCM,
fresh nonces and record-specific associated data. Identifiers, sizes and access
patterns remain visible, as with the original store. Device identity, prekeys,
Signal sessions, sender keys, app-state versions/keys/MACs, contacts, chat settings,
message secrets, privacy tokens, event buffers and LID mappings are persisted.

The context passed to `DoDecryptionTxn` carries a bbolt transaction. Nested
credential operations reuse it, making ratchet advancement and decrypted-event
buffering atomic. A failed transaction rolls both back. No in-memory credential
cache is authoritative. Encryption, rollback, reopening, prekey allocation and
PN-to-LID Signal-session migration have synthetic tests.

Pairing writes into a fresh encrypted credential namespace. Only successful
pairing changes the active namespace in the saved session; cancellation does not
replace the previous session. An account-number change automatically requests a
new entity epoch even if the client omitted `new_phone`. Old credential namespaces
are retained encrypted; credential garbage collection is not implemented. Offline
backups need just the one database and its separately protected storage key.

## Identity

Direct chat and participant IDs use non-device LID JIDs, such as `123@lid`.
Group IDs retain their `@g.us` JID. Device suffixes and hosted/legacy user domains
are normalized. Phone-number JIDs resolve through the encrypted LID mappings
learned by whatsmeow, history, live alternate addresses and read-only user lookup.
A phone number is an address, not a fallback public conversation identity.

If a PN cannot yet resolve, its encrypted event remains pending. It is not
published as a second conversation. This trades immediate visibility for stable
identity and can delay a contact or message while mappings are unavailable.
Participants expose E.164 `address` only when a full phone number is known; LID-only
and redacted numbers are never fabricated into dialable addresses.

## Durability and uncertainty

WhatsApp callbacks spool messages, receipts and history to encrypted records before
returning. A worker normalizes and commits snapshots, then removes the spooled
record. Unresolved identities and updates whose original message has not arrived
remain pending across restarts. Duplicate originals cannot overwrite an edit or a
newer receipt. This is not a claim that WhatsApp's network ACK and the local commit
form one transaction: whatsmeow owns upstream ACK/decryption behavior.

Generate a WhatsApp message ID before queueing and persist it as the outbox
transaction ID. A send uses that exact ID. A successful server response is
`accepted`; a later outgoing echo can confirm the matching conversation and
transaction. The initial local `server_ack` snapshot does not itself claim an echo.
Explicit validation/server refusals are rejected. Unknown failures are ambiguous.
The client queues one attachment per WhatsApp message, each with its own outbox key.

Disable whatsmeow auto-reconnect, login auto-reconnect and retry-receipt resends.
Never reconnect an already-used Client: the supervisor waits for its bridge-owned
operations to finish, then constructs a new client. Therefore the pinned library's
`retryFrame` cannot regain a connected socket and replay an uncertain mutation.
Media upload and history requests remain repeatable preflight/read operations.

Request full history at pairing with a 3650-day, 10240-MiB requested ceiling. These
are requests, not guarantees that WhatsApp supplies that archive. On-demand history
is asynchronous: store its boundary, wait for the matching chat response and its
queued messages to be committed, then advance the bridge history job. A pending
read-only request may be repeated after two minutes. Folder jobs enumerate the
local synchronized chats; there is no WhatsApp spam-folder archive scan.

## Validation and limits

Tests use synthetic events, fake clients and temporary encrypted stores. No account
was paired, no real upstream connection was opened and no message was sent.
Live QR/phone-code linking, actual history availability, app-state contact coverage,
group creation, media transfer, phone-offline behavior and server receipt timing
remain for Ivan to verify. Whatsmeow owns internal worker lifetimes; the bridge does
not claim the patched libgm lifecycle guarantees for WhatsApp.

View-once/ephemeral content is archived when the linked device actually receives
readable content. This local archive does not erase old versions or cached media
when the remote message expires or is revoked. Unsupported message forms (calls,
poll interaction, newsletters' specialized mutations, payments, etc.) are not
implemented as interactive client features. Undelivered pending updates and old
credential namespaces have no automatic retention cleanup.
