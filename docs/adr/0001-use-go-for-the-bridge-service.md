# 0001: Use Go for the bridge service

Status: accepted

## Context

The first connector speaks to Google Messages through
`go.mau.fi/mautrix-gmessages/pkg/libgm`. `libgm` implements account pairing,
session refresh, the long-polling connection, protobuf messages, media requests,
and live events such as messages, receipts, reactions, and typing state.

`libgm` is an evolving internal package rather than a stable network service or
language-neutral SDK. Using it from Rust would require one of these boundaries:

- reimplementing Google's private Messages for Web protocol and tracking changes;
- exposing Go through C FFI, including callbacks and Go runtime lifecycle; or
- running a separate Go connector and defining an IPC protocol before the product
  needs more than one process.

Each option increases the amount of connection and recovery behavior we own.

## Decision

Use Go for the bridge service and its initial connectors. Keep provider-specific
code behind an internal connector interface so the durable store and client API do
not depend directly on Google protobuf types over time.

Client applications may use a different language. A web client will likely use
TypeScript. A native client can use Rust if that is the best fit for its platform.

## Consequences

- We can consume and patch `libgm` directly and stay close to upstream changes.
- The first deployable can remain one static service binary with no IPC boundary.
- The project inherits `mautrix-gmessages`' AGPL-3.0-or-later licensing obligations.
- The public API and stored records need project-owned schemas before they can be
  considered stable; raw upstream protobuf JSON is only a prototype format.
- If connector isolation later becomes valuable, we can introduce a versioned IPC
  protocol then, based on observed requirements rather than guesses.
