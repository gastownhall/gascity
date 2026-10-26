---
title: "Exec Session Provider"
---

Gas City's exec session provider delegates each `runtime.Provider` operation
to a user-supplied script. This allows any terminal multiplexer or process
manager to be used as a session backend without writing Go code.

## Usage

Set the `GC_SESSION` environment variable to `exec:<script>`:

```bash
# Absolute path
export GC_SESSION=exec:/path/to/gc-session-screen

# PATH lookup
export GC_SESSION=exec:gc-session-screen
```

## Pack-Declared Runtimes

A pack can ship (or install) a runtime executable and bind it to a
selection name in `pack.toml`:

```toml
[runtimes.cloudflare]
command = "scripts/gc-runtime-cloudflare"   # pack-relative, or PATH name
protocol = 0
```

City composition registers the name into the runtime selection
registry, so `city.toml` selects it like a builtin:

```toml
[session]
provider = "cloudflare"
```

Rules:

- A `command` containing a path separator resolves relative to the pack
  directory; a bare name resolves on PATH at session start.
- `protocol` declares the RPP version the executable speaks (version 0
  is the only version today); any other value fails composition.
- Name collisions with builtin runtimes or other packs are composition
  errors — no silent shadowing. Identical re-declarations of the same
  pack reached through a diamond import graph dedupe.
- The `pack-runtimes` doctor check verifies each declared executable is
  installed and that its RPP handshake works. An executable with no
  `protocol` op is the version-0 floor and passes; a present-but-broken
  handshake fails.
- Config reload enforces the same registration rules, and rebuilds the
  session provider when the declaration behind the selected name changes
  (the executable binding is fixed at provider construction).
- `gc runtime check <name>` and `gc runtime conformance <name>` both
  resolve the declared name and run against the pack's executable:
  `check` is the smoke test, `conformance` the full requirement-coded
  suite.

## Calling Convention

The script receives the operation name as its first argument:

```
<script> <operation> <session-name> [args...]
```

No shell invocation — the script is exec'd directly.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Failure (stderr contains error message) |
| 2 | Unknown operation (treated as success — forward compatible). From `start`, and from an operation the script declares, it is not success; see below. |
| 75 | `start` only: temporary failure (`EX_TEMPFAIL`). The endpoint the agent launches against is at capacity or unavailable, so the start is retryable and not specific to this session. Gas City still tears the box down. On any other operation 75 is an ordinary failure. |

