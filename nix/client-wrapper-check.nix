{ pkgs }:
let
  inherit (pkgs) lib;
  wrapClient = import ./client-wrapper.nix { inherit lib pkgs; };
  # A stand-in for the real GUI binary: it just reports the preseed env.
  fakeClient = pkgs.writeShellScriptBin "google-messages-desktop" ''
    echo "TOKEN=''${GOOGLE_MESSAGES_BRIDGE_TOKEN:-<unset>} URL=''${GOOGLE_MESSAGES_BRIDGE_URL:-<unset>} FILE=''${GOOGLE_MESSAGES_BRIDGE_TOKEN_FILE:-<unset>}"
  '';
  fakePass = pkgs.writeShellScriptBin "pass" ''
    if [ "$1" = show ] && [ "$2" = services/test/api-token ]; then
      printf 's3cret-from-pass'
    else
      echo "unexpected pass invocation: $*" >&2
      exit 1
    fi
  '';
  wrappedPass = wrapClient {
    package = fakeClient;
    bridgeUrl = "https://bridge.example.ts.net:8443";
    apiTokenPassEntry = "services/test/api-token";
  };
  wrappedFile = wrapClient {
    package = fakeClient;
    apiTokenFile = "/run/secrets/bridge-api-token";
  };
in
assert wrapClient { package = fakeClient; } == fakeClient;
pkgs.runCommand "bridge-client-wrapper-check"
  {
    nativeBuildInputs = [
      wrappedPass
      wrappedFile
    ];
  }
  ''
    export PATH=${lib.makeBinPath [ fakePass ]}:$PATH
    got="$(${wrappedPass}/bin/google-messages-desktop)"
    expected="TOKEN=s3cret-from-pass URL=https://bridge.example.ts.net:8443 FILE=<unset>"
    [ "$got" = "$expected" ] || { echo "pass-preseed mismatch: got [$got] want [$expected]"; exit 1; }
    # An explicitly exported token wins over the pass lookup.
    got="$(GOOGLE_MESSAGES_BRIDGE_TOKEN=manual-token ${wrappedPass}/bin/google-messages-desktop)"
    expected="TOKEN=manual-token URL=https://bridge.example.ts.net:8443 FILE=<unset>"
    [ "$got" = "$expected" ] || { echo "env-override mismatch: got [$got]"; exit 1; }
    got="$(${wrappedFile}/bin/google-messages-desktop)"
    expected="TOKEN=<unset> URL=<unset> FILE=/run/secrets/bridge-api-token"
    [ "$got" = "$expected" ] || { echo "token-file mismatch: got [$got]"; exit 1; }
    touch "$out"
  ''
