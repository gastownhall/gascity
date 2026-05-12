# `gc reload` Design

Status: working design for GitHub issue `#787`

## Problem

Gas City has no user-facing command that forces the current city to
re-read effective config without restarting the city. When file watching
misses a config or pack edit, the only documented recovery path is
`gc restart`, which tears down active sessions and disrupts in-flight
work.

## Goals

- Add a user-facing `gc reload [path]` command for the current city.
- Work for both standalone and supervisor-managed cities.
- Reuse the existing config-dirty reload path instead of introducing a
  second reload implementation.
- Default to synchronous behavior so the user knows whether one reload
  tick completed successfully.
- Preserve existing runtime semantics after config apply, including
  per-session restarts caused by normal config drift rules.
- Surface structured outcomes and warnings so CLI behavior, tests, and
  trace data are stable.

## Non-Goals

- No top-level `gc poke`.
- No new HTTP API mutation such as `POST /v0/city/reload`.
- No lockfile update during reload-time remote pack fetches.
- No special transactional rollback for post-apply runtime side effects.
- No new troubleshooting guide in this change.

## User-Facing Contract

### Command

```text
gc reload [path] [--async] [--soft] [--timeout <duration>]
```

- `[path]` is optional and follows existing city-command resolution.
- `--async` returns after the current city controller accepts the reload
  request.
