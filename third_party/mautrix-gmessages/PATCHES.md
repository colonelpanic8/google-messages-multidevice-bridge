# Local libgm patches

This directory is a copy of `pkg/libgm` from the upstream module recorded in
`UPSTREAM`, wired in through a `replace` directive in the root `go.mod`. The
license is unchanged (AGPL-3.0-or-later, see `LICENSE`). `verify.sh` regenerates
`local-changes.patch` from the module cache so the delta stays reviewable.

## Why a local copy

The bridge does not intentionally repeat a send dispatch whose outcome is
unknown; this says nothing about transport-level behavior or Google's delivery
semantics. Upstream's HTTP layer re-POSTs any non-long-poll request up to three
times on a 5xx and Go's HTTP client replays a POST body on 307/308. Neither can
be disabled from outside the package.

## Changes

1. `http.go`, `session_handler.go`: `SendMessageParams.NoRetry` and a
   `noRetry` argument on `makeProtobufHTTPRequestContext`. `sendUserMessage`
   (SendMessage, SendReaction, DeleteMessage, UpdateConversation,
   GetFullSizeImage, GetOrCreateConversation) sets it, preventing libgm's 5xx
   retry loop. Regression tests drive the public mutation paths and observe one
   POST on a local 500 response.
2. `client.go`: `CheckRedirect` refuses redirects for non-GET requests and
   returns the 3xx response, which the caller reports as an error. The public
   send-path test also verifies that a 307 target is not reached. These guards
   prevent the known library-controlled replays; they are not an exactly-once
   delivery guarantee.
3. `crypto/aesctr.go`, `client.go`, `session_handler.go`, `media.go`,
   `longpoll.go`, `event_handler.go`, `pair.go`, `pair_google.go`: AES/HMAC key
   replacement, encryption, decryption, and cipher JSON marshaling use the
   cipher's narrow lock. `FinishGaiaPairing` changes both keys and `PairingID`
   while holding `AuthData.CookiesLock`, after `StartGaiaPairing` has already
   started the poll. Mutable auth tokens, device pointers, and auth UUIDs use
   the same outer lock. The bridge holds its read lock while marshaling
   `AuthData`; lock order is outer auth lock, then cipher lock.
4. `media.go`: `DownloadMediaContext`, `UploadMediaContext`,
   `StartUploadMediaContext`, and `FinalizeUploadMediaContext` bind caller
   cancellation to their HTTP requests, including the wait for response
   headers. The old methods remain as background-context compatibility
   wrappers. Final upload response parsing is capped at 1 MiB.
   `DownloadMediaContext` rejects non-2xx responses instead of handing an
   error body to the decrypting stream. The provider bounds downloaded
   plaintext, closes it once on cancellation, and joins its watcher.
5. `client.go`, `longpoll.go`, `session_handler.go`, `pair.go`, and
   `pair_google.go`: each active client lifecycle owns the poll loop, ack
   ticker, pinger and its response/recovery workers, poll timeout watchers,
   catch-up requests, post-connect work, and reconnect-after-pair work.
   `Disconnect` first closes callback admission and cancels the lifecycle,
   then closes the poll, fails response waiters, and joins all owned workers.
   ACK HTTP attempts have a 15-second upper bound in addition to lifecycle
   cancellation. Connect and Disconnect are serialized and a client can be
   reused after Disconnect. QR registration gained context-aware methods so
   cancellation also covers the request before the pairing poll starts.
6. `client.go`, `longpoll.go`, `event_handler.go`, and `methods.go`: poll
   generation/connection state, disconnect state, skip counts, event handlers,
   update deduplication, and first-conversation-list selection are synchronized.
   `SetEventHandlerWithError` is an opt-in handler whose successful return
   admits the incoming event ACK; an error leaves the RPC unacknowledged for
   redelivery. The original `SetEventHandler(func(any))` remains compatible and
   treats normal handler return as acceptance.

## Accepted upstream limitations

- `SetEventHandlerWithError` narrows the ACK/persistence gap, but it does not
  provide exactly-once processing. Google may redeliver an unacknowledged RPC;
  successful earlier items in a multi-item RPC are skipped by the in-memory
  deduplication window while later rejected items are retried. That window is
  neither durable nor a delivery contract. Malformed or undecryptable events
  are deliberately left unacknowledged and may redeliver indefinitely.
- ACKs are held only in memory, retries are capped at 1024 queued IDs, and a
  process crash can lose the queue. Response ACKs are admitted independently
  of application persistence. The no-retry mutation guards prevent known
  library-controlled replays, not network ambiguity or duplicate processing by
  Google or the phone.
- `Disconnect` joins client-owned background work and therefore guarantees no
  callback from those workers after it returns. Foreground calls are owned by
  their callers: in particular the `DoGaiaPairing` emoji callback and explicit
  request/media methods are not joined by `Disconnect`, so callers must cancel
  and join them. The bridge pairing supervisor does this before tearing down
  the client. A synchronous event handler must not call `Disconnect` itself,
  because it is running inside the worker that `Disconnect` joins.
- Context-free compatibility wrappers (`DownloadMedia`, `UploadMedia`, the
  split upload helpers, and the legacy QR relay helpers) intentionally use
  `context.Background`. Call the `Context` variants when cancellation is
  required. A custom `RoundTripper` that ignores request cancellation can still
  delay an owned worker and therefore delay `Disconnect`.
- The lifecycle is derived from the first Connect or pairing call. After that
  context is canceled, call `Disconnect` before starting the client's next
  logical lifecycle.
