---
title: Troubleshooting
description: Common installation and setup issues and how to fix them.
---

<Note>
If `gc start` fails after install, use the
[`gc start` failure walkthrough](/troubleshooting/gc-start-walkthrough) to
match the final `FATAL:` line to the likely cause and resolution.
</Note>

## Run the Built-in Doctor

`gc doctor` checks your city for structural, config, dependency, and runtime
issues. It is always the best first step:

```bash
gc doctor
gc doctor --verbose   # extra detail
gc doctor --fix       # attempt automatic repairs
```

## Add City-Local Doctor Checks

Use `[[doctor.check]]` in `city.toml` for a workspace-specific health check
that does not need to be packaged as a reusable pack doctor. Provide the bare
check name; `gc doctor` adds the `local:` prefix in output.

```toml
[doctor]

[[doctor.check]]
name = "gopath-symlink"
description = "Verify the GOPATH symlink used by local build scripts"
script = "scripts/check-gopath.sh"
fix = "scripts/fix-gopath.sh"
```

The `script` and optional `fix` paths are relative to the city root. Absolute
paths and paths that escape the city directory are rejected and reported as
named `StatusError` check results.

Local checks reuse the same script protocol as pack doctor checks:

| Exit code | Result |
|-----------|--------|
| 0 | OK |
| 1 | Warning |
| 2 or higher | Error |