- `--soft` accepts per-session config drift instead of draining the
  drifted sessions — see [Soft Reload](#soft-reload) below.
- `--timeout` applies only to synchronous mode, must be strictly
  positive, and defaults to `5m`.
- `--async --timeout ...` is invalid when `--timeout` is explicitly set.
  Implementation uses Cobra flag-change detection so the default `5m`
  does not make a plain `gc reload --async` invalid.

### Scope

- `gc reload` targets exactly one city: the resolved current city.
- It works whether that city is running standalone or under the
  supervisor, because both topologies expose the same per-city
  controller socket.
- It is a live runtime operation. If the city controller is not running,
  the command fails.

### Success and Failure Semantics

Sync mode waits for the first reload-processing tick only.

| Outcome | Exit | Stdout/Stderr contract |
| --- | --- | --- |
| `applied` | `0` | stdout: `Config reloaded: ... (rev <short>)` |
| `no_change` | `0` | stdout: `No config changes detected.` |
| `accepted` (`--async`) | `0` | stdout: `Reload requested.` |
| `failed` | `1` | stderr: specific config/load/fetch error |
| `busy` | `1` | stderr: controller too busy to accept reload |
| `timeout` | `1` | stderr: wait budget expired; reload may still finish later |

Warnings are non-fatal post-apply problems. On sync success with
warnings:

- stdout prints the main success line
- stderr prints one line per warning as
  `gc reload: warning: ...`

Rationale for exit codes:

- `gc reload` keeps the existing `gc` CLI convention of `0` for success
  and `1` for failure conditions.
- Finer-grained outcome differentiation lives in the structured
  controller reply and in trace/telemetry, not in a new CLI exit-code
  taxonomy for this feature.
- Scripts must not parse the human `message` text; they should branch on
  success vs failure at the CLI layer or use the controller protocol in
  future machine-facing integrations.

### Config Boundary

Reload failure is defined at the config-load boundary:

- remote pack fetch
- parse/load
- validation
- `workspace.name` mismatch
- required bead lifecycle setup for the newly loaded config

If any of those fail, reload returns non-zero and the old live config
stays active.

After a config is successfully applied, subsequent runtime-execution
issues are warnings rather than rollback triggers. Examples include:

- provider swap setup failures
- rig validation errors
- formula or script resolution errors
- service reload errors
- standalone city bead-store refresh errors

An `applied` reply with warnings means the new effective config is live,
but one or more post-apply runtime updates could not fully converge. For
example, if a session provider swap fails, the reload may still succeed
while the old provider remains active. Operators and scripts that need
to distinguish full convergence from degraded success must inspect the
`warnings` array in the structured reply or stderr warning lines in the
CLI projection.

### Existing Runtime Behavior Preserved

`gc reload`:

- does not restart the city/controller
- may still cause normal per-session restarts if the reconciler detects
  config drift under the existing rules — unless `--soft` is passed, in
  which case the detected drift is accepted in place (see [Soft Reload](#soft-reload))
- preserves existing service-manager reload behavior
- works while the city is suspended, as long as the controller is still
  running

### Soft Reload

Editing a running city's effective config — most often via
`.gc/settings.json` or one of the materialized overlay files — changes
each affected agent's `runtime.Config` and therefore its
`runtime.CoreFingerprint`. The reconciler sees the stored
`started_config_hash` no longer match what it would now produce and
treats the session as drifted, normally triggering a
`config-drift` drain. For idle pool slots this is fine; for sessions
holding live in-flight work it can be disruptive even with the
existing in-flight-work guards in place.

`gc reload --soft` accepts the drift in place instead of draining:

1. The CLI sends `{soft: true}` in the reload control request.
2. The controller runs the normal config reload (remote pack fetch,
   parse, validation, apply). If that fails, `--soft` is a no-op —
   reload returns `failed` exactly as it would without `--soft`.
3. Once the new config is live, the controller rebuilds desired state
   and walks every open session bead. For each session whose
   `session_name` has a desired-state entry and whose recorded
   `started_config_hash` differs from the value the new config would
   produce, the controller writes the new hash into
   `started_config_hash`. Sessions that have not yet stamped a
   `started_config_hash` (still in the startup window) are skipped —
   they will record the right value once they finish starting.
   Sessions whose `session_name` has no desired-state entry are
   skipped — the orphan/suspended drain handles them on the next tick.
4. The immediately-following reconcile tick sees no drift on the
   updated sessions and does not fire `config-drift` drains for them.
5. The CLI prints `soft reload: accepted config drift on N session(s)`
   on stdout when one or more sessions were updated, alongside the
   normal `Config reloaded: ...` line.

Soft reload is **not** a substitute for restarting sessions when a
config change requires a process restart to take effect (e.g.
changing the binary path or command-line). It updates only the
recorded hash so the reconciler stops trying to drain the session;
the live session continues running with the *old* command for the
rest of its life. Use `--soft` for changes that are cosmetic to a
running session — hook commands, matchers, environment variables that
the session reads via lookup rather than once-at-launch — or any time
you want to absorb a config edit without disrupting in-flight work
and intend to restart sessions naturally on the next idle moment.

Recommended live-edit workflow:

```bash
# 1. Edit your city's .gc/settings.json (or any other configured file)
$EDITOR ~/path/to/city/.gc/settings.json

# 2. Apply the change without draining live sessions
gc reload --soft

# 3. (Optional) Force-cycle individual sessions later if their
#    in-process state needs to pick up the new config
gc session reset <name>
```

### Remote Pack Behavior

Reload recomputes effective config using the same full-load semantics as
existing controller reload and startup paths:

- configured remote packs may be fetched before config load
- fetch failures are hard reload failures
- `pack.lock` is not modified

## Architecture

## CLI Layer

Add a top-level `reload` command in `cmd/gc`:

- resolve city with existing city-command rules
- validate `--async`/`--timeout`
- send a structured `reload` request to the per-city controller socket
- format reply message and warnings for human output
- on controller-connect failure, attempt to surface richer
  supervisor-known state when available:
  - city still starting under supervisor, including current phase
  - city failed to start and is in backoff, including last error
  - city not running
  - generic controller unavailable fallback when no richer state exists

## Controller Socket Protocol

The controller already has a fire-and-forget `reload` socket command
that marks config dirty and pokes the event loop. This feature upgrades
reload control by adding a structured variant:

```text
reload:<json>
```

### Request

```json
{
  "wait": true,
  "timeout": "5m"
}
```

Semantics:

- `wait=true` means sync mode
- `wait=false` means async mode
- `timeout` is required for sync mode, ignored internally for async mode

### Reply

```json
{
  "outcome": "applied|no_change|accepted|failed|busy|timeout",
  "message": "human readable summary",
  "revision": "full-config-revision-when-known",
  "warnings": ["normalized warning", "..."],
  "error": "specific failure string when outcome=failed"
}
```

Notes:

- `message` is controller-authored canonical wording; the CLI mainly
  relays it.
- `revision` is included for `applied` and `no_change`.
- `accepted` is used only for async acceptance.
- `busy` may be returned in both sync and async mode if the controller
  cannot register the request within the acceptance window.
- `warnings` are normalized, user-facing strings in encounter order.

The existing bare `reload` command remains as a compatibility
fire-and-forget path for internal callers and tests. `gc reload` uses
the structured `reload:<json>` command.

## Event-Loop Integration

Manual reload reuses the existing config-dirty tick path.

Implementation shape:

1. Add an unbuffered `reloadReqCh` to the controller/runtime plumbing so
   a request is handed directly to the event loop or rejected within the
   acceptance timeout.
2. The socket handler validates and attempts to enqueue a reload request
   to `reloadReqCh`.
3. Acceptance is defined as event-loop registration, not mere channel
   enqueue.
4. The socket handler waits up to `5s` for the event loop to receive and
   register the request.
5. When the event loop consumes a request:
   - if another manual reload is already active, it replies `busy`
   - otherwise it records the request as the active manual reload
   - then it sets `dirty=true`
   - then it pokes the reconciler loop
   - then it acks the request as accepted
6. The next tick after registration processes reload through the same
   `dirty`-gated config reload path already used by file watching.
7. When that tick resolves the reload outcome, it completes the active
   manual request and clears the slot.

Critical ordering rules:

- An accepted manual reload is never attached to an already-running tick.
- Registration of the active manual request happens-before
  `dirty=true`, which happens-before the poke.
- The reload result is bound to the first tick that starts after manual
  registration.
- If the event loop cannot register the request within `5s`, the caller
  gets `busy`.

Queueing rules:

- one manual reload request may be active at a time
- no manual-request piggybacking/coalescing contract is added
- a concurrent manual request may receive `busy`
- a manual request may coalesce with preexisting watcher dirtiness so
  the system performs one reload attempt, attributed as `manual`

Timeout rules:

- enqueue timeout (`5s`) and wait timeout (`--timeout`, default `5m`)
  remain distinct
- `timeout` means the caller stopped waiting; the reload may still
  complete later

## Runtime Result Capture

The current reload path prints directly to stdout/stderr and records only
coarse telemetry. To support `gc reload`, factor it to produce a
structured result in addition to existing logging behavior.

Proposed internal result shape:

- outcome: `applied`, `no_change`, or `failed`
- revision
- message
- warnings
- provider-changed marker
- error

This result is consumed by:

- the active manual reload request
- trace recording
- telemetry recording

Watcher-driven reloads continue using the same reload implementation but
without a waiting CLI caller.

## Observability

### Trace

Extend config-reload trace records to include:

- `source`: `manual` or `watch`
- `warnings[]`

Rules:

- source is recorded for every actual reload attempt outcome
- manual wins if a manual reload request is accepted while config is
  already dirty from watcher activity
- the resulting reload is still a single reload attempt, not a watcher
  reload followed by a separate manual replay
- tick trigger records the actual tick source; manual reload is
  distinguished with `trigger_detail="manual_reload"`

### Telemetry

Keep metrics/logging low-cardinality. Extend `RecordConfigReload` to
record:

- `status`: `ok` or `error`
- `source`: `manual` or `watch`
- `outcome`: `applied`, `no_change`, or `failed`
- `warning_count`

Full warning strings stay out of telemetry and live in trace plus CLI
stderr.

## Out-of-Scope Machine Interface

This feature does not add `gc reload --json` or a public HTTP reload
mutation.

Reasons:

- mutation commands in this CLI generally stay human-oriented
- the structured controller reply already provides the typed contract
  needed for the implementation and tests
- adding a CLI JSON contract or remote API surface would expand scope
  beyond issue `#787`

## Documentation

Update command help and generated CLI reference to state that:

- `gc reload` reloads the current city without restarting the
  city/controller
- it may fetch configured remote packs before recomputing effective
  config
- existing per-session restarts may still happen if config drift or
  provider changes require them

Do not add a separate troubleshooting guide in this feature.

## Testing

Minimum coverage:

- standalone city: sync reload applied
- supervisor-managed city: sync reload applied
- sync no-change
- sync invalid config keeps old config
- async accepted without waiting
- busy
- timeout
- controller unavailable / richer supervisor-state error surfaces
- remote pack fetch failure
- manual source attribution, including manual winning over preexisting
  watcher dirty state
- warnings surfaced in controller reply, CLI stderr, and trace
- `--async --timeout` validation only when the user explicitly sets
  `--timeout`
- stable `0`/`1` exit-code behavior for the documented success/failure
  groups

## Implementation Notes

- Add a new top-level command file for `gc reload`.
- Add controller request/reply types and socket handling for `reload`.
- Thread `reloadReqCh` through controller startup in standalone and
  supervisor-managed cities.
- Refactor runtime reload so it can return structured outcomes and
  warnings while preserving the existing watcher-driven behavior.
- Update telemetry and trace recorders plus their tests.
- Regenerate CLI reference docs after command help lands.
