---
title: Troubleshooting
description: Common installation and setup issues and how to fix them.
---

## Run the Built-in Doctor

`gc doctor` checks your city for structural, config, dependency, and runtime
issues. It is always the best first step:

```bash
gc doctor
gc doctor --verbose   # extra detail
gc doctor --fix       # attempt automatic repairs
```

## "command not found" After Install

If `gc` is installed but your shell cannot find it, the binary is not on your
`PATH`.

**Homebrew** puts binaries in a directory that is usually already on your PATH.
Run `brew --prefix` to confirm, then check that `$(brew --prefix)/bin` appears
in your `PATH`.

**Direct download** requires you to move or symlink the binary into a
directory on your PATH:

```bash
install -m 755 gc ~/.local/bin/gc   # or /usr/local/bin/gc
```

Then verify:

```bash
which gc
gc version
```

If you use a non-standard shell (fish, nushell), check that shell's PATH
configuration rather than `~/.bashrc` or `~/.zshrc`.

## Missing Prerequisites

`gc init` and `gc start` check for required tools and report any that are
missing. You can also run `gc doctor` inside an existing city for a fuller
check.

### Always required

| Tool | macOS | Debian / Ubuntu |
|------|-------|-----------------|
| tmux | `brew install tmux` | `apt install tmux` |
| git | `brew install git` | `apt install git` |
| jq | `brew install jq` | `apt install jq` |
| pgrep | included | `apt install procps` |
| lsof | included | `apt install lsof` |

### Required for the default beads provider (`bd`)

| Tool | Min version | macOS | Linux |
|------|-------------|-------|-------|
| dolt | 1.80.0 | `brew install dolt` | [releases](https://github.com/dolthub/dolt/releases) |
| bd | 0.61.0 | [releases](https://github.com/steveyegge/beads/releases) | [releases](https://github.com/steveyegge/beads/releases) |
| flock | -- | `brew install flock` | `apt install util-linux` |

If you do not want to install dolt, bd, and flock, switch to the file-based
store:

```bash
export GC_BEADS=file
```

Or add this to your `city.toml`:

```toml
[beads]
provider = "file"
```

The file provider is fine for trying Gas City locally. The `bd` provider adds
durable versioned storage and is recommended for real work.

## Dolt Version Too Old

Gas City requires dolt 1.80.0 or newer. Check your version:

```bash
dolt version
```

Upgrade via Homebrew (`brew upgrade dolt`) or download a newer release from
[dolthub/dolt/releases](https://github.com/dolthub/dolt/releases).

## flock Not Found (macOS)

macOS does not ship `flock`. Install it via Homebrew:

```bash
brew install flock
```

Alternatively, switch to the file-based beads provider (see above) to skip
the flock requirement entirely.

## `gc version` Prints Unexpected Output

If `gc version` prints git progress lines (`Enumerating objects...`) instead
of a clean version string, upgrade to Gas City v0.13.4 or later. This was a
bug where remote pack fetches wrote git sideband output to the terminal,
fixed in [PR #141](https://github.com/gastownhall/gascity/pull/141).

## WSL (Windows Subsystem for Linux)

Gas City works under WSL 2 with a standard Ubuntu or Debian distribution.
Install prerequisites using the Linux column in the tables above. tmux
requires a working terminal — use Windows Terminal or another WSL-aware
terminal emulator.

## Build From Source Fails

Building from source requires Go 1.25 or newer:

```bash
go version
```

If your Go version is too old, update it from [go.dev/dl](https://go.dev/dl/)
or via your package manager. Then:

```bash
make build
./bin/gc version
```

See [CONTRIBUTING.md](https://github.com/gastownhall/gascity/blob/main/CONTRIBUTING.md)
for the full contributor setup.

## Still Stuck?

Open an issue at
[gastownhall/gascity/issues](https://github.com/gastownhall/gascity/issues)
with the output of `gc doctor --verbose` and your OS/architecture.
