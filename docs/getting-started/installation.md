---
title: Installation
description: Install Gas City from a release binary, Homebrew, or build from source.
---

## Choose Your Install Method

| Method | Best for |
|--------|----------|
| **Homebrew** (macOS/Linux) | End users who want automatic dependency management and easy upgrades |
| **Release tarball** | Servers, CI, or environments where Homebrew is unavailable |
| **Build from source** | Contributors and developers who need the latest unreleased changes |

Go is **not required** for binary or Homebrew installs.

## Homebrew

```bash
brew install gastownhall/gascity/gascity
gc version
```

Homebrew auto-installs all runtime dependencies (tmux, jq, git, dolt, flock, beads).

**Upgrade to the latest release:**

```bash
brew upgrade gastownhall/gascity/gascity
gc version
```

If you installed before the `gastownhall/gascity` tap existed, tap it first:

```bash
brew tap gastownhall/gascity
brew install gascity
```

## Install From A Release Tarball

Supported platforms: `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`.

```bash
VERSION=0.13.3
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac

curl -fsSLO "https://github.com/gastownhall/gascity/releases/download/v${VERSION}/gascity_${VERSION}_${OS}_${ARCH}.tar.gz"
tar -xzf "gascity_${VERSION}_${OS}_${ARCH}.tar.gz"
install -m 755 gc ~/.local/bin/gc
gc version
```

Make sure `~/.local/bin` is in your `PATH`. Add this to your shell profile if needed:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

See all available releases at [github.com/gastownhall/gascity/releases](https://github.com/gastownhall/gascity/releases).

**Upgrading a tarball install:** re-run the download and install commands above with the new `VERSION` value.

### Runtime Dependencies (tarball installs)

Unlike Homebrew, tarball installs do not auto-install dependencies. Install these before running `gc start`:

| Dependency | macOS | Linux |
|------------|-------|-------|
| tmux | `brew install tmux` | `apt install tmux` |
| jq | `brew install jq` | `apt install jq` |
| git | (included) | `apt install git` |
| dolt | `brew install dolt` | [dolt releases](https://github.com/dolthub/dolt/releases) |
| flock | `brew install flock` | `apt install util-linux` |
| beads (`bd`) | [beads releases](https://github.com/steveyegge/beads/releases) | [beads releases](https://github.com/steveyegge/beads/releases) |

To use a file-based beads store (no dolt/bd/flock needed):

```bash
export GC_BEADS=file
# or add to city.toml: [beads] provider = "file"
```

## Build `gc` From Source

Requires Go 1.25 or newer.

From a clean clone:

```bash
make install
gc version
```

If you do not want to install globally, build the local binary instead:

```bash
make build
./bin/gc version
```

## Verify Your Install

Run these commands after any install method to confirm everything is working:

```bash
gc version          # prints version string
gc help             # lists available commands
gc doctor           # checks runtime dependencies and reports any missing tools
```

Expected output for `gc version`:

```
gc version 0.13.3
```

If `gc: command not found`, check that the install directory (`~/.local/bin` for
tarballs, `$(brew --prefix)/bin` for Homebrew) is in your `PATH`.

## Contributor Setup

Install local dev tooling and hooks:

```bash
make setup
make check
```

`make check` runs the fast Go quality gates, including the repo's docs sync and
local-link tests.

## Docs Preview

The docs site now uses Mintlify. Preview it locally with:

```bash
cd docs
npx --yes mint@latest dev
```

Run a local docs check without starting the preview server:

```bash
make check-docs
```
