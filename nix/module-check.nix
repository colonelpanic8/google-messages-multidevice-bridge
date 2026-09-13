{ pkgs, bridgePackage }:
let
  inherit (pkgs) lib;
  evaluated = lib.evalModules {
    specialArgs = { inherit pkgs; };
    modules = [
      ./home-manager.nix
      ({ lib, ... }: {
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
        };
        config.services.google-messages-multidevice-bridge = {
          enable = true;
          package = bridgePackage;
          storageKeyPassEntry = "keys/a'b\"c\\d%t$HOME";
          apiTokenPassEntry = "tokens/bridge";
        };
      })
    ];
  };
  service = evaluated.config.systemd.user.services.google-messages-multidevice-bridge.Service;
in
assert lib.hasInfix ''"keys/a'b\"c\\d%%t$$HOME"'' service.ExecStart;
assert lib.hasInfix ''"/test/data/google-messages-multidevice-bridge/bridge.db"'' service.ExecStart;
assert service.UMask == "0077";
assert service.Restart == "on-failure";
pkgs.runCommand "bridge-home-manager-module-check" { } ''touch "$out"''
