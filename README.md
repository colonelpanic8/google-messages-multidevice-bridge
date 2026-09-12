# Google Messages Multi-Device Bridge

[![CI](https://github.com/colonelpanic8/google-messages-multidevice-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/colonelpanic8/google-messages-multidevice-bridge/actions/workflows/ci.yml)

One always-on Google Messages connection, available to all your devices.

Google Messages Multi-Device Bridge is a personal messaging service built on
[`mautrix-gmessages/pkg/libgm`](https://github.com/mautrix/gmessages/tree/main/pkg/libgm).
It does not run a Matrix server or require a Matrix account. The libgm source is
pinned to `e6cc29974f92` and carried in `third_party/mautrix-gmessages` with narrow
reliability patches; see its [patch notes](third_party/mautrix-gmessages/PATCHES.md).

## Current milestone: shared history and text sending

- Google account pairing with the phone's emoji confirmation.
- Encrypted pairing credentials, message snapshots, outbox, and downloaded attachments.
- A responsive web client with shared conversations, message history, receipt/reaction
  display, live typing, explicit mark-read, text sending, and queued-send cancellation.
- An authenticated project-owned HTTP API and resumable SSE for simultaneous clients.
- Bounded recent-history reconciliation on connection recovery and periodically.
- Durable idempotency keys and explicit ambiguous-send handling across crashes.

This is an unverified live integration, not yet a replacement for Google Messages.
Tests and browser checks use synthetic data. Real pairing, SIM selection, Google
reconnection, delivery, receipt/reaction behavior, and media downloads still need
phone validation. No real messages were sent during implementation.

The first sending flow replies to **existing stored conversations**. Creating new
conversations, outgoing media, outgoing reactions/typing, full historical import,
and unattended deployment are not implemented. Incoming MMS/RCS attachment
metadata is displayed; supported attachments can be downloaded on demand, up to
20 MiB, and cached encrypted. Unavailable phone-side media remains unavailable.

Your phone must remain online for new traffic. Stored history remains available
without it. Upstream Google session/authentication changes can require pairing again.

## Development

Requires Go 1.26.7 or newer. Dependencies are pinned in `go.mod` and `go.sum`.
The Nix flake provides the matching Go toolchain and all development tools. With
direnv installed, allow the environment once; otherwise enter it directly:

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
package and flake outputs. The tests use synthetic provider events and local HTTP
servers; they never connect to Google or send messages. See
[`docs/adr/0001-use-go-for-the-bridge-service.md`](docs/adr/0001-use-go-for-the-bridge-service.md)
for the language decision.

## Secrets

Provide these at runtime:

- `GOOGLE_MESSAGES_MULTIDEVICE_BRIDGE_STORAGE_KEY`: base64 encoding of 32 random bytes. Keep it permanently
  in a password manager; it is needed to reopen the database.
- `GOOGLE_MESSAGES_MULTIDEVICE_BRIDGE_API_TOKEN`: a random bearer token of at least 32 characters.

Neither secret belongs in this repository, a command-line flag, or a plaintext
environment file. On this machine use `pass` and inject secrets through environment
variables. Changing the storage key is not a rotation operation: the old database
will refuse to open with a different key.

Database values use AES-256-GCM with fresh nonces and record-specific associated
data. Record identifiers, sizes, and event counts are not encrypted. The database
file is created mode 0600; protect backups along with the key. The API intentionally
does not return stored Google authentication data. Upstream logging is disabled
because it can include message bodies and credentials.

## Pair

Follow the upstream [Google account login instructions](https://docs.mau.fi/bridges/go/gmessages/authentication.html)
to obtain a JSON object mapping Google cookie names to values from a separate browser
session. Account pairing must be enabled in Google Messages. A Google password is
not a substitute for these cookies. Google Messages Multi-Device Bridge accepts the JSON object, not a cURL
command. Pipe it from a secure source:

```sh
your-secure-cookie-source | bin/google-messages-multidevice-bridge pair --db data/google-messages-multidevice-bridge.db
```

Select the displayed emoji on the phone. Pairing saves the encrypted session and
exits; it does not send any messages. Stop this project's server before re-pairing:
the database lock prevents two processes from using the same session/store.

## Serve

```sh
bin/google-messages-multidevice-bridge serve --db data/google-messages-multidevice-bridge.db
```

The default address is `127.0.0.1:0`: the OS picks an unused port and the service
prints the actual address. `--listen` selects a specific address and fails if it
is occupied. For access from other devices, bind to an appropriate interface and
use a private encrypted network such as Tailscale, or put TLS authentication in
front of the service. Plain HTTP bearer tokens should not traverse an untrusted
network. No existing server is stopped or reused.

Missing or expired pairing and provider failures leave the API and stored history
available. Check the displayed status, then pair again or restart the connection
as indicated.

`serve --offline` serves already persisted history without a Google connection.
It can also start against a fresh database to test the HTTP API before pairing.

## Web client and API

Open the service address in a browser and paste the API token to unlock it. The
client keeps the token in memory for that tab; locking or reloading requires
unlocking again. Each computer uses the same service, history, and outbox.
The web assets are public; every `/v1/` endpoint requires bearer authentication.
There are no external fonts, scripts, CDNs, or analytics.

Choose a conversation to read and reply. When the phone is offline, a send is
queued durably until the connection returns; cancel it before sending starts if
you change your mind. `accepted` means Google accepted the request, and
`confirmed` means its message was observed in history. Neither substitutes for
the message's delivery/read status. `ambiguous` means the outcome is unknown:
inspect the phone before deliberately sending another message. The bridge never
automatically retries an attempted ambiguous send.

If the browser loses the HTTP response, **Retry same request** preserves the
idempotency key. That retries submission to the local outbox, not an uncertain
Google send. Do not create another request merely because the first response was
lost. Incoming typing expires after five seconds. Mark read is an explicit button;
opening a conversation does not itself send a read receipt.

See [the schema 1 API contract](docs/api.md) for routes, send states, pagination,
SSE replay, and snapshot cursor semantics. The record definitions live in
[`internal/model/model.go`](internal/model/model.go). Legacy prototype events are
normalized before replay, excluding provider attachment credentials.

## Reliability boundaries

- Reconciliation covers a recent window: up to 30 inbox conversations and 50
  recent messages per conversation. Archive/spam folders and older gaps are not
  comprehensively imported. A completed recent check is not an archival guarantee.
- libgm can acknowledge upstream events before the bridge commits them locally.
  Reconciliation reduces gaps but does not establish exactly-once delivery or a
  complete archive of the phone.
- A history response cannot overwrite an entity observed locally after that fetch
  began, including identical live snapshots suppressed from the event log. Google
  supplies no reliable revision for every snapshot; original message
  timestamps cannot order subsequent receipt/reaction changes.
- An outbox attempt is durable before a send call. Interrupted attempts become
  ambiguous on startup. Matching transaction and conversation IDs can resolve them;
  matching text or timestamps cannot. Google acceptance is distinct from delivery.
- A cursor resumes this database's event log, not a Google-side event position.
  Restoring an older backup can require clients to reload their initial state.
- Message deletion snapshots do not erase older event versions or cached media.
  This is an encrypted local archive, not a remote-deletion mirror.
- The connection status separates transport and phone responsiveness from history
  reconciliation. It cannot guarantee the next message will arrive.
- Upstream diagnostics are disabled because they may contain private message data
  or credentials. Synthetic race tests cannot prove untested live protocol behavior.
- Some upstream poll/reconnect helpers remain unjoined and have unsynchronized
  fields. The bridge gates callbacks after shutdown; this does not make all
  upstream live paths race-free. See the precise [remaining limits](third_party/mautrix-gmessages/PATCHES.md).
- Storage keys are not rotatable by replacing the environment value. Retain the key
  and pinned source revision with protected backups. Database identifiers, sizes,
  and event counts remain visible even though payloads are encrypted.

## Synthetic browser fixture

For development only, a separate opt-in test starts a server with fake
conversations and no Google connection:

```sh
BRIDGE_BROWSER_TEST=1 go test ./internal/api -run '^TestBrowserFixture$' -v -timeout 30m
```

It binds all interfaces on a fresh OS-selected port and prints the port. The token
is `synthetic-browser-test-token-only`; this fixture contains no real secrets or
messages. Text sends only enter its temporary local queue. Normal tests skip it.

## License

AGPL-3.0-or-later. This project depends on mautrix-gmessages; retain its source and
license notices with distributed builds.
