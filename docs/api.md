# HTTP API, schema 1

The bridge serves the web client at `/` and the bundled Chromium extension source
at `/pairing-helper.zip`. Those assets are public. Every `/v1/` route requires
`Authorization: Bearer <token>` except the narrowly scoped
`POST /v1/pairing/credentials` handoff, which accepts only a current pairing ticket.
Tokens in query parameters and cookie authentication are not supported.

The API sends `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`.
It does not enable CORS. Authenticated non-GET/HEAD requests with an `Origin` header
must have the same HTTP or HTTPS origin as the request host.

The canonical record definitions are in [`internal/model/model.go`](../internal/model/model.go).
They contain `schema: 1`, use snake_case keys, and represent times as UTC RFC 3339
strings. Message time is the phone's original timestamp; durable event time is local
commit time. Unknown normalized Google status strings must remain displayable.

## Routes

| Method and route                                                    | Behavior                                                                                       |
| ------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `GET /v1/status`                                                    | Connection, phone responsiveness, and recent-reconciliation status                             |
| `POST /v1/connection/restart`                                       | Wake the supervisor and restart/cancel the current connection; returns 202                     |
| `GET /v1/pairing`                                                   | Current guided-pairing state                                                                   |
| `POST /v1/pairing/start`                                            | Start pairing/re-pairing and return a short-lived ticket; returns 202, or 400 in offline mode  |
| `POST /v1/pairing/cancel`                                           | Cancel the active pairing attempt and return its state                                         |
| `GET /v1/conversations`                                             | Latest stored conversations, newest first, and snapshot `cursor`                               |
| `POST /v1/conversations`                                            | Queue durable conversation creation from E.164 recipients                                      |
| `GET /v1/conversations/{id}/messages?limit=100&before={message_id}` | Latest locally stored message snapshots, newest first; limit 1–500                             |
| `POST /v1/conversations/{id}/reactions`                             | Queue a durable reaction add/remove                                                            |
| `POST /v1/conversations/{id}/typing`                                | Send an immediate, non-durable typing update; returns 204                                      |
| `POST /v1/conversations/{id}/read`                                  | Send an immediate, explicit read receipt; returns 204                                          |
| `POST /v1/uploads?name={filename}`                                  | Store one encrypted local upload from the raw request body; returns 201                        |
| `GET /v1/outbox`                                                    | Durable operation records and snapshot `cursor`                                                |
| `GET /v1/outbox/{idempotency_key}`                                  | One durable operation record                                                                   |
| `POST /v1/messages`                                                 | Queue a durable text and/or attachment message                                                 |
| `POST /v1/outbox/{idempotency_key}/cancel`                          | Cancel only while queued; returns 409 after an attempt starts                                  |
| `POST /v1/sync`                                                     | Request bounded recent reconciliation; requires a connected phone and returns 202              |
| `POST /v1/history`                                                  | Queue, resume, or restart a durable folder/conversation history job; returns 202               |
| `GET /v1/history`                                                   | Durable history jobs and snapshot `cursor`                                                     |
| `POST /v1/history/{id}/pause`                                       | Pause a job; an in-flight page may finish but cannot advance the paused checkpoint             |
| `GET /v1/attachments/{id}`                                          | Download a stored attachment, fetching and caching it encrypted when necessary; at most 20 MiB |
| `GET /v1/events?after=0&limit=100`                                  | Durable events and `next_cursor`; limit 1–1000                                                 |
| `GET /v1/stream?after=0`                                            | Durable event replay followed by SSE notifications and ephemeral typing                        |

## JSON and upload limits

Routes using the standard JSON decoder require media type `application/json`, reject
unknown fields and trailing JSON values, and cap bodies at 32 KiB. No-body mutation
routes do not use that decoder. The pairing credential handoff has its own stricter
contract below.

`POST /v1/uploads` is not JSON. Its body is the file bytes, `Content-Type` must parse
as a MIME media type, and `name` must be nonempty and at most 255 bytes without CR,
LF, or NUL. The stored name is reduced to its basename. Empty files and files larger
than 20 MiB are rejected. Upload IDs are opaque 64-character identifiers; uploading
the same name, MIME type, and bytes returns the existing record.

