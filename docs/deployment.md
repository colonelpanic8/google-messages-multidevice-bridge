# Deployment and operations

This service owns one bbolt database and one Google Messages connection. Run one
process per database; the database lock rejects a second process. The supported
interactive setup is the web app's **Pair / Re-pair** flow with the bundled Chromium
helper. Users do not need to export Google cookies manually.

The Google-facing paths remain validated only with synthetic providers and local
HTTP servers. Treat an initial deployment as an evaluation: keep the Android phone
and Google Messages available for comparison, and do not assume complete history or
exactly-once delivery.

## Runtime secrets with `pass`

The service needs:

- A permanent storage key: base64 encoding of exactly 32 random bytes.
- An API bearer token: at least 32 characters.

If suitable `pass` entries do not already exist, create them without putting their
values in shell history or plaintext files:

```sh
openssl rand -base64 32 | pass insert --multiline services/google-messages-bridge/storage-key
pass generate --no-symbols services/google-messages-bridge/api-token 48
```

Use entry names, not secret values, on the command line:

```sh
google-messages-multidevice-bridge serve \
  --db /absolute/path/to/bridge.db \
  --listen 127.0.0.1:8787 \
  --storage-key-pass-entry services/google-messages-bridge/storage-key \
  --api-token-pass-entry services/google-messages-bridge/api-token
```

At startup the process runs `pass show` with a 30-second timeout, trims surrounding
whitespace, and rejects secret output over 4096 bytes. Configure the user's GPG agent
so `pass` works in the service environment. Do not compensate by copying secrets to
an unencrypted environment file.

The API token can be replaced by restarting the service with a new value and updating
clients. The storage key cannot be rotated by changing the entry: a different key
makes the existing database unreadable.

## Home Manager

The flake exports `homeManagerModules.default`. Add it to the module list and set all
required options:

```nix
{
  inputs.google-messages-bridge.url =
    "github:colonelpanic8/google-messages-multidevice-bridge";

  outputs =
    inputs@{ nixpkgs, home-manager, google-messages-bridge, ... }:
    {
      homeConfigurations.example = home-manager.lib.homeManagerConfiguration {
        pkgs = nixpkgs.legacyPackages.x86_64-linux;
        modules = [
          google-messages-bridge.homeManagerModules.default
          ({ config, pkgs, ... }: {
            services.google-messages-multidevice-bridge = {
              enable = true;
              package =
                google-messages-bridge.packages.${pkgs.stdenv.hostPlatform.system}.default;
              listen = "127.0.0.1:8787";
              database =
                "${config.xdg.dataHome}/google-messages-multidevice-bridge/bridge.db";
              storageKeyPassEntry =
                "services/google-messages-bridge/storage-key";
              apiTokenPassEntry =
                "services/google-messages-bridge/api-token";
            };
          })
        ];
      };
    };
}
```

After `home-manager switch`, the module installs a hardened systemd user service
named `google-messages-multidevice-bridge`. It sets umask 0077, restarts on failure,
and adds `pass` and GnuPG to the service path. Normal user-service commands apply:

```sh
systemctl --user status google-messages-multidevice-bridge
systemctl --user restart google-messages-multidevice-bridge
journalctl --user -u google-messages-multidevice-bridge
```

The module intentionally has no offline-mode option. For recovery inspection, stop
the managed service and run one manual `serve --offline` process against the same
database, never both simultaneously.

### Desktop client on every host

The flake also exports `nixosModules.default` and the `google-messages-desktop`
overlay package (Linux only). Apply the overlay once in your flake, then every
host gets the client with one option. Only enable the bridge *service* on the
host that owns the database; the client just talks to the bridge over HTTP.

With Home Manager on every host:

```nix
{
  nixpkgs.overlays = [ google-messages-bridge.overlays.default ];

  imports = [ google-messages-bridge.homeManagerModules.default ];

  services.google-messages-multidevice-bridge = {
    # Server bits only on the host that owns the database:
    # enable = true;
    # package = google-messages-bridge.packages.${pkgs.stdenv.hostPlatform.system}.default;
    # storageKeyPassEntry = "...";
    # apiTokenPassEntry = "...";

    # Client on every host; the package defaults to the overlay package:
    client.enable = true;
  };
}
```

Or system-wide via NixOS (also needs the overlay for the default package):

