# Deployment and operations

This service owns one bbolt database and one upstream connection selected by
`serve --network google-messages|whatsapp`. Google Messages is the default. Run one
process per database; the database lock rejects a second process. The supported
interactive setup is the web app's **Pair / Re-pair** flow with the bundled Chromium
helper. Users do not need to export Google cookies manually.

Google and WhatsApp verification details are recorded in README.md. WhatsApp
linking has been observed; the sync recovery fixes are currently fake-tested only.
Do not assume complete history or exactly-once delivery.

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

### Secret files instead of `pass`

`--storage-key-file` and `--api-token-file` read the same two secrets from files, and
take precedence over the matching `pass` entry and environment variable. Use them
where an unattended start cannot wait for a GPG agent — a passphrase-protected key
makes a `pass`-backed service fail-loop after a reboot until someone unlocks it.
Point them at decrypted-at-boot paths from agenix or sops-nix, owned by the service
user and mode 0400. The same size limit and whitespace trimming apply.

```sh
google-messages-multidevice-bridge serve \
  --db /absolute/path/to/bridge.db \
  --listen 127.0.0.1:8787 \
  --storage-key-file /run/agenix/bridge-storage-key \
  --api-token-file /run/agenix/bridge-api-token
```

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

The `storageKeyFile` and `apiTokenFile` options are the file-backed equivalents of
the two `pass` options above; set exactly one source per secret.

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

The bridge hands the preseed to the served page, so a client that keeps asking
for the token usually means the bridge is running an older build than the
client. Update the server before debugging the client.

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
saved provider upload descriptors. Google same-phone re-pairing does not alter them. Those
changes occur when Google pairing was started with `new_phone`, or any WhatsApp
linking, and the session is saved
successfully: the epoch advances, descriptors are cleared, and history jobs are paused
and reset. Previous records remain readable but cannot be mutation targets until
observed by the new phone.

## Connection behavior

The HTTP service and encrypted local history stay available across ordinary provider
failures. The supervisor reconnects transient failures after five seconds and backs
off to at most five minutes; a connection that survives over a minute resets the
delay. Authentication failure waits for explicit **Retry connection** or re-pair,
unless WhatsApp pairing recovery is explicitly enabled (below).
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

- WhatsApp linking and read-only connected status were observed. Sync recovery and
  historical coverage still need live verification; see README.md.
- `complete` history jobs mean the mapped provider cursor ended, not that every phone
  record was archived. Unsupported, invalid, or repeated cursors stop a job.
- Provider event persistence before ACK admission reduces loss but does not provide
  exactly-once ingestion; ACK and deduplication state have documented memory-only
  limits.
- Mutations use conservative `rejected` versus `ambiguous` outcomes and avoid known
  automatic POST replay, but Google and network behavior cannot guarantee exactly-once
  effects.
- Google needs a responsive phone; WhatsApp ordinary messaging does not. Older
  WhatsApp history requests may require the primary device. Cached history and
  downloaded attachments remain local; unavailable phone-side media may remain
  unavailable.

See the [API contract](api.md), [durability ADR](adr/0002-durable-history-and-send-boundaries.md),
and [libgm patch notes](../third_party/mautrix-gmessages/PATCHES.md) for the precise
boundaries.


## Additional network instances

Keep the existing `services.google-messages-multidevice-bridge` configuration.
Its new `network` option defaults to `"google-messages"`. Both Home Manager and
NixOS modules also accept `instances.<name>` with the same server options:

```nix
services.google-messages-multidevice-bridge.instances.whatsapp = {
  enable = true;
  package = pkgs.google-messages-multidevice-bridge;
  network = "whatsapp";
  listen = "127.0.0.1:8788";
  storageKeyFile = "/run/agenix/whatsapp-storage-key";
  apiTokenFile = "/run/agenix/whatsapp-api-token";
};
```

With Home Manager this creates the user unit
`google-messages-multidevice-bridge-whatsapp.service` and defaults to
`${config.xdg.dataHome}/google-messages-multidevice-bridge-whatsapp/bridge.db`.
The original user unit and data directory retain their names. Use different secrets
and listen addresses for each instance. The modules reject duplicate database or
listen settings; the name `default` is reserved for the original service.

The NixOS module can now run server instances as system services, in addition to
its existing desktop-client options. Its default database is
`/var/lib/google-messages-multidevice-bridge-whatsapp/bridge.db`; systemd creates
the state directory for the service. These system services require file-backed secrets, loaded by systemd with
`LoadCredential` for their dynamic service users; the source files can stay
root-owned. Use Home Manager for `pass`-backed services.
Use either the Home Manager user service or the NixOS system service for a given
database, never both. Nix module evaluation checks exercise the two-instance Home
Manager configuration and system-service credential loading; actual systemd activation is not live-tested here.

