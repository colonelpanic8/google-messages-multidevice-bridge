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
   GetFullSizeImage) sets it, preventing libgm's 5xx retry loop. A regression
   test drives the public `Client.SendMessage` path and observes one POST on a
   local 500 response.
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
4. `media.go`: `DownloadMediaContext` binds cancellation to the GET before
   response headers arrive; `DownloadMedia` remains as a background-context
   compatibility wrapper. The provider uses the context-aware method, bounds
   the decrypted body, closes it once on cancellation, and joins its watcher.
5. `session_handler.go`, `client.go`: the ack ticker goroutine is stoppable and
   `Disconnect` joins it. Ack-run state and session IDs have narrow locks.

## Accepted upstream limitations

- Message acks are queued before the event handler runs and sent on a 5 s
  ticker. Committing inside the handler narrows but does not close the window
  in which Google may consider an event acknowledged before it is on disk.
  The bridge's periodic bounded reconciliation is the repair mechanism.
- `Disconnect` joins the ack ticker but not the poll loop, ditto pinger,
  `postConnect`, ping wait/recovery workers, reconnect-after-pair workers, or
  long-poll timeout watchers. Helpers spawned with `context.TODO()` (including
  `ackBrowserPresence`, `requestUpdatesAfterGap`, and `postConnect` requests)
  have an independent lifetime and may outlive poll cancellation until their
  HTTP calls or response waits finish. If the ack ticker is already sending,
  its joined shutdown likewise waits for that independent-context HTTP call to
  finish or time out. `listenID`, `longPollingConn`,
  `disconnecting`, `skipCount`, and event-handler access remain unsynchronized.
  This is a live lifecycle/race risk not covered by the synthetic tests. The
  bridge mitigates callbacks by closing its callback gate before disconnect.
- `conversationsFetchedOnce` is unsynchronized; the bridge serializes
  `ListConversations`.
- The default `DownloadMedia` wrapper still has no caller cancellation; users
  requiring cancellation must call `DownloadMediaContext`.
