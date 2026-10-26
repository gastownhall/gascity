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

## "does not import required builtin pack(s)" Warning

Builtin packs compose only through explicit pinned `[imports]` in
`pack.toml` — nothing splices them into config composition implicitly.
`gc init` writes the imports (plus a matching `packs.lock`) for new cities:

```toml
[imports.core]
source = "https://github.com/gastownhall/gascity.git//internal/bootstrap/packs/core"
version = "sha:<pinned commit>"

[imports.bd]
source = "https://github.com/gastownhall/gascity.git//examples/bd"
version = "sha:<pinned commit>"
```

(The `bd` entry is written only for bd-provider cities, the default;
non-bd providers get only `core`.)

If a required import is missing — typically in a city created before the
imports became explicit — config load still self-heals the user-global pack
cache and prints a once-per-city warning:

```
warning: this city does not import required builtin pack(s) core; run "gc doctor --fix" to add the missing import(s)
```

Run the suggested fix:

```bash
gc doctor --fix
```

The `builtin-pack-imports` doctor check migrates the city to the imports
model: it strips legacy `workspace.includes` entries pointing at the retired
per-city `.gc/system/packs` tree, adds the missing pinned import(s) to
`pack.toml`, and refreshes `packs.lock` and the cache. Leftover
`.gc/system/packs` directories on disk are pruned automatically.

## `gascity-pack-binding` Doctor Warning

Cities created by gc v1.4.x import the public Gas City pack under the key
`gascity`:

```toml
[imports.gascity]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"
```

The import key namespaces the pack's commands and skills, and the pack
ecosystem is written against `gc`: the pack documents skill `gc.mayor`, and
role prompts in Gas City pack releases after 0.1.6 run `gc gc claim`. Under
`[imports.gascity]` that command is `gc gascity claim`, so role workers fail
their first claim once the pack is bumped past 0.1.6. Current `gc init` writes
`[imports.gc]`.

`gc doctor` reports this as a warning; the city keeps working at the pinned
0.1.6 pack. Run:

```bash
gc doctor --fix
```

The `gascity-pack-binding` check renames the key to `[imports.gc]` in
`pack.toml` (or in a city.toml root `[imports]` override), keeping the source,
version, and every other field. `packs.lock` is keyed by source, so no
reinstall is needed. The check identifies the pack by its source (any https,
http, or SSH spelling of `gastownhall/gascity-packs` with the `gascity` pack
directory), not by the key name, so a fork or a different pack bound as
`gascity` is left alone, and it never
touches rig imports or `[defaults.rig.imports]`.

The fix changes nothing and explains why when:

- `[imports.gc]` already imports a different pack. Rename one of the imports
  by hand.
- `[imports.gc]` already imports the Gas City pack with a different version or
  settings. Remove one of the two imports by hand. An exact duplicate (same
  source and version) is removed automatically.