Expose the second listener through a separate HTTPS origin/port, enter that
instance's API token, and choose **Link with QR** or enter your E.164 number for a
pairing code. No Chromium helper is involved. QR/phone-code pairing lasts up to
three minutes and cancellation clears the displayed linking secret. Logout,
stream replacement, temporary ban and obsolete-client failures wait for explicit
attention rather than continually reconnecting. Opt-in recovery (below) can
initiate linking for session expiry only.

Back up each instance's database and its matching storage key separately, following
the offline backup procedure above. WhatsApp credentials and pending history/events
are inside that database; no extra credential file is needed. The same legacy
environment-variable names for storage key and API token work for both networks.
`--offline` never opens either upstream connection.

## Recovering WhatsApp history after the legacy inbox failure

Use a fresh link with the fixed build; deleting the encrypted database is not
required. WhatsApp offers on-demand history for known chat boundaries, but that is
not a reliable way to recover an initial archive containing unknown chats. The
bridge requests full history at linking; WhatsApp still controls how much it sends.

1. Deploy this build to the WhatsApp instance. Keep its data directory and storage
   key. Do not delete the database or use the pairing `adopt` operation.
2. On the primary phone, open WhatsApp → Settings → Linked devices, select the
   existing **whatsmeow** device, and log it out.
3. Open the WhatsApp bridge web client, choose **Pair / Re-pair**, and start QR
   linking. On the phone choose **Link a device** and scan the QR (or use the
   phone-number linking option). Leave WhatsApp open with a reliable connection
   while the initial history transfer runs.
4. A successful link creates fresh device keys and a new credential namespace,
   advances `session_epoch`, clears attachment descriptors and pauses old history
   jobs. Old records remain encrypted and read-only until re-observed. The old
   quarantine also remains in its old namespace; the new session count starts at
   zero. An abandoned/failed pairing does not replace the saved session.
5. Refresh the web client and any EVA snapshot caches after the epoch changes.
   The web client reloads snapshots when it sees a new epoch. Empty legacy protocol
   messages are hidden from message snapshots, and metadata refresh cannot revive
   previous-epoch records. Do not replay the old event log from cursor zero into a
   fresh client cache; resume from current snapshot cursors.
6. Check GET `/v1/status`, `/v1/conversations` and `/v1/contacts?refresh=1`. Confirm
   nonzero conversation timestamps and increasing chat/message/contact counts.
   Logs include `history_received` (sync type/progress/counts), `history_expanded`,
   `history_messages_queued`, `appstate_fetch`, `appstate_error` and
   `appstate_waiting_keys`. Missing-key/incomplete sync retries every 30 seconds.
   `metadata_refreshed` does not mean full history transfer is complete.

Safe diagnostics are enabled by default, while upstream logs remain disabled.
They contain only event type names, counts, sync types/progress and coarse error
classes—no message bodies, identifiers, names, tokens or keys. Startup counts of
stored keys, app-state versions, contacts, inbox and quarantine (including event
kinds) help distinguish missing history from an incomplete app-state sync.

## Opt-in WhatsApp pairing recovery

In **Pair / Re-pair**, enter your E.164 phone number and choose **Enable automatic
recovery for this number**. The setting is saved encrypted in this instance's
existing database, so Nix declarations and secret files do not need to change.
Enable browser notifications separately if you want Web Push alerts when a code
is ready. EVA can use `PUT /v1/pairing/recovery` and poll the pairing state; its
Android notification/open-WhatsApp action must be implemented by the EVA client.

On logout or expired authentication, the bridge starts phone-code linking and
makes up to three attempts with a cooldown. Open the bridge for the code, open
WhatsApp's **Linked devices**, and approve linking (including any face/fingerprint
prompt). Expired codes are replaced automatically while the budget permits.
After cancellation or exhaustion, select **Resume automatic recovery** when your
phone is ready. **Disable automatic recovery** stops automatic linking. Ordinary
network failures still reconnect without pairing. Bans, invalid sessions,
stream replacement, outdated clients and quarantined events require separate
attention and never cause automatic linking.

Starting an automatic attempt cancels queued sends. Success creates a new session
epoch and fresh credentials, preserving earlier records as described above.
The new recovery controller is tested with fakes for eligibility, retry limits,
restart persistence, cooldown, cancellation, notifications and successful epoch
transition. Unattended recovery and notification delivery have not been live
verified; no production logout, pairing or message send was performed to test it.
