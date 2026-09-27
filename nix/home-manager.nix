{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.google-messages-multidevice-bridge;
  serverOptions = database: import ./server-options.nix { inherit lib database; };
  instances = lib.filterAttrs (_: instance: instance.enable) (
    cfg.instances // lib.optionalAttrs cfg.enable { default = cfg; }
  );
  unitName =
    name:
    if name == "default" then
      "google-messages-multidevice-bridge"
    else
      "google-messages-multidevice-bridge-${name}";

in
{
  options.services.google-messages-multidevice-bridge =
    (serverOptions "${config.xdg.dataHome}/google-messages-multidevice-bridge/bridge.db")
    // {
      instances = lib.mkOption {
        default = { };
        description = "Additional independently locked bridge instances. The name default is reserved.";
        type = lib.types.attrsOf (
          lib.types.submodule (
            { name, ... }: {
              options = serverOptions "${config.xdg.dataHome}/google-messages-multidevice-bridge-${name}/bridge.db";
            }
          )
        );
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
    {
      assertions = [
        {
          assertion = !(cfg.instances ? default);
          message = "The bridge instance name default is reserved.";
        }
        {
          assertion =
            builtins.length (lib.unique (map (i: i.database) (builtins.attrValues instances)))
            == builtins.length (builtins.attrValues instances);
          message = "Bridge instances must use distinct databases.";
        }
        {
          assertion =
            builtins.length (lib.unique (map (i: i.listen) (builtins.attrValues instances)))
            == builtins.length (builtins.attrValues instances);
          message = "Bridge instances must use distinct listen addresses.";
        }
      ]
      ++ lib.concatMap (instance: [
        {
          assertion = (instance.storageKeyPassEntry == null) != (instance.storageKeyFile == null);
          message = "Set exactly one storage key source for each bridge instance.";
        }
        {
          assertion = (instance.apiTokenPassEntry == null) != (instance.apiTokenFile == null);
          message = "Set exactly one API token source for each bridge instance.";
        }
      ]) (builtins.attrValues instances);
      home.packages = map (i: i.package) (builtins.attrValues instances);
      systemd.user.services = lib.mapAttrs' (
        name: instance:
        lib.nameValuePair (unitName name) {
          Unit = {
            Description = "${instance.network} Multi-Device Bridge";
            After = [ "network-online.target" ];
            Wants = [ "network-online.target" ];
          };
          Service = {
            ExecStart = import ./server-command.nix { inherit lib instance; };
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
        }
      ) instances;
    }
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