```nix
{
  nixpkgs.overlays = [ google-messages-bridge.overlays.default ];

  imports = [ google-messages-bridge.nixosModules.default ];

  services.google-messages-multidevice-bridge.client.enable = true;
}
```

Without the overlay, set `client.package` explicitly to
`google-messages-bridge.packages.${pkgs.stdenv.hostPlatform.system}.desktop`.

### Preseeding the client so nothing is typed

Three more client options remove the per-host setup screens:

```nix
{
  services.google-messages-multidevice-bridge.client = {
    enable = true;
    bridgeUrl = "https://bridge.example.ts.net:8443";
    apiTokenPassEntry = "services/google-messages-bridge/api-token";
  };
}
```

- `bridgeUrl` skips the setup screen and always opens that bridge.
- `apiTokenPassEntry` wraps the binary so it runs `pass show <entry>` on every
  launch and unlocks with the result. A manually exported
  `GOOGLE_MESSAGES_BRIDGE_TOKEN` still wins, and if `pass` fails (locked GPG
  agent, missing entry) the client falls back to the keyring prompt instead of
  failing.
- `apiTokenFile` (for agenix/sops-nix, e.g. `/run/secrets/bridge-api-token`)
  preseeds from a file instead of `pass`; the client also honors
  `GOOGLE_MESSAGES_BRIDGE_TOKEN_FILE` directly. With agenix imported in your
  (private) host config, the receiving end looks like this — the encrypted
  `.age` file itself lives beside your host config, not in this repo:

```nix
{
  age.secrets.bridge-api-token.file = ./secrets/bridge-api-token.age;
  services.google-messages-multidevice-bridge.client = {
    enable = true;
    bridgeUrl = "https://bridge.example.ts.net:8443";
    apiTokenFile = config.age.secrets.bridge-api-token.path;
  };
}
```

Token precedence inside the client is: `GOOGLE_MESSAGES_BRIDGE_TOKEN` →
`GOOGLE_MESSAGES_BRIDGE_TOKEN_FILE` → OS keyring → private fallback file.
Locking from the menu forgets all of them for that run; the preseed returns on
next launch.

Security notes: only entry names, URLs, and file paths land in `/nix/store`.
The secret itself is read at launch time, but it does live in the client's
process environment while running (visible to the same user, as with any env
secret). Prefer the keyring entry the client saves on first manual unlock only
if you would rather type the token once per host and never involve `pass`.

## Network exposure

The default listener is loopback with an OS-selected port. For a stable local
deployment, set an explicit loopback port. For other devices, either:

- Bind to this host's Tailscale address and rely on the encrypted private tailnet, or
- Keep the service on loopback and place an authenticated TLS reverse proxy in front.

Do not send bearer tokens over untrusted plaintext HTTP. Mount the bridge at the
origin root, not under a path prefix: the web app and pairing helper use `/v1/` and
`/pairing-helper.zip` paths. Preserve streaming responses and avoid proxy buffering
for `/v1/stream`.

The pairing extension accepts HTTPS bridge origins. Its only HTTP exceptions are
loopback and literal addresses inside Tailscale `100.64.0.0/10`; DNS names over HTTP
are rejected. Redirects on the credential handoff are rejected, so configure the
bridge URL with its final scheme, host, and port. The browser running the helper must
be able to reach that origin directly.

Web assets and `/pairing-helper.zip` are public. All `/v1/` endpoints require the API
bearer except `POST /v1/pairing/credentials`, which accepts only the current
43-character
one-use ticket and fixed cookie handoff. Do not expose a second proxy route that
bypasses these application checks.

## Installing as an app, and notifications

Installing the client and receiving notifications both require a secure context,
which plain HTTP over a tailnet address is not. Serving over HTTPS unlocks the web
app manifest, the service worker, and Web Push on desktop and Android.

On a tailnet with HTTPS certificates enabled, the simplest route is to let
`tailscaled` terminate TLS and renew the certificate:

```sh
tailscale serve --bg --https=8443 http://127.0.0.1:<bridge-port>
tailscale serve status
```

That publishes `https://<machine>.<tailnet>.ts.net:8443` to the tailnet only. Pick a
port that is not already serving something else; `tailscale serve status` lists the
current mappings, and `tailscale serve --https=8443 off` removes just that one. Any
authenticated TLS reverse proxy works equally well, subject to the routing rules
above.

