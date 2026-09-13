# 0002: Durable history and send boundaries

Status: accepted for the implemented milestone; live Google behavior remains
unverified.

## Decision

Keep one Go service, one locked bbolt database, and one active Google Messages
session. Browsers use only the project-owned HTTP API. Provider protobufs, Google
media credentials, and opaque history cursors are mapped into public records or
encrypted private state before they reach clients. Retain libgm as a small pinned
source copy with explicit patches where upstream retry, cancellation, ACK, or
lifecycle behavior conflicts with these boundaries.

The milestone includes guided pairing/re-pairing, text and attachment sends,
conversation creation, reactions, typing, mark-read, bounded reconciliation,
resumable history jobs, and supervised reconnection. These features share session,
mutation, persistence, and uncertainty rules rather than treating each UI action as
an independent provider call.

## Incoming persistence and ACKs

Commit incoming durable snapshots synchronously at the provider callback boundary.
One store transaction updates the latest entity, local event log, entity observation
version, private attachment metadata, and matching outbox observation. Ephemeral
typing bypasses durable storage and uses bounded per-client queues.

The patched client exposes an error-aware event handler. The bridge returns success
only after a handled durable event is persisted, so ACK admission for those incoming
events follows successful local commit. A persistence error leaves the RPC
unacknowledged, marks storage failed, and stops the connection/service rather than
continuing with an untrustworthy store.

This narrows the ACK/persistence gap but does not provide exactly-once ingestion.
Google may redeliver an unacknowledged RPC. ACK queues and update deduplication are
memory-only, the ACK queue is bounded, response ACKs are independent of application
persistence, and a process crash can lose pending ACK state. Malformed or
undecryptable events may redeliver indefinitely. The detailed accepted limits remain
in [`third_party/mautrix-gmessages/PATCHES.md`](../../third_party/mautrix-gmessages/PATCHES.md).

## Durable mutation boundary

Conversation creation, message sends, and reaction updates enter one durable outbox.
An idempotency key identifies the normalized local request. Repeating the same key
and body returns the existing record; changing the body conflicts. A queued record
can be canceled until an atomic claim records `sending`.

Read-only destination/SIM preparation happens before the claim and can retry while
the record stays queued. Attachment source bytes are encrypted locally. Provider
media uploads are preflight operations whose opaque descriptors are encrypted and
persisted before claim; an upload with an uncertain outcome can be repeated if no
descriptor was saved. After claim, the message send itself is one attempt.

Create-conversation is also one durable attempt, although the protocol may require
two intentional RPCs: if the first response is `CREATE_RCS`, issue exactly one second
request with `CreateRCSGroup=true` and an empty group name. This is explicit protocol
confirmation, not automatic transport replay. Any transport failure or unclear
response at either step is ambiguous and is not automatically repeated.

Disable libgm's automatic 5xx re-POSTs on user mutations and refuse redirects that
could replay their bodies. A known provider refusal becomes `rejected`; a transport
error, cancellation after claim, lost response, or uncertain response becomes
`ambiguous`. A matching transaction plus conversation ID may later confirm a sent
message. Matching text or timestamps may not. Acceptance, observation, delivery,
and read status are distinct facts.

Typing and mark-read remain direct, connected-only, non-durable operations. They are
not presented as outbox-guaranteed actions and are not automatically retried.

## Pairing and session epochs

Serve pairing control through the bearer-authenticated API and hand credentials from
the bundled Chromium helper through a separate ticket-only endpoint. The 43-character
ticket is random, one-use, scoped only to credential handoff, and expires after ten
minutes. The endpoint accepts exact JSON up to 64 KiB and never accepts the API bearer
as a substitute. The helper stores no credentials and requires an explicit click
before requesting host permissions or reading the fixed cookie allowlist.

