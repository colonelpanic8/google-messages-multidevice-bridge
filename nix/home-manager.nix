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
  };
  config = lib.mkIf cfg.enable {
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
  };
}