- A string value in `pack.toml` or `city.toml` is shaped like a
  `gascity.`-qualified name, for example a patch targeting `gascity.<agent>`.
  The message names the file and key. Update the value to `gc.` and rerun, or
  rename the import by hand if the value is unrelated. Comments are ignored.

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
| dolt | 2.6.0 or newer | `brew install dolt` | [releases](https://github.com/dolthub/dolt/releases) |
| bd | 1.3.0 | [releases](https://github.com/gastownhall/beads/releases) | [releases](https://github.com/gastownhall/beads/releases) |
| flock | -- | `brew install flock` | `apt install util-linux` |

### Optional for GitHub gates

| Tool | macOS | Linux |
|------|-------|-------|
| gh | `brew install gh` | [cli.github.com](https://cli.github.com/) |

Gas City can run without `gh`. The core pack's maintenance orders skip
GitHub gate checks when the GitHub CLI is not installed.

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

Gas City requires a final Dolt 2.6.0 or newer. Older and pre-release builds
are below the managed bd/Dolt compatibility floor: 2.6.0 is the first Dolt
built with a Go toolchain that distrusts the compromised eMudhra CA
intermediates. Releases before 1.86.2 can also miss the upstream GC/writer
deadlock fix in dolthub/dolt commit `ccf7bde206`, which can hang
`dolt_backup sync` under heavy write load. Check your version:

```bash
dolt version
```

Upgrade via Homebrew (`brew upgrade dolt`) or download a newer release from
[dolthub/dolt/releases](https://github.com/dolthub/dolt/releases).

## `bd` Version Too Old

Gas City requires `bd` 1.3.0 or newer. `gc init` and `gc start` refuse an
older `bd` with `bd (found vX, need v1.3.0+)`, and `gc doctor`'s `check-bd`
check reports `bd X is too old (need >= 1.3.0)`.

1.3.0 is the first tested release with the `bd update --if-assignee
--if-status` conditional release, which Gas City uses to release a stale
work assignment without clobbering a concurrent re-claim. A `bd` without
those flags gets a typed "conditional assignment release unsupported" error,
and Gas City releases no stale assignment through it: work orphaned by a dead
session stays assigned to that session until you upgrade `bd`. A 1.2.x version
number is not enough: bd 1.2.0 and 1.2.1 were accidental, untested releases,
and bd 1.2.2 re-released the 1.1.2 code without the flags. Check your
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

For orchestrator-managed Gas City deployments, confirm that the orchestrator is
wrapping stores with `CachingStore` and emitting `bead.created`,
`bead.updated`, `bead.closed`, and `bead.deleted` events to the event bus. After
that migration is verified, remove the executable hook scripts from the city or
rig `.beads/hooks/` directory to allow native store adoption.

While a city still depends on those hook scripts directly, keep its stores on
the subprocess-backed store by turning native transport off in `city.toml`:

```toml
[beads]
native_transport = "off"
```

Restart a running city after the change: the stores it holds open keep the
value it read at boot. `gc` commands, and the stores the city opens for a
single tick, read the current `city.toml` every time. This setting replaces
the `GC_BEADS_FORCE_FALLBACK=1` environment variable, which still works but is
deprecated and applies to every city the process serves. With either one,
`gc start` refuses a `[storage]` binding served by `beads-workspace`, because
that provider opens the native store. Turn native transport off only in cities
that have no such binding, or remove the binding first.

## Native Store Falls Back Because Dolt Is in Embedded Mode

**Symptom:** `gc status` or `gc session list` is slower than expected, or
`gc doctor` reports `native_store_unavailable gate=dolt_mode_safe`.

Gas City's native in-process beads store requires that `bd context` reports
`dolt_mode=server`. When `bd` is configured with an embedded Dolt instance (the
default for a freshly installed `bd`), the `dolt_mode_safe` gate fails and Gas
City falls back to invoking the `bd` CLI as a subprocess for every store
operation. Each subprocess call adds tens to hundreds of milliseconds of
overhead, which accumulates noticeably during `gc status` and `gc session list`.

**Remedy:** Run `bd` against a Dolt SQL server so that `bd context` reports
`dolt_mode=server`:

```bash
# Start a local Dolt SQL server (one-time setup)
dolt sql-server --port 28231

# Confirm bd resolves server mode
bd context --json | grep dolt_mode
# expected: "dolt_mode": "server"
```

Once `bd context` reports `dolt_mode=server`, `gc doctor` will clear the
`dolt_mode_safe` gate and Gas City will use the native store automatically
on the next start.

<Note>
The `gascity_native_beads` build tag visible in some source files is a
test-only mechanism — it requires `GC_NATIVE_DOLTLITE_BEADS=true` and is
not a supported operator path. Use Dolt server mode instead.
</Note>

## `dolt_mode_safe` Preflight Gate Fails

The native in-process store is unavailable after `gc start`, and the supervisor
log shows:

```
native_store_unavailable gate=dolt_mode_safe reason="dolt_mode=embedded; native store requires Dolt server mode (bd context must report dolt_mode=server) — falling back to per-call bd. See troubleshooting."
```

`gc status --json | jq .beads` reports `"preflight_gate":"dolt_mode_safe"` with
`"native_store_eligible":false`.

The `dolt_mode_safe` gate keys off the `dolt_mode` value that `bd context`
reports; a value of `embedded` fails the gate. The gate reads this from
`bd context`, not from `.beads/config.yaml`, so hand-editing the config file is
not the supported repair. Confirm what `bd` currently reports:

```bash
bd context --json | jq .dolt_mode   # should print "server"
```

**Remedy:** This is the same native-store fallback covered under
[Native Store Falls Back Because Dolt Is in Embedded Mode](#native-store-falls-back-because-dolt-is-in-embedded-mode).
Follow that section to run `bd` against a Dolt SQL server so `bd context`
reports `dolt_mode=server`, then apply and re-check:

```bash
gc restart
```

Do not bypass or disable the `dolt_mode_safe` check — it guards the store-mode
contract that keeps `bd` and gc in agreement.

## Listings Miss Durable Beads After Changing `types.infra`

**Symptom:** after a type is added to a store's `types.infra` setting, a
running city's listings of that type return only its ephemeral beads.

The native store asks the server to filter a listing by type only for types
the server keeps durable, and it reads which types those are from the store's
stored `types.infra` setting when it opens. A running city keeps what it read
for the stores it holds open. `gc` commands, and the stores the city opens for
a single tick, read the current setting every time.

**Remedy:** change the stored setting, then restart the city:

```bash
gc bd config set types.infra "<types>"   # add --rig <name> for a rig's store
gc restart
```

Gas City reads only the stored setting. If the server of a remote beads
workspace names `types.infra` in its own `.beads/config.yaml`, put the same
types in the stored setting too.

## flock Not Found (macOS)

macOS does not ship `flock`. Install it via Homebrew:

```bash
brew install flock
```

Alternatively, switch to the file-based beads provider (see above) to skip
the flock requirement entirely.

## Cursor MCP Tools Still Prompt or Appear Unavailable

The built-in `cursor` provider starts `cursor-agent` with `-f --trust` so an
unattended worker does not stop at Cursor's workspace-trust dialog. Use it only
for workspaces whose contents you trust. The flag does not approve MCP servers;
Cursor's MCP approval prompt remains enabled by default.

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
args. Include `-f --trust` yourself and add `--approve-mcps` only for the same
explicit MCP trust decision. Agent-level `args` overrides behave the same way.

Existing Cursor sessions keep the command fingerprint they were created with.
The supervisor reconciler restarts sessions automatically after the fingerprint
changes. Drain the pool first when you need a controlled handoff rather than
waiting for the next automatic restart.

## Gas City Leaves a Project's Own MCP Config Alone

gc writes the MCP servers from your city's catalog into each provider's
project config: `.mcp.json`, `.codex/config.toml`, `.gemini/settings.json`,
`.cursor/mcp.json`, `opencode.json`, or `mimocode.json`, at the city root,
each rig root, and agent work dirs. When one of those files already holds MCP
servers the catalog does not define, or defines a server differently, such as
a team's `.mcp.json` committed to a rig's repository, gc does not take it
over, since that would replace the team's servers. It leaves the file as it
is and warns:

```text
gc: warning: agent "frontend/coder" starts without gc's MCP server "notes": not adopting /src/frontend/.mcp.json: gc does not manage it yet, and adopting it would drop MCP server "team-db"; to let gc manage it, define the servers you still need in gc's MCP catalog (an mcp/ directory) and remove those entries from the file
```

The agents that read that file keep the team's servers and start without
gc's. To see where this applies:

- `gc doctor` lists every such file under `mcp-adoption`.
- `gc start --dry-run` shows a file at the city root or a rig root as
  `leave ... as it is`, and warns about a file in an agent's own work dir.
- `gc mcp list --agent <name>`, or `--session <id>` for a pool or worktree
  session, says "Not delivered" for an agent that reads one.
- The supervisor log shows the warning when the city starts, and when gc
  next resolves an agent whose own work dir holds such a file, such as a
  session's checkout of a repository that commits its own `.mcp.json`. It
  shows it once per agent, and again if the file's content changes or the
  file comes back after gc saw it gone.

gc also leaves alone a file it cannot read or parse, a symlinked one, and
one that is not a regular file (a named pipe, a device) or is larger than
4 MiB, which it does not read at all.

To give those agents gc's servers too, define the file's servers you still
need in an `mcp/` catalog that reaches every agent reading the file, one
`<name>.toml` per server, and remove the file's own entries:

- A file at the city root or a rig root is shared by every agent of that
  provider there, including the one gc adds for each declared provider.
  Use the city's `mcp/`, or for a rig root, a pack the rig imports.
- A file in one agent's own work dir can also take the agent's own
  `agents/<name>/mcp/`.

gc adopts the file the next time it projects there: at the next city start
for the city and rig roots, and at the agent's next session start for its
own work dir. Editing the file does not restart anything. On
adoption, gc saves the original under `.gc/mcp-adopted/` and writes every
server from the catalog.

If the repository tracks the file, think twice before letting gc adopt it.
gc's version can hold secrets expanded from `env` or `headers`, and it shows
up as a change to the tracked file; `.gitignore` does not hide changes to
tracked files. gc does not ask before it adopts a tracked file that holds
no servers of its own, such as a committed `opencode.json` or
`.gemini/settings.json` with only settings. It notes the adoption on stderr,
adds gc's servers to the file, keeps its other settings, and rewrites its
formatting (comments in a `.codex/config.toml` are lost).

gc starts Claude with project MCP servers pre-approved, so the servers in a
`.mcp.json` gc leaves alone run in its Claude agents without a trust prompt,
as they do in a city with no MCP catalog. Review a repository's MCP config
before you add it as a rig.

`gc doctor` also reports files an earlier gc took over while they held
servers the catalog does not define. Their definitions are in the snapshot
it names under `.gc/mcp-adopted/`. To hand such a file back, stop the city
(`gc stop`), restore the file from the snapshot, and delete the marker gc
keeps for it, `.gc/mcp-managed/<provider>.json` at the same root; gc then
leaves it alone. Deleting the snapshots clears the report.

## An Agent Starts Without an MCP Server

The supervisor log (`gc supervisor logs`) shows a warning like this one when
the city starts or reloads its config; `gc start --dry-run` prints it too.
`gc start` and `gc reload` print it as well, but they work it out on your
shell's `PATH`, which can differ from the supervisor's. If they disagree, for
example about a provider that is not installed, the supervisor log is right:

```text
gc: warning: city "my-city": agents "cursor" (the implicit agent for provider "cursor"), "editor" start without MCP server "search" (http, from import "search"): gc projects only stdio MCP servers for the "cursor" harness
```

An implicit agent is one gc adds for every provider the city declares, so
work can be slung to that provider without defining an agent; the warning
names its provider because you never wrote it.

MCP servers from the city's `mcp/` directory, a rig pack, or an import apply
to every agent in that scope. When an agent's provider cannot receive one of
them, Gas City leaves that server out of the agent's MCP config and starts the
agent without it. Every other agent starts as usual.

| Provider family | Receives |
|---|---|
| `claude`, `codex`, `gemini`, `opencode`, `mimocode` | stdio and HTTP servers |
| `cursor` | stdio servers only |
| any other provider, such as `copilot` | no MCP servers |

Agents whose sessions use ACP receive every server through the protocol
itself, whatever their provider family.

`gc mcp list --agent <name>` lists the skipped servers under **Not
projected**. `gc doctor`, `gc config show --validate`, and `gc import add`
report the same warning, so you can see it before the city starts.

If the agent does not need the server, nothing needs to change. If it does,
switch the agent to a provider that can receive it.

A server in an agent's own `agents/<name>/mcp/` directory is meant for that
agent alone. If the agent's provider cannot receive it, that agent does not
start, and the warning says `cannot start`; the rest of the city still starts.

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
`OPENAI_`, `GEMINI_`), a short built-in list of settings (among them the Dolt
credential and logging settings `GC_DOLT_USER`, `GC_DOLT_PASSWORD`, and
`GC_DOLT_LOGLEVEL`, and the beads pool deadlines below), plus any keys you opt
in via `GC_SUPERVISOR_ENV`; any other key in the file is ignored. A value
exported in the calling shell still takes precedence over the file, and
`GC_SUPERVISOR_OMIT_PROVIDER_CREDS=1` suppresses provider credentials from both
sources.

The same file is the durable home for the beads connection-pool deadlines.
`gc` reads `BEADS_DOLT_POOL_READ_TIMEOUT` and `BEADS_DOLT_POOL_WRITE_TIMEOUT`
for its own store connections only from its process environment; the
`dolt.pool-*-timeout` keys in `.beads/config.yaml` and entries in `.beads/.env`
reach the `bd` CLI, not `gc`. If the 10-second default is too tight for a
loaded shared Dolt server (store operations fail with `i/o timeout` or
`invalid connection`), raise both here:

```bash
# ~/.gc/secrets.env
BEADS_DOLT_POOL_READ_TIMEOUT=90s
BEADS_DOLT_POOL_WRITE_TIMEOUT=90s
```

These values also reach the `bd` commands and agent sessions that inherit the
supervisor's environment, where they take precedence over the per-workspace
`.beads/` settings.

Apply the change by regenerating the service file:

```bash
gc supervisor install  # rewrites the service file; restarts on change
```

## A Custom Environment Variable Doesn't Reach Agent Sessions

Symptom: a non-`GC_`-prefixed variable you've exported and confirmed is set
(e.g. in your shell, `~/.bash_env`, or a systemd/launchd unit) never shows up
inside a spawned agent session's environment, even though `gc supervisor
run` itself can see it.

Cause: `passthroughEnv` only forwards a variable into a session if it is
either `GC_`-prefixed, part of the small fixed provider/locale/XDG set, or
named in `GC_SUPERVISOR_ENV` — the same opt-in `gc supervisor install` uses to
widen the persisted service-file env (see above). Everything else is dropped
silently, by design: an unbounded sweep of the calling environment would leak
whatever secrets happen to be sitting in the supervisor's process, into every
agent session.

Fix: name the variable in `GC_SUPERVISOR_ENV` in the environment the
supervisor daemon itself runs with, then restart it so the daemon process
picks up both the opt-in list and the variable's value:

```bash
export GC_SUPERVISOR_ENV=GC_SUPERVISOR_ENV,MY_CUSTOM_VAR   # the list names itself so it survives restarts; comma or space separated
export MY_CUSTOM_VAR=/path/to/thing
gc supervisor install   # regenerates the service file with both persisted
gc supervisor stop && gc supervisor start   # restart the supervisor so it inherits them
```

Sessions that are already running keep their old environment; restart them to
receive a newly forwarded variable.

`GC_SUPERVISOR_ENV` itself needs to be present in the supervisor daemon's own
environment for this to survive a later restart — it is not automatically
persisted into the generated service file the way `PATH`/`GC_HOME` are. If
you rely on a managed service file, either list `GC_SUPERVISOR_ENV` among its
own opted-in names (`GC_SUPERVISOR_ENV=GC_SUPERVISOR_ENV,MY_CUSTOM_VAR`) or
set it directly in the unit's `Environment=` lines.

For a value that's the same on every city, `[workspace.env]` in `city.toml` is
usually simpler than an opt-in — it doesn't depend on the supervisor's own
process environment at all:

```toml
[workspace.env]
MY_CUSTOM_VAR = "/path/to/thing"
# or, to read it from whatever the supervisor's own environment holds:
MY_CUSTOM_VAR = "$MY_CUSTOM_VAR"
```

## Supervisor Log Written Twice (journald + supervisor.log)

`gc supervisor run` tees its output into `${GC_HOME}/supervisor.log`
(defaults to `~/.gc/supervisor.log`) so `gc supervisor logs` works no matter
how the supervisor was started. Under a hand-managed systemd unit with
`StandardOutput=journal`, that tee becomes a second copy of every line:
journald keeps one, and `supervisor.log` grows without rotation.

Set `GC_SUPERVISOR_LOG_TEE=0` in the supervisor's environment to disable the
tee so the service manager's log is the single sink. Only the literal value
`0` disables it; any other value (or unset) keeps the default tee.

```ini
# hand-managed ~/.config/systemd/user/gascity-supervisor.service
[Service]
StandardOutput=journal
StandardError=journal
Environment=GC_SUPERVISOR_LOG_TEE=0
```

Scope and caveats:

- **The variable matters in two places.** The supervisor process's
  environment controls the tee. The shell running `gc supervisor logs`
  controls only what that command reports: when the variable is set there
  and `supervisor.log` exists, the file is tailed with a staleness warning;
  when the file is absent, the command points at the service manager's log
  (`journalctl --user -u gascity-supervisor.service` on Linux) instead. A
  unit's `Environment=` lines are invisible to your interactive shell, so
  export the variable in both for coherent behavior.
- **Service files generated by `gc supervisor install` or `gc start` do not
  need — and do not honor — the opt-out.** Generated units redirect
  supervisor output straight into `supervisor.log` (systemd
  `StandardOutput=append:`, launchd `StandardOutPath`), and the tee already
  suppresses itself when its output is that same file, so `supervisor.log`
  is the single sink in those shapes. The variable is not captured into
  generated service files automatically; it exists for units you manage by
  hand.
- **To persist the variable into a generated service file anyway** — for
  example as a starting point you then hand-edit to
  `StandardOutput=journal` — opt it in explicitly and regenerate:

  ```bash
  export GC_SUPERVISOR_LOG_TEE=0
  GC_SUPERVISOR_ENV=GC_SUPERVISOR_LOG_TEE gc supervisor install
  ```

  Note that `gc start` regenerates the service file with the file-redirect
  defaults, so a hand-edited unit at gc's service path stays journal-only
  only on hosts where gc never manages the unit.

## Delegating the Supervisor Lifecycle to an Operator-Managed systemd Unit

By default `gc` owns the supervisor lifecycle: `gc start` installs and
starts a per-user service (`gascity-supervisor`), and binary-drift
detection restarts that service directly. Hosts that run the supervisor
under an operator-managed systemd unit instead — for example a hardened
system service with its own restart policy — can delegate the lifecycle:

```bash
GC_SUPERVISOR_SYSTEMD_UNIT=gascity-prod.service  # unit that owns the supervisor
GC_SUPERVISOR_SYSTEMD_SCOPE=system               # "system" (default) or "user"
```

With the unit configured:

- `gc supervisor start` and the `gc start` ensure path run
  `systemctl [--user] start <unit>` (bounded, so a wedged unit cannot
  hold the CLI indefinitely) and wait for the control socket to answer.
  When the socket stays unreachable — the usual situation for a
  system-scope unit running under a different user — start falls back
  to the same liveness evidence `gc supervisor status` trusts: an
  active unit, then the supervisor HTTP API. Only when all three are
  silent does start fail. gc never writes, loads, or daemon-reloads its
  own service files in delegated mode; `gc supervisor install` refuses
  to run, and `gc supervisor uninstall` only removes gc's own legacy
  service.
- `gc supervisor stop` runs `systemctl [--user] stop <unit>`
  synchronously, bounded by `--wait-timeout` (default 30s) whether or
  not `--wait` is set, then verifies a previously-running supervisor
  actually exited. A live supervisor the unit does not manage (common
  mid-migration) fails the stop with its PID instead of reporting a
  false "Supervisor stopped.", and stop with nothing running keeps the
  legacy exit-1 "supervisor is not running" contract.
- The `gc start` drift auto-restart runs `systemctl try-restart <unit>`
  (a unit the operator stopped stays stopped) and fails unless the
  restart verifiably resolved the drift: a supervisor that was not
  replaced, a replacement still serving the drifted build (the unit's
  `ExecStart` launches a stale binary), or an unverifiable post-restart
  probe each fail instead of declaring "ready" while a stale supervisor
  keeps serving.
- `gc supervisor status` probes the delegated unit
  (`systemctl [--user] is-active <unit>`) when the control socket is
  unreachable — the usual situation for a system-scope unit running
  under a different user — and reports a broken delegation config (a
  warning in text mode, a `config_error` field in `--json`) instead of
  a bare "not running".

An invalid `GC_SUPERVISOR_SYSTEMD_SCOPE` value is a hard error on every
lifecycle path; gc never silently falls back to the default unit.
Setting `GC_SUPERVISOR_SYSTEMD_UNIT` on a non-Linux platform is the
same kind of hard error — delegation is a systemd contract.

## Supervisor Does Not Come Back After Logout (macOS launchd Agent Disabled)

Symptom: on macOS the supervisor is running, but it does not come back after
logout or reboot, and `gc supervisor status` prints a warning on stderr:

```text
gc supervisor status: warning: launchd agent com.gascity.supervisor is disabled; it will not start after logout or reboot. Run 'gc supervisor stop --wait' then 'gc supervisor start' to put it back under launchd
```

Cause: `gc supervisor stop` disables the launchd agent so the stopped
supervisor stays stopped, even across logins. That override is persistent: it
survives logout and reboot, and only `launchctl enable` clears it. A
supervisor started any other way afterwards, such as a foreground
`gc supervisor run`, passes every liveness check while launchd would never
restart it. An installed agent that is not loaded has the same effect, and
status warns about it too (`... is not loaded; it will not restart if the
supervisor exits`). To see the override yourself:

```bash
launchctl print-disabled "gui/$(id -u)" | grep com.gascity.supervisor
```

The warning leaves the liveness report on stdout and the exit code unchanged.
With `--json`, status adds `"supervised": false` and a `supervision_warning`
string carrying the same message. Both keys are absent when launchd is
supervising, and also when status cannot tell: no agent is installed, its
plist cannot be read, or launchd does not answer.

Fix: run the command the warning names. It depends on what status found:

| Status found | Remedy |
|---|---|
| The agent runs this `gc`, and no supervisor is running | `gc supervisor start`, which enables, loads, and starts the agent |
| The agent runs this `gc`, and a supervisor is running | `gc supervisor stop --wait`, then `gc supervisor start` |
| The agent runs a different `gc` binary | `<this gc's path> supervisor install --force`, after `gc supervisor stop --wait` if a supervisor is running |

Use `stop --wait`: a plain `gc supervisor stop` returns while the supervisor
is still shutting down, and `gc supervisor start` refuses while a supervisor
is running. A plain `gc supervisor install` is not a substitute either: when
the service file is already current and a supervisor is running, it reports
success without enabling the agent.

## JSONL Archive Push Failures

The core pack runs `jsonl-export` every 15 minutes to export each bead
store (the city and every rig) with `bd export` into a text-diffable JSONL
snapshot inside a local git repository (the "JSONL archive"): one issue per
line, with its labels, dependencies and comments. The archive serves as a
disaster-recovery backup: a snapshot from any commit restores with
`gc bd import <file>`.

`jsonl-export` (every 15 minutes) and `reaper` (every 30 minutes) ship in
the core pack, so they are active in every city by default — including
cities that previously ran them only via the opt-in gastown maintenance
pack. Both reach every bead store through `gc bd`, so they work the same on
bd-owned proxied, gc-managed and mixed cities. On cities whose beads
provider is not bd (for example `[beads] provider = "file"`), both orders
skip with a one-line message and an `order.skipped` event instead of
running. To turn them off entirely, skip them by name in `city.toml`:

```toml
[orders]
skip = ["jsonl-export", "reaper"]
```

Cities that had skipped the old formula orders (`mol-dog-jsonl`,
`mol-dog-reaper`) stay opted out; the renamed orders honor the legacy
skip entries.

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

### Archive object maintenance

Every snapshot commit creates loose Git objects, including in local-only
mode. The exporter checks the archive before each export and after each new
commit. It runs a foreground, low-priority `git gc` when either of these
defaults is reached:

- `GC_JSONL_GC_LOOSE_OBJECTS=2000`
- `GC_JSONL_GC_LOOSE_KB=262144` (256 MiB)

Packing is deliberately single-threaded and uses
`GC_JSONL_GC_WINDOW_MEMORY=256m` (a positive byte count, without leading
zeros, with an optional `k`, `m`, or `g` suffix in either case) to avoid
competing with the Dolt server and worker sessions for memory. These values
can be lowered for small hosts.

Packing writes a new copy of the archive's objects before it deletes the old
ones. It starts only when the archive's filesystem can hold that copy and
still keep `GC_JSONL_GC_MIN_FREE_KB=512000` (500 MiB) free; otherwise the
order fails with a message giving the free space. The check counts loose
objects at their full size, though snapshots usually pack to a small fraction
of it: one archive's 9.01 GiB of loose objects packed to 67.40 MiB. So an
archive with a large backlog of loose objects can be refused on every run
even when packing it would fit. To recover, pack it once by hand with the
command the failure message prints (or the lower-memory one below), free disk
space, or set `GC_JSONL_GC_MIN_FREE_KB=0` to disable the check. If `df`
cannot report the free space, packing goes ahead with a warning.

An invalid setting fails the order before it exports anything. A refused
free-space check or a failed `git gc` fails the order visibly, and later runs
export no new snapshots until packing succeeds. Snapshots already committed
still reach `origin`: a run packs only after its push, and a run that finds
the archive cannot be packed pushes any waiting snapshot before it stops. A
spike halt sends its alert, or saves it for retry, before it packs.

To inspect the archive without changing it:

```bash
# The default location. GC_JSONL_ARCHIVE_REPO overrides it, and cities
# migrated from the gastown maintenance pack keep their legacy path.
ARCHIVE="$(gc status --json | jq -r '.city_path')/.gc/runtime/packs/core/jsonl-archive"
git -C "$ARCHIVE" count-objects -vH
git -C "$ARCHIVE" fsck --connectivity-only
```

To pack it once by hand with less memory:

```bash
git -C "$ARCHIVE" -c pack.windowMemory=64m -c pack.threads=1 gc
```

### Enabling off-box backup

Pick a repository that only this host will push to (the archive contains
bead content and should not be shared across cities). Then:

```bash
# Create a private repo on your git host (example: GitHub via gh)
gh repo create my-city-jsonl-archive --private

# Point the archive at it (run from anywhere inside your city)
ARCHIVE="$(gc status --json | jq -r '.city_path')/.gc/runtime/packs/core/jsonl-archive"
git -C "$ARCHIVE" remote add origin git@github.com:<you>/my-city-jsonl-archive.git

# Seed the remote with the existing local history
git -C "$ARCHIVE" push -u origin main
```

On the next 15-minute tick, `jsonl-export` detects the new `origin`,
logs `archive running in push mode`, and resumes pushing every run.

On cities migrated from the gastown maintenance pack, the archive stays
at its legacy location — `.gc/runtime/packs/maintenance/jsonl-archive`
(or `.gc/jsonl-archive` for pre-pack cities) — and the
`packs/core/jsonl-archive` path above does not exist. Point `ARCHIVE` at
the legacy path instead; `gc doctor` reports the resolved archive path
for the city.

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
times in a row (default: 3), the default human escalation mailbox receives an
`ESCALATION: JSONL push failed [HIGH]` message with a body shaped like:

```
Order: jsonl-export
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

### Maintenance escalation and completion routing

Core maintenance scripts route alerts through a generic escalation hook
instead of mailing a hardcoded role. Orders inherit the orchestrator's
environment, so set these at orchestrator start to customize routing:

- `GC_ESCALATION_RECIPIENT` — mail recipient for escalations (default:
  `human`, the reserved human mailbox). An agent recipient is woken with
  `--notify`; the `human` default is a mailbox with no session behind it,
  so nothing is woken and the escalation waits until somebody reads that
  inbox. Point this at the agent that surfaces alerts (the manager, on a
  Slack-connected city) if you want maintenance advisories acted on rather
  than filed.
- `GC_ESCALATE_SCRIPT` — absolute path to an escalation script to run
  instead of searching packs.
- `GC_ESCALATE_SEARCH_PACKS` — space-separated pack names searched (in
  order) for an `assets/scripts/escalate.sh` override (default:
  `gastown maintenance bd core`). A pack earlier in the list wins.
- `GC_ESCALATE_SEND_TIMEOUT_SECS` — wall-clock bound on one escalation
  send (default: 30). The wake can outlive the send it follows, and
  escalations run inline in maintenance orders, so the bound keeps a slow
  wake from stalling the run that raised the alarm. `gc mail send` prints
  `Sent message` once the mail is written, before the wake. A bound that
  trips after that line costs at most the wake, never the message, and the
  script still exits 0. A bound that trips before it means the mail may not
  exist, for any recipient: the script exits non-zero and the next run
  sends again.
- `GC_ESCALATE_REMINDER_SECONDS` — how long an unchanged incident stays
  quiet before it is mailed again (default: 86400, one day). See
  [Why a maintenance alert did not repeat](#why-a-maintenance-alert-did-not-repeat).
- `GC_ESCALATE_INCIDENT_DIR` — where incident state is kept (default:
  `<city>/.gc/incidents`).
- `GC_ESCALATE_NOW_EPOCH` — test-only: pins the script's clock. Leave it
  unset; a fixed clock never reaches the reminder interval, so an unchanged
  incident is never mailed again.
- `GC_MAINTENANCE_DONE_TARGET` — session target to nudge with
  `MAINTENANCE_DONE:`/warn summaries when a maintenance run completes
  (default: unset, no completion nudge). Deployments that relied on the
  old hardcoded completion nudges to a health-patrol session should set
  this to restore that loop.

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
`GC_JSONL_MAX_PUSH_FAILURES=99` in the orchestrator's environment and
restart the city with `gc restart`. That bumps the escalation threshold
from 3 to 99, which at the current 15-minute tick rate is ~24 hours of
silence.

### Why a maintenance alert did not repeat

A maintenance order that keeps finding the same problem would otherwise mail
it on every run: 48 times a day for the 30-minute `reaper` order. So two
orders raise their failures as **incidents**: an alert that is unchanged since
it was last mailed stays quiet for `GC_ESCALATE_REMINDER_SECONDS` (default:
one day). A change in what failed mails at once, and a clean run resolves the
incident, so the next failure mails at once too.

| Order | Incident key | Mails again sooner when |
| --- | --- | --- |
| `mol-dog-backup` | `dolt-backup:required-sync` | the failing databases or their diagnostics change |
| `reaper` | `reaper:anomalies` | the anomalies change; a stale backup that only grows older does not count |

A quiet run says so in its output:

```
escalate: incident reaper:anomalies unchanged since 2026-10-08T09:00:00Z; next reminder after 2026-10-09T09:00:00Z
```

The backup order's missing-`flock` and offsite-publication alerts are not
incidents and still mail on every run.

Deduplication is best-effort. When the incident state cannot be read or
written, the alert is mailed anyway, and a send that fails is not recorded,
so the next run sends it again. To make the next run mail an incident at
once, delete its state file. From the city directory:

```bash
# Forget one incident...
rm -f "$(grep -l '^key=reaper:anomalies$' .gc/incidents/*.state)"
# ...or all of them.
rm -f .gc/incidents/*.state
```

A pack that overrides `escalate.sh` receives `--incident-key KEY
--fingerprint F` on an alert and `--resolve-incident KEY` on a clean run. The
override must accept or ignore those flags and must send nothing for
`--resolve-incident`. An override written before incident keys, which rejects
the flags as a usage error (exit 2, or an error that names the flag), still
alerts: the order sends the alert again without the flags, so it is mailed on
every run instead of deduplicated. The order's output then names the override
as one that predates incident keys; run the order by hand to see it, for
example `gc order run reaper`.

## WSL (Windows Subsystem for Linux)

Gas City works under WSL 2 with a standard Ubuntu or Debian distribution.
Install prerequisites using the Linux column in the tables above. tmux
requires a working terminal — use Windows Terminal or another WSL-aware
terminal emulator.

## Build From Source Fails

Building from source requires `make` and Go 1.27.3 or newer:

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

If a symptom only makes sense once you know how the pieces fit together, see
[The six primitives](/getting-started/how-gas-city-works) for the underlying model.

Open an issue at
[gastownhall/gascity/issues](https://github.com/gastownhall/gascity/issues)
with the output of `gc doctor --verbose` and your OS/architecture.