Starting pairing is forbidden in offline mode. Pairing start and mutation admission
share a lock: queueing, outbox claiming, mark-read, and typing cannot cross the
old/new-session boundary. Starting re-pair cancels still-queued operations. Already
attempted records retain their state because silently replaying them against another
phone/session would be unsafe.

Each saved paired session advances a store epoch, clears private provider upload and
download descriptors and history cursors, and pauses/resets every history job.
Previous-session records are retained for explicitly labeled read-only access. A
conversation or message becomes a valid mutation target again only after the new
connection observes it. Outbox and history records retain their owning epoch, so a
new-session event cannot confirm an old-session attempt. Pairing generation checks
prevent a canceled older attempt or callback from replacing a newer ticket or result.
An active-attempt marker contains timestamps but no ticket or credentials; startup
consumes it to report an interrupted attempt while keeping the last saved session.

## History and client synchronization

Keep bounded reconciliation for recent repair: at most 30 inbox conversations and 50
messages per conversation after recovery, periodically, and after relevant actions.
It is not an archival scan.

Add durable paginated history jobs for inbox, archive, spam, and individual
conversations. Folder pages enqueue message jobs for every observed conversation.
Encrypt the provider cursor and bind it to the job scope. Checkpoint page count,
record count, and next cursor atomically. Pausing or a concurrent job generation
change prevents an in-flight response from advancing the checkpoint, so resume can
safely re-read the page. Restart deliberately clears the checkpoint and counters.

Transient phone/provider errors preserve the current cursor and schedule another
attempt after 30 seconds. Invalid cursors and cursor-bytes-only conversation
responses that the mapped protocol cannot interpret transition the job to `failed`
with an explicit unsupported/invalid detail. Repeated opaque cursors also fail. These
rules prevent silent cursor loss and infinite retry without claiming that a terminal
cursor proves a complete archive.

Capture a local observation watermark before every history request. A history result
cannot overwrite an entity observed after that watermark, including an identical live
snapshot suppressed from the public event log. Original message timestamps cannot
order later receipt or reaction changes, and absence from a bounded response never
implies deletion.

Serve latest records and their cursor from one read transaction. Clients replay later
durable events or refresh affected views. Local SSE cursors and encrypted provider
history cursors are separate domains; neither is portable to another database.

## Connection and lifecycle

Keep the HTTP service and stored history available while a supervisor reconnects
transient provider failures with bounded exponential backoff. Authentication failure
is classified from libgm's invalid-credential, missing-registration, and HTTP
401/403/404 errors, is surfaced as an explicit re-pair requirement, and waits for
explicit restart or re-pair. Storage failure is fatal because further serving could
conceal lost commits.

The patched libgm lifecycle owns and joins its poll loop, ACK ticker, pinger,
response/recovery workers, timeout watchers, catch-up requests, post-connect work,
and reconnect-after-pair work. Disconnect closes callback admission, cancels the
lifecycle, fails response waiters, and joins those workers before returning. The
bridge also waits for borrowed provider calls and its send, history, and sync workers.

Foreground pairing and explicit request/media calls remain caller-owned. The bridge
must cancel and join pairing before replacing the client, and every context-aware
call still depends on its transport honoring cancellation. A synchronous event
handler must not call Disconnect from inside a worker that Disconnect joins. Passing
synthetic race tests does not establish that every live Google path is correct.

## Validation boundary

Use fake providers and local HTTP servers to exercise duplicate submissions,
cancellation during preparation, pairing/session changes, crash recovery, uncertain
mutations, media bounds, stale history responses, cursor failures, encrypted record
isolation, callback persistence failures, reconnect supervision, and multi-client
replay. Browser tests use only synthetic records.

Real account pairing, phone emoji confirmation, cookie availability, SIM resolution,
Google protocol statuses, media transfer, folder pagination, receipt/reaction
behavior, and reconnect timing require separate user-authorized validation. Until
that happens, do not describe the bridge as an exactly-once sender, a complete
archive, or a fully validated unattended Google Messages client.
