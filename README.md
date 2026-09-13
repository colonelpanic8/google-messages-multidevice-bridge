# Google Messages Multi-Device Bridge

[![CI](https://github.com/colonelpanic8/google-messages-multidevice-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/colonelpanic8/google-messages-multidevice-bridge/actions/workflows/ci.yml)

One always-on Google Messages connection, available to all your devices.

Google Messages Multi-Device Bridge is a personal messaging service built on
[`mautrix-gmessages/pkg/libgm`](https://github.com/mautrix/gmessages/tree/main/pkg/libgm).
It does not run a Matrix server or require a Matrix account. The libgm source is
pinned to `e6cc29974f92` and carried in `third_party/mautrix-gmessages` with narrow
reliability patches; see its [patch notes](third_party/mautrix-gmessages/PATCHES.md).

## Current milestone

- Guided pairing and re-pairing from the web app using a bundled Chromium helper.
  The user does not export cookies or give the helper an API bearer token.
- Encrypted pairing state, provider-independent message snapshots, durable outbox,
  local uploads, downloaded attachments, and private media descriptors.
- Text and attachment sends, new conversations, reaction updates, outgoing typing,
  explicit mark-read, queued-operation cancellation, and incoming typing/reactions.
- Resumable history jobs for inbox, archive, spam, and each discovered conversation,
  in addition to bounded recent-history reconciliation.
- An authenticated schema-1 API, resumable SSE, a multi-client web app, and a
  connection supervisor that retains the local service while reconnecting.
- An installable app: a web app manifest, icons, and a service worker that caches
  the static shell and delivers Web Push notifications while no window is open.
- A Nix package and Home Manager user service with direct `pass` integration.

This is a **live-verified integration on one phone**, not yet a replacement for
Google Messages. Verified against a paired Android phone on 2026-09-12: guided
Google sign-in, helper credential handoff, emoji confirmation, paired-session
persistence across restarts, initial history sync, inbox history import across
hundreds of conversations, SSE replay, attachment download, full-media requests,
and, in a conversation with the owner's own number over RCS, conversation creation,
text sends, image sends, a caption queued as a second message, a reaction, typing,
and mark-read, each confirmed through Google's history echo and delivery status. A
message composed elsewhere on the phone appeared in a running client within seconds
over the live stream, which is the same event path incoming messages use.
Not yet live-verified: messages received from other parties, SMS/MMS sends to
non-RCS recipients, multi-SIM selection, RCS group creation, and authentication
expiry handling. Structured libgm authentication errors and revoked-session events
are covered by synthetic tests, but have not been induced against the live phone.
Automated tests use fake providers, synthetic protocol messages, and local HTTP
servers. No messages were sent to other people during implementation.

The history importer follows the pages exposed by the current private protocol. A
job marked `complete` means that response stream ended; it is not proof of a complete
phone archive. The bridge does not promise exactly-once processing or sending. Your
phone must remain online for new traffic, while stored history remains readable
without it. Upstream authentication or protocol changes can require pairing again.

## Development

Requires Go 1.26.7 or newer. Dependencies are pinned in `go.mod` and `go.sum`.
The Nix flake provides the matching Go toolchain and development tools:

```sh
direnv allow
# or: nix develop
```

Format, lint, test with the race detector, and build with:

```sh
just check
just build
```

`nix build` produces the packaged service, while `nix flake check` verifies the
package and flake outputs. Tests never connect to Google. See
[`docs/adr/0001-use-go-for-the-bridge-service.md`](docs/adr/0001-use-go-for-the-bridge-service.md)
for the language decision.

## Secrets

Provide two runtime secrets, either through the environment or the corresponding
`pass` entry flags:

- `GOOGLE_MESSAGES_MULTIDEVICE_BRIDGE_STORAGE_KEY`: base64 encoding of exactly
  32 random bytes, or `--storage-key-pass-entry NAME`.
- `GOOGLE_MESSAGES_MULTIDEVICE_BRIDGE_API_TOKEN`: a random bearer token of at least
  32 characters, or `--api-token-pass-entry NAME`.

Neither value belongs in the repository, command-line arguments, or a plaintext
environment file. Replacing the storage key is not key rotation: the existing
database will no longer open. Keep the original key in a backed-up password manager.

Database payloads use AES-256-GCM with fresh nonces and record-specific associated
data. Record identifiers, sizes, and event counts remain visible. The database is
created mode 0600; backups need the same protection as the database and storage key.
Upstream logging is disabled because it can contain credentials and message bodies.

## Serve and pair

Start the bridge before it has a Google session:

```sh
bin/google-messages-multidevice-bridge serve \
  --db data/google-messages-multidevice-bridge.db
```

The default address is `127.0.0.1:0`, so the OS chooses an unused port and the
service prints it. Open that address, enter the API token, and choose **Pair / Re-pair**:

1. Download `/pairing-helper.zip`, extract it, and load that directory from
   `chrome://extensions` using **Developer mode** and **Load unpacked**.
2. Choose **Start pairing** in the bridge. Copy the displayed bridge origin and
   short-lived pairing ticket into the extension's setup tab.
3. Use the helper's sign-in button, complete Google sign-in, return to the setup
   tab, and explicitly choose **Connect this Google account**.
4. Return to the bridge and choose the displayed emoji on the phone.

The ticket is a 43-character, one-use capability scoped to credential handoff and
expires after ten minutes. It is not the API token. The helper requests Google and
bridge host access only on the explicit Connect click, reads only the allowlisted
Google cookies required by libgm, sends them in one JSON handoff, and asks Chromium
to remove the granted host permissions afterward. It persists neither inputs nor
cookies. Bridge URLs must use HTTPS, except loopback or a literal Tailscale
`100.64.0.0/10` address may use HTTP.

Starting a re-pair immediately cancels queued outbox operations. Canceling or failing
that pairing attempt does not advance the entity epoch, clear provider upload
descriptors, or reset history jobs, although the canceled outbox records stay
canceled. Only successfully saving the new paired session advances the epoch, clears
private provider upload, download, and history descriptors, and resets existing
history jobs to paused with no checkpoint. Stored entities from the previous session
remain readable but cannot be mutation targets until the new connection observes
them. Messages from earlier pairings are labeled read-only, and status reports their
conversation/message counts. Locally cached attachment bytes remain available as
stored history. Review recipients and start fresh history imports after successful
pairing. If the bridge restarts mid-pairing, it keeps the last saved session,
invalidates the old ticket, and reports that the attempt was interrupted.

`serve --offline` exposes stored history and permits durable operations to be queued
without starting Google. Pairing is intentionally disabled in this mode and the API
returns 400 for a pairing start. Restart without `--offline` to pair or reconnect.

For a stable service, `pass`-backed flags, Home Manager configuration, network
guidance, and offline-consistent backups, see [deployment](docs/deployment.md).

## Web client and API

The browser keeps the bearer token only in that tab's memory; locking or reloading
requires it again. Image attachments render as inline previews that are discarded on
lock. An opt-in toggle raises browser notifications for new incoming messages in
hidden tabs or unselected conversations; replayed history never notifies. Web assets
and the pairing-helper ZIP are public. Every `/v1/` route requires bearer
authentication except the ticket-only `POST /v1/pairing/credentials` handoff.

The web app can create conversations from E.164 phone numbers, queue text or up to
ten attachments totaling 20 MiB, add reactions, send typing indicators, mark a
conversation read, download available attachments, and manage history jobs. The API
also supports reaction removal. Local upload bytes are encrypted before they are
needed by the provider; provider media descriptors remain private.

Conversation creation may receive Google's `CREATE_RCS` response. In that case the
provider sends exactly one explicit unnamed-group confirmation as the second RPC in
the same durable attempt. A transport error or unclear response makes the attempt
ambiguous; neither RPC is automatically replayed.

When offline, durable message, conversation, and reaction operations remain queued
until a connected provider can prepare them. Typing and mark-read are immediate,
non-durable operations and require a connected phone. `accepted` means Google
accepted a request, not that it was delivered. `confirmed` means a matching sent
message appeared in stored history. `ambiguous` means the outcome is unknown: inspect
the phone before intentionally authorizing another operation. The bridge never
automatically retries an attempted ambiguous mutation.

If an HTTP response is lost while submitting an outbox operation, **Retry same
request** reuses its idempotency key. That safely rechecks the local durable record;
it does not replay a provider attempt. Incoming typing expires after five seconds,
and opening a conversation does not itself send a read receipt.

See [the schema-1 API contract](docs/api.md) for routes, request limits, outbox and
history states, pagination, pairing, and SSE replay.

## Reliability and protocol boundaries

- Recent reconciliation is deliberately bounded to 30 inbox conversations and 50
  recent messages per conversation. Durable history jobs page beyond that window,
  but private-protocol omissions can still prevent a complete archive.
- History cursors are opaque and scoped to the job. Invalid cursors, cursor-bytes-only
  responses unsupported by the mapped protocol, and repeated cursors fail the job
  without looping. Transient phone failures preserve the checkpoint and retry after
  30 seconds.
- A history response cannot overwrite an entity observed locally after that fetch
  began. Original message timestamps cannot order later receipt/reaction changes,
  and absence from a bounded response does not imply deletion.
- Incoming callback ACK admission now follows successful bridge persistence through
  libgm's error-aware handler. This reduces one loss window but is not exactly-once:
  ACK queues and deduplication are memory-only, Google can redeliver, response ACKs
  are independent, and crashes can still lose ACK state.
- The outbox is durable before provider mutation. Interrupted attempts are ambiguous;
  only matching transaction and conversation IDs can confirm a message. Matching text
  or timestamps is insufficient. Media upload is a separate preflight, so an
  uncertain upload can be repeated before the single message-send attempt is claimed.
- Re-pair generation checks prevent a canceled attempt from replacing a newer ticket.
  Mutation admission is serialized so queueing, claiming, mark-read, and typing do
  not cross the start of a re-pair.
- The supervisor retries transient connection failures with bounded exponential
  backoff. Authentication failure is reported as `authentication_required` with a
  machine-readable reason and waits for explicit restart or re-pair. Stored history
  and the HTTP service remain available unless storage itself fails.
- Patched libgm joins client-owned poll, ACK, ping, recovery, and post-connect workers
  on disconnect. Foreground pairing and request/media calls remain caller-owned and
  must be canceled and joined by the bridge. A transport that ignores context can
  still delay shutdown. See [the exact patch limits](third_party/mautrix-gmessages/PATCHES.md).
- A local event cursor is not a Google cursor. Restoring an older backup can require
  clients to reload. Remote deletion snapshots do not erase older event versions or
  cached media from this encrypted local archive.
- Attachment downloads prefer the full upload, then its thumbnail, then the inline
  preview; `preview` marks the latter two. Google serves web clients a compressed
  rendition, so downloaded bytes can be smaller than the declared size and HEIC
  originals arrive as JPEG. A `requestable` attachment can ask the phone to upload
  its full media; the message updates when the phone answers, which older MMS may
  never do.

## Synthetic browser fixture

For development only, an opt-in test starts a server with fake conversations and no
Google connection:

```sh
BRIDGE_BROWSER_TEST=1 go test ./internal/api -run '^TestBrowserFixture$' -v -timeout 30m
```

It binds all interfaces on a fresh OS-selected port and prints the port. The token
is `synthetic-browser-test-token-only`; the fixture contains no real secrets or
messages. Sends only enter its temporary local queue. Normal tests skip it.
Set `BRIDGE_BROWSER_RECOVERY_TEST=1` as well to render an expired-session state
with synthetic previous-pairing records for recovery UI review.

## License

AGPL-3.0-or-later. This project depends on mautrix-gmessages; retain its source and
license notices with distributed builds.
