# HTTP API, schema 1

The bridge serves its web client at `/`. All `/v1/` routes require
`Authorization: Bearer <token>`; there is no cookie authentication or CORS access.
Mutation JSON must use `Content-Type: application/json`; unknown fields, trailing
JSON values, and bodies over 32 KiB are rejected. Responses are not cached.

The canonical record definitions are in [`internal/model/model.go`](../internal/model/model.go).
They contain `schema: 1`, use snake_case keys, and represent times as UTC RFC 3339
strings. Message time is the phone's original timestamp; event time is local
commit time. Status strings preserve normalized Google status names, such as
`outgoing_displayed`; unrecognized values must remain displayable.

| Method and route | Behavior |
| --- | --- |
| `GET /v1/status` | Connection and recent-reconciliation status |
| `GET /v1/conversations` | Latest stored conversations, newest first, and snapshot `cursor` |
| `GET /v1/conversations/{id}/messages?limit=100&before={message_id}` | Latest stored message snapshots, newest first; `next_before` is empty at the end of locally stored history; limit 1–500 |
| `GET /v1/outbox` | Durable send records and snapshot `cursor` |
| `GET /v1/outbox/{idempotency_key}` | One durable send record |
| `POST /v1/messages` | Queue a text message for an existing stored conversation |
| `POST /v1/outbox/{idempotency_key}/cancel` | Cancel only while still queued; 409 after an attempt starts |
| `POST /v1/conversations/{id}/read` | Explicit read receipt; body `{"message_id":"..."}` |
| `POST /v1/sync` | Request a bounded recent-history reconciliation; requires connected phone |
| `GET /v1/attachments/{id}` | Authenticated download of a stored attachment reference, cached encrypted after download; at most 20 MiB |
| `GET /v1/events?after=0&limit=100` | Durable events and `next_cursor`; limit 1–1000 |
| `GET /v1/stream?after=0` | Durable event replay followed by SSE notifications and ephemeral typing |

## Send requests

A request uses an `Idempotency-Key` header: 16–128 ASCII letters, digits, hyphens,
or underscores. The key belongs to this bridge database, across every client.
The JSON body is:

```json
{"conversation_id":"stored-conversation-id","text":"Message text"}
```

Text must be nonblank UTF-8 and at most 16,000 bytes. A new record returns 202;
repeating the same key and body returns the existing record with 200. A different
body under that key returns 409, even after the first send completed. Reusing
another key authorizes another send. Keys are retained with the outbox.

Queuing works offline. A queued record remains eligible when the phone reconnects
or the service restarts, unless canceled first. Read-only recipient/SIM preparation
can be retried while queued. A transient failure keeps later sends in that
conversation behind it. There is no automatic expiry.

| Outbox state | Meaning |
| --- | --- |
| `queued` | Persisted locally; no send attempt started; cancellation remains possible |
| `sending` | Attempt recorded durably before issuing the provider send |
| `accepted` | Google accepted the request; not a delivery receipt |
| `rejected` | Provider explicitly rejected the request or outgoing configuration was unusable |
| `ambiguous` | An attempt's outcome is unknown, including interruption or a lost response; never automatically resent |
| `confirmed` | A Google message with matching transaction and conversation IDs was observed; consult the message's status for delivery |
| `canceled` | Canceled locally before an attempt started |

An HTTP response can be lost after a queue transaction commits. Retry **the same
key and body**, or look up that key. An ambiguous provider send is different:
inspect the phone before deliberately authorizing another message. Matching text
or timestamps alone never resolve an ambiguous send.

## Synchronization

Durable events contain `id`, `type`, `entity_id`, `time`, and `data`. Apply message,
conversation, and outbox snapshots by entity ID, rather than treating every event
as a new message. A snapshot response and its cursor come from one database read
transaction. Replay events strictly after that cursor. When combining multiple
snapshot queries, track their cursors separately or re-fetch the views on replay.

SSE `Last-Event-ID` takes precedence over the `after` query parameter. Cursors ahead
of the current database return 400; a restored backup may require resynchronizing
clients. A cursor is local to this database, not a Google event position.

Typing events have no durable ID. Their data contains `schema`, `conversation_id`,
`participant_id`, and `active`. Discard them after five seconds and do not advance a
durable cursor. Incoming receipt/reaction changes arrive as message snapshots.
Use fetch streaming to supply the bearer header; native EventSource does not.

The server sanitizes legacy protobuf events into schema 1 before replay. Provider
media IDs, decryption keys, cookies, and pairing state are not part of the public
schema. Attachments use opaque bridge IDs and download as files; clients must not
render arbitrary attachment HTML in the bridge origin.
