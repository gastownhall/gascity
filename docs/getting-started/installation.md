---
title: Installation
description: Install Gas City from Homebrew, a release tarball, or source.
---

## Which method should I use?

| Method                              | Best for                         | Installs default-stack deps? | Auto-upgrades? |
| ----------------------------------- | -------------------------------- | ---------------------------- | -------------- |
| [Homebrew](#homebrew-recommended)   | macOS / Linux daily use          | Yes                          | `brew upgrade` |
| [Direct download](#direct-download) | CI, containers, air-gapped hosts | No                           | Manual         |
| [Source build](#build-from-source)  | Contributors, bleeding-edge      | No                           | Manual         |

**Most users should use Homebrew.** It installs `gc` plus the default backend
toolchain automatically and keeps `gc` on your PATH. Choose direct download
when you cannot use Homebrew (CI images, Docker layers, machines without
package managers). Choose source when you need unreleased changes or plan to
contribute.

## Prerequisites

Gas City's prerequisites depend on which session backend and beads backend you
actually run. Homebrew installs the default stack for you; the other methods
require manual installation of the rows that match your setup.

| Tool           | When required                                         | macOS                | Linux                                                     | Notes                                                          |
| -------------- | ----------------------------------------------------- | -------------------- | --------------------------------------------------------- | -------------------------------------------------------------- |
| git            | Always                                                | (built-in)           | (built-in)                                                | Version control                                                |
| jq             | Always                                                | `brew install jq`    | `apt install jq`                                          | JSON processing                                                |
| pgrep          | Always                                                | (built-in)           | `apt install procps`                                      | Process discovery for runtime checks                           |
| lsof           | Always                                                | (built-in)           | `apt install lsof`                                        | Process/port inspection                                        |
| tmux           | Default session backend only (`""`, `tmux`, `hybrid`) | `brew install tmux`  | `apt install tmux`                                        | Skip when using `subprocess`, `acp`, `exec:<script>`, or `k8s` |
| dolt           | Default beads backend only (`""`, `bd`)               | `brew install dolt`  | [releases](https://github.com/dolthub/dolt/releases)      | Skip when `[beads].provider = "file"`                          |
| bd (Beads CLI) | Default beads backend only (`""`, `bd`)               | `brew install beads` | [releases](https://github.com/gastownhall/beads/releases) | Skip when `[beads].provider = "file"`                          |
| flock          | Default beads backend only (`""`, `bd`)               | `brew install flock` | (built-in via util-linux)                                 | File locking for the `bd` backend                              |
| Go 1.25+       | Source only                                           | `brew install go`    | [golang.org](https://go.dev/dl/)                          | Compiler                                                       |

If you plan to follow the Quickstart exactly as written, install the default
stack: `tmux` for sessions plus `bd`, `dolt`, and `flock` for beads.

The exact versions CI pins are in [`deps.env`](https://github.com/gastownhall/gascity/blob/main/deps.env).

## Homebrew (recommended)

```bash
brew install gastownhall/gascity/gascity
```

This taps the `gastownhall/gascity` formula, builds or fetches the `gc` binary,
and installs the default backend toolchain (`tmux`, `jq`, `git`, `dolt`,
`flock`, `beads`).

Verify the installation:

```bash
gc version
```

### Upgrading via Homebrew

```bash
brew update
brew upgrade gascity
```

After upgrading, restart any running city so the supervisor picks up the new
binary:

```bash
gc service restart     # restarts the launchd/systemd service
```

`gc start` auto-regenerates the service file on each invocation, so a
`brew upgrade` followed by `gc start` always picks up template changes
(see [v0.13.3 release notes](https://github.com/gastownhall/gascity/releases/tag/v0.13.3)).

### Uninstalling via Homebrew

```bash
gc stop <city-path>                        # stop running city first
brew uninstall gascity
brew untap gastownhall/gascity             # remove the tap
```

## Direct download

Release tarballs are published for every tagged version. Supported platforms:

| OS             | Architecture          | Archive name                          |
| -------------- | --------------------- | ------------------------------------- |
| macOS (darwin) | Apple Silicon (arm64) | `gascity_VERSION_darwin_arm64.tar.gz` |
| macOS (darwin) | Intel (amd64)         | `gascity_VERSION_darwin_amd64.tar.gz` |
| Linux          | x86_64 (amd64)        | `gascity_VERSION_linux_amd64.tar.gz`  |
| Linux          | ARM (arm64)           | `gascity_VERSION_linux_arm64.tar.gz`  |

### Download and install

```bash
# Set the version you want (check https://github.com/gastownhall/gascity/releases)
VERSION=0.13.3

# Detect platform
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)         ARCH=amd64 ;;
  aarch64|arm64)  ARCH=arm64 ;;
esac

# Download and extract
curl -fsSLO "https://github.com/gastownhall/gascity/releases/download/v${VERSION}/gascity_${VERSION}_${OS}_${ARCH}.tar.gz"
tar -xzf "gascity_${VERSION}_${OS}_${ARCH}.tar.gz"

# Move to a directory on your PATH
sudo install -m 755 gc /usr/local/bin/gc

# Verify
gc version
```

### Upgrading a direct-download install

Repeat the download steps above with the new version number. The `gc` binary is
a single static file — overwriting it is safe.

<Tip>
You still need to install the [prerequisites](#prerequisites) that match your
chosen session and beads backends when using direct download.
</Tip>

## Build from source

Requires Go 1.25+ (pinned in `go.mod`).

```bash
git clone https://github.com/gastownhall/gascity.git
cd gascity
make install        # builds and installs to $(GOPATH)/bin/gc
gc version
```

To build without installing globally:

```bash
make build          # outputs bin/gc in the repo root
./bin/gc version
```

On macOS, `make build` automatically ad-hoc code-signs the binary (`codesign -s -`).

### Contributor setup

After building, install the dev toolchain and pre-commit hooks:

```bash
make setup
make check          # runs fmt, lint, vet, and unit tests
```

See [CONTRIBUTING.md](https://github.com/gastownhall/gascity/blob/main/CONTRIBUTING.md)
for the full contributor workflow.

## Verify your installation

Regardless of install method, confirm everything is working:

```bash
gc version          # should print the installed version and commit
```

Then create your first city:

```bash
gc init ~/my-city
cd ~/my-city
gc start
```

See the [Quickstart](./quickstart.md) for a complete walkthrough.

## Docs preview

The docs site uses [Mintlify](https://mintlify.com). Preview locally:

```bash
cd docs
npx --yes mint@latest dev
```

Or run a link check without starting the server:

```bash
make check-docs
```
