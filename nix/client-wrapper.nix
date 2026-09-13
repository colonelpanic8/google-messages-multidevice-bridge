{ lib, pkgs }:
# Wrap the desktop client so managed hosts unlock without typing anything.
# Only names and URLs land in the store: the secret itself is read from `pass`
# (or a secret file) at launch time, never baked into /nix/store.
{
  package,
  bridgeUrl ? null,
  apiTokenPassEntry ? null,
  apiTokenFile ? null,
}:
let
  needsWrap = bridgeUrl != null || apiTokenPassEntry != null || apiTokenFile != null;
  # Single-quote a value for embedding in the wrapper's bash snippet.
  bashQuote = value: "'" + lib.replaceStrings [ "'" ] [ "'\\''" ] value + "'";
  tokenSnippet = ''
    if [ -z "''${GOOGLE_MESSAGES_BRIDGE_TOKEN:-}" ] && [ -z "''${GOOGLE_MESSAGES_BRIDGE_TOKEN_FILE:-}" ]; then
      token="$(pass show ${bashQuote apiTokenPassEntry} 2>/dev/null)" || token=""
      if [ -n "$token" ]; then
        export GOOGLE_MESSAGES_BRIDGE_TOKEN="$token"
      fi
    fi
  '';
in
if !needsWrap then
  package
else
  pkgs.symlinkJoin {
    name = "${package.name or "google-messages-desktop"}-configured";
    paths = [ package ];
    nativeBuildInputs = [ pkgs.makeWrapper ];
    postBuild = ''
      for bin in "$out"/bin/*; do
        [ -f "$bin" ] && [ -x "$bin" ] || continue
        ${lib.optionalString (
          bridgeUrl != null
        ) ''wrapProgram "$bin" --set-default GOOGLE_MESSAGES_BRIDGE_URL ${lib.escapeShellArg bridgeUrl}''}
        ${lib.optionalString (apiTokenFile != null)
          ''wrapProgram "$bin" --set-default GOOGLE_MESSAGES_BRIDGE_TOKEN_FILE ${lib.escapeShellArg (toString apiTokenFile)}''
        }
        ${lib.optionalString (apiTokenPassEntry != null)
          ''wrapProgram "$bin" --suffix PATH : ${
            lib.escapeShellArg (
              lib.makeBinPath [
                pkgs.pass
                pkgs.gnupg
              ]
            )
          } --run ${lib.escapeShellArg tokenSnippet}''
        }
      done
    '';
    meta = (package.meta or { }) // {
      description = "Google Messages desktop client preconfigured for this host (bridge URL / token lookup); see client.bridgeUrl and client.apiTokenPassEntry.";
    };
  }
