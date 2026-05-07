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

## Oh My Zsh Git Plugin Hides `gc`

Oh My Zsh's `git` plugin defines `gc` as an alias for
`git commit --verbose`. When that alias is active, commands like `gc version`,
`gc init`, or `gc start` run git instead of the Gas City binary.

Temporary workaround:

```bash
command gc version
command gc init ~/my-city
```

`command` bypasses shell aliases for that invocation.

Persistent fix in `~/.zshrc`:

```bash
source "$ZSH/oh-my-zsh.sh"
unalias gc 2>/dev/null
```

The `unalias` line must come **after** Oh My Zsh loads. If it appears before
`source "$ZSH/oh-my-zsh.sh"`, the `git` plugin recreates the alias later.

Oh My Zsh also loads files in `$ZSH_CUSTOM` after built-in plugins, so this is
a good alternative:

```bash
mkdir -p ~/.oh-my-zsh/custom
printf '%s\n' 'unalias gc 2>/dev/null' > ~/.oh-my-zsh/custom/gascity.zsh
```

If you do not use Oh My Zsh git aliases, you can also remove `git` from the
`plugins=(...)` list.

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
| dolt | 1.86.2 or newer | `brew install dolt` | [releases](https://github.com/dolthub/dolt/releases) |
| bd | 1.0.0 | [releases](https://github.com/gastownhall/beads/releases) | [releases](https://github.com/gastownhall/beads/releases) |
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

Gas City requires a final Dolt 1.86.2 or newer. Older and pre-release builds
can miss the upstream GC/writer deadlock fix in dolthub/dolt commit
`ccf7bde206`, which can hang `dolt_backup sync` under heavy write load. Check
your version:

```bash
dolt version
```

Upgrade via Homebrew (`brew upgrade dolt`) or download a newer release from
[dolthub/dolt/releases](https://github.com/dolthub/dolt/releases).

## `bd` Version Too Old

Gas City requires `bd` 1.0.0 or newer. Check your version:

```bash
bd version
```

Upgrade via Homebrew (`brew upgrade beads`) or download a newer release from
[gastownhall/beads/releases](https://github.com/gastownhall/beads/releases).

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

## JSONL Archive Push Failures

The maintenance pack runs `jsonl-export` every 15 minutes to dump each bead
database to a text-diffable JSONL snapshot inside a local git repository
(the "JSONL archive"). The archive serves as a disaster-recovery backup:
if the live Dolt server loses data, the last-known-good bead graph can be
reconstructed from the archive's commit history.

### Local-only vs push mode

The archive operates in one of two modes, detected from the state of its
git remotes on every run:

- **Local-only (default).** No `origin` remote is configured. Commits are
  created and retained on the host but never leave the machine. This mode
  is safe to run indefinitely; its only limitation is that the archive is
  not backed up off-box, so a disk failure on this host loses the archive
  alongside the live Dolt data.
- **Push.** An `origin` remote is configured. Each run rebases onto
  `origin/main` and pushes new commits so the archive survives a host
  loss.

On each run `jsonl-export` logs the active mode to stderr on transitions
(e.g. after you add or remove `origin`) and re-logs it at least weekly so
that an operator reading the log file can always find the current mode.

### Enabling off-box backup

Pick a repository that only this host will push to (the archive contains
bead content and should not be shared across cities). Then:

```bash
# Create a private repo on your git host (example: GitHub via gh)
gh repo create my-city-jsonl-archive --private

# Point the archive at it
ARCHIVE=$(gc config get state_dir)/packs/maintenance/jsonl-archive
git -C "$ARCHIVE" remote add origin git@github.com:<you>/my-city-jsonl-archive.git

# Seed the remote with the existing local history
git -C "$ARCHIVE" push -u origin main
```

On the next 15-minute tick, `jsonl-export` detects the new `origin`,
logs `archive running in push mode`, and resumes pushing every run.

### Switching back to local-only

Remove the remote:

```bash
git -C "$ARCHIVE" remote remove origin
```

Re-detection is automatic on the next run — no state-file edits are
required. The next log line will read `archive running in local-only
mode`.

### Reading a `JSONL push failed [HIGH]` escalation

When push mode is active and `git push` fails `GC_JSONL_MAX_PUSH_FAILURES`
times in a row (default: 3), the mayor's inbox receives an
`ESCALATION: JSONL push failed [HIGH]` message with a body shaped like:

```
Order: mol-dog-jsonl
Archive: /path/to/archive
Consecutive failures: 3 (threshold: 3)

Last git push stderr:
<last ~20 lines of captured stderr from fetch / rebase / push>

Remediation:
- Check remote: git -C <archive> remote -v
- Verify remote is reachable and credentials are valid
- Temporarily suppress: export GC_JSONL_MAX_PUSH_FAILURES=99
- See docs/getting-started/troubleshooting.md#jsonl-archive-push-failures
```

Common root causes, in rough order of frequency:

- **Credentials rotated or expired.** SSH key removed from the remote
  host, HTTPS token expired. The captured stderr usually reads
  `Permission denied (publickey)` or `remote: Invalid username or
  password`.
- **Remote URL typo or deleted repo.** stderr reads `does not appear to
  be a git repository` or `repository not found`.
- **Network partition.** stderr reads `Could not resolve host` or a
  connection-timeout message. If the host is also firewalled from the
  rest of the internet, this will recover once connectivity returns.
- **Diverged history.** Very unusual — the archive rebases onto
  `origin/main` automatically — but if the remote was force-pushed from
  another host, rebase may fail with a conflict. Inspecting the archive
  and resolving manually is the only option.

If the underlying problem cannot be fixed immediately (e.g., the remote
host is down for scheduled maintenance), set
`GC_JSONL_MAX_PUSH_FAILURES=99` in the maintenance pack's environment and
restart the city with `gc restart`. That bumps the escalation threshold
from 3 to 99, which at the current 15-minute tick rate is ~24 hours of
silence.

## WSL (Windows Subsystem for Linux)

Gas City works under WSL 2 with a standard Ubuntu or Debian distribution.
Install prerequisites using the Linux column in the tables above. tmux
requires a working terminal — use Windows Terminal or another WSL-aware
terminal emulator.

## Build From Source Fails

Building from source requires `make` and Go 1.25 or newer:

```bash
make --version
go version
```

If `make` is missing, install it (`apt install make` on Debian/Ubuntu, or
`xcode-select --install` on macOS). If your Go version is too old, update it
from [go.dev/dl](https://go.dev/dl/) or via your package manager. Then:

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
