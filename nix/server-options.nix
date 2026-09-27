{ lib, database }:
{
  network = lib.mkOption {
    type = lib.types.enum [
      "google-messages"
      "whatsapp"
    ];
    default = "google-messages";
    description = "Upstream network for this database.";
  };
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
    default = database;
    description = "Encrypted database path.";
  };
  storageKeyPassEntry = lib.mkOption {
    type = lib.types.nullOr lib.types.str;
    default = null;
    description = "pass entry containing the permanent base64 storage key.";
  };
  apiTokenPassEntry = lib.mkOption {
    type = lib.types.nullOr lib.types.str;
    default = null;
    description = "pass entry containing the API bearer token.";
  };
  storageKeyFile = lib.mkOption {
    type = lib.types.nullOr (lib.types.either lib.types.str lib.types.path);
    default = null;
    example = "/run/secrets/bridge-storage-key";
    description = "File containing the permanent base64 storage key (for agenix/sops-nix). Unlike the pass entry it needs no GPG agent, so the service survives a reboot unattended.";
  };
  apiTokenFile = lib.mkOption {
    type = lib.types.nullOr (lib.types.either lib.types.str lib.types.path);
    default = null;
    example = "/run/secrets/bridge-api-token";
    description = "File containing the API bearer token (for agenix/sops-nix). Unlike the pass entry it needs no GPG agent, so the service survives a reboot unattended.";
  };
}
