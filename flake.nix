{
  description = "One Google Messages connection for all your devices";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      supportedSystems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.buildGoModule {
            pname = "google-messages-multidevice-bridge";
            version = "0.1.0-dev";
            src = ./.;

            go = pkgs.go_1_26;
            subPackages = [ "cmd/google-messages-multidevice-bridge" ];
            checkPhase = ''
              runHook preCheck
              go test ./...
              runHook postCheck
            '';

            vendorHash = "sha256-MkEtf8xzwQU9gQfg7+aBAcwnfADrRJ+ZIzm/GamK6+s=";

            meta = {
              description = "One Google Messages connection for all your devices";
              homepage = "https://github.com/colonelpanic8/google-messages-multidevice-bridge";
              license = pkgs.lib.licenses.agpl3Plus;
              mainProgram = "google-messages-multidevice-bridge";
              platforms = supportedSystems;
            };
          };
        }
      );

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = nixpkgs.lib.getExe self.packages.${system}.default;
          meta.description = "Run Google Messages Multi-Device Bridge";
        };
      });

      checks = forAllSystems (system: {
        package = self.packages.${system}.default;
        home-manager-module = import ./nix/module-check.nix {
          pkgs = nixpkgs.legacyPackages.${system};
          bridgePackage = self.packages.${system}.default;
        };
      });

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              actionlint
              go_1_26
              go-tools
              gopls
              just
              nixfmt
              nodejs
              prettier
            ];
          };
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);

      homeManagerModules.default = import ./nix/home-manager.nix;

      overlays.default = final: _prev: {
        google-messages-multidevice-bridge = self.packages.${final.stdenv.hostPlatform.system}.default;
      };
    };
}