An upload only stores encrypted source bytes locally. When a queued message is
prepared, the provider upload result is encrypted as private state before the outbox
record is claimed for its single message-send attempt.

## Pairing

Authenticated pairing control uses these states:

- `waiting_for_login`: a ticket is available for the helper.
- `connecting`: valid cookies were accepted and the bridge is contacting Google.
- `confirm_on_phone`: `emoji` must be selected in Google Messages on the phone.
- `paired`, `failed`, or `canceled`: terminal state for that generation.

`POST /v1/pairing/start` returns the existing state if pairing is already active.
Otherwise it creates a random 32-byte base64url ticket: exactly 43 characters,
one-use, credential-handoff-only, and expiring after ten minutes. Starting pairing is
disabled under `serve --offline` and returns 400. A pairing generation guard prevents
a canceled older attempt from overwriting a newer ticket or result.

The extension sends this request without bearer authentication:

```http
POST /v1/pairing/credentials
Content-Type: application/json
```

```json
{
  "ticket": "43-character-one-time-ticket",
  "cookies": {
    "SID": "...",
    "HSID": "...",
    "OSID": "...",
    "SSID": "...",
    "APISID": "...",
    "SAPISID": "...",
    "__Secure-1PSIDTS": "optional"
  }
}
```

This endpoint requires the `Content-Type` value to be exactly `application/json`,
caps the body at 64 KiB, rejects unknown top-level fields and trailing JSON, and
returns 202 with an empty body after accepting the handoff. A wrong, expired, used,
or inactive ticket returns 403. Missing required cookies, empty required values, or
any allowlisted cookie over 8192 bytes returns 400. Unknown cookie-map entries are
discarded. The ticket is cleared when a valid handoff transitions to `connecting`;
it is never accepted again.

The helper does not receive the bearer token. It validates the bridge as an origin
with no userinfo, non-root path, query, or fragment; requires HTTPS except for
loopback or a literal Tailscale `100.64.0.0/10` HTTP address; requests optional host
permissions only on the explicit Connect click; reads only the seven cookie names
shown above; uses `redirect: "error"`; and keeps inputs and cookie values in memory
only. The required six-cookie set follows the pinned libgm login contract.

Starting re-pairing serializes mutation admission with the session change and
immediately cancels all still-queued outbox operations; attempted records retain
their existing terminal/ambiguous state. Cancel or failure does not roll those
cancellations back, but it also does not change the entity epoch, provider upload
descriptors, or history jobs. Only successfully saving the new paired session clears
provider upload descriptors, pauses and resets existing history jobs, and advances
the entity epoch. Previously stored entities remain readable, but conversations and
messages cannot be mutation targets until the new session observes them again.

## Durable operations and idempotency

Conversation, message, and reaction requests require an `Idempotency-Key` header of
16–128 ASCII letters, digits, hyphens, or underscores. The key belongs to this bridge
database across every client. A new record returns 202. Repeating the same normalized
request returns the existing record with 200; changing the request under the same key
returns 409. Reusing a different key authorizes a distinct operation.

### Message

```json
{
  "conversation_id": "stored-conversation-id",
  "text": "Message text may be empty when attachments are present",
  "attachment_ids": ["opaque-upload-id"]
}
```

`conversation_id` is required and limited to 256 bytes. Text must be valid UTF-8 and
at most 16,000 bytes. A request needs nonblank text or at least one attachment. It may
reference at most ten distinct stored upload IDs, totaling at most 20 MiB.

### Conversation creation

```json
{ "recipients": ["+14155550100", "+442071838750"] }
```

One to twenty unique E.164 phone numbers are required. Recipients are sorted before
idempotency comparison. Conversation creation sends no message. If Google first
responds `CREATE_RCS`, the provider performs one explicit second confirmation with
`CreateRCSGroup=true` and an empty group name, within the same durable attempt. Any
transport error or uncertain response is `ambiguous`; neither request is replayed
automatically.

### Reaction

```json
{ "message_id": "stored-message-id", "emoji": "👍", "remove": false }
```

The message must be a current, non-deleted record in the path conversation. Emoji is
nonblank valid UTF-8 and at most 64 bytes. `remove:true` requests removal. Reactions
use the durable outbox and the same uncertainty rules as messages.

### Typing and mark-read

