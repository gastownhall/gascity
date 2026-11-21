{
  description = "Gas City — orchestration-builder SDK for multi-agent systems";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      # Version comes from the flake's git metadata: short commit SHA for clean
      # builds, "-dirty" suffix for dirty trees, "dev" if no rev is available.
      # The Makefile uses `git describe --tags --exact-match` instead, which the
      # nix sandbox can't replicate (no .git access). For tagged-release builds
      # via nixpkgs, override `version` at callPackage time.
      version = self.shortRev or self.dirtyShortRev or "dev";

      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-darwin"
        "x86_64-linux"
      ];

      forAllSystems =
        f:
        nixpkgs.lib.genAttrs systems (
          system:
          f {
            pkgs = nixpkgs.legacyPackages.${system};
            inherit system self;
          }
        );
    in
    {
      packages = forAllSystems (
        { pkgs, ... }:
        let
          gc = pkgs.callPackage ./contrib/nix/package.nix {
            inherit version;
            src = self;
          };
        in
        {
          default = gc;
          gascity = gc;
        }
      );

      apps = forAllSystems (
        { self, system, ... }:
        {
          default = {
            type = "app";
            program = "${self.packages.${system}.default}/bin/gc";
          };
        }
      );

      devShells = forAllSystems (
        { pkgs, ... }:
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.tmux
              pkgs.git
              pkgs.jq
              pkgs.procps # pgrep
              pkgs.lsof
              pkgs.dolt
              pkgs.util-linux # flock
              # bd (beads CLI >=0.61.0) — nixpkgs ships 0.42.0 which is too old.
              # Add to your shell via: nix shell github:gastownhall/beads
            ];

            shellHook = ''
              echo "Gas City dev shell"
              echo "  build:   go build ./cmd/gc"
              echo "  test:    go test ./..."
              echo "  install: make install"
              if ! command -v bd >/dev/null 2>&1; then
                echo ""
                echo "  Warning: bd (beads CLI) not found."
                echo "  Add it: nix shell github:gastownhall/beads"
              fi
            '';
          };
        }
      );

      homeManagerModules.default = import ./contrib/nix/hm-module.nix { inputs = { gascity = self; }; };
      homeManagerModules.gascity = import ./contrib/nix/hm-module.nix { inputs = { gascity = self; }; };

      formatter = forAllSystems ({ pkgs, ... }: pkgs.nixfmt-rfc-style);
    };
}
