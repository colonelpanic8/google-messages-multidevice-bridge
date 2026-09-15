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

            # Covers third_party/mautrix-gmessages too, which go.mod replaces with
            # a local path: editing that copy changes the vendored tree, so this
            # hash must be updated or the build silently reuses the cached one.
            vendorHash = "sha256-/bY57g1SKqhAPV7gya97I8GifXUMwymPSZrbgUbae1Q=";

            meta = {
              description = "One Google Messages connection for all your devices";
              homepage = "https://github.com/colonelpanic8/google-messages-multidevice-bridge";
              license = pkgs.lib.licenses.agpl3Plus;
              mainProgram = "google-messages-multidevice-bridge";
              platforms = supportedSystems;
            };
          };
        }
        // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          desktop = pkgs.callPackage ./nix/desktop.nix { };
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
        client-preseed = import ./nix/client-wrapper-check.nix {
          pkgs = nixpkgs.legacyPackages.${system};
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
        // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          desktop = pkgs.mkShell {
            inputsFrom = [ self.packages.${system}.desktop ];
            packages = with pkgs; [
              cargo
              rustc
              rust-analyzer
            ];
          };
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);

      homeManagerModules.default = import ./nix/home-manager.nix;

      nixosModules.default = import ./nix/nixos.nix;

      overlays.default =
        final: prev:
        {
          google-messages-multidevice-bridge = self.packages.${prev.stdenv.hostPlatform.system}.default;
        }
        // prev.lib.optionalAttrs prev.stdenv.hostPlatform.isLinux {
          google-messages-desktop = self.packages.${prev.stdenv.hostPlatform.system}.desktop;
        };
    };
}