The first stdout line becomes the check message. Additional stdout lines are
shown by `gc doctor --verbose`.

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
| dolt | 2.1.0 or newer | `brew install dolt` | [releases](https://github.com/dolthub/dolt/releases) |
| bd | 1.0.0 | [releases](https://github.com/gastownhall/beads/releases) | [releases](https://github.com/gastownhall/beads/releases) |
| flock | -- | `brew install flock` | `apt install util-linux` |

### Optional for GitHub gates

| Tool | macOS | Linux |
|------|-------|-------|
| gh | `brew install gh` | [cli.github.com](https://cli.github.com/) |

Gas City can run without `gh`. Maintenance skips GitHub gate checks when the
GitHub CLI is not installed.

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

Gas City requires a final Dolt 2.1.0 or newer. Older and pre-release builds
are below the managed bd/Dolt compatibility floor; releases before 1.86.2 can
also miss the upstream GC/writer deadlock fix in dolthub/dolt commit
`ccf7bde206`, which can hang `dolt_backup sync` under heavy write load. Check
your version:

```bash
dolt version
```

Upgrade via Homebrew (`brew upgrade dolt`) or download a newer release from
[dolthub/dolt/releases](https://github.com/dolthub/dolt/releases).

## `bd` Version Too Old

Gas City requires `bd` 1.0.0 or newer. The bd-backed store relies on wisps
support, including `bd create --ephemeral` and `bd query ephemeral=true`, so
older binaries can fail order-tracking and wisp cleanup paths. Check your
version:

```bash
bd version
```

Upgrade via Homebrew (`brew upgrade beads`) or download a newer release from
[gastownhall/beads/releases](https://github.com/gastownhall/beads/releases).

## Native Store Falls Back Because Hooks Are Installed

Native `bd` store selection intentionally falls back to the subprocess-backed
store when executable `.beads/hooks/on_create`, `.beads/hooks/on_update`, or
`.beads/hooks/on_close` scripts are present. Those hooks historically emitted
bead events for external `bd` writes; the native in-process store does not run
shell hooks.

For controller-managed Gas City deployments, confirm that the controller is
wrapping stores with `CachingStore` and emitting `bead.created`,
`bead.updated`, `bead.closed`, and `bead.deleted` events to the event bus. After
that migration is verified, remove the executable hook scripts from the city or
rig `.beads/hooks/` directory to allow native store adoption. Keep
`GC_BEADS_FORCE_FALLBACK=1` set when a deployment still depends on those hook
scripts directly.

## flock Not Found (macOS)

macOS does not ship `flock`. Install it via Homebrew:

```bash
brew install flock
```

Alternatively, switch to the file-based beads provider (see above) to skip
the flock requirement entirely.

## Cursor MCP Tools Still Prompt or Appear Unavailable

The built-in `cursor` provider starts `cursor-agent` with `-f` and leaves
Cursor's MCP approval prompt enabled by default. This avoids silently approving
user or global MCP servers that Cursor can also see through `~/.cursor/mcp.json`.

For unattended Cursor pool workers, opt in only after confirming that every
workspace and user/global MCP server visible to Cursor is trusted. The
`--approve-mcps` flag approves every visible server, including servers projected
from Gas City's catalog into `.cursor/mcp.json` and servers from
`~/.cursor/mcp.json`.

```toml
[providers.cursor.option_defaults]
mcp_approval = "approve"
```

If you override Cursor `args` directly, the override replaces the built-in
args. Include `-f` yourself and add `--approve-mcps` only for the same explicit
trust decision. Agent-level `args` overrides behave the same way.

Existing Cursor sessions keep the command fingerprint they were created with.
The supervisor reconciler restarts sessions automatically after the fingerprint
changes. Drain the pool first when you need a controlled handoff rather than
waiting for the next automatic restart.

## `gc version` Prints Unexpected Output

If `gc version` prints git progress lines (`Enumerating objects...`) instead
of a clean version string, upgrade to Gas City v0.13.4 or later. This was a
bug where remote pack fetches wrote git sideband output to the terminal,
fixed in [PR #141](https://github.com/gastownhall/gascity/pull/141).

## Provider Credentials Dropped When the Supervisor Starts

Symptom: agents authenticate fine when you launch a city from your normal
interactive shell, but fail to authenticate (or silently fall back to a
different provider) when the city is started by the supervisor at login or
after a reboot.

Cause: the supervisor service file (launchd plist / systemd unit) captures
provider credentials by snapshotting the environment of the shell that ran
`gc start` (or `gc supervisor install`). A credential that is only present
in an interactive shell — for example sourced from an rc file that the login
service manager never reads — is not in that snapshot, so it never reaches
the supervised process.

Fix: put the durable credentials in a machine-local secrets file at
`${GC_HOME}/secrets.env` (defaults to `~/.gc/secrets.env`). On every service
file regeneration, `gc` merges this file into the supervisor environment, so
the value survives a reboot regardless of which shell ran `gc start`.

```bash
# ~/.gc/secrets.env  (chmod 600)
ANTHROPIC_API_KEY=sk-ant-...
OPENAI_API_KEY=sk-...
```

The file uses dotenv syntax: `KEY=VALUE` per line, `#` comments, blank lines,
an optional `export ` prefix, and optional surrounding quotes. Only keys that
are already eligible for the supervisor environment are merged — provider
credentials (recognized by their standard prefixes such as `ANTHROPIC_`,
`OPENAI_`, `GEMINI_`) plus any keys you opt in via `GC_SUPERVISOR_ENV`; any
other key in the file is ignored. A value exported in the calling shell still
takes precedence over the file, and `GC_SUPERVISOR_OMIT_PROVIDER_CREDS=1`
suppresses provider credentials from both sources.

Apply the change by regenerating the service file:

```bash
gc service restart     # restarts the launchd/systemd service
```

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

# Point the archive at it (run from anywhere inside your city)
ARCHIVE="$(gc status --json | jq -r '.city_path')/.gc/runtime/packs/maintenance/jsonl-archive"
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
mode`. If push mode had accumulated failures before the remote was
removed, local-only detection clears that stale failure counter while
retaining `pending_archive_push` so deferred commits are still pushed if
`origin` returns.

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

Transient ref-update races are retried before the escalation counter is
incremented. By default, each retry sleeps for a random delay from 1 to 5
seconds. Set `GC_JSONL_PUSH_RETRY_DELAY_MIN` to change the lower bound and
`GC_JSONL_PUSH_RETRY_DELAY_SPAN` to change the random span added above that
minimum.

The exporter sends one HIGH escalation for a still-unresolved push
failure. It continues recording `consecutive_push_failures` and
`pending_archive_push` in state, but does not mail the same failure on
every tick. A successful push or a switch back to local-only mode clears
the escalation marker.

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

## Slung Beads Not Reaching Agents (managed-city mode)

If `gc sling` accepts work but agents don't process it — especially if
your supervisor log shows `rigStores=0` or `assignedWorkBeads=0`, or
your `bd dolt set port` edits keep reverting at the next `gc start` —
you're likely looking at a rig whose Dolt view has drifted from the
managed city Dolt. Do **not** edit `.beads/dolt-server.port` or
`bd dolt set port` directly; both self-revert.

See the
[Managed-city Dolt endpoints runbook](/runbooks/managed-city-endpoints)
for the mental model, the forbidden edits, the sanctioned escape
hatches (`gc rig set-endpoint --inherit`/`--self --force`/`--external`),
and an end-to-end recovery recipe.

## Still Stuck?

Open an issue at
[gastownhall/gascity/issues](https://github.com/gastownhall/gascity/issues)
with the output of `gc doctor --verbose` and your OS/architecture.
