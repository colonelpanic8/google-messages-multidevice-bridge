{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.google-messages-multidevice-bridge;
  quoteExec =
    arg:
    "\""
    + lib.replaceStrings [ "\\" "\"" "%" "$" "\n" "\r" ] [ "\\\\" "\\\"" "%%" "$$" "\\n" "\\r" ] arg
    + "\"";
in
{
  options.services.google-messages-multidevice-bridge = {
    enable = lib.mkEnableOption "Google Messages Multi-Device Bridge";
    package = lib.mkOption {
      type = lib.types.package;
      description = "Bridge package, typically the package from this flake.";
    };
    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8787";
      description = "Listen address; use a Tailscale address or a TLS reverse proxy for other devices.";
    };
    database = lib.mkOption {
      type = lib.types.str;
      default = "${config.xdg.dataHome}/google-messages-multidevice-bridge/bridge.db";
      description = "Encrypted database path.";
    };
    storageKeyPassEntry = lib.mkOption {
      type = lib.types.str;
      description = "pass entry containing the permanent base64 storage key.";
    };
    apiTokenPassEntry = lib.mkOption {
      type = lib.types.str;
      description = "pass entry containing the API bearer token.";
    };
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
  config = lib.mkMerge [
    (lib.mkIf cfg.enable {
      home.packages = [ cfg.package ];
      systemd.user.services.google-messages-multidevice-bridge = {
        Unit = {
          Description = "Google Messages Multi-Device Bridge";
          After = [ "network-online.target" ];
          Wants = [ "network-online.target" ];
        };
        Service = {
          ExecStart = lib.concatMapStringsSep " " quoteExec [
            (lib.getExe cfg.package)
            "serve"
            "--db"
            cfg.database
            "--listen"
            cfg.listen
            "--storage-key-pass-entry"
            cfg.storageKeyPassEntry
            "--api-token-pass-entry"
            cfg.apiTokenPassEntry
          ];
          Environment = "PATH=${
            lib.makeBinPath [
              pkgs.pass
              pkgs.gnupg
            ]
          }:${config.home.profileDirectory}/bin";
          Restart = "on-failure";
          RestartSec = 10;
          TimeoutStopSec = 45;
          UMask = "0077";
          NoNewPrivileges = true;
          PrivateTmp = true;
        };
        Install.WantedBy = [ "default.target" ];
      };
    })
    (lib.mkIf cfg.client.enable (
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
        home.packages = lib.mkIf (cfg.client.package != null) [ finalClient ];
      }
    ))
  ];
}
