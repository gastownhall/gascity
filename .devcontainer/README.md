# Devcontainer for gascity

This devcontainer reproduces a development environment for [gastownhall/gascity](https://github.com/gastownhall/gascity) per the [official installation guide](https://github.com/gastownhall/gascity/blob/main/docs/getting-started/installation.md).

## What it installs

| Tool | Source | Version |
|---|---|---|
| Go 1.26 | `mcr.microsoft.com/devcontainers/go` base image | 1.26 (matches `go.mod`) |
| `tmux` | apt | system |
| `jq` | apt | system |
| `libicu-dev` | apt | system |
| `flock` | apt | system (via `util-linux`) |
| `git` | devcontainer feature | latest |
| `gh` | devcontainer feature | latest |
| `dolt` | `.github/scripts/install-dolt-archive.sh` (SHA-256 verified) | `DOLT_VERSION` from `deps.env` (currently 2.1.7) |
| `bd` (Beads CLI) | `.github/scripts/install-bd-archive.sh` (SHA-256 verified) | `BD_VERSION` from `deps.env` (currently v1.0.4) |
| `gc` (Gas City) | `make install` from source | built from current commit |

Versions come from `deps.env` so bumping is one file change.

## Lifecycle

| Hook | Runs | Notes |
|---|---|---|
| `onCreateCommand` | Once on container create | `apt install` of the system packages |
| `postCreateCommand` | Once after `onCreate` | Installs `dolt` and `bd` via the canonical `.github/scripts/install-*-archive.sh` scripts (pinned, SHA-256 verified), then builds and installs `gc` from source |
| `postStartCommand` | Every time the container starts | Smoke check that all binaries are on PATH |

## Why source build, not Homebrew

The [installation guide](https://github.com/gastownhall/gascity/blob/main/docs/getting-started/installation.md) recommends Homebrew for daily use. The devcontainer uses the source-build path because:

1. The devcontainer is for contributors and reviewers — `make install` from source is the path documented in "Build from source" and "Contributor setup"
2. Homebrew is not available in the base `devcontainers/go` Ubuntu image
3. Source build matches what CI does in `.github/actions/setup-gascity-ubuntu/`

## Dolt data persistence

`mounts` declares a named volume `gc-cities` at `/home/vscode/gc-cities`. Gas City stores managed Dolt data under each city's `.beads/dolt`, so **initializing cities under `~/gc-cities/` is what makes them survive container rebuilds.** A city created at `~/gc-cities/my-city` writes its Dolt databases to `~/gc-cities/my-city/.beads/dolt`, which lives inside the persisted volume. Cities initialized elsewhere are not persisted.

## What this does NOT install

- `claude` (Claude Code CLI) — not required for gascity itself, only for agent runtime providers. Install manually if you `gc sling claude`.
- Other agent CLIs (codex, gemini, etc.) — same as above.
- Homebrew — not present in Linux devcontainer base image.

## Testing the devcontainer

```bash
# From repo root, with the devcontainer CLI installed:
devcontainer up --workspace-folder .
devcontainer exec --workspace-folder . bash -c \
  'gc version && gc init ~/gc-cities/test-city && cd ~/gc-cities/test-city && mkdir -p /tmp/test-rig && git -C /tmp/test-rig init && gc rig add /tmp/test-rig'
```

Or in VS Code: `Ctrl+Shift+P` → "Dev Containers: Reopen in Container".

## After `gc init`

Follow the [Quickstart](https://github.com/gastownhall/gascity/blob/main/docs/getting-started/quickstart.md):

```bash
gc init ~/gc-cities/my-city
cd ~/gc-cities/my-city
mkdir ~/hello-world && cd ~/hello-world && git init && cd -
gc rig add ~/hello-world
cd ~/hello-world
gc sling claude "Create a script that prints hello world"
bd show <bead-id> --watch
```

Initialize cities under `~/gc-cities/` so their managed Dolt data persists across rebuilds (see [Dolt data persistence](#dolt-data-persistence)).

## Note on `gc` and Oh My Zsh

If your shell aliases `gc` to `git commit --verbose`, use `command gc ...` to bypass it. The base image here uses bash by default so this is not a problem, but if you opt into Oh My Zsh you'll need the workaround from the [troubleshooting guide](https://github.com/gastownhall/gascity/blob/main/docs/getting-started/troubleshooting.md#oh-my-zsh-git-plugin-hides-gc).
