# Gas City — Home Manager module.
#
# Usage in your flake.nix:
#
#   inputs = {
#     gascity.url = "github:gastownhall/gascity";
#     beads.url = "github:gastownhall/beads";  # >=0.61.0; nixpkgs ships 0.42.0
#   };
#
#   home-manager.sharedModules = [
#     inputs.gascity.homeManagerModules.default
#     { programs.gascity.enable = true; }
#   ];
#
#   # In your home config, add bd from the beads flake:
#   home.packages = [ inputs.beads.packages.${system}.default ];
{ inputs, ... }:
{ config, lib, pkgs, ... }:

let
  cfg = config.programs.gascity;
  system = pkgs.stdenv.hostPlatform.system;
  gcPkg = inputs.gascity.packages.${system}.default;
in
{
  options.programs.gascity = {
    enable = lib.mkEnableOption "Gas City multi-agent orchestration system";
  };

  config = lib.mkIf cfg.enable {
    home.packages = [
      gcPkg

      # Required runtime deps
      pkgs.tmux
      pkgs.git
      pkgs.jq
      pkgs.procps   # pgrep
      pkgs.lsof

      # Beads backend deps
      pkgs.dolt       # >=1.85.0 per deps.env
      pkgs.util-linux # flock

      # bd (beads CLI) — nixpkgs ships 0.42.0 which is too old.
      # Add inputs.beads.url = "github:gastownhall/beads" to your flake
      # and include inputs.beads.packages.${system}.default here.
    ];
  };
}