Exit code 2 is the forward-compatibility mechanism. When Gas City adds new
operations in the future, old scripts return exit 2 and the provider treats
it as a no-op success. Scripts only need to implement the operations they
care about. An operation a script declares in its
[handshake](#protocol-handshake-protocol) cannot be unknown, so declared
operations do not read exit 2 as success: under `proc.exec`, `exec` returns it
as the command's own exit code, and under `proc.provision` and
`report-attachment`, `provision` and `is-attached` report it as a failure.
`start` cannot be unknown either, so its exit 2 (for example a helper's
`grep` or `jq` exit status escaping a `set -e` script) is a failed start.

## Cancellation

Each invocation runs in its own process group. When an operation is canceled
(its timeout expires, or Gas City abandons the call), the script is stopped in
two steps:

| Step | Signal | Sent to | What the script should do |
|------|--------|---------|---------------------------|
| 1 | `SIGTERM` | the whole process group (the script and any foreground child) | Roll back anything this invocation created, then exit |
| 2 | `SIGKILL` | the script, after a 2-second grace period | Nothing; it cannot be caught |

A canceled call is reported as canceled whatever the script's exit code, so a
`TERM` handler may exit with any status, including 2.

Trap `TERM` (or `EXIT`, which bash also runs when `TERM` ends the script) to
clean up. `SIGINT` is not sent: a script that traps only `INT` gets no chance
to roll back. Keep the rollback inside the grace period; prefer one bounded
step (for example `docker rm -f <id>`) over a graceful stop that waits out its
own timeout.

```bash
created=$(my-backend create "$name")
trap 'my-backend remove "$created"' EXIT   # runs on failure, on exit, and on SIGTERM
trap 'exit 143' TERM                       # turn SIGTERM into an ordinary exit
my-backend wait-ready "$created"
trap - EXIT TERM                           # startup succeeded; keep the session
```

Each call has a deadline. `start` and `provision` get 120 seconds, `attach`
none (it holds your terminal), and `watch-startup` runs only while gc clears
startup dialogs. Every other operation gets 30 seconds, and a caller can
cancel a call sooner. Two operations are stopped differently from the steps
above: a canceled `exec` or `watch-startup` call is killed outright, with
`SIGKILL` to the script process and no `SIGTERM` first.

## Operations

| Operation | Invocation | Stdin | Stdout |
|-----------|-----------|-------|--------|
| `start` | `script start <name>` | JSON config | — |
| `stop` | `script stop <name>` | — | — |
| `interrupt` | `script interrupt <name>` | — | — |
| `is-running` | `script is-running <name>` | — | `true` or `false` |
| `attach` | `script attach <name>` | tty passthrough | tty passthrough |
| `process-alive` | `script process-alive <name>` | process names (1/line) | `true` or `false` |
| `nudge` | `script nudge <name>` | message text | — |
| `set-meta` | `script set-meta <name> <key>` | value on stdin | — |
| `get-meta` | `script get-meta <name> <key>` | — | value (empty = not set) |
| `remove-meta` | `script remove-meta <name> <key>` | — | — |
| `peek` | `script peek <name> <lines>` | — | captured text |
| `list-running` | `script list-running <prefix>` | — | one name per line |
| `get-last-activity` | `script get-last-activity <name>` | — | RFC3339 or empty |
| `protocol` | `script protocol` | — | handshake JSON (see below) |
| `is-attached` | `script is-attached <name>` | — | `true` or `false` |
| `exec` | `script exec <name>` | command | combined output (op exit == command exit) |
| `provision` | `script provision <name>` | JSON config | — |

**Box without agent (the un-weld).** `provision` is `start` MINUS the agent
launch: it creates/prepares the box (PreStart, SessionSetup, SessionSetupScript,
SessionLive — every box step `start` runs EXCEPT spawning the agent in tmux) and
returns. The controller then launches the agent itself by exec-ing `tmux
new-session` / `respawn-pane -k` over the `exec` op, so a launch-only config
change relaunches the agent in the warm box instead of reprovisioning (B2.3). A
pack opts in by declaring `proc.provision` (and `proc.exec`, since the controller
drives the launch over `exec`); without it, the welded `start` op provisions and
launches as before, and the controller issues no `provision`/launch. The op is
gated by the `RPP-PROVISION-001` conformance requirement.

**The connection primitive (slim RPP).** `exec` is the connection op
(`RPP-CONN-001`): a carrier drives a box *through* `exec` rather than via
dedicated driving ops, and any runtime that declares an `env.*` capability
already implements it. It is **optional for now** — conformance verifies it
only when present (the output reaches the caller and the op exit mirrors the
command's exit code) — and becomes required as Gas City moves its own input
delivery and observation onto `exec`. The dedicated driving ops (`interrupt`,
`nudge`, `peek`, `clear-scrollback`, `send-keys`) are reproducible over `exec`
and are deliberately NOT conformance requirements: gc now drives input and reads
output **over `exec`** (via the tmux carrier) when a runtime implements it, and
**falls back** to the dedicated driving ops when `exec` is unsupported
(`RPP-CONN-001` answered exit 2). So a runtime that ships `exec` + tmux-in-box
needs none of the driving ops, while one that implements only the driving ops
keeps working via the fallback. (`watch-startup` is a streaming op the
request/response `exec` connection cannot carry, so it stays a dedicated op.)

### Protocol Handshake (`protocol`)

The `protocol` operation declares which Runtime Provider Protocol version
the script speaks and which optional capabilities it implements:

```json
{"version": 0, "capabilities": ["report-attachment", "report-activity"]}
```

Scripts that do not implement `protocol` (exit 2) are treated as version 0
with no optional capabilities — every pre-handshake script remains valid.
Unknown capability strings are ignored, so scripts may declare
capabilities for newer Gas City versions without breaking older ones.
Malformed handshake JSON is an error: capability probes fall back to the
no-capability behavior and the failure is reported by conformance and
doctor checks.

Capabilities:

| Capability | Effect |
|------------|--------|
| `report-attachment` | `is-attached <name>` is called and trusted; without it, sessions always read as detached and `is-attached` is never invoked. Because the op is declared, its exit 2 is a failure like any other non-zero exit, not the "unknown op" sentinel. Also required for full idle sleep (see [Idle sleep](#idle-sleep)). |
| `report-activity` | `get-last-activity <name>` results are treated as meaningful for idle/health decisions. Report the time of the agent's last output. For a tmux-in-box runtime that is the newest `#{window_activity}` of the session's windows: `#{session_activity}` does not advance while no client is attached. Also required for full idle sleep. |
| `proc.exec` | The `exec` op's process exit code carries the in-box command's exit code, so an exec-op exit of 2 is read as the command's own exit 2 rather than the "unknown op" sentinel (`ErrExecUnsupported`). Lets the carrier drive input/output over `exec`; without it, gc uses the dedicated driving ops (the fallback path). Also required for full idle sleep. |
| `input-fence` | With `proc.exec`, declares that the box's tmux can hold an input fence, so gc sends nudges, keys and interrupts only to the agent incarnation they are meant for (see [Fenced input over `exec`](#fenced-input-over-exec)). Without it, gc touches no fence in the box, and a fenced request to the runtime is refused (`ErrInputFenceUnsupported`); unfenced input is unaffected. |
| `proc.provision` | The script implements the box-without-agent `provision` op (see Operations), so the controller provisions the box, then launches the agent over `exec` (the un-weld). Without it, `start` provisions and launches in one op. |
| `proc.stream` | Reserved (connection-plane family, parallel to `env.*`): declares the persistent bidirectional `stream` connection op (ACP over a stream, tmux pipe-pane). Sets `CanStream`. The `stream` op and its capability-gated conformance entry land with the connection rewrite. |
| `tty.attach` | Reserved: declares an interactive PTY `attach` connection op. Sets `CanAttachTTY`. |

The handshake runs once per provider instance and is cached.

### Idle sleep

An idle sandbox still costs money and capacity. With a `[session_sleep]`
policy (or an agent's `sleep_after_idle`), the orchestrator stops a session
that has been idle long enough and starts it again when work, a nudge, or an
attach arrives. Which sessions a script-backed runtime can put to sleep
depends on its handshake:

| Handshake declares | `sleep_capability` | What sleeps |
|---|---|---|
| `report-activity`, `report-attachment` **and** `proc.exec` | `full` | Every session class, interactive ones included. |
| anything less (or no handshake) | `timed_only` | Non-interactive sessions only. An interactive policy resolves to `off` with `sleep_policy_adjustment_reason = interactive_capability_insufficient`. |

`full` needs a safe idle boundary, so an interactive session is never stopped
in the middle of a turn. A runtime that declares all three must:

- **Run the agent in the in-box tmux session `main`.** The orchestrator reads
  the pane over `exec` (`tmux capture-pane -t main -p -S -120`). It treats the
  session as idle after two consecutive captures, 200 ms apart, that show the
  agent's ready prompt (`ready_prompt_prefix`, default `❯ `) and no busy
  indicator such as `esc to interrupt`. This is the same check a local tmux
  session gets.
- **Answer `is-attached` honestly.** Print `false` and exit 0 for a session
  that does not exist. Exit non-zero only when you cannot tell; any non-zero
  exit, 2 included, keeps the session awake.
- **Treat `stop` as the sleep.** Sleeping a session calls `stop`, and waking it
  calls `start`. Anything the runtime keeps only inside the box, such as the
  agent's conversation, is lost across a sleep unless `stop` preserves it.

Each capture is a full round trip to the box, so the orchestrator gives one
idle check 5 s, not the 1 s a local tmux session gets. Two captures and the
pause between them fit in 5 s when each capture finishes in under 2.4 s. A
check in a process that has not yet remembered the session's ready prompt
(see below) first reads it from the box within the same 5 s, so that check
needs three round trips, each under 1.6 s. Every check needs the read while
`GC_READY_PROMPT_PREFIX` is absent from the session. Treat 2.4 s and 1.6 s as
limits, not targets: a capture that ends exactly at the 5 s mark does not
count. A slower check fails closed: the session stays awake, and the check
runs again on the next orchestrator tick.

Once the agent's tmux session exists, the orchestrator stores the session's
`ready_prompt_prefix` in that session's environment as
`GC_READY_PROMPT_PREFIX` (`tmux -u set-environment -t main`; a blank prefix
stores the default prompt, `❯ `), as local tmux sessions do. A process that
did not start the session, such as a restarted orchestrator or a `gc` command
that cannot reach the API, reads it back with `tmux -u show-environment -t
main` and then remembers it. Like local tmux, gc passes `-u` to these
commands, so the prompt survives an exec environment without a UTF-8 locale.
Do not clear the `main` session's environment. While the read fails or the
variable is absent, that process looks for the default prompt and reads again
on its next wait. The variable is absent when storing it failed (gc does not
retry; the next start stores it again) and for a session started by an older
gc. A session whose agent shows another prompt then never reads as idle. It
stays awake, and every wait for its idle boundary runs to its timeout:
interrupting it (`interrupt_now`) can restart the session, and stopping a turn
reports an error.

The `is-attached` rule above applies to every runtime that declares
`report-attachment`, even without the other two capabilities. gc runs the op
directly and treats any non-zero exit, including 2, as "attached": the session
stays awake and destructive actions on it are held.

```toml
[session_sleep]
interactive_resume = "10m"   # interactive sessions: needs sleep_capability = full
noninteractive = "60s"
```

### Fenced input over `exec`

A runtime that declares both `proc.exec` and `input-fence` takes fenced
input: gc sends a nudge, keys or an interrupt only to the agent incarnation it
is meant for. The fence is the pane option `@gc_fence` on the agent pane of
the in-box tmux session `main`, and gc manages it over `exec`. Declare
`input-fence` only for a runtime that:

- **Runs tmux 3.0 or later in the box.** gc sets and clears the fence with
  `set-option -p` (pane options), which older tmux rejects. With older tmux
  every `start` fails and the box is torn down, and a separable launch fails
  as a whole, because gc appends the fence command to the launch's own tmux
  command list.
- **Has the `main` session when `start` returns.** gc publishes the fence
  right after `start`. A publish that fails fails the start and tears the box
  down, unless the box was running before the attempt.
- **Provides `sh` in the box.** gc reads the fence, sends guarded input and
  loads nudge text into a tmux buffer through short `sh -c` scripts.

A runtime that declares `proc.exec` without `input-fence` starts and takes
unfenced input as usual: gc touches no fence in its box, and refuses only a
fenced request to it.

Before a `start` of a box that may already be running, gc reads and clears
the pane's fence, so a start that reuses the pane never leaves the previous
incarnation's fence on it. When `start`, or the fence publish after it, then
fails and the box stays up (it reports that the session already exists, or
`is-running` said it was running before the attempt), gc puts the fence back
on the same pane, as long as the pane's root process is unchanged and no
other fence has been set since.

Fenced input sends nothing until tmux in the box answers the fence read. A
live tmux server without the `main` session or its agent pane answers that
the session is absent. A command the box does not answer, because the `exec`
op fails (its own non-zero exit) or tmux in the box fails (no server, a
missing tmux, a socket it cannot reach), is an outage
(`ErrRuntimeUnavailable`), unless `is-running` then answers `false`, which
makes it an absent session. Either way, a failure after part of the input
went in (nudge text without its Enter) reports the input as partly
delivered. An `exec` op that is [canceled](#cancellation) or cannot be run at
all is an outage too, without asking `is-running`, and it does not say
whether part of the input went in before it stopped. A pane started before gc
published fences takes no fenced input until its agent is relaunched.

### Start Config (JSON on stdin)

The `start` and `provision` operations receive a JSON object on stdin:

```json
{
  "work_dir": "/path/to/working/directory",
  "command": "claude --dangerously-skip-permissions",
  "env": {"GC_AGENT": "mayor", "GC_CITY": "/home/user/bright-lights"},
  "lifecycle": "one_shot",
  "process_names": ["claude", "node"],
  "nudge": "initial prompt text",
  "pre_start": ["mkdir -p /workspace", "git clone repo /workspace"]
}
```

Every field is optional and omitted when empty. The complete set:

| Field | Contents |
|-------|----------|
| `work_dir` | The session's working directory, as a path on the orchestrator host |
| `command` | The agent launch command |
| `lifecycle` | `"one_shot"` for a command expected to exit after one turn |
| `env` | The session's environment (see [Session environment](#session-environment)) |
| `process_names` | The process names the agent runs as |
| `nudge` | Text to type into the agent once it is ready |
| `ready_prompt_prefix` | The prompt prefix that shows the agent is ready |
| `ready_delay_ms` | A fixed delay to wait instead, when there is no prompt to watch for |
| `pre_start` | Commands to run on the target before the session is created |
| `session_setup` | Commands to run in the session after it is created |
| `session_setup_script` | A script on the orchestrator host, run after `session_setup` |
| `session_live` | Idempotent commands to run after `session_setup` |
| `pack_overlay_dirs` | Pack overlay directories on the orchestrator host, copied into the work dir first |
| `overlay_dir` | The agent's overlay directory on the orchestrator host, copied into the work dir last |
| `copy_files` | `{"src", "rel_dst"}` entries: a file or directory on the orchestrator host to stage at `rel_dst` under the work dir (the work dir itself when `rel_dst` is absent) |
| `work_dir_exclude` | Patterns for state that must stay on the orchestrator host (see [Work-dir export exclusions](#work-dir-export-exclusions)) |

On a fresh start the agent's startup prompt is not in the start config: gc
renders it as a command-line argument (`prompt_mode = "arg"`, the default) or
flag (`"flag"`), and the exec provider passes neither to the script, so the
agent starts without it. gc puts the prompt at the front of `nudge` instead in
three cases: with `prompt_mode = "none"`, on a start that resumes an existing
conversation (any prompt mode), and for a prompt too large for a command line
on a runtime whose `[runtimes.<name>]` entry sets
`prompt_delivery = "nudge-fallback"`. A script should handle any non-empty
`nudge`.

### Session environment

`env` is what gc wants set in the session, to apply over the box's own
environment. For a configured agent's session it holds:

- `PATH`, `HOME`, `USER`, `LOGNAME`, `TZ`, the locale and XDG directory
  variables, and a few Claude Code and telemetry settings, from gc's own
  environment.
- Every non-empty `GC_*` variable in gc's environment except
  `GC_CONTROLLER_TOKEN`, and any variable named in `GC_SUPERVISOR_ENV`.
- Provider credentials from gc's environment: every variable whose name starts
  with a provider prefix such as `ANTHROPIC_`, `OPENAI_`, `GEMINI_`, `GOOGLE_`,
  `AZURE_` or `XAI_`, and a fixed list of AWS variables (`AWS_ACCESS_KEY_ID`,
  `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`, `AWS_PROFILE`
  and others), not every `AWS_*` variable.
- The city's `[workspace]` env, then the provider's, then the agent's, then
  the session's own variables (`GC_AGENT`, `GC_ALIAS`, `GC_CITY`, `GC_DIR`,
  `GC_SESSION_ID` and more), and last the agent's selected upstream's
  variables and a few gc-owned settings, the empty `GC_CONTROLLER_TOKEN`
  among them.

An empty value means the variable must be unset. gc sends some keys empty on
purpose, `GC_CONTROLLER_TOKEN` among them, so that a value inherited from its
own environment does not reach the agent. The script itself runs with gc's
environment, so a script that starts the agent as its own child must apply
those empty values too.

Provider credentials in the supervisor's environment therefore reach every
session. `GC_SUPERVISOR_OMIT_PROVIDER_CREDS=1` does not change that: it only
keeps them out of the launchd plist or systemd unit that `gc` generates, and
out of a `gc supervisor run` that `gc` forks. The bundled
`contrib/session-scripts/gc-session-k8s` copies `env` into the pod spec as
literal `env` values (it drops or rewrites a few `GC_*` keys), and Kubernetes
gives a literal `env` entry precedence over the same key from `envFrom`, so a
credential in the supervisor's environment overrides one you supply through
`envFrom`.

### Startup Hints

The JSON config contains fields that the tmux provider uses for multi-step
startup orchestration. The exec provider does not fire `start` and forget it.
gc first runs `is-running` to learn whether a box already holds the name, then
runs `start` and waits for it to exit. After a successful `start` it can run
more operations before it counts the session started: it publishes the
[input fence](#fenced-input-over-exec) and the [ready prompt](#idle-sleep) for
a runtime that declares the capabilities they need, and when the start config
lists `process_names` or the provider declares a bypass-permissions dialog (a
provider's `accept_startup_dialogs` setting overrides both) it dismisses known
startup dialogs, such as workspace trust, through `watch-startup`, `peek` and
`send-keys` (`peek` and `send-keys` go over `exec` when the runtime implements
it).

If `start` fails or times out, or a step after it fails, gc runs
`stop <name>` to tear down whatever the attempt created, then reports the
start as failed. It skips that `stop` when the box belongs to a live session:
when `is-running` printed `true` before the attempt, or when `start` exited
non-zero on its own with stderr saying the name is taken (it contains
`already exists` or `already running`). The stderr check does not apply to a
`start` that gc canceled at its deadline. A runtime that declares both
`proc.provision` and `proc.exec` gets `provision` in place of that
`is-running` and `start`: no `stop` follows a failed `provision`, and one
always follows a failed agent launch after it.

Scripts may handle these hints or ignore them:

- **`process_names`** — the tmux adapter polls for these process names to
  appear in the session's process tree (30s timeout) before considering the
  agent "started." A script can implement this by polling its backend's
  process tree after session creation, or ignore it for fire-and-forget
  behavior (like the subprocess provider does).

- **`lifecycle`** — `"one_shot"` marks a short-lived command that is
  expected to exit after handling its prompt. Providers should not require
  a persistent post-start process for one-shot starts.

- **`nudge`** — text that the tmux adapter types into the session after
  the agent is ready. gc does not send it through the `nudge` operation
  when `start` returns, so a script that wants the agent to receive it
  types it in itself, in `start`, once the agent is ready. Later nudges,
  such as `gc session nudge`, go through the `nudge` operation, or over
  `exec` when the runtime implements it.

- **`pre_start`** — array of shell commands to run on the target
  filesystem **before** the session is created. Used for directory
  preparation, worktree creation, or other setup that must exist before
  the agent starts. Scripts should execute each command in the target
  environment before creating the tmux session. Non-fatal: warn on
  stderr if a command fails, but don't abort start.

- **`session_setup`** — array of shell commands to run on the target
  filesystem after the session is created and ready, before returning.
  Scripts should execute each command inside the session environment
  (e.g. `kubectl exec -- sh -c '<cmd>'` for K8s, `docker exec -- sh -c
  '<cmd>'` for Docker, or plain `sh -c '<cmd>'` for local providers).
  Non-fatal: warn on stderr if a command fails, but don't abort start.

- **`session_setup_script`** — path to a script on the orchestrator
  filesystem, run after `session_setup` commands. For remote providers
  (K8s, Docker), read the file locally and pipe its contents into the
  session (e.g. `kubectl exec -i -- sh < script`). For local providers,
  run directly via `sh -c`. Non-fatal like `session_setup`.

Fields that are **not** included in the JSON (gc-internal, not part of
the exec protocol):

- `emits_permission_warning` — bypass-permissions dialog handling
- `fingerprint_extra` — config change detection metadata

`ready_prompt_prefix` and `ready_delay_ms` are sent, but gc does not wait
for the agent to show the prompt, or wait out the delay, before it counts
the start done. A script that must reach a ready agent before it returns
(to type `nudge`, say) waits itself; otherwise it may ignore them. Session setup
commands are the *script's* responsibility — they run on the target
filesystem, not the orchestrator.

### Work-dir export exclusions

- **`work_dir_exclude`** — array of patterns, relative to `work_dir`,
  naming orchestrator-local state that must not leave the orchestrator
  host. gc sends it with every start config that has a `work_dir`, whatever
  the directory holds: a work dir with no top-level `.gc/` or `.beads/` can
  still hold rig scopes below it. A script that copies, archives, or syncs
  `work_dir` into another filesystem (a remote sandbox, a container image)
  must skip every matching path. A script that runs the agent in place on
  the orchestrator's filesystem ignores it.

The list is a floor, not the complete set. A script must skip every
matching path and may skip more. A script that carries its own built-in
denylist applies the union of that list and `work_dir_exclude`; it never
replaces its list with gc's, so a gap on either side cannot widen what
leaves the host.

Rather than implement the grammar below, a script can ask gc for the
entries that survive it:

```sh
set -o pipefail
"$GC_BIN" runtime workdir-export --null "$WORK_DIR" |
  tar -C "$WORK_DIR" --null --no-recursion -T - -czf -
```

- Call the gc named by the start config's `env.GC_BIN`, the binary that
  produced the list. An older `gc` on `PATH` may lack the subcommand.
- Check its exit status (`set -o pipefail` in a pipeline). On failure it
  prints nothing to stdout, and an unchecked pipeline archives an empty work
  dir.
- Use `--null`. The default newline-terminated output is for humans only:
  no byte filter makes it safe for a consumer that splits on blanks or
  quotes, such as `xargs`. As a backstop it refuses, printing nothing, any
  entry that contains a newline, a carriage return, or a backslash (GNU
  `tar -T` reads `\057` as `/`), or that starts with `-`.
- GNU tar reads a `--null` list verbatim only from 1.30; bsdtar always
  does. Older GNU tar unquotes backslash escapes even with `--null`, so a
  crafted file name can make it archive `.gc/controller.token` or a file
  above the work dir. Where GNU tar may predate 1.30, use the form below:
  1.29 reads the list verbatim with it, and older releases reject its
  options, which fails the pipeline. bsdtar rejects those options, so keep
  the form above there.

  ```sh
  set -o pipefail
  "$GC_BIN" runtime workdir-export --null "$WORK_DIR" |
    tar -C "$WORK_DIR" --null --verbatim-files-from --no-unquote \
      --no-recursion -T - -czf -
  ```
- The output lists every surviving file and symlink, plus every surviving
  directory that is empty on disk. A non-empty directory is never listed, so
  a consumer that recurses into listed entries still copies nothing
  excluded; a consumer that does not recurse recreates parent directories
  from the paths below them. A directory whose every entry is excluded is
  absent.
- Entries come in depth-first walk order, each directory's entries sorted by
  name. This is not `sort` order: `a/b` comes before `a-c`.

A script that applies `work_dir_exclude` itself copies symlinks as links and
never dereferences them: an in-tree link such as `x -> .gc/controller.token`
would otherwise ship its target.

Pattern grammar:

- Patterns and paths are `/`-separated and relative to `work_dir`.
- A segment of exactly `**` matches zero or more whole path segments.
- Every other segment is a single-segment glob (Go `path.Match` syntax):
  `*` matches any run of characters except `/`, leading dots included.
- A path is excluded when it, or any directory above it, matches. A
  trailing `/**` spells out that a whole subtree stays behind.
- This is not gitignore, rsync, or tar `--exclude` syntax. A pattern with
  no `/` matches only at the work-dir root, and `.gc/**/*.sock` matches
  `.gc/controller.sock` because `**` may match zero segments. Translating
  patterns into another tool's syntax is error-prone; prefer
  `gc runtime workdir-export`.

The list covers secrets and credentials (`.gc/services/**`,
`.gc/secrets/**`, `.gc/controller.token`, `.gc/credentials.toml`,
`.gc/credentials.json`, `.gc/mcp-adopted/**`, `.beads/.env`, `.beads/.beads-credential-key`,
`.beads/proxy.secret`), the event and usage logs, the nudge queue,
per-session diagnostics under `.gc/sessions/` and ACP transcripts under
`.gc/transcripts/`, sockets, locks and pid files, the stores under
`.gc/store/` (the default SQLite binding root), `.gc/storage/`,
`.gc/infra/` and `.gc/dolt-data/`, bd's stores and their backups in any
scope (`.beads/dolt/`, `.beads/doltlite/`, `.beads/embeddeddolt/`, its
SQLite files, bd backups and corrupt-store backups), the file provider's
`.gc/beads.json` and its `.bak` in any scope, the legacy JSONL archive,
every file named `beads.sqlite*` wherever it sits, bd server, proxy and
sync state, the provider-lifecycle journal `.gc/scope-ownership.json`,
orchestrator runtime state, and other agents' work dirs under
`.gc/worktrees/` and `.gc/agents/`. Copying any of it gives the session a
stale, private fork of state the orchestrator owns, or hands it secrets it
must not hold. Files the session needs are never listed:
`.gc/settings.json`, `.gc/site.toml`, `.gc/cache/` and `.gc/system/` (pid
and socket files aside), `.gc/scripts/`, the prompt and skill snapshots
under `.gc/tmp/`, the staged prompts under `.gc/synth/`, the MCP
projections under `.gc/mcp-managed/`, the step and iteration artifacts
agents read and write under `.gc/molecules/` and `.gc/artifacts/`, the
`.gc/file-beads-layout` marker, `.beads/metadata.json`,
`.beads/config.yaml`, `.beads/http_target.json`, and `.beads/redirect`.

The list is fixed and matches paths by name. It does not read the city's
configuration: when a SQLite storage binding's `path` moves its root to a
path inside the work dir that the list does not name, only that root's
`beads.sqlite*` files match, and a script for such a city adds the root to
its own denylist. Matching names also cannot stop a process on the
orchestrator host from copying or hard-linking excluded content to a path
the list keeps.

`.gc/site.toml` ships because session-side `gc` commands load it with
`city.toml`: it carries the workspace name and bead prefix as well as rig
path bindings. Its rig paths are orchestrator-host paths, so a script that
relocates the work dir remaps them as it remaps the `GC_*` path variables.

The field is additive. A script that predates it ignores the unknown key
and keeps copying the whole work dir.

### Conventions

- **stdin for values**: `set-meta`, `nudge`, and `start` pass data on stdin
  to avoid shell quoting and argument length limits.
- **stdout for results**: `is-running`, `process-alive` return `true`/`false`.
  `get-meta` returns the value or empty for unset. `list-running` returns one
  name per line.
- **Idempotent stop**: `stop` must succeed (exit 0) even if the session
  doesn't exist.
- **Best-effort interrupt/nudge**: Return 0 even if the session doesn't exist.
- **Empty = unsupported**: `get-last-activity` returning empty stdout means
  the backend doesn't support activity tracking (zero time in Go).

### Session identity

The `env` object in the start config carries the session's identity:
`GC_SESSION_ID`, `GC_INSTANCE_TOKEN` and `GC_RUNTIME_EPOCH`. When `get-meta`
returns these keys from the moment `is-running` reports the session, the
orchestrator can tell which session a running box belongs to. After an
orchestrator restart it then adopts a session whose start was cut short,
instead of failing it.

A script whose `get-meta` answers them empty still works. If a session
created with `gc session new` (or the API) fails to start, or its start is cut
short, the orchestrator stops any box running under its name before it marks
the session failed. A box that reports another session's `GC_SESSION_ID` is
left alone.

That teardown reads the identity keys first, so `get-meta` must print nothing
and exit 0 both for an unset key and for a session that does not exist. A
non-zero exit reads as "cannot tell": a failed `gc session new` or API session
then stays open in `creating` until `get-meta` answers, or until an operator
runs `gc session close`. A script that runs its backend's CLI under
`set -e` should catch the "no such session" case and print nothing.

## Writing Your Own Script

1. Start with `contrib/session-scripts/gc-session-screen` as a template.
2. Implement the operations your backend supports.
3. Return exit 2 for operations you don't support.
4. Validate with `gc runtime check ./your-script` — it runs the protocol
   handshake, the required lifecycle round-trip (start, is-running, stop,
   idempotent stop), exercises every capability the handshake declares,
   and probes optional operations (absent ones are reported, not failed).
   It exits non-zero if any check fails, so CI can gate on it.
5. Test with `GC_SESSION=exec:./your-script gc start <city>`.

### Minimal script (start/stop/is-running only)

```bash
#!/bin/sh
op="$1"
name="$2"
case "$op" in
  start)     cat > /dev/null; my-mux new "$name" ;;
  stop)      my-mux kill "$name" 2>/dev/null; exit 0 ;;
  is-running) my-mux list | grep -q "^${name}$" && echo true || echo false ;;
  *)         exit 2 ;;
esac
```

## Environment Variables

Scripts can use `GC_EXEC_STATE_DIR` (if set) as a directory for sidecar
state files (metadata, wrappers). If not set, scripts should use a
reasonable default under `$TMPDIR` or `/tmp`.

A script runs with gc's own environment. The session's environment arrives
in the start config's `env` (see [Session environment](#session-environment)).

## Shipped Scripts

See `contrib/session-scripts/` for maintained implementations:

- **gc-session-screen** — GNU screen backend. Dependencies: `screen`,
  `jq`, `bash`.
