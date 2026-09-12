# 0002: Durable history and send boundaries

Status: accepted for the first usable slice; live behavior remains unverified.

## Decision

Keep one Go service, one locked bbolt database, and one Google Messages session.
Browsers talk only to the bridge's authenticated API. Provider protobufs and
private media credentials are mapped into project-owned records before they
reach clients. Retain the protocol dependency as a small local source copy with
an explicit upstream revision and reviewable patches where its behavior conflicts
with bridge invariants. A separate process would not itself fix request retries
or concurrent auth access inside that dependency.

Commit incoming durable snapshots synchronously at the provider callback boundary.
This applies backpressure through storage instead of accumulating an ingestion
queue that can overflow. Ephemeral typing uses bounded per-client queues; durable
SSE notifications are coalesced hints, with the database as the replay source.
Google's ACK scheduling still has a residual window before local commit.

Each snapshot transaction updates the latest entity, event log, entity version,
private attachment metadata, and any matching outbox observation together. A
Google transaction ID indexes the corresponding local send. We never infer a
send's identity from matching message text or timestamps.

The outbox separates read-only preparation from the mutating send attempt. A
queued record may be canceled while preparation runs. An atomic claim by ID must
still find it queued before committing `sending`; only then may the provider
send method execute. The outbox is not reclaimed automatically after an attempt.
An interrupted attempt becomes `ambiguous`, and a matching Google message may
subsequently resolve it. Acceptance, observation, and delivery are different facts.
Known preflight validation failures and explicit phone failures are rejected;
uncertain transport outcomes remain ambiguous.

Disable upstream automatic 5xx re-POSTs for user mutations and refuse redirects
that could replay their bodies. Test the actual send path through a local fake
transport. These measures constrain bridge dispatch, not Google's internal
processing or eventual delivery semantics.

## History and client synchronization

Reconcile a bounded recent window after connection recovery and periodically.
Message receipts and reactions can change without changing the conversation
summary, so periodic checks must not rely solely on changed summaries. Targeted
checks can supplement the periodic scan after a send or explicit read action.
This is a practical repair mechanism for recent gaps, not complete archival sync.

Capture a local observation watermark before each history request. It advances
even when an identical live snapshot is suppressed from the public event log.
Do not let a history response overwrite a snapshot observed after that watermark. Original
message timestamps cannot order subsequent receipt or reaction updates. Missing entities in a
bounded response do not imply deletion.

Serve latest records and their cursor from the same read transaction. Clients
replay subsequent durable events or re-fetch their affected views. Legacy events
are normalized before replay; neither old nor new API responses expose provider
attachment keys. Download bytes are bounded and cached encrypted, and are served
as files rather than executable content in the bridge origin.

## Lifecycle and validation

Prepare and migrate storage before HTTP starts accepting requests. A failed or
expired phone session should leave local history readable. Failures while persisting
incoming history or send outcomes stop the service. Shutdown cancels operations,
closes callback admission, joins owned
workers and HTTP handlers, and only then releases the database. Remaining upstream
lifecycle limitations must be listed precisely in the local patch notes; passing
fake-provider race tests is not evidence that every live upstream path is safe.

Use fake providers and local HTTP servers to exercise duplicate submissions,
cancellation during preparation, crash recovery, uncertain send responses, stale
history responses, encrypted record isolation, and multi-client replay. Browser
checks use synthetic data. Phone pairing and real sends are a separate validation
step requiring user participation and explicit send authorization.