Notifications are then enabled per device from the client. The bridge generates a
VAPID key pair on first use, each browser subscribes through its own push service,
and `POST /v1/push/test` proves the whole chain end to end. Delivery while no window
is open depends on the platform: a running browser on desktop, or the system push
service on Android. A browser that is fully quit receives nothing until it starts
again.

## Initial pairing and re-pairing

1. Start the service and open its web origin.
2. Unlock the web app with the API token.
3. Open **Pair / Re-pair**, download the helper ZIP, and load the extracted directory
   through `chrome://extensions` using **Developer mode** and **Load unpacked**.
4. Start pairing, then copy the displayed bridge origin and ticket into the helper's
   persistent setup tab.
5. Complete Google sign-in using the helper button, return to the helper, explicitly
   connect the account, then return to the bridge and confirm the phone emoji.

The ticket expires after ten minutes. Pairing cannot start while the process uses
`--offline`; that request returns 400.

Starting re-pair immediately cancels every still-queued outbox operation so it cannot
cross into a different session. Canceling or failing the attempt does not restore
those queued operations. It also does not alter the entity epoch, history jobs, or
saved provider upload descriptors. Those latter changes occur only after a new
session is paired and saved successfully: the epoch advances, descriptors are
cleared, and history jobs are paused and reset. Previous records remain readable but
cannot be mutation targets until observed by the new session.

## Connection behavior

The HTTP service and encrypted local history stay available across ordinary provider
failures. The supervisor reconnects transient failures after five seconds and backs
off to at most five minutes; a connection that survives over a minute resets the
delay. Authentication failure waits for explicit **Retry connection** or re-pair.
`POST /v1/connection/restart` supplies the same restart wake-up for API clients.

`serve --offline` never opens a Google connection. Durable conversation, message,
and reaction requests can still be queued; history jobs remain queued; typing,
mark-read, sync, and attachment fetches that need the phone are unavailable. Pairing
is disabled. Restart normally to process queued work.

Storage failure is different from provider failure: the service stops because it can
no longer guarantee durable commits. Inspect disk space, permissions, and the
database before restarting rather than repeatedly cycling it.

## Backups and restore

There is no application hot-backup command. Do not copy the bbolt file while the
service is running. Take an offline-consistent backup:

1. Stop the only process using the database.
2. Copy the database to a new protected destination and retain mode 0600.
3. Confirm the copy exists and has the expected ownership and permissions.
4. Restart the service.

For a Home Manager service, the operational sequence is:

```sh
systemctl --user stop google-messages-multidevice-bridge
cp --preserve=mode,timestamps /absolute/path/to/bridge.db /new/secure/backup/bridge.db
chmod 600 /new/secure/backup/bridge.db
systemctl --user start google-messages-multidevice-bridge
```

Choose a new destination so an older known-good backup is not overwritten. A
filesystem or volume snapshot is useful only if it captures the database while the
service is stopped.

Back up the matching storage-key `pass` entry and the password store's GPG recovery
material through the password store's encrypted backup procedure. Never export the
storage key beside the database as plaintext. Retain the pinned application source or
package revision with the backup. The API token is not needed to decrypt the database
and can be replaced after restore; the original storage key is indispensable.

To restore, stop all bridge processes, place the database at the configured path with
the service user's ownership and mode 0600, restore the matching storage-key entry,
then start one process. A restored database can have a lower local SSE event cursor
than connected browsers remember, so clients may need to lock/reload and fetch fresh
snapshots.

## Operational limits

- No live Google account, pairing, message, media, or long-history validation has
  been completed for this milestone.
- `complete` history jobs mean the mapped provider cursor ended, not that every phone
  record was archived. Unsupported, invalid, or repeated cursors stop a job.
- Provider event persistence before ACK admission reduces loss but does not provide
  exactly-once ingestion; ACK and deduplication state have documented memory-only
  limits.
- Mutations use conservative `rejected` versus `ambiguous` outcomes and avoid known
  automatic POST replay, but Google and network behavior cannot guarantee exactly-once
  effects.
- The phone must remain responsive for new provider traffic. Cached history and
  downloaded attachments remain local; unavailable phone-side media may remain
  unavailable.

See the [API contract](api.md), [durability ADR](adr/0002-durable-history-and-send-boundaries.md),
and [libgm patch notes](../third_party/mautrix-gmessages/PATCHES.md) for the precise
boundaries.
