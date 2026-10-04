# Nix packaging for Gas City

## Quick start

```bash
# Run gc without installing
nix run github:gastownhall/gascity

# Or from a local checkout
nix run .
```

## Install via flake input

Add to your `flake.nix`:

```nix
inputs.gascity.url = "github:gastownhall/gascity";
```

Then add the package to your profile:

```nix
home.packages = [ inputs.gascity.packages.${system}.default ];
```

## Home Manager module

```nix
# flake.nix
inputs = {
  gascity.url = "github:gastownhall/gascity";
  home-manager.url = "github:nix-community/home-manager";
};

outputs = { self, home-manager, gascity, ... }: {
  homeConfigurations.yourhost = home-manager.lib.homeManagerConfiguration {
    modules = [
      gascity.homeManagerModules.default
      {
        programs.gascity.enable = true;
      }
    ];
  };
};
```

This installs `gc` and all required runtime dependencies.

## Development shell

```bash
# Full dev environment with all runtime deps
nix develop
```

## Runtime dependencies

These are required at runtime but not bundled in the `gc` binary:

| Dependency | Required for | Minimum version |
|---|---|---|
| tmux | Session runtime provider | any |
| git | Repo operations | any |
| jq | JSON processing | any |
| pgrep | Process monitoring | any |
| lsof | Port detection | any |
| dolt | Beads backend (default) | 1.85.0 |
| bd | Beads backend (default) | 0.61.0 |
| flock | Beads backend (default) | any |

Set `GC_BEADS=file` to skip dolt/bd/flock and use a file-based store.

Note: `bd` (beads CLI) is not yet in nixpkgs. Install it from
[github.com/gastownhall/beads/releases](https://github.com/gastownhall/beads/releases).

## Update procedure

When a new gc release is cut, update the derivation as follows:

1. Bump `version` in `flake.nix` (project flake) or in the nixpkgs derivation.
2. Reset `vendorHash` in `contrib/nix/package.nix` to `lib.fakeHash`.
3. Run `nix build .#default`. The build will fail with a hash mismatch.
4. Copy the `got: sha256-...` value from the error and replace `lib.fakeHash`.
5. Re-run `nix build .#default` to confirm.

## Files

| File | Purpose |
|---|---|
| `package.nix` | Standalone derivation — usable from nixpkgs or any flake |
| `hm-module.nix` | Home Manager module with `programs.gascity.enable` |
| `../../flake.nix` | Root project flake — wires `src = self` into `package.nix` |
