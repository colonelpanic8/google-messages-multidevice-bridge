{ lib, pkgs }:
# Options shared by the NixOS and Home Manager modules for the `gmessages`
# command-line client and MCP server. Profiles name token sources, never tokens.
let
  profileType = lib.types.submodule {
    options = {
      url = lib.mkOption {
        type = lib.types.str;
        example = "https://bridge.example.ts.net:8443";
        description = "Bridge URL.";
      };
      tokenFile = lib.mkOption {
        type = lib.types.nullOr (lib.types.either lib.types.str lib.types.path);
        default = null;
        description = "File holding the API bearer token.";
      };
      tokenPassEntry = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "pass entry holding the API bearer token, read on each invocation.";
      };
    };
  };
in
{
  options = {
    enable = lib.mkEnableOption "the gmessages command-line client and MCP server";
    package = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default =
        if builtins.hasAttr "google-messages-multidevice-bridge" pkgs then
          pkgs.google-messages-multidevice-bridge
        else
          null;
      defaultText = lib.literalExpression "pkgs.google-messages-multidevice-bridge";
      description = "Package providing `gmessages`, typically the package from this flake.";
    };
    profiles = lib.mkOption {
      type = lib.types.attrsOf profileType;
      default = { };
      example = lib.literalExpression ''
        {
          default = { url = "https://bridge.example.ts.net:8443"; tokenFile = "/run/agenix/bridge-token"; };
          whatsapp = { url = "https://bridge.example.ts.net:8444"; tokenFile = "/run/agenix/whatsapp-token"; };
        }
      '';
      description = "Bridges `gmessages --profile NAME` can reach. A `default` profile is derived from the client options when client.bridgeUrl is set.";
    };
    defaultProfile = lib.mkOption {
      type = lib.types.str;
      default = "default";
      description = "Profile used when neither --profile nor the environment names a bridge.";
    };
  };

  # The default profile mirrors the desktop client's preseeded bridge.
  clientProfile =
    cfg:
    lib.mkIf (cfg.client.bridgeUrl != null) {
      default = lib.mkDefault {
        url = cfg.client.bridgeUrl;
        tokenFile = cfg.client.apiTokenFile;
        tokenPassEntry = cfg.client.apiTokenPassEntry;
      };
    };

  configJSON =
    cli:
    builtins.toJSON {
      default_profile = cli.defaultProfile;
      profiles = lib.mapAttrs (
        _: p:
        lib.filterAttrs (_: v: v != null) {
          inherit (p) url;
          token_file = if p.tokenFile == null then null else toString p.tokenFile;
          token_pass_entry = p.tokenPassEntry;
        }
      ) cli.profiles;
    };

  assertions = cli: [
    {
      assertion = cli.package != null;
      message = "services.google-messages-multidevice-bridge.cli.package must be set (apply this flake's overlay or set it explicitly).";
    }
  ];
}
