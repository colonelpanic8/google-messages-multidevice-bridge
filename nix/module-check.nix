{ pkgs, bridgePackage }:
let
  inherit (pkgs) lib;
  stubOptions =
    { lib, ... }:
    {
      options = {
        home.packages = lib.mkOption {
          type = lib.types.listOf lib.types.package;
          default = [ ];
        };
        home.profileDirectory = lib.mkOption {
          type = lib.types.str;
          default = "/test/profile";
        };
        xdg.dataHome = lib.mkOption {
          type = lib.types.str;
          default = "/test/data";
        };
        systemd.user.services = lib.mkOption {
          type = lib.types.attrsOf lib.types.anything;
          default = { };
        };
        assertions = lib.mkOption {
          type = lib.types.listOf lib.types.anything;
          default = [ ];
        };
      };
    };
  evaluated = lib.evalModules {
    specialArgs = { inherit pkgs; };
    modules = [
      ./home-manager.nix
      stubOptions
      {
        config.services.google-messages-multidevice-bridge = {
          enable = true;
          package = bridgePackage;
          storageKeyPassEntry = "keys/a'b\"c\\d%t$HOME";
          apiTokenPassEntry = "tokens/bridge";
        };
      }
    ];
  };
  service = evaluated.config.systemd.user.services.google-messages-multidevice-bridge.Service;
  fileSecrets = lib.evalModules {
    specialArgs = { inherit pkgs; };
    modules = [
      ./home-manager.nix
      stubOptions
      {
        config.services.google-messages-multidevice-bridge = {
          enable = true;
          package = bridgePackage;
          storageKeyFile = "/run/agenix/storage-key";
          apiTokenFile = "/run/agenix/api-token";
        };
      }
    ];
  };
  fileSecretsService =
    fileSecrets.config.systemd.user.services.google-messages-multidevice-bridge.Service;
  bothSecretSources = lib.evalModules {
    specialArgs = { inherit pkgs; };
    modules = [
      ./home-manager.nix
      stubOptions
      {
        config.services.google-messages-multidevice-bridge = {
          enable = true;
          package = bridgePackage;
          storageKeyPassEntry = "keys/storage";
          storageKeyFile = "/run/agenix/storage-key";
          apiTokenPassEntry = "tokens/bridge";
        };
      }
    ];
  };
  clientEvaluated = lib.evalModules {
    specialArgs = { inherit pkgs; };
    modules = [
      ./home-manager.nix
      stubOptions
      {
        config.services.google-messages-multidevice-bridge = {
          client.enable = true;
          client.package = bridgePackage;
        };
      }
    ];
  };
  clientPreseeded = lib.evalModules {
    specialArgs = { inherit pkgs; };
    modules = [
      ./home-manager.nix
      stubOptions
      {
        config.services.google-messages-multidevice-bridge = {
          client.enable = true;
          client.package = bridgePackage;
          client.bridgeUrl = "https://bridge.example.ts.net:8443";
          client.apiTokenPassEntry = "services/test/api-token";
        };
      }
    ];
  };
  nixosEvaluated = lib.evalModules {
    specialArgs = { inherit pkgs; };
    modules = [
      ./nixos.nix
      (
        { lib, ... }:
        {
          options = {
            environment.systemPackages = lib.mkOption {
              type = lib.types.listOf lib.types.package;
              default = [ ];
            };
            assertions = lib.mkOption {
              type = lib.types.listOf lib.types.anything;
              default = [ ];
            };
          };
          config.services.google-messages-multidevice-bridge = {
            client.enable = true;
            client.package = bridgePackage;
          };
        }
      )
    ];
  };
in
assert lib.hasInfix ''"keys/a'b\"c\\d%%t$$HOME"'' service.ExecStart;
assert lib.hasInfix ''"/test/data/google-messages-multidevice-bridge/bridge.db"'' service.ExecStart;
assert service.UMask == "0077";
assert lib.hasInfix ''"--storage-key-file" "/run/agenix/storage-key"'' fileSecretsService.ExecStart;
assert lib.hasInfix ''"--api-token-file" "/run/agenix/api-token"'' fileSecretsService.ExecStart;
assert !(lib.hasInfix "pass-entry" fileSecretsService.ExecStart);
assert builtins.all (a: a.assertion) fileSecrets.config.assertions;
# Two sources for one secret is ambiguous, so the module refuses it.
assert !(builtins.all (a: a.assertion) bothSecretSources.config.assertions);
assert service.Restart == "on-failure";
assert clientEvaluated.config.home.packages == [ bridgePackage ];
assert builtins.all (a: a.assertion) clientEvaluated.config.assertions;
# Preseed options wrap the package instead of installing it bare. The wrapped
# derivation is only compared here, never built by this check.
assert builtins.length clientPreseeded.config.home.packages == 1;
assert builtins.head clientPreseeded.config.home.packages != bridgePackage;
assert builtins.all (a: a.assertion) clientPreseeded.config.assertions;
assert nixosEvaluated.config.environment.systemPackages == [ bridgePackage ];
assert builtins.all (a: a.assertion) nixosEvaluated.config.assertions;
pkgs.runCommand "bridge-home-manager-module-check" { } ''touch "$out"''
