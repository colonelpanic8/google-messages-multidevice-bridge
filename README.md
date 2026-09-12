# Multiconnect Bridge

[![CI](https://github.com/colonelpanic8/multiconnect-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/colonelpanic8/multiconnect-bridge/actions/workflows/ci.yml)

One always-on Google Messages connection, available to all your devices.

Multiconnect Bridge is a personal messaging service built on
[`mautrix-gmessages/pkg/libgm`](https://github.com/mautrix/gmessages/tree/main/pkg/libgm).
It does not run a Matrix server or require a Matrix account.

## Current milestone: receiving bridge

- Google account pairing with the phone's emoji confirmation.
- Encrypted storage for pairing credentials and received message/conversation snapshots.
- Authenticated HTTP history API with persistent, increasing cursors.
- Server-sent events (SSE) with replay and live updates for multiple clients.
- Live typing notifications, kept out of persistent history.
- Consecutive identical snapshots are suppressed; changes to the same message are retained.
- Connection status and upstream reconnect handling through libgm.

This is a prototype, not yet a replacement for Google Messages. Sending, historical
backfill/reconciliation, downloading attachment bytes, a client UI, and unattended
deployment are next steps. Incoming attachment metadata, reactions, and receipts
are retained where present in upstream message snapshots. Their client rendering
is not implemented. Live Google pairing and phone behavior have not yet been tested.

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

- `MULTICONNECT_BRIDGE_STORAGE_KEY`: base64 encoding of 32 random bytes. Keep it permanently
  in a password manager; it is needed to reopen the database.
- `MULTICONNECT_BRIDGE_API_TOKEN`: a random bearer token of at least 32 characters.

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
not a substitute for these cookies. Multiconnect Bridge accepts the JSON object, not a cURL
command. Pipe it from a secure source:

```sh
your-secure-cookie-source | bin/multiconnect-bridge pair --db data/multiconnect-bridge.db
```

Select the displayed emoji on the phone. Pairing saves the encrypted session and
exits; it does not send any messages. Stop this project's server before re-pairing:
the database lock prevents two processes from using the same session/store.

## Serve

```sh
bin/multiconnect-bridge serve --db data/multiconnect-bridge.db
```

The default address is `127.0.0.1:0`: the OS picks an unused port and the service
prints the actual address. `--listen` selects a specific address and fails if it
is occupied. For access from other devices, bind to an appropriate interface and
use a private encrypted network such as Tailscale, or put TLS authentication in
front of the service. Plain HTTP bearer tokens should not traverse an untrusted
network. No existing server is stopped or reused.

`serve --offline` serves already persisted history without a Google connection.
It can also start against a fresh database to test the HTTP API before pairing.

## API

Every endpoint requires `Authorization: Bearer <token>`. Tokens in query strings
are rejected. No CORS access is enabled. Responses use `Cache-Control: no-store`.

| Endpoint | Result |
| --- | --- |
| `GET /v1/status` | Current provider state and when it changed |
| `GET /v1/events?after=0&limit=100` | Durable snapshots and `next_cursor`; limit 1–1000 |
| `GET /v1/stream?after=0` | Replay followed by live SSE events |

Durable events contain `id`, `type`, `entity_id`, `time`, and `data`. `data` currently
uses the pinned upstream protobuf JSON schema; it is not a stable application API.
`time` is ingestion time, not the original message timestamp. Clients should apply
snapshots by entity ID, rather than displaying every update as a new message.

SSE `message` and `conversation` events have durable IDs. Reconnect using
`Last-Event-ID` (which takes precedence over `after`). The last ID is exclusive.
Typing events have no SSE ID, are not stored or replayed, and expire from the live
queue after five seconds. Clients should also expire their displayed typing state.
Use fetch-based streaming in a browser to provide the Authorization header; native
EventSource does not support custom headers.

Slow clients cannot block ingestion. Durable notifications are coalesced and read
from disk; clients overwhelmed by ephemeral events are disconnected and can resume
from their last durable ID. Upstream ingestion uses a bounded queue and exits with
an error on overflow instead of pretending history is complete.

## Reliability boundaries

- There is no full-history import or gap reconciliation yet. This cannot be used
  as a complete archive of the phone.
- libgm may acknowledge upstream events before Multiconnect Bridge commits them to disk.
  A crash or overflow can therefore leave a gap; backfill/reconciliation is needed
  before relying on the service for archival completeness.
- A cursor resumes events in this database, not a Google-side event position.
- Message deletion snapshots do not erase prior versions from this archive.
- The status endpoint is a coarse connection indicator, not a delivery guarantee.
- The storage format and upstream JSON API are experimental; retain the pinned
  source revision with backups until migrations are implemented.

## Next milestones

1. Verify pairing, reconnection, typing, and receipts against a real phone.
2. Import recent history and reconcile gaps after reconnects.
3. Add text sending with persistent idempotency keys and explicit ambiguous-send
   states; never blindly retry a send whose acknowledgement was lost.
4. Add attachment transfer and a focused web client.

## License

AGPL-3.0-or-later. This project depends on mautrix-gmessages; its source and license
are included by reference through the pinned Go module dependency.
