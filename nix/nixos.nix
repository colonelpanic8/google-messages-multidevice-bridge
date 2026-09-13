{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.google-messages-multidevice-bridge;
in
{
  options.services.google-messages-multidevice-bridge = {
    client.enable = lib.mkEnableOption "Google Messages desktop client (Tauri window around the served web client)";
    client.package = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default =
        if builtins.hasAttr "google-messages-desktop" pkgs then pkgs."google-messages-desktop" else null;
      defaultText = lib.literalExpression "pkgs.\"google-messages-desktop\"";
      description = "Desktop client package, typically the desktop package from this flake (Linux only). Set explicitly when the overlay is not applied.";
    };
    client.bridgeUrl = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "https://bridge.example.ts.net:8443";
      description = "Bridge URL preseeded into the desktop client so the setup screen is skipped. The client still accepts a manually saved URL when this is unset.";
    };
    client.apiTokenPassEntry = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "services/google-messages-bridge/api-token";
      description = "pass entry holding the API bearer token. The wrapped client runs `pass show` on every launch; only the entry name lands in the store, never the secret.";
    };
    client.apiTokenFile = lib.mkOption {
      type = lib.types.nullOr (lib.types.either lib.types.str lib.types.path);
      default = null;
      example = "/run/secrets/bridge-api-token";
      description = "File holding the API bearer token (for agenix/sops-nix users). The pass lookup is skipped when this is set, and an explicit GOOGLE_MESSAGES_BRIDGE_TOKEN still wins over both.";
    };
  };
  config = lib.mkIf cfg.client.enable (
    let
      wrapClient = import ./client-wrapper.nix { inherit lib pkgs; };
      finalClient = wrapClient {
        package = cfg.client.package;
        bridgeUrl = cfg.client.bridgeUrl;
        apiTokenPassEntry = cfg.client.apiTokenPassEntry;
        apiTokenFile = cfg.client.apiTokenFile;
      };
    in
    {
      assertions = [
        {
          assertion = cfg.client.package != null;
          message = "services.google-messages-multidevice-bridge.client.package must be set (apply this flake's overlay or set it to the desktop package explicitly).";
        }
      ];
      environment.systemPackages = lib.mkIf (cfg.client.package != null) [ finalClient ];
    }
  );
}