Typing has no body or idempotency key. It is direct, connected-only, and locally
rate-limited to one provider update per conversation every four seconds. Mark-read
uses `{"message_id":"..."}` and validates that the current message belongs to the
path conversation. These calls are not durable and are not automatically retried.

Mutation admission for queueing, outbox claiming, typing, and mark-read is serialized
with pairing start. Once pairing begins, those operations fail with 503 instead of
crossing the old/new-session boundary.

## Outbox states

| State       | Meaning                                                                                               |
| ----------- | ----------------------------------------------------------------------------------------------------- |
| `queued`    | Persisted locally; no provider attempt has been claimed; cancellation remains possible                |
| `sending`   | The attempt was recorded durably before issuing the provider mutation                                 |
| `accepted`  | Google accepted the operation; this is not a message delivery receipt                                 |
| `rejected`  | The provider explicitly refused the operation or outgoing configuration was unusable                  |
| `ambiguous` | The attempt outcome is unknown, including interruption or a lost response; never automatically resent |
| `confirmed` | A sent message with matching transaction and conversation IDs was observed in history                 |
| `canceled`  | Canceled locally before an attempt started, including cancellation at re-pair start                   |

Queuing works while disconnected or under `serve --offline`. Read-only recipient/SIM
preparation can be retried while the record remains queued. Media upload is also
preflight: a successful descriptor is saved privately, but an uncertain upload that
did not produce a saved descriptor can be attempted again. The message mutation is
issued once after all descriptors exist and the outbox transitions to `sending`.

An HTTP response can be lost after a local queue transaction commits. Retry the same
key and body, or fetch that key. Do not turn an `ambiguous` operation into a new key
without inspecting the phone. Text or timestamp similarity never confirms a send.

## History jobs

Queue exactly one of:

```json
{ "folder": "inbox" }
```

```json
{ "conversation_id": "stored-conversation-id" }
```

Folders are `inbox`, `archive`, and `spam`. A folder job pages conversation snapshots
and queues a message-history job for each observed conversation. Cursors are encrypted
private provider data, scoped to the job kind plus folder or conversation. The API
never exposes them.

Jobs have `queued`, `paused`, `failed`, or `complete` state plus page/record counters.
Submitting a paused or failed job without `restart` resumes from its checkpoint.
`{"restart":true}` clears its counters, cursor, and seen-page set. Pause prevents an
in-flight page from advancing the checkpoint; resume safely re-reads that page.

Transient phone/provider errors leave the cursor unchanged and set `retry_at` 30
seconds later. Invalid cursors and conversation-list responses that contain only an
unsupported cursor byte field fail with an explicit unsupported/invalid detail while
preserving the checkpoint. A repeated opaque cursor also fails instead of retrying
forever. A `complete` job only means the provider returned no next mapped cursor; it
does not guarantee that Google exposed every historical entity or attachment.

Recent reconciliation is separate: it checks up to 30 inbox conversations and 50
messages per conversation after recovery, every five minutes, and on explicit sync.

## Snapshots, events, and SSE

Durable events contain `id`, `type`, `entity_id`, `time`, and `data`. Apply message,
conversation, upload, history, and outbox snapshots by entity ID rather than treating
every event as a new object. A snapshot response and its cursor come from one database
read transaction. Replay events strictly after that cursor. When combining snapshots,
track each cursor or re-fetch affected views after replay.

Message pagination is over locally stored snapshots. `next_before` is the last record
of the returned page when more local records exist and is empty at the end. Supplying
an unknown `before` message returns 400.

`Last-Event-ID` takes precedence over the SSE `after` query parameter. Cursors ahead
of the current database return 400; a restored backup may require a full snapshot
reload. These cursors are local event IDs, not Google history cursors.

Typing events have no durable ID. Their data contains `schema`, `conversation_id`,
`participant_id`, and `active`. Discard them after five seconds and do not advance a
durable cursor. Use fetch streaming to provide the bearer header; native EventSource
cannot supply it.

Legacy protobuf events are sanitized into schema 1 before replay. Provider media IDs,
decryption keys, cookies, pairing state, and history cursors are not public schema.
Attachments use opaque bridge IDs and download as files; clients must not render
arbitrary attachment HTML in the bridge origin.
