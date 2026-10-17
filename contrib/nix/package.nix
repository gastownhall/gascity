# Gas City — Nix package derivation.
#
# Used two ways:
#   1. From the project flake (src = self, version from flake outputs)
#   2. From nixpkgs (src = fetchFromGitHub { ... }, version pinned there)
#
# To update for a new release:
#   - Bump version and rev in the fetchFromGitHub call (nixpkgs usage)
#   - Reset vendorHash to lib.fakeHash, run `nix build`, fill in the hash
#     from the error output.
#
# Build mirrors the Makefile `build` target:
#   go build -ldflags "..." -o bin/gc ./cmd/gc
# We replicate the ldflags rather than calling `make` because the Makefile
# uses `git describe` for VERSION, which is unavailable in the Nix sandbox.
{
  lib,
  buildGoModule,
  makeWrapper,
  src,
  version,
}:

let
  gcBase = buildGoModule {
    pname = "gascity";
    inherit src version;

    vendorHash = "sha256-59k7xFBaLZJ50KWNhwIzttE8j7GXZPneq6o4eUTlvBI=";

    # Upstream does not commit vendor/. proxyVendor tells buildGoModule to
    # fetch dependencies via the Go module proxy using go.mod + go.sum,
    # validated against vendorHash. If upstream ever starts committing
    # vendor/ again, this can be removed.
    proxyVendor = true;

    doCheck = false;

    subPackages = [ "cmd/gc" ];

    postPatch = ''
      goVer="$(go env GOVERSION | sed 's/^go//')"
      go mod edit -go="$goVer"
    '';
    env.GOTOOLCHAIN = "auto";

    ldflags = [
      "-s"
      "-w"
      "-X main.version=${version}"
    ];

    meta = {
      description = "Orchestration-builder SDK for multi-agent systems";
      longDescription = ''
        Gas City provides a configurable multi-agent orchestration toolkit:
        declarative city.toml configuration, runtime providers (tmux, subprocess,
        exec, ACP, Kubernetes), beads-backed work routing, formula dispatch, and
        a controller/supervisor reconciliation loop.

        Runtime dependencies (not managed by this derivation):
          Required:  tmux, git, jq, pgrep, lsof
          Beads backend (default): dolt (>=1.85.0), bd/beads (>=0.61.0), flock
          Set GC_BEADS=file to skip the beads backend deps.
      '';
      homepage = "https://github.com/gastownhall/gascity";
      changelog = "https://github.com/gastownhall/gascity/releases/tag/v${version}";
      license = lib.licenses.mit;
      maintainers = [ ];
      mainProgram = "gc";
      platforms = lib.platforms.linux ++ lib.platforms.darwin;
    };
  };
in

# Wrap gc with a runtime check for bd (beads CLI).
# If bd is not in PATH and GC_BEADS=file is not set, gc exits immediately
# with a clear message rather than failing cryptically mid-command.
#
# nixpkgs ships beads 0.42.0 which is too old (gc requires >=0.61.0).
# Install a compatible version via: nix shell github:gastownhall/beads
gcBase.overrideAttrs (prev: {
  nativeBuildInputs = (prev.nativeBuildInputs or [ ]) ++ [ makeWrapper ];

  postInstall = (prev.postInstall or "") + ''
    wrapProgram $out/bin/gc \
      --run '
        if [ "''${GC_BEADS:-}" != "file" ] && ! command -v bd >/dev/null 2>&1; then
          echo "gc: bd (beads CLI >=0.61.0) not found in PATH." >&2
          echo "    nixpkgs ships beads 0.42.0 which is too old for gc." >&2
          echo "    Install a compatible version:" >&2
          echo "      nix shell github:gastownhall/beads" >&2
          echo "    Or bypass the beads backend:" >&2
          echo "      GC_BEADS=file gc ..." >&2
          exit 1
        fi
      '
  '';
})
