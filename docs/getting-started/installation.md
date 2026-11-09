---
title: Installation
description: Install Gas City from Homebrew, a release tarball, or source.
---

## Which method should I use?

| Method | Best for | Installs deps? | Auto-upgrades? |
|--------|----------|----------------|----------------|
| [Homebrew](#homebrew-recommended) | macOS / Linux daily use | Yes (runtime deps) | `brew upgrade` |
| [Direct download](#direct-download) | CI, containers, air-gapped hosts | No | Manual |
| [Source build](#build-from-source) | Contributors, bleeding-edge | No | Manual |

**Most users should use Homebrew.** It installs all runtime dependencies
automatically and keeps `gc` on your PATH. Choose direct download when you
cannot use Homebrew (CI images, Docker layers, machines without package
managers). Choose source when you need unreleased changes or plan to contribute.

## Prerequisites

Gas City requires a small set of runtime tools. Homebrew installs all of them
for you; the other methods require manual installation.

| Tool | Required | Min version | macOS | Linux | Notes |
|------|----------|-------------|-------|-------|-------|
| tmux | Yes | — | `brew install tmux` | `apt install tmux` | Session management |
| jq | Yes | — | `brew install jq` | `apt install jq` | JSON processing |
| git | Yes | — | (built-in) | (built-in) | Version control |
| dolt | Yes | 2.6.0 or newer | `brew install dolt` | [releases](https://github.com/dolthub/dolt/releases) | Beads data plane |
| bd (Beads CLI) | Yes | 1.3.0 or newer | `brew install beads` | [releases](https://github.com/gastownhall/beads/releases) | Issue tracking |
| flock | Yes | — | `brew install flock` | (built-in via util-linux) | File locking |
| gh | Optional | — | `brew install gh` | [cli.github.com](https://cli.github.com/) | GitHub gate checks |
| Go 1.27.3+ | Source only | 1.27.3 | `brew install go` | [golang.org](https://go.dev/dl/) | Compiler |
| make | Source only | — | (built-in) | `apt install make` (or `build-essential`) | Drives `make install` |

Use a final Dolt 2.6.0 or newer. Gas City's managed Dolt checks reject older
and pre-release builds because they are below the managed bd/Dolt compatibility
floor: 2.6.0 is the first Dolt built with a Go toolchain that distrusts the
compromised eMudhra CA intermediates.

`brew install gascity` installs a current `bd` along with `gc`. With another
install method, install the latest
[Beads release](https://github.com/gastownhall/beads/releases).

An upgrade that moves an existing shared database to a newer Beads schema needs
every client of that database upgraded together and a one-time `bd migrate
schema`; see [Upgrading an existing city](/getting-started/upgrading). Fresh
workspaces start at the current schema.

## Homebrew (recommended)

```bash
brew install gascity
```

This installs the latest `gc` release and its runtime dependencies (beads,
dolt, tmux, jq, and flock). Gas City is in homebrew-core, so no tap is needed.

Verify the installation:

```bash
gc version
```

<Warning>
If you use Oh My Zsh with the `git` plugin, `gc` may already be an alias for
`git commit --verbose`. Run `command gc version` once to bypass the alias. For
a persistent fix, add `unalias gc 2>/dev/null` or
`zstyle ':omz:plugins:git' aliases no 'gc'` after Oh My Zsh loads in
`~/.zshrc`, or put that line in a file such as
`~/.oh-my-zsh/custom/gascity.zsh`.
</Warning>

### Upgrading via Homebrew

```bash
brew update
brew upgrade gascity
```

The `gascity` formula depends on `beads`, so this upgrades `bd` too.

After upgrading, run `gc start` for each city. When the supervisor is still
running the old binary, `gc start` restarts it on the new one, and it
regenerates the supervisor's service file on each invocation, so a
`brew upgrade` followed by `gc start` always picks up template changes.

An upgrade that changes the Beads version can also need a backup and a
one-time database migration. Before upgrading a city you rely on, follow
[Upgrading an existing city](/getting-started/upgrading).

### Uninstalling via Homebrew

```bash
gc stop <city-path>                        # stop running city first
gc supervisor uninstall                    # remove the background supervisor service
brew uninstall gascity
```

## Direct download

Release tarballs are published for every tagged version. Supported platforms:

| OS | Architecture | Archive name |
|----|-------------|--------------|
| macOS (darwin) | Apple Silicon (arm64) | `gascity_VERSION_darwin_arm64.tar.gz` |
| macOS (darwin) | Intel (amd64) | `gascity_VERSION_darwin_amd64.tar.gz` |
| Linux | x86_64 (amd64) | `gascity_VERSION_linux_amd64.tar.gz` |
| Linux | ARM (arm64) | `gascity_VERSION_linux_arm64.tar.gz` |

### Download and install

```bash
# The latest release (or set a version from https://github.com/gastownhall/gascity/releases)
VERSION=$(curl -fsSL https://api.github.com/repos/gastownhall/gascity/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')

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

### Verify release artifacts

Homebrew verifies release checksums from the formula automatically. For direct
downloads, verify the archive before installing it:

```bash
ARCHIVE="gascity_${VERSION}_${OS}_${ARCH}.tar.gz"
CHECKSUMS="gascity_${VERSION}_checksums.txt"

curl -fsSLO "https://github.com/gastownhall/gascity/releases/download/v${VERSION}/${CHECKSUMS}"
grep "  ${ARCHIVE}$" "${CHECKSUMS}" > "${ARCHIVE}.sha256"

if command -v sha256sum >/dev/null 2>&1; then
  sha256sum -c "${ARCHIVE}.sha256"
else
  shasum -a 256 -c "${ARCHIVE}.sha256"
fi
```

Release archives are also published with GitHub artifact attestations. If you
have the GitHub CLI installed, verify the downloaded archive against the
`gastownhall/gascity` repository:

```bash
gh attestation verify "${ARCHIVE}" --repo gastownhall/gascity
```

Each release also includes an SPDX SBOM asset:

```bash
curl -fsSLO "https://github.com/gastownhall/gascity/releases/download/v${VERSION}/gascity-v${VERSION}.spdx.json"
```

### Upgrading a direct-download install

Repeat the download steps above with the new version number. The `gc` binary is
a single static file — overwriting it is safe. Install the matching `bd` as
well, and see [Upgrading an existing city](/getting-started/upgrading) for the
backup and Beads migration steps.

<Tip>
You still need to install the [prerequisites](#prerequisites) separately when
using direct download. Homebrew handles this automatically.
</Tip>

## Build from source

Requires `make` and Go 1.27.3+ (pinned in `go.mod` as 1.27.3).

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

On macOS, `make build` signs the binary with a stable local codesigning
identity when one is available, which helps macOS remember local permission
grants across rebuilds. Without a stable identity, the build leaves Go's
linker-produced signature unchanged. Set `GC_SIGN_IDENTITY=<certificate name>`
to choose a specific certificate, `GC_SIGN_IDENTIFIER=<identifier>` to use a
separate local TCC identity, or `GC_ADHOC_SIGN=1` to opt into ad-hoc signing
for a local experiment. Successful local signing also removes stale
`com.apple.provenance` metadata when present.

### Contributor setup

To change Gas City itself (tests, lint, docs preview), follow
[CONTRIBUTING.md](https://github.com/gastownhall/gascity/blob/main/CONTRIBUTING.md).

## Verify your installation

Regardless of install method, confirm everything is working:

```bash
gc version          # prints the installed version
```

If that runs `git commit` instead of Gas City, your shell has a `gc` alias.
Use `command gc version` for this check and see
[Troubleshooting](/getting-started/troubleshooting#oh-my-zsh-git-plugin-hides-gc)
for the permanent fix.

Next, follow the [Quickstart](/getting-started/quickstart) to create your
first city and send work through a formula.

Gas City ships a JSONL archive that snapshots every bead database for
disaster recovery. By default it runs in local-only mode and keeps commits
on this host. To enable off-box backup, see
[JSONL archive push failures](/getting-started/troubleshooting#jsonl-archive-push-failures).
