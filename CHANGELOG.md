# Changelog

All notable changes to Gas City will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Upgrading Notes

- **Restart agent sessions after upgrading gc so their `gc nudge poll`
  sidecars run the new binary.** The file nudge queue's lock now covers only
  its `state.json`: shadow-bead writes run after the lock is released, as
  debts recorded in the queue (`retired` items and per-item `shadow`
  records) that the verb, or later the controller, pays. A sidecar started by
  an earlier gc keeps doing bead-store I/O while it holds the lock, so it can
  still time out other queue users on a slow store, and every queue rewrite it
  makes drops the `retired` and `shadow` records, leaving those shadow beads
  for the stale-shadow sweep instead of their own outcome. Restarting the
  sessions (or the city) replaces the sidecars; nothing else needs to change.
  `gc nudge status --json` output is unchanged.

- **An unset `[daemon] session_reconciler` now runs the v2 reconciler where
  the city can run it, and `[beads] conditional_writes` now defaults to
  `"auto"`.** With the key unset, the controller resolves v2 at boot from
  config alone when no feature v2 lacks is configured (agent `depends_on` or
  `max_session_age`, `[chat_sessions] idle_timeout`, a session provider other
  than tmux, acp or subprocess), conditional writes resolve on, and the
  sessions and graph classes sit on a `sqlite-beads` `[storage]` binding.
  Otherwise it runs legacy, as before, and logs a warning naming each reason;
  `gc doctor` (`daemon-session-reconciler`) reports the same. Set
  `session_reconciler = "legacy"` to keep legacy and silence the warning.
  An explicit `"v2"` still refuses to start over those features. Under v2,
  `[daemon] session_circuit_breaker`, `[session] progress_stall_timeout` and
  `[session] claim_holder_stall_timeout` are deprecated and ignored (the work
  ladder replaces them), with a boot and doctor warning; legacy still honors
  them. Unset conditional writes now take compare-and-swap wherever the store
  supports it, under legacy too; a store that cannot (an exec store, or a bd
  without `--if-revision`) runs as before, quietly, with no degraded event or
  status warning. Set `conditional_writes = "off"` (or
  `GC_BEADS_CONDITIONAL_WRITES=off`) to keep unconditional writes. The mode
  stays boot-latched, and `gc runtime drain`, `drain-ack` (including
  `gc hook`'s drain-ack) and `undrain` follow the running controller's mode
  until it restarts. A controller started by an earlier gc does not report
  its mode, so these commands treat it as legacy unless city.toml sets
  `session_reconciler = "v2"`: restart the controller after upgrading gc to
  pick up the new default. The messages that said to remove
  `session_reconciler` to run legacy now say to set it to `"legacy"`.
  Rolling back: after rolling back the gc binary, restart the controller, so
  the controller and the CLI resolve the mode under the same build.

- **An exec session provider's `start` or `provision` that exits 2 now fails
  the start.** Exit 2 meant "unknown operation, treat as success" on `start`
  and `provision` too, so a `set -e` wrapper whose `grep` or `jq` exited 2 during `start`
  counted as started and the half-created box was never cleaned up. Neither op
  can be unknown, so exit 2 from them is now a failure, and a failed `start`
  runs `stop` to tear the box down unless the name was already in use. Wrapper authors: return 0 from `start` and
  `provision` on success. Exit 2 keeps meaning "unknown operation" for other
  undeclared operations.
- **Gas City now requires bd 1.3.0 or newer (was 1.0.4).** `gc init` and
  `gc start` refuse an older `bd` with `bd (found vX, need v1.3.0+)`, and the
  bd pack's `gc doctor` check `check-bd` now fails with `bd X is too old (need
  >= 1.3.0)`. bd 1.3.0 is the first tested release with `bd update
  --if-assignee/--if-status`, which `ReleaseIfCurrent` (the orphan-sweep
  release of a stale work assignment) requires. A 1.2.x version is not enough:
  bd 1.2.0 and 1.2.1 were accidental, untested releases, and bd 1.2.2
  re-released the 1.1.2 code without the flags. The raw-SQL fallback (`bd sql`,
  and the `dolt sql` CLI on embedded Dolt) that served older bd is deleted, so
  gc no longer bypasses bd with direct SQL for old-bd compatibility. A bd
  without the flags now gets a typed "conditional assignment release
  unsupported" error, and gc releases no stale assignment through it: the
  fenced fallback cannot guard its write on that bd either, so it refuses,
  and orphaned work stays assigned to its dead session until bd is upgraded.
  The default `beads.bd_compatibility` (`bd-1.0.4` semantics) is unchanged.
  Upgrade with `brew upgrade beads` or from
  https://github.com/gastownhall/beads/releases.

- **The generic bead API no longer writes session rows, startup-health
  episodes or convergence roots.** `POST /beads`, `PATCH /bead/{id}`,
  `POST /bead/{id}/update`, `/close`, `/reopen`, `/assign`, `DELETE
  /bead/{id}`, `POST /sling` naming one, and convoy create, add and remove
  tracking one return 422, writing nothing. Workflow delete, graph convoy
  delete, run cancel, sling `force` and wisp autoclose leave one alone
  whatever links it to the workflow. Type and label match in any case and
  spacing. A row is a session row by type `session` or by the `gc:session`
  label. Use the owner's routes instead:
  - client metadata: the new `metadata` map (`real_world_app_*` keys only)
    on `POST /v0/city/{cityName}/sessions` and `PATCH /session/{id}`;
  - pinning: the new `POST /session/{id}/pin` and `/unpin` (`gc session
    pin|unpin`);
  - title and alias: `PATCH /session/{id}`; lifecycle: the session verbs.

  **Deliberately removed, with no replacement route:** setting a session
  row's labels, description, priority, assignee or parent, or any other
  metadata key, over HTTP. A typed work bead carrying the `gc:session`
  label is written only by `bd`. `bd update` stays the operator escape
  hatch for all of these.

- **The generic bead API no longer writes coordination beads either.** Mail
  messages, session waits (the `gc:wait` label, or a `gate` carrying its
  `session:<id>`), queued nudges (`gc:nudge`, `gc:nudge-queue`), order run
  records (`order-tracking`, `order-run:*`) and external-messaging rows
  (`gc:extmsg-*`) get the same 422, on a class-split city too, whatever rig
  the body names; `POST /beads` no longer creates them, and neither a formula
  (`PUT /formulas`, its pour, `POST /sling`) nor a sling batch makes or routes
  one. Use their routes: `/mail`, `gc session wait`, `gc nudge`,
  `gc order run`, `/extmsg/*`. One generic write remains on an order's run
  root (the wisp or molecule root a run poured): close (`POST
  /bead/{id}/close`, `DELETE /bead/{id}`); for anything else use `bd` or the
  workflow's teardown. Wisps stay generic. `POST /mail` and its reply now
  send only as a configured identity: the operator (`human`, or the
  dashboard's `DASHBOARD_OPERATOR_WIRE_ALIAS`), a configured agent, a named
  session or a live session of a configured agent; any other `from`, or none,
  is a 422. And an `order-run:` label, or an `order:` and `seq:` pair, counts
  toward an order's cooldown, event cursor and single-flight (in dispatch,
  `gc order check` and `GET /orders/check`) only on its tracking bead or the
  root its run poured.

- **`gc runtime drain-check` has a third exit code, 3 ("unknown").** It now
  reads the session row's state as well as the runtime's `GC_DRAIN`, and
  exits 1 ("not draining") only when both sources answered. When a source
  cannot be read and none says draining, it exits 3 with one stderr line
  naming the source that failed. `if gc runtime drain-check` still treats 3
  as false. Under `--json`, that case is now the error envelope with code
  `drain_state_unknown` and `exit_code` 3, where it used to be
  `command_failed` with exit 1; a successful result adds a `source` field.
  Cases that move from exit 1 to exit 3: a `[name]` target that cannot be
  resolved because the city, its config or its store cannot be read, or
  that is ambiguous (a name a store that opened does not know still exits
  1); a session row that cannot be read or found (for example a
  `GC_SESSION_ID` that names no session row) while `GC_DRAIN` is clear; a
  session provider that cannot be constructed; a runtime error that is not
  a typed "session not found", including a local city whose tmux server is
  not running and an exec adapter whose `get-meta` fails for any reason.

- **New proxied scopes retire their proxy and Dolt child after 30 minutes
  idle.** Before this release every scope gc created was pinned to never, so a
  pair started by a stray read after `gc stop`, by `gc init --no-start`, or by
  `gc rig add` on an unstarted city stayed resident for good (about 300 MB RSS
  per scope). New scopes are now created with `[beads] proxied_idle_timeout`,
  default `30m`. The next bd command restarts a retired pair transparently
  (about half a second). One that arrives while the old pair is still shutting
  down can wait several seconds. **Existing cities do not change.** bd cannot
  yet change an initialized scope's idle timeout and gc does not edit bd's
  sidecar, so scopes created before this release keep never. `gc doctor`'s
  `proxied-idle-timeout` check reports the drift. Set `proxied_idle_timeout =
  "0"` to keep new scopes resident. With a finite timeout the opt-in
  `GC_BEADS_PROXIED_NATIVE` lane still serves short-lived reads natively, but
  the controller's long-lived handles read through bd (verdict
  `idle_policy_finite`) (#6561).

- **The first restart after upgrading reaps pre-upgrade ACP agents whose owner
  is gone.** Any city routing a session to ACP had process-table orphan
  reaping off — for its ACP sessions, and in a city that mixes ACP with a
  tmux or subprocess default, for every session including the tmux and
  subprocess ones. It is on again. An ACP agent started by the previous binary
  carries no `GC_ACP_CONTROL_SOCKET` marker, and once its supervisor has
  restarted no connection to it survives, so it reads untracked and the
  pre-start orphan sweep terminates it. That is the intended verdict — its
  owner's control socket died with the owner, so the agent could no longer be
  driven — and it happens once, on the first restart, not on every one
  (#6543).

- **`[daemon] session_reconciler` is now a known key.** A city that still sets
  the gc-enterprise spelling `"off"`, `"auto"` or `"require"` boots on the
  legacy reconciler with a deprecation warning, including under strict mode;
  remove the key. `"v2"` is reserved: a controller configured with it refuses
  to start in this release, and `gc doctor` reports it as an error. An unknown
  value also refuses controller start. Remove the key before rolling back to an
  older gc, which rejects it under strict mode.

- **`[beads] native_transport = "off"` and `GC_BEADS_FORCE_FALLBACK=1` refuse
  a `[storage]` binding served by `beads-workspace`.** That provider opens the
  native Dolt store, which "off" forbids. `gc start` refuses the binding before
  it opens anything or records an outcome, and the error names the cause: the
  city's setting or the process-wide variable. A deployment that sets
  `GC_BEADS_FORCE_FALLBACK=1`, for example to keep `.beads/hooks` scripts
  running, and binds a class to a beads workspace stops starting after the
  upgrade. Unset the variable and set `native_transport = "off"` only in the
  cities that need it, or remove the binding. `sqlite-beads` and the other
  providers that never open the native store are unaffected (#7036).

- **A command that opens a city's bead store fails when `city.toml` exists but
  does not load.** It used to open the store with default settings, which would
  ignore a `native_transport = "off"` in the file it failed to read. Fix the
  reported error to proceed; setting `GC_BEADS_FORCE_FALLBACK=1` does not
  bypass it. A directory with no `city.toml` is unaffected. A running
  controller keeps the stores it holds open on the value it read at boot, but
  the stores it opens for a single tick fail the same way. The `file` provider
  and `exec:` providers other than the bundled `gc-beads-bd` script are
  unaffected too, unless gc has to read the provider from a `city.toml` that
  cannot be parsed: it then falls back to `bd`, and the command fails (#7036).

- **An out-of-enum `[beads] conditional_writes`, `guarded_release` or
  `native_transport` value fails config load.** These keys are now checked on
  the composed config, so a bad value in `city.toml`, or in a fragment it
  includes, fails the load with an error naming the key. Before, a city
  with a bad `conditional_writes` or `guarded_release` value loaded, and its
  controller warned and ran with that gate off. Correct the value before
  upgrading (#7036).

- **An exec runtime that declares `report-activity`, `report-attachment` and
  `proc.exec` now reports `sleep_capability = full`, and its sleep is
  `stop`.** Before, every exec runtime reported `timed_only`. With all three
  declared, an interactive `[session_sleep]` policy applies: an idle
  interactive session is stopped and started again on wake, so anything kept
  only inside the box, such as the agent's conversation, is lost unless
  `stop` preserves it. Interrupts, nudges and restarts on such a runtime wait
  for the agent's ready prompt in the in-box tmux session `main`. For any
  runtime that declares `report-attachment`, an `is-attached` op that fails
  (any non-zero exit, 2 included) used to read as "not attached"; it now
  keeps the session awake and holds destructive actions. No runtime shipped
  with gc declares these capabilities (#7533).

- **A convoy sling through the API that routes some children and fails others
  answers 200 with `status: "partial"`.** The routed children have already
  changed state, so `POST /sling` returns the per-child outcomes instead of an
  error; a child refused because its source bead already has a live workflow
  also names the blocking workflows and the cleanup command. `gc sling`
  against a remote city prints the same per-child lines as a local sling and
  exits 1, or 3 when a child was refused that way. A gc built before this
  release, slinging a convoy through an upgraded supervisor, prints
  `"success": true` and exits 0 for such a result, though its `status` reads
  `partial`: upgrade gc with the supervisor, or have scripts check `status`
  (#7083).

- **`session.pending` and `session.pending_cleared` now carry actor
  `controller` instead of `api`.** The controller records them whether or not
  a client watches the event stream; a consumer that matches these events on
  actor `api` must match `controller` (#7083).

- **An ACP agent's permission request now waits for an answer through `/pending`
  and `/respond`.** gc used to drop the agent's `session/request_permission`, so
  the turn hung. It now holds the request, and `POST .../respond` answers it
  with 202 where it used to return 501. Echo the `request_id` from `/pending`;
  ACP ids have the form `acp-<nonce>-<id>`, so do not construct one. A stale id,
  an unknown `action` or an option kind the agent did not offer gets `409
  invalid_interaction`, and the request stays pending. An `action` equal to an
  offered option id selects that option, even an id that reads like `approve` or
  `cancel`. While a request is outstanding, a submit or nudge is refused with
  `409 pending_interaction`, including one already waiting on the busy turn,
  which used to wait out `nudge_busy_timeout`. Queued nudges stay queued,
  without spending retry attempts, until the request is answered or the nudge
  expires. The reconciler treats such a session as waiting on a human: it stays
  awake, and it is exempt from idle kill, max-age restart and config-drift
  drain. An unattended agent that asks therefore waits until an API client
  answers, so configure unattended pools not to ask, through the agent's
  permission mode or a permission timeout in the agent. A pool slot that
  holds a claimed bead is the exception, as before: the pool-slot backstop
  still drains it once it goes quiet, and the request goes with it. Every
  other agent-to-client request, such as `fs/read_text_file`, now gets
  JSON-RPC `-32601` instead of hanging the turn (#6598).

- **The core pack now runs `gc convoy check` every hour.** The new
  `convoy-autoclose-backstop` order closes any open convoy whose members are
  all closed and that is not labeled `owned`, catching closes the controller
  never acted on (made while it was down, before its autoclose sweep
  shipped, or in a store it does not cache). Its first run in an existing
  city can close many old convoys at once, each with a `convoy.closed`
  event. Label a convoy `owned` to keep it open, or turn the order off with
  `[orders] skip = ["convoy-autoclose-backstop"]` or an
  `[[orders.overrides]]` entry for it with `enabled = false`; the same
  entry's `interval` and `timeout` retune it (keep the timeout shorter than
  the interval). `gc convoy check` itself now skips suspended rigs (naming
  them on stderr), re-reads each convoy before closing it, goes on past a
  store it cannot open or list and a convoy it cannot read or close, and
  exits 1 when any convoy or store went unchecked; a convoy that changed or
  disappeared after the list is left for the next run and does not fail the
  check. Before, it stopped at the first error and skipped a store it could
  not open without saying so (#6614).

- **Builtin `codex` agents no longer default to `gpt-5.5` at `xhigh`
  reasoning effort.** Unless the city, agent or session chooses a `model` or
  `effort`, Gas City passes no `--model` or `model_reasoning_effort` flag, and
  codex runs on the model and effort from the user's own
  `~/.codex/config.toml`, which is often a different model and a lower effort.
  The builtin pin broke every turn for logins that cannot use that model. The
  launch command of every unpinned `builtin:codex` session changes, so each
  such session restarts once after the upgrade. To keep the old behavior, pin
  it for every agent on the harness in `city.toml` (`[providers.codex]` with
  `option_defaults = { model = "gpt-5.5", effort = "xhigh" }`), or for one
  agent with the same `option_defaults` line in its `agent.toml` (#7944).

### Added

- **`[beads] proxied_idle_timeout` sets how long a bd-owned proxied scope's
  proxy and Dolt child stay up with no connections.** bd retires the pair
  after that much quiet and the next bd command restarts it. The value is a Go
  duration; `"0"` means never, and a finite value must be at least `1m`. A rig
  can override it with `beads_proxied_idle_timeout`, except a rig that shares
  the city's proxy root, which always uses the city's value. The
  `GC_BEADS_PROXIED_IDLE_TIMEOUT` environment variable overrides both, for
  tests. gc applies the value where bd lets it: `gc init`, `gc rig add` and
  `gc beads city migrate-proxied`. bd has no way yet to change the value of a
  scope that already exists (#6561).

- **`[beads] native_transport` chooses whether a city's bead stores may open
  the native Dolt store.** `"auto"`, the default, keeps the current behavior:
  native when preflight-eligible, the bd subprocess otherwise. `"off"` keeps
  every store of the city on the bd subprocess, which a city needs while it
  still depends on `.beads/hooks` scripts. A `[beads]` fragment that
  `city.toml` includes keeps the city's value unless the fragment sets
  `native_transport` itself. A running controller keeps the stores it holds
  open on the value it read at boot until it restarts; `gc` commands, and the
  stores it opens for a single tick, read the current value.
  `GC_BEADS_FORCE_FALLBACK=1` still works as a deprecated alias for
  `"off"`; it overrides every city in the process and logs a deprecation
  warning once (#7036).

- **`gc rig add --include <binding>=<source>` chooses the rig import's
  binding.** Without it the binding is still the pack's name (its `[packs]` key
  or the source's last path segment); `--include gc=<source>` writes
  `[rigs.imports.gc]` instead. The left side must be letters, digits, `-` and
  `_`, so a URL or path is never split. A binding that two `--include` flags
  claim for different packs fails the add.

### Changed

- **`gc init` pins the gastown pack to registry release 0.1.11.** Fresh
  cities, `examples/gastown`, and the pinned-import examples in the docs now
  use `sha:66c672b5ed00af143158b4eac960523c56b977ee`; the 0.1.10 pin
  (`sha:33d3a430a67d`) is recorded as superseded, so `gc doctor` migrates
  cities still pinned to it.

- **A suspended rig or city is left cold.** gc no longer touches the bead
  store of a suspended rig, or of any scope of a suspended city. That covers
  cache scans, demand, order-tracking sweeps, the completions sweep,
  convergence, `beads-health`, `/status` counts, the dashboard rig probe and
  the core maintenance orders (reaper, jsonl-export, orphan-sweep, renudge).
  Once a suspended scope's sessions have drained, gc stops its bd proxy and
  Dolt child with `bd dolt stop`, the same step `gc stop` runs last, so a
  suspended scope holds no memory. A suspended city with no running session
  runs no controller tick and no order pass at all. `gc resume` / `gc rig
  resume` let the next command restart the pair; if something restarts a
  suspended scope's pair in the meantime, gc stops it again on its next tick.
  `/status` work counts no longer include suspended rigs, and report how many
  they left out in `work.suspended_rigs_excluded`. On a suspended city
  `/status` reads no bead store at all and sets `stores_not_read`, so an open
  dashboard does not restart the city's pair (#6561).

- **`gc doctor`, `beads-health` and the dashboard understand a finite proxied
  idle timeout.** A scope whose proxy retired on its idle timeout is idle, not
  down. On a running city, doctor reads it, which wakes it for one more idle
  period, instead of warning "store not running". `beads-health` and the
  dashboard's per-rig probe count it as healthy and do not ping it. A stopped
  city is still never started. A suspended rig or city is never woken: doctor
  reports its store checks as "not checked: suspended", and `gc start` and
  health leave it alone. The `proxied-idle-timeout` doctor check now compares
  the configured value with the scope's sidecar and with the running proxy,
  and reports drift as an advisory (#6561).

- **`gc rig add --include` resolves a remote pack's version like
  `gc import add`.** A non-bundled remote include used to be written with no
  version and no `packs.lock` entry, so `gc import check` reported a missing
  lock entry and the import floated. It now gets the constraint
  `gc import add` would write (the constraint the city already holds for that
  source, else the newest registry release, else the newest semver tag, else
  the remote HEAD commit) and a `packs.lock` entry in the same add. That needs
  network access to the source; if resolution fails, the add fails and
  `city.toml` and `packs.lock` are untouched. Only explicit `--include` flags
  changed: bundled packs, local paths, sources with an embedded `#ref`, and the
  imports a new rig gets from `[defaults.rig.imports]` or `default_rig_includes`
  are written as before.

- **`gc import add` without `--version` keeps the constraint the city already
  holds for a source.** `packs.lock` has one entry per source, so adding a
  source the city already imports (in any scope) or locks now writes the
  existing import's constraint, else one matching the source's `packs.lock`
  entry, instead of the newest release's, which could conflict with the
  existing constraint or move the lock entry every importer shares. A local
  path inside a git worktree is still locked to its current commit. Adding a
  pack with `POST /v0/city/{cityName}/packs` and no `version` changes the
  same way. Pass `--version` to choose another constraint.

### Fixed

- **`gc start --dry-run` no longer rewrites MCP config, skill links, or
  `.gitignore` in the city and its rigs.** The preview ran the stage-1
  passes for real while printing "No side effects executed": it adopted
  provider-native MCP files (a rig's own `.mcp.json` was rewritten to hold
  only gc's servers, its old content saved to a snapshot under
  `.gc/mcp-adopted/`), linked skills into `.claude/skills/` and the other
  provider sinks, added MCP entries to `.gitignore`, created agents' work
  dirs and installed provider hooks into them, refreshed formula links, and
  opened the event log. It now computes the same stage-1 changes without
  applying them and lists them under "Would change in the city and its
  rigs" (adopt, create, update, or remove each MCP file; links and legacy
  stubs per skill sink; `.gitignore` additions), resolves the agents it
  lists read-only, and skips the warm-up doctor scan, which can send mail.
  A preview still fails where the start would, for example on conflicting
  MCP targets. As before, it still writes gc's own state: it runs the
  bead-store lifecycle to read pool demand (the store is started, and a rig
  whose store is not yet initialized is initialized), fills the pack cache
  with what it takes to load the config, and creates the city's runtime
  directories under `.gc/` (`cache`, `system`, `runtime`, and an empty
  `events.jsonl`) when they are missing. Its footer now says so, in place of
  "No side effects executed".

- **The core `orphan-sweep` order times out a minute before its next run is
  due.** The order declared no timeout, so it ran under the 300s exec
  default, equal to its 5m interval. The cooldown counts from when a run
  starts, so a sweep killed at its timeout was due again at once, and on a
  slow store the sweep ran continuously. The order now sets `timeout = "4m"`,
  which leaves at most two minutes after the 120s foreign-identity gate
  budget, less the claim collection that precedes it. A city whose sweep
  needs longer can raise it with `[[orders.overrides]]`,
  `name = "orphan-sweep"` and a `timeout` (plus `rig = "*"` if a rig's own
  pack also carries the order).

- **The reaper's session prune no longer escalates a stale or missing backup
  when it has nothing to delete.** The pattern path
  (`GC_REAPER_SESSION_BEAD_PATTERN`, default `gm-*`) checked the city store's
  backup age before anything else, so a city without a fresh bd backup of its
  city store raised a MEDIUM "Reaper anomalies detected" escalation ("bulk
  prune skipped: backup stale or absent") for a prune that, in most cities,
  would delete nothing. The reaper now first counts the prune's candidates
  (closed beads that match the pattern and closed more than
  `GC_REAPER_SESSION_PURGE_AGE` ago) and, when there are none, skips the
  backup check and the prune without an anomaly. When there are candidates,
  the backup-age and type-scope gates apply as before. A count that fails is
  reported as its own anomaly (`session prune candidate count failed for
  <db>`), and the gates still apply. An invalid pattern is now reported before
  the backup is checked (#6284).

- **The work ladder and the execution backstop read a rig seat's started
  graph steps as started.** On a city that relocates its coordination classes
  (`[beads] infra = "local"`), a rig-scoped seat's `gc hook --claim` recorded
  a graph step it claimed in the session's `work_started` cell under the
  rig's work store (`rig:<rig>`), while the step lives in the city-scope
  class binding, where the ladder and the backstop look it up (`city`). The
  ladder then treated the seat's started claim as unstarted and nudged it as
  claimed (a release was still refused by the bead's `gc.session_id`), and
  the backstop missed the record. The hook now records the store the claim
  resides in, the spelling every reader uses, and marks the entry
  `resident`. An unmarked entry (one an older `gc` wrote, or an adoption
  whose store the hook could not prove) is read by bead alone, and only at
  the session generation it was recorded in; the hook rewrites an entry of
  the current generation to the resident spelling the next time it proves the
  claim's store. A rollback still reads the new entries.

- **A run the controller deletes no longer stays `pending` in the run
  census.** When the controller's wisp GC deleted a run whose rows its bead
  cache did not hold (closed rows, a restarted controller, an evicted entry),
  or the read just before a delete failed, no `bead.deleted` event was
  recorded. The run view drops a run only on that event and replays the
  retained event log on every restart, so the run stayed listed as `pending`,
  and canceling it returned `run-not-found`. Every delete made through the
  controller's bead cache now records `bead.deleted`, carrying just the bead
  id when no copy of the row is at hand, and a bead listed twice in one batch
  records it once. Deletes made outside the controller, such as a script's
  `bd purge` or `gc bd delete`, are not covered, and runs stranded before this
  fix stay listed (#5934).

- **A config reload no longer creates or rebinds a city's bead store.** When a
  city's `.beads/metadata.json` went missing (for example, a `git` checkout
  deleted a file an older commit had tracked), the next reload fell back to
  the default `hq` database, created it with `bd init`, and rebound the live
  city to that empty store. Reload, including a plain `gc reload` with no
  config change, now holds the city to the store binding it booted on. If
  metadata.json is missing or unusable, or names a different store, the
  reload fails and keeps the old config. The error says whether to restore
  the file (do not restart first: boot treats a missing binding as a new
  city) or to restart to adopt a deliberate rebinding. Switching a city from
  a non-bd beads provider (such as `file`) to bd now also takes a restart; a
  reload refuses it. Rig scopes and boot are unchanged.

- **Codex structured history keeps every turn's model and token usage.**
  Usage was read only from the rollout tail, so older turns (and older
  history pages) lost it once the session grew (thanks @csells).

- **The run API lists a formula-v1 run's steps and reports the run active once
  a step starts.** Formula-v1 instantiation types each untyped or `task`-typed
  non-root step `step`, and the run projection dropped that type. For a v1
  run, `GET /v0/city/{city}/runs/{id}/steps` returned no steps, and the run
  list, `GET /runs/{id}` and the run census kept the run `pending` while its
  steps were claimed and closed.

- **`GET /v0/city/{city}/sessions?state=closed` and `?state=all` list closed
  sessions.** The session list read only open sessions, whatever the filter,
  so a filter naming `closed` matched nothing. A filter that can match a
  closed session (`all`, or a list naming `closed`) now reads closed sessions
  too, reported with state `closed`; they come from the bead store rather than
  the cache. The default and every other filter still read open sessions from
  the cache. `gc session list --state closed` and `--state all` list them too
  when the city's supervisor answers the command.

- **Stopping a Claude turn that is waiting on a permission prompt now cancels
  the prompt.** The tmux interrupt sent Ctrl-C, which leaves Claude's prompt
  up; it now sends Escape (the prompt's own cancel key) when a prompt is on
  screen, never a numbered option. When it cannot read the pane it logs a
  warning and sends Ctrl-C as before. (thanks @csells)

- **A Claude API failure now shows as a provider error in structured session
  history, not as an assistant reply.** Entries Claude flags with
  `isApiErrorMessage` carry a `provider_error` system event (with Claude's
  error code) and are projected as system messages; refusals and text that
  merely quotes an API error stay replies. (thanks @csells)

- **A Claude turn interrupted while a tool was running now reads as idle.**
  Claude writes `[Request interrupted by user for tool use]` for that case,
  which the transcript activity check did not recognize, so the session kept
  reporting `in-turn`. The marker counts only as the whole message, not inside
  ordinary prose. (thanks @csells)

- **Claude transcripts are found again for work directories with underscores,
  spaces or other punctuation.** Current Claude Code names a project
  directory by replacing every UTF-16 code unit outside `[A-Za-z0-9]` with
  `-`; Gas City only replaced `/` and `.`, so history, resume-key probes and activity missed
  the transcript. Both spellings are now searched. (thanks @csells)

- **Codex `suggest` sessions start on current codex-cli and ask before
  acting.** `permission_mode = "suggest"` passed `--ask-for-approval
  untrusted`, a policy codex-cli 0.149.0 retired, so suggest sessions exited
  at launch. Suggest now passes `--ask-for-approval on-request --sandbox
  read-only`. Provider or agent `args` that spell the retired `untrusted`
  policy, or name `--ask-for-approval on-request` alone or beside `--sandbox
  workspace-write`, select suggest. Before, depending on the shape and the
  start path, they launched with the unrestricted
  `--dangerously-bypass-approvals-and-sandbox` default or failed to start.
  Agent `args` are now normalized like provider `args` when every
  schema-managed flag in them belongs to an option the `args` select: the
  options they select beat the agent's own `option_defaults` for those keys.
  Before, `gc session new` and API-created sessions dropped such flags and
  launched the provider's default mode, so an agent that asked for approvals
  through `args` could run with the bypass flag. Agent `args` with a flag that
  belongs to no option they select, such as a lone `--ask-for-approval never`
  or `--sandbox workspace-write`, are kept as written, as before, and are
  still stripped by `gc session new` and API-created sessions, which then
  launch the default mode: for codex, the bypass flag unless `option_defaults`
  choose another mode (ga-kg2mqb). A provider `resume_command` that names an
  `--ask-for-approval` policy no longer gets the bypass flag added beside it,
  which codex rejected. Suggest sessions, and sessions of agents whose `args`
  are now normalized, restart once after the upgrade because their launch
  command changes (#7671).

- **The OpenCode and MiMo Code plugins keep the stable prime at the head of
  the system prompt and drain queued nudges once per turn, per session.**
  `experimental.chat.system.transform` fires once per model generation, and
  on every generation of every session in the directory it ran the
  consumptive `gc nudge drain --inject` and `gc mail check --inject` and
  prepended their output, with the cached `gc prime --hook` output, to
  `system[0]` as one block. The drain emits a `Current time: ...` line on
  every call, even with an empty queue, so the bytes right after the prime
  changed every generation and the provider's prompt-cache prefix ended at
  the prime; everything after it, including OpenCode's own system text, was
  re-prefilled each time (#5732). Every tool call in a turn also emptied the
  queue again, into whichever generation won the race (#5552). The plugins
  now prepend only the prime to `system[0]` and append the per-turn text as
  a trailing system entry, after every stable entry, and they stop injecting
  from `chat.message`: OpenCode persists that hook's `output.message.system`
  on the user message and joins it into the tail of `system[0]` on every
  generation of the turn, which put the prime and the clock line back into
  the header. They run the two commands once per turn per session and repeat
  the drained text verbatim for every later generation of that turn, so a
  turn's system messages are identical across generations; with no known
  session or turn they drain every time as before. A turn opens on a user
  message the session has not reported before (its `message.updated`, which
  OpenCode publishes before the turn's first generation), so the background
  diff-summary update to an earlier user message, which can land several
  turns later, does not rewind the turn. Message ids are not compared: their
  encoded creation time wrapped on 2026-08-14 and follows the wall clock, so
  a newer message can sort below an older one. Each session remembers its
  last 64 user-message ids, so an update that arrives after its id has aged
  out of that window opens a turn and costs at most one extra drain. Child
  sessions no longer refresh the shared prime on `session.created` /
  `session.compacted`, which re-baked the beacon time into the parent's
  `system[0]` mid-turn and handed the child id to `gc prime --hook` as a
  candidate resume key. The MiMo Code plugin now also closes the `gc`
  child's stdin, as the OpenCode plugin already did (#5562); without that,
  `gc nudge drain --inject` waited for input until the 30-second timeout and
  the drain returned nothing. Turn state is per session, but the nudge queue
  is not: a child or human session in the same directory still drains the
  agent's queue, once per its own turn. OpenCode hook version 8, MiMo Code
  hook version 4; `gc` upgrades installed plugins below those or without the
  new transform and turn handling. (#5552, #5732, #6629)

- **An attached session is no longer idle-slept.** Under a `[session_sleep]`
  policy or an agent's `sleep_after_idle`, interactive sessions on local tmux
  and on exec runtimes that declare `report-activity`, `report-attachment` and
  `proc.exec` were drained "idle" once their pane had been quiet for the
  window, while someone was reading them in a terminal. On any runtime that
  reports attachment (tmux, k8s, ssh, exec with `report-attachment`), so were
  non-interactive sessions someone had attached to. An attached session now
  stays awake, and an attach overrides an idle drain the reconciler had
  already begun. For the interactive sessions above, the idle window starts at
  detach; a non-interactive session's window still runs from its pane's last
  activity, so a quiet one can sleep as soon as it is detached. A user hold or
  another explicit sleep request still wins over the attachment. Both the
  legacy reconciler and the v2 allocator apply the rule (#7594, follow-up to
  #7533).

- **A message sent after stopping a Claude turn early is no longer merged
  into the stopped prompt.** When `POST .../stop` (or `interrupt_now`) lands
  before Claude's first response chunk, Claude Code puts the interrupted
  prompt back into its input box. gc cleared the input box with one Ctrl-U,
  which on Claude Code 2.1.288 deletes only one wrapped row, so the next
  message was pasted behind the rest of the old prompt and both went out as
  one. On the tmux runtime gc now presses Ctrl-U Ctrl-K until the input box
  reads empty, wherever the cursor is, and fails the delivery if it will not
  empty; `stop` clears the restored prompt too (see 1.5.1 Security). A
  submit whose input box still holds any text is no longer reported as
  delivered.

- **A new scope directory over an existing current-era managed Dolt database
  initializes instead of being refused as a legacy Dolt server workspace.**
  `gc-beads-bd init` now stamps bd's version witness on a store that already
  existed only when its `schema_migrations` table proves it current-era (an
  adopted pre-1.0 store is still refused, #5294), never forces a reinit when
  a schema probe does not answer, and resets a bootstrap that was interrupted
  between a migration's DDL and its commit, only while it holds the
  database's init lock exclusively. Behavior change: with bd 1.0.5 or later,
  init on an already-initialized scope now runs `bd migrate schema`, and fails
  with bd's message when that fails for any reason other than bd's
  remote-migrate refusal (which is reported as a warning). An older bd has no
  `bd migrate schema`, so init skips the step with a warning and bd applies
  any pending migrations on its next write. With bd 1.3.0 or later the step is
  also bd's consent to promote the schema of a database on a shared server
  (gastownhall/beads#5920), which can lock out an older bd that uses the same
  database; init writes what the step reports to its standard error, which gc
  shows only when init fails. Init's migration steps also wait up to
  `GC_DOLT_INIT_LOCK_TIMEOUT_MS` for a concurrent initializer's reset or
  forced reinit to finish, and fail closed if it does not. That lock lives in
  a per-user `gc-beads-bd-init-locks-<uid>` directory under `$TMPDIR` or
  `/tmp` unless `GC_DOLT_INIT_LOCK_DIR` names another; where `flock` is
  installed, init fails naming the lock file when it cannot create it.
  Without `flock`, init refuses to reset an interrupted bootstrap and says
  how to install it (#5926).

- **`passthroughEnv` now honors `GC_SUPERVISOR_ENV` when deciding which
  non-`GC_`-prefixed variables reach a spawned agent session, not only which
  ones survive into the persisted service file.** The two allowlists used to
  be independent: opting a variable into `GC_SUPERVISOR_ENV` widened the
  systemd/launchd unit's environment, but `passthroughEnv`'s sweep still only
  forwarded `GC_`-prefixed keys into sessions, so a variable could be fully
  persisted into the supervisor's own process and still never reach an agent.
  One opt-in list now governs both, so declaring a variable once is enough.
  Behavior change: variables already listed in `GC_SUPERVISOR_ENV` will now
  also be forwarded into agent sessions.

- **`gc mail send --from` can no longer forge another live session's
  identity.** A session could previously claim `--from <name>` for any
  other live, named session in the city with zero authentication — mail
  would display as coming from a privileged coordinator role even though
  it was actually sent by an unrelated worker. `--from` is now checked
  against the calling session's own identity. A live agent session may only
  send as itself — it can no longer claim another live session, the
  operator's `human` identity, or `controller`. A caller with no
  `GC_SESSION_ID`/`GC_ALIAS`/`GC_AGENT` set (an interactive terminal, or an
  exec order running under the supervisor's environment) may still claim
  any sender, which keeps scripted `--from controller` automation such as
  the dolt compact quarantine alert working. A caller whose identity
  variables are set but resolve to no live session is refused. This is a
  spoofing guard, not authentication: those variables are caller-controlled.

  **Upgrading:** scripts that pass `--from human` or `--from controller`
  while running inside an agent session (or under a supervisor that
  inherited one's `GC_*` session variables) now exit 1 with "does not match
  this session's own identity".

- **The reaper's stale workflow-root sweep works on large stores again.**
  Step 2 checked every candidate workflow root for live descendants in one
  recursive query, which on a large store ran past bd's 10s read timeout, so
  no stale root ever closed. It now checks
  `GC_REAPER_WORKFLOW_ROOT_BATCH_SIZE` candidates per query (a positive
  integer, default 1) in id order, and stamps and closes each page's stale
  roots before it reads the next. Keep the batch small: one query walks the
  subtrees of a whole page, so a batch as large as the candidate set brings
  back the single query that timed out. `GC_REAPER_WORKFLOW_ROOT_BUDGET_SECS`
  (default 300s, inside `GC_REAPER_RUN_BUDGET_SECS`) is a window that opens at
  the run's first census and runs on through the purge, the later steps and
  the later scopes; once it has closed, each census checks one page. Each
  census starts at a random candidate and wraps around, so roots it keeps
  cannot hold back the roots behind them run after run. A census cut short is
  reported as a partial outcome. The summary's new
  `workflow_root_census:<checked>/<candidates>` field shows how many of the
  counted candidates a run checked; a root that becomes a candidate during a
  census can wait for the next run. Outside the city scope, workflow issue
  roots are only counted: `skipped_non_city_workflow_issue_roots` is now a
  candidate count (#6050).

- **Suspending a session shortly after it woke keeps its conversation.** An
  operator, idle or `gc stop` suspend no longer counts as a rapid crash or
  churn that cleared `session_key`, so the next wake resumes. (thanks @csells)

## [1.5.1] - 2026-10-24

### Upgrading Notes

- **Exec adapters and setup commands are now cancelled with SIGTERM, not
  SIGINT.** When gc cancels an exec session adapter, or a `session_setup`,
  `session_setup_script`, `session_live` or `pre_start` command run by the
  tmux or herdr provider, it sends SIGTERM to the command's process group,
  then SIGKILL after the grace period: 2s, or 10s for setup commands when
  `[session] setup_max_timeout` is set (#7018, backported in #7953). Under
  SIGINT a rollback trap could be skipped, leaving behind whatever the script
  created: a shell that inherits SIGINT ignored (as a background job of a
  non-interactive shell does) cannot trap it, and a SIGINT that lands during
  a command substitution can wedge bash 5.2. If your script traps only `INT`,
  add `TERM` (e.g. `trap rollback INT TERM`). Non-shell adapters that relied
  on SIGINT's default action (e.g. Python `KeyboardInterrupt`/`finally`) must
  handle SIGTERM explicitly. The bundled `scripts/gc-session-docker` already
  traps `TERM`.
- **Upgrade Beads (`bd`) to v1.3.2.** gc 1.5.1 pins and is tested against bd
  v1.3.2 (`deps.env` `BD_VERSION` and the go.mod library), a stable release
  that keeps bd v1.3.1's schema: v1.3.1 plus the `bd purge` live-dependent fix
  (beads#7031), the backup watermark fix (beads#7032), the blocked-state
  recheck publication fix (beads#6876, backported in beads#7146), the skipped
  no-op `DOLT_CHECKOUT` on fresh connections, so a least-privilege Dolt user
  works with the native store (beads#7322), batched bulk ID reads, so
  `bd export` and gc's `jsonl-export` order no longer stall on very large
  wisp sets (beads#7603), `bd purge` keeping `--limit` in its confirmation
  hint (beads#7540), and a Go 1.26.10 / x/net / grpc / thrift security
  rebuild. Install it with `brew upgrade beads` (or `brew install beads`),
  from the
  [v1.3.2 release assets](https://github.com/gastownhall/beads/releases/tag/v1.3.2),
  or with `go install github.com/steveyegge/beads/cmd/bd@v1.3.2`. Read beads'
  [v1.3.2 upgrade notes](https://github.com/gastownhall/beads/blob/v1.3.2/CHANGELOG.md#132---2026-10-23)
  before upgrading scripts that call bd directly. A city that stays on bd
  v1.3.1 keeps working: a proxied city or rig (the default for new cities)
  always uses the bd CLI, and `gc doctor` does not flag bd v1.3.1 there; one
  that connects to its Dolt server directly falls back from its native store
  to the bd CLI (`version_compat` preflight gate), with a `gc doctor`
  `beads-store` warning.
- **A city's first start after a bd upgrade runs `bd recompute-blocked` once
  per scope.** beads migration 0059 wrongly marks some beads blocked across
  relates-to, discovered-from and other non-blocking edges (beads#7037). It
  hits any store that crossed 0059: one upgraded from bd v1.2.x or older, or
  one that ever ran bd v1.3.0 or v1.3.1. Those beads drop out of `bd ready`
  and gc stops dispatching them. gc repairs the city and each Dolt-backed bd
  scope it owns, records the bd version in that scope's bd config
  (`custom.gascity.blocked_repair_bd_version`) so later starts skip it, logs
  `recomputed is_blocked for <scope> under bd <version>: N rows corrected`,
  and emits `beads.blocked.recomputed` (a fresh city records one 0-row event
  per scope). The supervisor does the same, and `gc rig add` repairs the rig
  it adds. It does not touch external Dolt servers, complete storage bindings
  or non-bd providers: run `bd recompute-blocked` in those scopes yourself.
  Scopes are repaired four at a time before agents start: a city with three
  rigs and 5,000 beads per scope took about 1.5s. A failed repair only warns,
  and gc retries it on the next start. The repair runs only when a city
  starts: if you upgrade bd while a city is running, run `gc restart` for it
  (or `gc stop`, then `gc start`), or restart the supervisor and every city it
  hosts with `gc supervisor stop --wait`, then `gc start`. `gc start` on a
  city that is already running does not repair it (#7087).
- **A city migrated by `gc storage migrate` in gc 1.5.0 refuses to start
  until you repair it.** 1.5.0's migration kept the work store's copy of every
  bead it moved, and any reader that reached the work store first was served
  the frozen copy, so finished workflow steps were dispatched again and again
  (#5987). `gc storage migrate` now clears those copies after the copy is
  proven: it writes every row the copy manifest names, with its edges and
  payloads, to `<binding root>/infra.retained-source.jsonl`, re-reads the
  backup and checks it against the source, and only then removes the rows.
  Rows the manifest never proved block the clear and name
  `gc storage recover-stranded`. Boot refuses a converged city that still
  holds copies (`storage.binding.unconverged`, outcome `retained-copies`)
  and prints the repair: `gc stop`, then
  `gc storage migrate --from-work --fleet-stopped`, then `gc start`. The same
  run resumes an interrupted clear. `gc storage status` reports the backup
  and the retained copies and exits non-zero while copies remain. After a
  clear, pointing `[storage.classes]` back at `work` (or deleting
  `[storage]`) is refused, because the work store no longer holds that
  state; rolling back now needs the backup. The backup holds the bead fields
  the copy carried, not bd-only fields (notes, comments, close reason,
  history). On bd and native Dolt work stores, a blocking edge from a work
  bead into a still-open moved bead, and `tracks`/`related` edges into moved
  beads, cannot be kept: migrate lists each one before it deletes anything,
  records it in `gc storage status` and on the `storage.binding.*` event
  (`lost_cross_edges`), and keeps it in the backup. Of the six split-city
  issues that 1.5.0's "`gc storage migrate` is experimental" note listed as
  open, #5987 is fixed by this change, and #5974, #6015 and #6129 are fixed in
  this release (see Fixed). #6242 was already fixed in 1.5.0:
  `gc convoy delete-source` and `reopen-source` read the relocated graph
  binding (#5633). #6348 applies only to a city that relocates nudges alone,
  which boot refuses: this build serves either no relocated class or all five
  infrastructure classes on one binding. `gc storage migrate` is still
  experimental: run `gc storage preflight` first, back up every store
  involved, and do not migrate a production city you cannot restore. See the
  "Upgrading a city migrated by gc 1.5.0" section of
  `docs/runbooks/split-storage-classes.md` (#7074).
- **`gc init` and `gc start` refuse to register a city with a supervisor
  from a different gc installation.** When the running supervisor's
  executable is a different file from the invoking `gc` (for example a
  Homebrew launchd supervisor and a tarball `gc`), registering would hand the
  city to the other binary. Both commands now refuse and print both
  identities and the fix: `<this gc> supervisor install --force` for a
  launchd or systemd supervisor, or stop a directly started one. `gc init`
  has already written the city and tells you to run `gc start` afterwards.
  Pass `--allow-supervisor-mismatch` to keep a deliberate mixed setup; `gc
  start` then also skips the build-drift restart, which could only relaunch
  the other binary. An in-place upgrade of the same executable never refuses:
  `gc start` restarts the supervisor on build drift, now also after a
  Homebrew upgrade, where both build ids read `unknown` and the release
  version decides. `gc status` only warns, on stderr. The check adds one
  `/health` call (2s bound) when a supervisor is running and is silent when
  none answers (#7046).
- **Restart the supervisor on the new binary before running
  `gc doctor --fix`.** gc 1.5.1 moves the pin `gc init` writes for the
  bundled `[imports.core]` and `[imports.bd]` packs to a commit whose trees
  match the content this binary embeds. The June 2026 pin every gc through
  1.5.0 wrote is now always served from the embedded packs and never fetched
  (fetching it would install the June core pack), so an upgraded city keeps
  loading unchanged.
  `gc doctor` now reports the old pin as a `packv2-import-state` warning, and
  `--fix` re-pins it offline. A 1.5.0 supervisor that is still running does
  not know the new pin and fails with "locked but not cached" until
  `gc import install` fetches it, so run `gc supervisor stop --wait`, then
  `gc start`, on the new binary first. Stopping the supervisor stops every
  city it hosts until `gc start` runs. Each superseded pin gets its own
  synthetic cache directory until `gc doctor --fix`. Downgrading a city
  initialized or re-pinned by 1.5.1 to gc 1.5.0 needs one `gc import install`
  with network (#7070).
- **Dolt 2.2.0 is the pinned Dolt.** `deps.env`, the installers, CI, the
  container images and the Kubernetes manifests move from Dolt 2.1.7 to 2.2.0,
  the Dolt the pinned beads release is tested on. Do not move a city to Dolt
  2.3.x: 2.3.0 and 2.3.1 break `CALL DOLT_RESET('--hard')` on some fresh
  databases, and later releases have not been measured by beads (#7070).
- **`gc stop` exits non-zero when it cannot confirm that every session
  stopped.** When listing sessions fails completely or in part, the orphan
  sweep's listing fails, or checking a target session returns an error,
  `gc stop` no longer prints `City stopped.` and exits 0. It still stops
  every session it did see and shuts down servers and the bead store as
  before, names the sessions it could not verify, prints
  `gc stop: stop not confirmed: ...` and exits 1; on the supervisor path the
  registration is restored. An absent runtime server still counts as a
  complete, empty inventory, so stopping a stopped city still succeeds, except
  on the herdr runtime, which cannot tell a missing server from a failing one:
  a repeated `gc stop` of a herdr city whose server is already gone reports
  `stop not confirmed` and exits 1. Scripts that treat a non-zero `gc stop` as
  fatal should resolve the reported runtime error and run `gc stop` again; on
  herdr, once you have confirmed the herdr server is not running, treat that
  exit as stopped (#6764).
- **An exec session provider's `start` or `provision` that exits 2 now fails
  the start.** Exit 2 meant "unknown operation, treat as success" on `start`
  and `provision` too, so a `set -e` wrapper whose `grep` or `jq` exited 2 during `start`
  counted as started and the half-created box was never cleaned up. Neither op
  can be unknown, so exit 2 from them is now a failure, and a failed `start`
  runs `stop` to tear the box down unless the name was already in use. Wrapper authors: return 0 from `start` and
  `provision` on success. Exit 2 keeps meaning "unknown operation" for other
  undeclared operations.
- **A SQLite bead store holding wrapped ids refuses to mint until you run
  `gc storage repair-sequence`.** Older builds could wrap a SQLite store's id
  sequence past 9223372036854775807 into `<prefix>--<n>` ids and then re-issue
  ids. gc now keeps a persisted, fsynced floor per prefix and never re-issues
  an id: on a store where a negative `<prefix>--<n>` id ranks above the floor,
  creates fail with an error naming `gc storage repair-sequence`. Run it
  without `--floor` to see the floor and the highest id; the command help
  explains whether to delete stray rows or raise the floor (a negative floor
  on a wrapped store cannot be undone). Stop every process running an older
  build before repairing. Builds without this fix refuse to open a store whose
  floor is negative (#6834).
- **`gc doctor --json` reports `"ok": false` when a blocking check fails.**
  The JSON envelope always said `"ok": true`. It now matches the exit code:
  `false` only when `blocking_failed` is non-zero, `true` for advisory-only
  failures. Automation that read `ok` alone was missing real failures
  (#5950, #6006).
- **`gc import add --version <semver>` resolves registry packs against the
  registry's releases.** A pack published in a configured registry, such as
  the gascity-packs catalog, is versioned by its `[[pack.release]]` entries,
  not by git tags, so the "Exactly this version" command
  `gc pack registry show` prints failed with `no tags ... match constraint`.
  Worse, a range such as `--version '>=0.1.1'`, a plain `gc import add`, and
  `gc import install`/`upgrade` of a semver constraint resolved against the
  gascity-packs repository's own tags and could lock the wrong content (for
  bmad, `0.4.0`, a repository tag that is no pack release). Every lock sync
  (add, install, upgrade, `gc rig add`, doctor repairs, init) now resolves
  those sources against the registry's non-withdrawn releases, records the
  release version and commit, and verifies the fetched content against the
  release hash; a mismatch fails without writing `pack.toml` or
  `packs.lock`. A plain 1.5.0 `gc import add` of a registry pack recorded a
  range built from the repository tag it locked (for bmad,
  `version = "^0.4"`), and that range matches none of the pack's releases:
  `gc import upgrade` fails with
  `registry pack main:bmad has no release matching "^0.4" (available: ...)`,
  and once that run has cached the registry catalog, `gc import install` fails
  the same way. Set `version` in every such `[imports.<name>]` entry to a
  range of the listed releases (for example `^0.1`), then run
  `gc import upgrade`; one import left unedited fails the whole sync. For a
  city with a single such import, `gc import remove <name>` followed by
  `gc import add <source>` also works (#7071).
- **A formula step that means "blocked, stop the graph" must close with
  `gc.outcome=fail`.** A formula step bead (one carrying `gc.step_ref`) that
  closes with `gc.outcome=pass` now satisfies its dependents even when it also
  records `gc.work_outcome=blocked`, so workflows no longer stall after such
  a step. Plain work beads are unchanged: a blocked work outcome still
  withholds their dependents.
- **`bead.closed` is emitted for every close gc observes.** A
  `Update(status=closed)` now emits `bead.closed` instead of `bead.updated`,
  re-closing an already-closed bead no longer re-emits, and orders and
  autoclose now fire for closes made with `bd` or `gc bd` (#6860).
- **`nudge-on-route` no longer retries a failed nudge on a repeated, unchanged
  `bead.updated`.** The controller now drops a `bead.updated` that repeats the
  last one recorded for the bead within 20 minutes (see Fixed). The order
  nudges a routed bead's target once, and it used to retry a failed nudge each
  time the unchanged bead was announced again; it now retries when the bead
  changes, or when the unchanged bead is recorded again after 20 minutes.
  Other orders and scripts that trigger on `bead.updated` likewise no longer
  see those repeats (#7023, backported in #8228).
- **API session creates for a demand-only singleton are refused.** `POST
  /sessions` with `kind: "agent"` for a pool agent with
  `max_active_sessions = 1`, `min_active_sessions = 0` and no
  `[[named_session]]` now returns 400 `demand-only-singleton` instead of 202
  and a session that never started. An API wake of such a session records the
  wake (clearing holds) and returns the same error unless the session is
  running or its start is already in flight (#6858).
- **A partially routed convoy sling answers 200 with status `partial`.**
  `POST /v0/city/{city}/sling` reports a convoy whose children were routed
  only in part as 200 with `status: "partial"` and the failed children in
  `batch.failures`, so API clients must check `status`, not only the HTTP
  code. A convoy where nothing routed is still an error. A remote `gc sling`
  older than 1.5.1 does not check `status`: it exits 0, and its `--json`
  output reports `success: true`. Upgrade remote `gc` clients alongside the
  server.
- **Supervisor API status codes and payload shapes changed for several
  edits and reads.** API clients that branch on the old codes or on `null`
  fields need updating:
  - Edits to a rig-qualified agent served by a generic `scope = "rig"`
    template (`PATCH`, `suspend`, `resume` on `/agent/{rig}/{name}`) now
    apply to the template, and so to that agent in every rig the template
    serves, and return 200 instead of 404; `DELETE` on such an agent returns
    409; delete the template instead (ga-l24lrx, #7076).
  - A config edit whose `[patches]` entry names a target missing from the
    merged config returns 400 instead of 500.
  - Closed sessions report `state: "closed"` instead of an empty state, on
    `GET /session/{id}` and in the session list, which returns them for
    `?state=closed` or `?state=all`.
  - Unset optional fields are omitted instead of sent as `null`: the map and
    struct fields of agent, rig and provider patches (`Env`,
    `OptionDefaults`, `Pool`, `ContextAdvisory`, `FormulaVars`), and the
    absent records and `Metadata` of external-messaging inbound/outbound
    results. The OpenAPI spec no longer lists them as required.
- **The reaper's age gates use UTC on non-UTC Dolt servers.** Bead
  timestamps are stored in UTC, but the reaper compared them with the
  server-local `NOW()`. It now uses `UTC_TIMESTAMP()`. On a UTC server
  nothing changes. West of UTC, the first run after upgrading catches up on
  the closes, stale-issue closes and session prunes the old gates delayed by
  the server's offset; east of UTC, rows the old gates handled early now wait
  the full configured age. The session-prune guard now counts candidates on
  the same UTC cutoff `bd prune` uses, so it can no longer miss non-session
  beads `bd prune` would delete (#7136, backported in #7498).
- **An unchanged reaper or Dolt backup escalation mails once a day, not on
  every run.** The `reaper` order (every 30m) and the `mol-dog-backup` order
  now raise their failures as incidents. `escalate.sh` records each delivered
  incident under `GC_ESCALATE_INCIDENT_DIR` (default `<city>/.gc/incidents`)
  and, while its fingerprint is unchanged, sends nothing until
  `GC_ESCALATE_REMINDER_SECONDS` (default 86400) have passed; a changed
  fingerprint mails at once, and a clean run resolves the incident so the
  next failure mails at once too. A suppressed run says so on stdout and
  exits 0. To have the next run mail an incident at once, delete its
  `.gc/incidents/*.state` file. A pack that overrides `escalate.sh` now
  receives `--incident-key KEY --fingerprint F` on an alert and
  `--resolve-incident KEY` on a clean run: the override must accept or
  ignore those flags and must send nothing for `--resolve-incident`. An
  override written for 1.5.0, which rejects the flags as a usage error (exit
  2, or an error that names the flag), keeps alerting: the order sends the
  alert again without them, so it mails on every run, without
  deduplication, and the order's output names the override as predating
  incident keys. See "Why a maintenance alert did not repeat" in
  `docs/getting-started/troubleshooting.md` (#6363 and #8231, backported
  in #8218 and #8232).

### Added

- **`gc storage repair-sequence`** inspects a SQLite bead store's id-sequence
  floor and, with `--floor`, raises it (`--dir` for a store other than the
  city's SQLite infrastructure binding, `--prefix`, default `gcg`). See
  Upgrading Notes (#6834).
- **`gc storage migrate --from-backup`** re-copies a cleared city's binding
  whose database is gone from the retained-source backup, with edges and
  payloads. It runs only in that state; `--from-work` refuses there, since the
  work store no longer holds the slice (#7074).
- **`--allow-supervisor-mismatch` on `gc init` and `gc start`** registers or
  starts a city even when the running supervisor is a different gc
  installation (#7046).
- **`gc agent is-foreign <identity>` reports whether this city can vouch for
  a claim's assignee.** It exits 0 (`local`) when the identity resolves
  against this city's roster, or when one of this city's own session records,
  open or closed, answers to it under its bead ID, session name, alias,
  configured named identity or sole alias-history entry; 1 (`foreign`) when
  neither does; and 2 (`unknown`) when it cannot answer (no roster, a failed
  lookup, an unresolvable city, bad usage). `--json` adds the reason
  (`roster`, `session_record`, `foreign_binding`, `absent_from_roster`,
  `no_config`, `lookup_failed`) and its detail. `local` is not a liveness
  claim. Scripts and formulas that reap claims should ask it instead of
  re-deriving identity rules, and treat anything but exit 0 as "protect"
  (#5955, #7895, backported in #8230).

### Changed

- **Ready work in a SQLite infra ledger is ordered priority-first.** On a city
  that relocates classes to a `sqlite-beads` binding, that ledger's ready read
  returned rows oldest-first (`created_at, id`). It now returns the canonical
  `(priority, created_at, id)` order that `bd ready` and gc's cached ready
  reads already use, and applies `Limit` after sorting. When several ready
  beads in the ledger route to one pool template, the bead a new session takes
  its trigger, worktree and pack from is now the highest-priority one, oldest
  first within a priority; the order in which sessions claim work does not
  change. `GET /v0/beads/ready` also returns the relocated graph leg's rows in
  this order (#6863).
- **`gc supervisor start` on macOS starts the installed launchd job.** When
  the supervisor LaunchAgent is installed and launches this `gc`, `gc
  supervisor start` loads and kickstarts it instead of forking a detached
  supervisor that ran outside launchd's KeepAlive and passed every provider
  credential in your shell to agent sessions, even with
  `GC_SUPERVISOR_OMIT_PROVIDER_CREDS=1`. Where launchctl cannot start the job
  (no GUI session over SSH, CI) it warns and forks as before. A plist that
  launches a different gc installation is never started; `gc` forks and names
  `<gc> supervisor install --force` (#6966, #7048).
- **A slow Dolt identity probe is retried instead of failing `gc init` and
  `gc start`.** The `dolt config` identity probes (`user.name` and
  `user.email`) ran once with a fixed 2s deadline, so a loaded host stopped
  startup with `dolt config probe timed out after 2s`. They now get a 5s
  budget, then retries at 10s and 20s with a stderr notice per retry, and
  share a 60s total. The `dolt version` and `bd version` probes, whose timeout
  only skipped the minimum-version check, get the same retries. A probe that
  exits on its own still reports its result at once (#7044).
- **The `gascity/roles` rig pack is served from the gascity pack's embedded
  copy.** `gc init` with the default gascity template and `gc rig add` no
  longer clone the whole gascity-packs repository for it, so they work
  offline. Existing cities need no migration; once the old clone has not been
  modified for `--keep-days` days (default 7), `gc import prune --apply`
  reclaims it (ga-73eoo, #7005).
- **Tutorial 01's sample `city.toml` matches what `gc init` writes,** and it
  explains that `sha:<pinned commit>` on the `core` and `bd` imports names the
  copy of each pack bundled with `gc` (#7045).
- **Config, exec-provider, agent, orders and API reference docs now match the
  code.** The `wisp_ttl` example was `"7d"`, which does not parse (Go
  durations have no day unit) and leaves wisp GC off; it is now `"168h"`.
  `idle_timeout` and `max_session_age` say they do not apply to
  `mode = "always"` named sessions, the Gastown recipes stop setting
  `idle_timeout` on the always-on mayor and show how to suspend `boot`, and an
  exec order's default `timeout` reads 300s, not 60s. The exec session
  provider page lists every field `start` receives, the per-operation
  deadlines, the `stop` after a failed start and what the session `env`
  carries, and no longer calls `start` fire-and-forget, says gc sends the
  startup `nudge` through the `nudge` operation, or says the start config
  leaves out `ready_prompt_prefix` and `ready_delay_ms`. The agent guide covers
  Claude's builtin `permission_mode = "unrestricted"` and `effort = "max"`
  defaults and how to override them; the orders tutorial covers `rig = "*"`
  and not pairing an override with `skip`; the API reference says what each
  endpoint's agent count counts and that the supervisor serves its
  spec at `/openapi.json`, `/openapi.yaml` and `/docs`; the events reference
  says the `cache-reconcile` actor names the cache, not whoever closed the
  bead; the one-controller rule in the hardened-city runbook now covers every
  city and Kubernetes rollouts; and the `contrib/` scripts link to pages that
  exist (#8212).

### Fixed

- **Split storage: `gc bd show` and `gc bd dep list`/`dep tree` no longer
  answer a relocated bead from the work store's pre-migration copy.**
  `gc bd show --long <id>`, `gc bd show <id1> <id2>` and
  `gc bd dep list`/`dep tree` of a relocated bead fell through to bd on the
  work store and answered from the retained copy; unserved by-id reads now go
  through the same residence check as writes (#6015, #7066).
- **Split storage: `gc sling <target> <formula> --formula` works on a city
  whose graph class is relocated.** It failed with `setting gc.routed_to on
  gcg-1: ... bead not found` and leaked an open, unrouted wisp per attempt;
  the root is now routed through the store that minted it (#6054, #7059).
- **Split storage: `gc sling` refuses a reserved coordination-class id**
  (`gcg-`, `gcm-`, `gcs-`, `gco-`, `gcn-`, `gcnq-`) that no store holds,
  instead of creating a new task bead titled with it (#5974, #5975).
- **Split storage: `gc workflow delete` and `gc convoy delete-source` reach
  ephemeral steps in a relocated binding.** Wisp-tier steps, ephemeral roots
  and nested workflows launched from a wisp step were left open and rootless
  (#6129).
- **Split storage: pools demand-spawn for routed work in the city work
  ledger.** On a converged split city the pool-demand probe read only the
  class binding, so a routed bead in the work ledger was claimable but never
  counted; demand now counts over the work ledger then the binding, as
  `gc ready` reads them. Single-store cities are unchanged (#6019).
- **Fenced writes to relocated classes honor `[beads] conditional_writes` on
  split cities.** On a city that relocates sessions, graph, messaging, orders
  or nudges to a storage binding, nothing stamped the conditional-writes mode
  onto the binding engine, and the one-shot CLI's emitting wrapper hid the
  stamp again. Under `auto` and `require` every fenced write to a relocated
  class (session wake/kill fences, pending-create rollback, molecule attach,
  control epoch, drain reservations) silently took the unconditional legacy
  path. The binding engine is now stamped, the SQLite engine reports whether it
  can fence, the `require` boot preflight probes the binding once per engine,
  and `gc status` reports the binding engine's mode (#6917, #7194).
- **A control row only wakes the control dispatcher of its own root scope on a
  split city.** The default `scale_check` probe re-read the durable route of a
  control bead whose route repair had been suppressed or deferred, and on a
  class-binding city every dispatcher reads the binding, so the dispatcher the
  stale route named was demand-spawned for a row it must not serve (#7206).
- **Split-storage cities no longer close seats that still hold work-store
  claims.** On a city whose sessions live on a class binding, the pool-slot
  close gate, the drain-ack escalation, the pool-slot retire and the stranded
  repair read the sessions binding as the work leg and never saw claims on the
  city work store, so seats closed while holding claims and the stranded
  branch never ran. Every close gate now reads the census's leg set (the city
  work store, the serving rigs, the bindings) for every seat, from a per-tick
  seat work index that reads each leg once per tick. A rig-bound seat's
  city-store claim now holds the seat, and rig stores `city.toml` does not
  declare are no longer read (#7480).
- **The close cascade releases a closed seat's work on every leg.** On a split
  city a dead or closed seat's claims on the city work store and the rigs
  stayed on the closed row until the orphan sweep reset them. The cascade now
  releases on the gates' legs and under the gates' identities (a claim made
  under an old alias is left to the orphan sweep), fencing each release on the
  row's status and assignee (#7487).
- **A claim made after a tick's read stops the close.** Before a gate-decided
  close, and before an idle-timeout kill, gc re-reads the seat's claims and
  keeps the seat when it finds one; a close that releases anyway releases the
  late claim too. The re-read is live on local stores and answers from the
  cache only on the work store and bd rigs: when the cache cannot answer, the
  close or kill goes ahead on the tick's read, and a late claim there stays on
  the seat (#7525).
- **`gc bd show <id>` works again on a refused split city whose binding holds
  migrated beads.** The by-id relic refusal now applies per id: only an id the
  class binding holds, or that the migration's copy manifest records
  delivering, is refused as a frozen pre-migration copy. Every other work bead
  reaches bd. Without a readable manifest the old city-wide refusal still
  applies whenever the binding holds any id outside its reserved prefixes
  (#7430).
- **`gc storage status` reports a boot-plan refusal and exits non-zero.** Two
  of its three paths, including a served split city, returned 0 without
  resolving the boot plan; the refusal is now reported in the readout and in
  the exit code. An unsupported `[storage.classes]` arrangement is still
  reported only by `gc start`: status prints `binding: none` for it and exits
  0 (#5219, #5233).
- **Large graph cooks into a SQLite store no longer fail with
  `database is locked (517)`.** The id allocator no longer overflows into a
  walk over occupied ids, the graph transaction starts `BEGIN IMMEDIATE`
  (also when the store path does not parse as a URL), and busy retries back
  off with jittered exponential backoff. MemStore's pinned-id sequence lift
  uses the same strict suffix parse as SQLite, so an overflowed pinned id
  cannot wrap the sequence (#6827, #6833).
- **Releasing a dead session's claim no longer erases a newer claim on the
  same bead.** With `[beads].conditional_writes` at `auto` or `require` on a
  capable store, the retired-session and orphaned-pool release paths clear the
  assignment and affinity metadata in one write guarded by the snapshot's
  revision, so a successor that re-claimed the bead after the controller's
  `List` (even under a reused assignee name) is left alone. Native-store list
  rows carry that revision. The guard cannot tell a newer claim from any other
  write, so a status, assignee or metadata change since the `List` is reported
  as a conflict, the bead is left as it is, and the next tick decides again.
  `require` refuses a store without the capability, `auto` degrades to the
  legacy release, and `off` (the builtin default) is unchanged. A snapshot
  without a revision (a row not rewritten since its store began recording
  revisions) keeps the legacy status-and-assignee fence (#7384, backported in
  #7581).
- **The core `orphan-sweep` order no longer releases claims it cannot vouch
  for.** On a rig store shared with another city it used to release that
  city's live claims as dead, in every shape they take: `<rig>/<name>` crew,
  foreign-binding pool instances (`<rig>/<binding>.<name>-<n>`), runtime
  session names and bare session bead IDs, with a false cause note and a mail
  for each sweep (#5955). The order now asks `gc agent is-foreign` about every
  claimant that is neither configured nor live, in HQ and in every rig, before
  any revalidation or session probe, and keeps the claim unless the verb
  answers `local`. Each sweep that keeps claims prints `protected K
  foreign/unknown claims` in its summary and a line naming every kept identity
  and its claims, and declares a `partial` outcome, which the controller
  records as an `order.skipped` event with one scope per identity; the run
  still exits 0, and a sweep that only protects sends no mail. The verb is
  asked at most once per distinct identity per sweep, within a gate budget of
  `GC_ORPHAN_SWEEP_GATE_BUDGET_SECONDS` (default 120, under the order's 4m
  timeout); past it, the remaining claimants are not asked, their claims are
  kept and named with a "gate budget … exhausted" reason, and the sweep says
  how many identities it did not ask. Where `timeout` is installed, an ask
  still running when the budget runs out is abandoned and its claim kept and
  named too, so one stalled store cannot run the sweep into the order's
  timeout. Identities are asked from a random starting point each sweep, so a
  large set of foreign claimants delays, but does not starve, release of a
  dead local claim. Unlike the Go
  pool sweep, the order has no route filter, so it gives bare identities no
  shape exemption: a dead local ephemeral or bead-ID claim stays releasable
  through its own session record. Migration notes: a person's claim, or a
  local agent's claim whose config and session records are both gone, is now
  kept and named every sweep instead of released; release it by hand with
  `gc bd release-if-current <id> <assignee>` (#7895, backported in #8230).
- **The pool orphan sweep treats a claim it cannot observe as unobservable,
  not dead.** On a shared store, an assignee in `<rig>/<name>` form that this
  city's config does not mint — another city's identity, a binding this city
  imports only on another rig, or an agent, binding or namepool name since
  REMOVED from local config — used to be judged by local session evidence
  alone and silently stripped (`in_progress` → `open`, assignee cleared).
  The dead-assignee sweep now keeps such a claim unless one of this city's
  own session beads, open or closed, answers to the identity under its alias,
  its configured named identity or its sole alias-history entry (looked up per
  identity, at most once per pass), and logs one
  `protected N foreign/unknown identities` summary per pass naming each
  identity it kept and its claims. A namepool seat whose name was trimmed
  therefore stays reapable after it closes; claims under the earlier aliases
  of a seat renamed more than once are kept and named. The orphan-close
  release of a confirmed orphaned session is unchanged. The core `orphan-sweep`
  order now applies the same gate through `gc agent is-foreign`; see the entry
  above. Migration notes: a `<rig>/<name>` identity moved to another city on
  the shared rig store keeps being released here until this city's closed
  session records for it are purged or re-aliased.
  The session record of a removed agent, binding or namepool name lasts until
  closed session beads are purged, which happens only after
  `GC_INFRA_SESSION_PURGE_AGE` (default 72h) on a relocated SQLite sessions
  ledger with `wisp_ttl` set, or after `GC_REAPER_SESSION_PURGE_AGE` (default
  720h) in the city store when the reaper's `GC_REAPER_SESSION_BEAD_PATTERN`
  (default `gm-*`) matches the city's session bead IDs or is set to `""`.
  Anywhere else closed session beads are kept indefinitely. Once the record is
  purged, the sweep protects the identity's outstanding claims, so release them
  explicitly (`gc bd release-if-current <id> <assignee>`/reassign) when
  retiring one, and watch the summary line for claims held back (#5959,
  #7763, backported in #8210).
- **Closed session rows no longer read as live.** On file-backed and SQLite
  stores, closing a session wrote its terminal metadata and its closed status
  as two writes, so a concurrent wake could leave a row `status=closed
  state=awake` that the reconciler and `gc session list` treated as running.
  Session closes, controller closes and pending-create rollbacks now use one
  revision-fenced write; a stale status heal no longer reverts a concurrent
  `gc session suspend`, `kill` or close; a terminal provider error during a
  pending create closes the row as `failed-create`; and ACP startup and
  rollback are fenced so a completed start is not closed as failed (#6109,
  #6114, #6583, #6745, #6747, #6756, #6784, #6802).
- **The tmux runtime stops, reads and types into sessions by exact name.**
  tmux resolves a bare `-t name` by unique prefix, so stopping an
  already-exited `worker-1` killed a live `worker-10`, and metadata reads,
  `send-keys` and `capture-pane` could land on another agent. The tmux
  runtime's stop and liveness checks, metadata reads and writes, `send-keys`,
  `paste-buffer`, `capture-pane`, rename and respawn now use an exact target,
  and a tmux metadata read error is no longer reported as an unset key. Still
  matched by prefix in this release: every session-targeted command of the ssh
  runtime, whose stop and relaunch can still kill or replace a prefix-matching
  remote session (fixed on main by #7682), and the tmux runtime's
  `set-option`, `set-hook`, `resize-pane`, `attach-session`, `switch-client`,
  `select-window`, `list-windows` and `display-message -d` (#6876).
- **tmux session state no longer reads every session as absent on a busy
  city.** Steady invalidation could keep the state cache from publishing any
  fetch until it went stale, so healthy sessions were drained, restarted or
  reaped (#6800).
- **`gc stop` holds the controller lock through bead-store shutdown, and
  `gc start --foreground` takes it before starting the bead-store
  provider,** so a racing start or supervisor restart can no longer run a
  controller against a provider that `gc stop` is retiring, and a racing
  `gc start --foreground` no longer starts or restarts that provider. A
  supervisor-hosted `gc start` or `gc register` that races a `gc stop` still
  runs the bead-store start before it takes the lock, then waits for the stop
  and starts the city again (#6742, #6789).
- `gc stop` stops the city's nudge pollers and never reopens a stopped city's
  store (#6857).
- **Event log rotation no longer loses events.** A short-lived `gc` command
  could rotate `events.jsonl` from a stale file handle, after which the
  supervisor appended to a deleted file; one city lost about 225,000 events.
  Only the supervisor's recorder (or a standalone controller holding the
  controller lock) rotates now, every writer locks a stable
  `events.jsonl.lock` sidecar and reopens a replaced handle, sequence numbers
  stay monotonic across rotation, and watchers no longer hold a rotated file
  open. The on-disk format and archive names are unchanged (#7022).
- **A run the controller deletes no longer stays `pending` in the run
  census.** When the controller's wisp GC deleted a run whose rows its bead
  cache did not hold (closed rows, a restarted controller, an evicted entry),
  or the read just before a delete failed, no `bead.deleted` event was
  recorded. The run view drops a run only on that event and replays the
  retained event log on every restart, so the run stayed listed as `pending`,
  and canceling it returned `run-not-found`. Every delete made through the
  controller's bead cache now records `bead.deleted`, carrying just the bead
  id when no copy of the row is at hand, and a bead listed twice in one batch
  records it once. Deletes made outside the controller, such as a script's
  `bd purge` or `gc bd delete`, are not covered, and runs stranded before this
  fix stay listed (#5934).
- The supervisor event stream includes cities started after the client
  connected (#6861). Delivery is at-least-once when a city's watcher is
  re-attached mid-connection, for example after the city stops and starts
  again: events already buffered for that city can be delivered again, and
  its seq in the composite SSE id can move back. Dedupe on city and seq.
- **The controller no longer records an unchanged bead as a new
  `bead.updated` event on every reconcile pass.** Its bead caches announced
  rows whose state had not changed, so on a busy city most `bead.updated`
  events were byte-identical repeats, and each one fed every event consumer,
  from the controller's own cache and wake path to orders that trigger on
  `bead.updated`. A cache now drops a `bead.updated` whose payload is
  identical to the last create or update the event log acknowledged for that
  bead within 20 minutes; after that an unchanged bead is recorded again.
  Creates, closes and deletes are never dropped, and a close or delete resets
  the bead. A failed append is logged and not remembered, so the next copy is
  recorded. See Upgrading Notes for `nudge-on-route` (#7023, backported in
  #8228).
- **Concurrent `gc init` runs sharing a Dolt database no longer force a
  second initialization over an active migration,** and a verified-empty
  store is initialized with plain `bd init` instead of a forced reinit that
  could stop partway through the schema on a loaded host (#6658, #6709).
- **`gc dolt compact` no longer looks for a pinned rig's database on the
  city's managed Dolt server.** A rig pinned to another Dolt endpoint
  (`gc rig set-endpoint --external`, a rig-local `--self` server,
  `[[rigs]] dolt_host`/`dolt_port`, or a bd binding to a remote host) keeps its
  database there, but the compactor queried the managed server for it. The
  `mol-dog-compactor` order failed every two hours with `Error 1049 (HY000):
  database not found`, or, when the managed server still held a same-named
  copy, flattened that copy and reported the rig's database as compacted.
  Compact now skips such a rig with a log line naming its endpoint, and skips
  a same-named copy on the managed server with a warning; `--only-db` cannot
  reach either. A rig whose `.beads/config.yaml` cannot be read is skipped
  too, with a warning naming the file. Rigs that inherit the city endpoint are
  compacted as before (#7994, #7997, backported in #8215).
- **The Dolt compactor docs no longer claim it only rewrites history this
  city grew.** The `gc-compact-base` watermark is a position, not an author:
  a database several cities write to is flattened for all of them when the
  city whose managed server holds it, or any city that reaches it over
  loopback, compacts it. A city skips an external endpoint on a non-loopback
  (for example cloud-hosted) Dolt host with exit 0, so it never compacts that
  database. "Shared and non-local databases" in
  `docs/troubleshooting/dolt-bloat-recovery.md` covers keeping a shared
  database out of `mol-dog-compactor` and a stop-everything `gc bd compact`
  procedure (#7896, #7993, backported in #8215).
- **One agent on a harness that cannot receive MCP no longer fails the whole
  city's start.** gc skips that agent's MCP projection with a warning naming
  the harness and the fix; that agent's own session start still fails with the
  same error (#6661).
- **The codex, copilot and antigravity managed hooks use the launcher's
  `gc`** (`${GC_BIN:-gc}`, with user bin directories appended to `PATH`), so
  an older `gc` in `~/go/bin` no longer rewrites a fresh city's bd shim
  (#6579).
- **Terminal steps of the core commit, report and prompt-synthesis formulas
  close themselves before `gc runtime drain-ack`,** so another session can no
  longer re-run a step the drain released. After three failed pushes the
  commit formula closes its step as a hard failure and leaves the work bead
  open with the commits in the checkout (#6992).
- **Drained on-demand named sessions wake for routed demand** aimed at their
  identity (#6692).
- **Namepool and other stable-alias seats are no longer retired while they
  hold a claim.** The awake, drain and reuse guards now match claims recorded
  under a session's stable alias (#6413, #6766).
- **The execution backstop's stall latch is bound to the session incarnation
  and expires after 30 minutes,** so a supervisor restart mid-drain no longer
  silences it for the rest of the seat's life. The session bead records the
  current hold in `execution_claim_hold` as `<reason> <time>` (#5701).
- **The reaper's session prune no longer escalates a stale or missing backup
  when it has nothing to delete.** The pattern path
  (`GC_REAPER_SESSION_BEAD_PATTERN`, default `gm-*`) checked the city store's
  backup age before anything else, so a city without a fresh bd backup of its
  city store raised a MEDIUM "Reaper anomalies detected" escalation ("bulk
  prune skipped: backup stale or absent") for a prune that, in most cities,
  would delete nothing. The reaper now first counts the prune's candidates
  (closed beads that match the pattern and closed more than
  `GC_REAPER_SESSION_PURGE_AGE` ago) and, when there are none, skips the
  backup check and the prune without an anomaly. When there are candidates,
  the backup-age and type-scope gates apply as before. A count that fails is
  reported as its own anomaly (`session prune candidate count failed for
  <db>`), and the gates still apply. An invalid pattern is now reported before
  the backup is checked (#6284).
- The core `orphan-sweep` order no longer misses live sessions when the
  session list exceeds the pipe buffer (#6714, #6728).
- **Unchanged Dolt backup sync failures and reaper anomalies no longer mail
  on every run.** `escalate.sh` takes `--incident-key` and `--fingerprint`:
  an alert whose fingerprint matches its last delivered one stays quiet for
  `GC_ESCALATE_REMINDER_SECONDS` (default one day), a changed fingerprint
  alerts at once, and both orders pass `--resolve-incident` on a clean run so
  the next failure alerts at once too. Deduplication is best-effort: when the
  state under `.gc/incidents/` cannot be read or written the alert is sent
  anyway, and a failed send is retried on the next run. The reaper's
  fingerprint ignores only a measured backup age, so a backup that goes
  missing alerts at once. The backup order's escalation reports each
  database's first failed attempt instead of its last, with common credential
  shapes redacted from every diagnostic. When `GC_ESCALATE_SEND_TIMEOUT_SECS`
  cuts a send off before `gc mail send` reports the mail written,
  `escalate.sh` now exits non-zero so callers that check the exit status can
  retry it; keyed incidents also retry, because a failed send records no
  incident state. A send cut off only after that report still counts as
  delivered. The backup order's missing-`flock` and offsite-publication
  alerts still mail on every run, and an `escalate.sh` override must accept
  or ignore the new flags. One that rejects them as a usage error, as one
  written for 1.5.0 does, is sent the alert again without them, so it keeps
  alerting, undeduplicated (#6363 and #8231, backported in #8218 and
  #8232).
- **The core `orphan-sweep` order times out a minute before its next run is
  due.** The order declared no timeout, so it ran under the 300s exec
  default, equal to its 5m interval. The cooldown counts from when a run
  starts, so a sweep killed at its timeout was due again at once, and on a
  slow store the sweep ran continuously. The order now sets `timeout = "4m"`,
  which leaves at most two minutes after the 120s foreign-identity gate
  budget, less the claim collection that precedes it. A city whose sweep
  needs longer can raise it with `[[orders.overrides]]`,
  `name = "orphan-sweep"` and a `timeout` (plus `rig = "*"` if a rig's own
  pack also carries the order).
- herdr agent starts retry when a fresh pane reports `agent_pane_busy` on a
  non-zero exit (#6637).
- A nudge that races an ACP agent's exit is dropped with a stderr line instead
  of failing with `file already closed` (#6540).
- Claude Code's feedback-survey dismissal no longer submits a stray `0` into
  the session (#6859, #7013).
- **Cost estimates price the Claude 5 family and Claude Opus 5.5.** The
  shipped default price table stopped at the Claude 4.x models, so every
  invocation on `claude-opus-5`, `claude-opus-5-5`, `claude-sonnet-5`,
  `claude-fable-5` or `claude-fable-5-1` was recorded unpriced and left out
  of `gc costs`' `EST_USD`, which read $0 for a city running those models.
  The table now carries Anthropic's list prices for all five. A price is
  found only for an exact provider and model match (ignoring case), so
  another spelling of the same model, such as a Bedrock model id, still needs
  its own `[[pricing]]` entry in `city.toml` or `pack.toml`; an existing
  `[[pricing]]` entry still outranks the defaults (#6384, #7713, backported
  in #8213).
- A gateway login reaches the Claude provider readiness probe.
- `POST /v0/city/{city}/sling` routes and cooks formulas exactly like
  `gc sling`: a rig bead's default formula is cooked in the rig store, title
  and vars reach the default formula, and a convoy's children are routed one
  by one. `gc sling` against a remote city reports a partial convoy failure
  like a local one: it prints the failed children, `--json` reports
  `success: false`, and it exits 1. `gc sling --on` stays refused for a
  remote city.
- **Order history reports `has_output` only for a run that recorded output,
  and an order that runs out of time says so.** Order history
  (`GET /v0/city/{city}/orders/history`) set `has_output` on every run of an
  exec order, because exec orders capture output, so a run that recorded
  nothing advertised output its history detail then served empty;
  `capture_output` still describes the order. An order stopped by its
  `timeout` (or the `[orders] max_timeout` cap) failed with a bare
  `context deadline exceeded` or `signal: killed` in its `order.failed`
  event, in the controller log for an exec order, and in `gc order run`'s
  error; the message now leads with the timeout it ran out of, for example
  `order timeout of 5m0s exceeded`.
- A session can close a bead it claimed under its session bead id without
  `--force`; `gc bd close` runs bd as that id. Claims recorded under a session
  name or alias still need `--force` (#6324). The close's audit actor is the
  assignee string.
- `gc session pin` and `gc session wake` report a demand-only singleton's pool
  session honestly instead of succeeding: pin is refused, and wake records the
  wake but exits non-zero unless the session is running or its start is
  already in flight (#6858).
- An API config edit is no longer lost when it lands while the controller is
  reloading config: a load hashes the city's convention trees before reading
  them, and a runtime reload can no longer publish an older config over a
  concurrent API mutation (ga-opn27h; backport of #7084).
- `GET /v0/city/{city}/patches/...` returns the patches declared in
  `city.toml` instead of 404/empty after they were applied (ga-8hk1fe).
- **The run API lists a formula-v1 run's steps and reports the run active once
  a step starts.** Formula-v1 instantiation types each untyped or `task`-typed
  non-root step `step`, and the run projection dropped that type. For a v1
  run, `GET /v0/city/{city}/runs/{id}/steps` returned no steps, and the run
  list, `GET /runs/{id}` and the run census kept the run `pending` while its
  steps were claimed and closed.
- **`GET /v0/city/{city}/sessions?state=closed` and `?state=all` list closed
  sessions.** The session list read only open sessions, whatever the filter,
  so a filter naming `closed` matched nothing. A filter that can match a
  closed session (`all`, or a list naming `closed`) now reads closed sessions
  too, reported with state `closed`; they come from the bead store rather than
  the cache. The default and every other filter still read open sessions from
  the cache. `gc session list --state closed` and `--state all` list them too
  when the city's supervisor answers the command.
- `GET /v0/city/{city}/agents` reflects a config change right away instead of
  serving a cached list keyed only on session state (ga-79peco).
- **`GET /v0/city/{city}/agents` reports a live bounded-pool worker as
  running, and no provider for the control dispatcher.** The agent list, the
  agent read and the rig counts probed a pool slot (`<pool>-1`) on a session
  name derived from the slot, but the controller runs an unaliased pool
  session under a name carrying its session bead id (`<template>-<beadID>`).
  Every worker of a pool with `max_active_sessions` set read `stopped` while
  `/sessions` showed it active, `?running=true` listed none of them, and a
  rig's `running_count` left them out. When a slot's derived name is not
  live, these endpoints now find the slot's session the way `/status` does
  and probe that one. A session whose bead still says active but whose
  runtime is gone still reads stopped, and a session store that cannot be
  read makes the list partial. The SDK's control dispatcher, which runs
  `gc convoy control --serve` and has no provider, was reported under the
  workspace provider, marked unavailable when that provider's binary was not
  on `PATH`, and could take its session's model and context from another
  agent's transcript in a shared work dir. It now reports no provider, its
  session reads no transcript, and a Claude agent sharing its work dir no
  longer loses its model and context on its account. (#4703, backport of
  #8211; thanks @remuscazacu)
- **The test suite's startup Dolt orphan sweep no longer deletes a live Dolt
  data directory when its `lsof` scan fails.** A scan that hit its 30-second
  deadline returned partial output with no error, so directories a running
  Dolt server held open looked orphaned and were removed. The sweep now
  refuses to delete anything when the scan times out, is killed by a signal,
  exits with an unexpected status, or prints no output (#7138, backported in
  #7507). This affects only people running the gascity test suite; no `gc`
  command runs this sweep.

### Security

- **Text sent to a Claude session no longer answers its permission prompt.**
  On Claude Code 2.1.28x gc missed the permission dialog, so
  `GET .../pending` stayed empty, `respond` returned `409 no_pending`, and an
  ordinary `submit` or nudge typed into the prompt, where its Enter approved
  the pending tool call. gc now recognizes the dialog by its "Do you want to
  …?" question and numbered menu (also when the question wraps), and
  `respond` picks the option by its label, so `deny` can no longer select
  "Yes, and switch to auto mode"; when no single option matches it sends
  nothing and returns 409 `invalid_interaction`. While a prompt is pending,
  `POST /v0/session/{id}/submit` and `.../messages` answer 409
  `session-conflict` instead of 202, `submit` with intent `interrupt_now` is
  refused, `gc session nudge --delivery=immediate` exits non-zero and
  `--delivery=wait-idle` queues behind the prompt; the tmux runtime looks
  again under the nudge lock, right before it pastes and right before Enter,
  and refuses when it cannot look. A Claude session on a permission prompt no
  longer reads as idle. Stopping a Claude turn on the tmux runtime (`stop` or
  `interrupt_now`) now also clears the prompt Claude puts back into the input
  box, which can add up to 2 s, and leaves the box alone when a client is
  attached. The exec, k8s, ssh and herdr runtimes do not look for the prompt
  yet. Thanks to Chris Sells (@csells) (#2892, #6832, backported in #8046).
- **Go toolchain 1.26.10.** Go 1.26.10 fixes GO-2026-6809: `crypto/x509`
  now refuses the eight compromised eMudhra intermediate CAs (EM DV/OV/EV TLS
  CA - G3B/G3C and their Sectigo cross-signs) even where the host trust store
  still lists them. The `go.mod` floor, the Bazel `go_sdk` and the
  `contrib/k8s` Go build images move to 1.26.10, and the `gc-mcp-mail` image
  takes certifi 2026.10.19 for the same distrust. Building from source now
  needs Go 1.26.10 or newer (#8165).
- **Go toolchain 1.26.9 and `golang.org/x/net` v0.60.0.** Go 1.26.9 fixes
  the 2026-10-08 standard library advisories GO-2026-6599, 6600, 6603, 6604,
  6605, 6607, 6608, 6609, 6610, 6611, 6612, 6613 and 6617 in `net/http`
  (HTTP/1 and HTTP/2), `html/template`, `crypto/tls`, `net/textproto`,
  `mime/multipart` and `os`. x/net v0.60.0 fixes the HTTP/2 ones among them
  (GO-2026-6603, 6610, 6611, 6612 and 6617) in the `golang.org/x/net/http2`
  package the `gc` binary links. The `go.mod` floor, the Bazel `go_sdk` and
  the `contrib/k8s` Go build images all move to 1.26.9; building from source
  now needs Go 1.26.9 or newer (#7387).
- **OpenTelemetry log modules v0.21.0** fix GO-2026-6615 (the log
  `BatchProcessor` could busy-spin when its export buffer was full) (#7056).
- **The `gc` binary links `apache/thrift` v0.24.0** (CVE-2026-43871), the
  version the container images already forced into the `dolt` and `bd` they
  rebuild (#7510, backported in #7621).
- **`gc-mcp-mail` image: PyJWT 2.14.0, urllib3 2.8.0 and GitPython 3.1.60.**
  Fixes CVE-2026-102268 (CRITICAL) and CVE-2026-102266, -102267, -102271,
  -102272 and -102273 in PyJWT, CVE-2026-97687 and CVE-2026-97689 in urllib3,
  and CVE-2026-87817 and CVE-2026-87819 in GitPython (all HIGH unless noted)
  (#7058).
- **`gc-mcp-mail` image: `fsspec` 2026.6.0.** Fixes CVE-2026-104851 (HIGH,
  arbitrary code execution via a crafted URL) in the transitive `fsspec`
  dependency (#7388).
- **Container images rebuild `gh`, `dolt` and `bd` with `golang.org/x/net`
  v0.60.0** (CVE-2026-78669). The `kubectl` prebuilt in the controller image
  still carries CVE-2026-78669, CVE-2026-78667 and CVE-2026-97031, which no
  kubectl release fixes yet; they are waived for that path only, through
  2026-11-07 (#7579, backported in #7621).
- CodeQL, govulncheck, container-scan, complexity and macOS regression checks
  now also run on pull requests to `hotfix/**` and `release/**` branches
  (#7055, backported in #7494).

### Known Issues

- **On a city using the default bd-owned proxied Dolt, the proxy and its Dolt
  server can keep running after `gc stop`.** Read-only commands on a stopped
  city, such as `gc session list` and `gc bd list`, can also start them again.
  Retiring idle proxied scopes is tracked in #6561.
- **`gc doctor` can skip a proxied store's checks without saying the store
  might be running.** When gc cannot read the proxy's process details (for
  example under `hidepid`, or with a proxy owned by another user), the store
  checks of a stopped city report `not checked: store not running` as passing
  (on a running city they warn that the store's proxy is not running). Planned
  for 1.5.2.
- **The Dolt compactor still flattens a database that several cities write
  to.** The `mol-dog-compactor` order squashes every commit after its
  watermark, whoever wrote it, so the city whose managed Dolt server holds a
  shared database, and every city that reaches it over loopback (including a
  relay or tunnel on a loopback port), flattens the other cities' history
  too. Making compaction safe for shared and cloud-hosted databases is
  tracked in #7896 and is not in 1.5.1. On those cities, keep the shared
  database out of the order with `GC_DOLT_COMPACT_ONLY_DBS`, or disable it
  (`[[orders.overrides]]` with `name = "mol-dog-compactor"` and
  `enabled = false`), and compact the shared database by hand with every
  writer stopped, as "Shared and non-local databases" in
  `docs/troubleshooting/dolt-bloat-recovery.md` describes.
- **The reaper reports `partial` on every run while a subtree is held.** On a
  city with a permanently held subtree, every reaper run (every 30 minutes)
  emits an `order.skipped` event with outcome `partial`. This is expected and
  not a failure. Under `GC_REAPER_DRY_RUN`, the summary reports the prune
  preview as `sessions-pruned:N`, although nothing was pruned. Planned for
  1.5.2.
- **Two cities sharing one rig store can still disturb each other's claims
  when their agent identities overlap.** The controller's pool orphan sweep
  and the core `orphan-sweep` order no longer release a claim this city
  cannot vouch for (#5959, #5955; see Fixed), but two cities that import the
  same agent pack share one identity space: neither sweep can tell a foreign
  claim whose assignee matches a local identity byte for byte from a local
  one (ga-dk24ao). And when one city runs an agent under an import binding
  and the other runs the same agent unbound, each controller's legacy-binding
  canonicalization rewrites the other city's `gc.routed_to` and assignee back
  and forth (#5846). Avoid running agents with the same name in two cities
  that share a rig store.

## [1.5.0] - 2026-10-05

### Upgrading Notes

- **Upgrade Beads (`bd`) to v1.3.1.** v1.5.0 pins and is tested against bd
  v1.3.1 (`deps.env` `BD_VERSION` and the go.mod library), a stable release
  that keeps bd v1.3.0's schema. Install it with `brew upgrade beads` (or
  `brew install beads`), from the
  [v1.3.1 release assets](https://github.com/gastownhall/beads/releases/tag/v1.3.1),
  or with `go install github.com/steveyegge/beads/cmd/bd@v1.3.1`. A city
  that stays on bd v1.3.0 keeps working through the bd CLI, but gc refuses
  that bd for its native store (it is older than the linked library), and the
  proxied `bd backup` and closed-wisp `bd purge` steps of `mol-dog-backup` and
  `reaper` are reported as skipped. Read beads'
  [v1.3.1 upgrade notes](https://github.com/gastownhall/beads/blob/v1.3.1/CHANGELOG.md#131---2026-09-30)
  before upgrading scripts that call bd directly. Do not move a city to a
  newer `bd` until a gc release pins it.
- **`gc storage migrate` is experimental.** The command is new in v1.5.0 and
  still has open correctness issues on split cities (#5987, #6015, #5974,
  #6129, #6242, #6348). Run `gc storage preflight` first, back up every store
  involved, and do not migrate a production city you cannot restore.
- **Run `gc doctor` after upgrading a city whose beads store gc does not
  own.** gc now registers its required bead types (including the new
  `startup-health-episode`) on start for gc-managed and provider-owned stores.
  It never writes to external Dolt servers, complete storage bindings, hosted
  gateways or legacy proxied opt-in scopes: on those, run `gc doctor` and, if
  `custom-types` fails, `gc doctor --fix` (#6495).
- **Unaliased pool and ephemeral sessions now act as their session bead id.**
  `GC_AGENT` and `BEADS_ACTOR` (and the event actor and telemetry `gc.agent`)
  are the session bead id instead of the session name; restart running
  sessions to pick it up. `gc hook --claim` re-stamps a bead still held under
  the old spelling when it adopts it; if that fails it prints the exact
  `bd update <id> --if-assignee <old> --if-status in_progress --assignee <new>`
  command to recover (#5716).
- **Pool rows from a pre-release build keep their old names, and a failed
  pool start can now stall its slot.** Open pool rows created by an RC or main
  build named bare `claude` or `claude-N-pool` keep those names until they
  close, and each can leave at most one orphan runtime, once; new workers use
  `<template>-<beadID>`. When a pool worker fails to start, gc stops its
  runtime before the slot retries; if that stop fails, the slot stalls with a
  `holding pool session <id> open` log line instead of starting another box
  (#6549).
- **The Dolt compactor never flattens a database that has a Dolt remote.**
  Such databases (including bd `refs/dolt/data` and backup-only `file://`
  remotes) get a bare `CALL DOLT_GC()` and their history grows, and a leftover
  `compact-pending-push` marker is held for review instead of pushed. To
  flatten one on purpose, announce a window to every clone, then run
  `GC_DOLT_COMPACT_ALLOW_FEDERATED=1 gc dolt compact --only-db <db>`. On first
  sight the compactor tags each database `gc-compact-base`: at its root if the
  compactor already flattened it or it is marked `.compact-full-history`,
  otherwise at HEAD, keeping its pre-upgrade history. Only commits after the
  tag are squashed. If you roll a database back to before its tag, compaction
  of it fails with instructions until you delete the tag
  (`CALL DOLT_TAG('-d', 'gc-compact-base')`) (#5958).
- **`secrets.env` keys must be shell identifiers.** Any other key (optionally
  after `export `) is rejected as `line N: invalid key`, and an unclosed quote
  skips through its closing line as one block reported by key and line range;
  the rest of the file still applies. Check the supervisor's stderr after
  `gc start` for skipped lines (#5982, #6022).
- **Child processes now see `BD_BIN=` (empty) when no `bd` pin is
  configured,** instead of an inherited ambient value. A relative or
  non-executable ambient `BD_BIN` is ignored with a warning; a `BD_BIN` pinned
  in `workspace.env` still fails closed (#6550).
- **Formula steps and the agent-script `notes:` key now append to work-bead
  notes** (`bd update --append-notes`) instead of replacing them. A retried
  step can append the same text twice (#6412).
- **A retry step's own counter moved to `gc.retry_attempt`.** Inside a ralph
  or review loop, `gc.attempt` names the loop iteration on every bead of the
  iteration body again (the v1.4.2 contract), and `gc.retry_attempt` counts a
  step's retries from 1 in each iteration. Packs that read `gc.attempt` as a
  retry counter should read `gc.retry_attempt`, falling back to `gc.attempt`;
  beads created before the upgrade have no `gc.retry_attempt` (#6548).
- **Main and nightly builds only: the seat-claim and named-seat execution
  backstops are removed.** Named always-on seats again have no controller
  claim or execution backstop, so an agent must claim its work at startup or
  be nudged. The pool-slot backstop stays, without #6287's changes: a slot with
  no `nudge` is drained after the 90s grace instead of nudged first, a slot is
  nudged or drained even while a human is attached, and activity no longer
  re-arms it. The `execution.claim_stalled` event type is gone; leftover
  `seat_claim_*` and `execution_claim_nudge_*` session metadata is inert
  (#6524).
- **New cities import the Gas City pack as `[imports.gc]`.** `gc init`
  (default or `--template gascity`) used to write `[imports.gascity]`; the new
  key matches the binding the pack documents, so the pack's skill now shows as
  `gc.mayor` instead of `gascity.mayor`. Cities with `[imports.gascity]` are
  not rewritten and keep working. Before moving a city past Gas City pack
  0.1.6, whose role prompts run `gc gc claim`, rename the key to `gc`;
  `gc doctor --fix` offers the rename (#6683). Do not add a second `gc` import
  next to the old key: that imports the pack twice (#4508).
- **The `reaper`, `jsonl-export` and dolt `mol-dog-backup` orders now work
  through bd on every city topology.** Each bead scope (the city and every
  rig) is reached with `gc bd`, so bd-owned proxied, gc-managed and mixed
  cities are all maintained; the orders no longer dial Dolt themselves.
  - **Unbound databases are no longer maintained.** Databases on a gc-managed
    Dolt server that no city or rig binds are no longer reaped, exported or
    backed up. Drop orphaned databases, or bind them to a rig.
  - **The JSONL archive switches to `bd export` format** (one issue per line,
    with labels, dependencies and comments), which restores with
    `bd import <file>`. The first run moves each database's old
    `{"rows":[...]}` snapshot files into `<db>/legacy/` with `git mv`; nothing
    is deleted.
  - **Backups use `bd backup`.** `mol-dog-backup` registers
    `<city>/.dolt-backup/<db>` as each scope's `bd backup` destination when it
    has none, then runs `bd backup sync`; the reaper's session-prune backup gate
    reads `bd backup status`. A bd older than v1.3.1 refuses backup on proxied
    scopes: those are reported as skipped (an `order.skipped` event) and their
    session-bead prune waits for a backup it can see.
  - **Closed-wisp purge uses `bd purge`.** It needs a bd whose `bd purge`
    selects the whole wisps plane (`--wisps-plane`), keeps closed wisps a live
    wisp depends on, and purges in bounded batches (`--limit`); a backlog is
    cleared across runs within `GC_REAPER_PURGE_BUDGET_SECS` (default 300s).
    With a bd older than v1.3.1 the purge step is reported as skipped. A run
    that uses up `GC_REAPER_RUN_BUDGET_SECS` (default 780s, below the 900s
    order timeout) stops starting new work, reports a partial outcome, and the
    next run continues.
  - Both orders now run with a 900s timeout.

### Added

- **`order.skipped` event.** An exec order that exits 0 but could not do all
  of its work (an unreachable bead scope, a safety gate that held a step back)
  now says so: the controller records a typed `order.skipped` event, with
  outcome `skipped` or `partial` and the scopes that did not run, next to
  `order.completed`. `gc order run` prints the same declaration.

- **`gc doctor` offers to rebind the Gas City pack import as `gc`.** The new
  `gascity-pack-binding` check warns when a city imports the public Gas City
  pack under another key, and `gc doctor --fix` renames it to `[imports.gc]`
  in place; `packs.lock` is unchanged. The fix refuses and explains when `gc`
  is already bound to a different pack, when the pack is imported twice with
  different settings, or when `pack.toml` or `city.toml` still reference
  `gascity.`-qualified names (#6683).

- **`gc storage preflight` reports everything the infra-class cutover would
  refuse, from outside the window.** `gc storage migrate --from-work` runs its
  refusals with the fleet stopped, so an operator learned that a rig scope
  holds an infrastructure bead this binary carries no importer for only after
  spending the window on it. The new verb runs the same checks — every one of
  them the migration's own function, not a copy — against a LIVE city, while
  copying nothing, creating nothing, taking no migration guard, and publishing
  no event. A live controller is reported by PID rather than refused, because
  it names the window itself rather than something to go and fix.

- **`storage.binding.not_configured` makes "this city has no split" a verdict
  a subscriber can see.** A city that relocates nothing used to leave the boot
  gate having published nothing at all, and nothing reads the same as a gate
  that crashed before deciding or a build too old to have one. The fifth
  `storage.binding.*` type carries the same `StorageBindingOutcomePayload` as
  the other four, so a deploy gated on these events can tell an absent split
  apart from an absent answer.

- **`storage.binding.*` events now carry `proven_beads`, the size of the
  proven-copy manifest a serving verdict rests on.** "Converged" alone did not
  distinguish a city serving its whole infrastructure slice from the binding
  from one whose copy carried nothing, and those are the two situations an
  operator watching a cutover most needs to tell apart. Every path that
  reaches a serving verdict has already read the manifest, so the number costs
  nothing. Every other outcome leaves it zero, and zero there means the copy's
  size is not something the verdict established — not that the copy is empty.

### Changed

- **Closed session beads in a SQLite infra ledger are purged after
  `GC_INFRA_SESSION_PURGE_AGE` (default 72h).** This applies only when a city
  relocates sessions to a SQLite infra ledger and `wisp_ttl` is greater than
  zero; an invalid value logs one warning and falls back to 72h. A named
  session closed for more than 3 days loses its bead and reopens fresh. The
  Dolt reaper keeps `GC_REAPER_SESSION_PURGE_AGE` (default 720h) (#6689).

- **`gc pack registry publish` now refuses an unscoped pack name unless you
  pass `--allow-unscoped-name`.** Registry pack names are scoped as
  `<github-owner>/<pack>`, and the registry has always reserved bare names for
  packs it already holds a claim for — but the CLI submitted one anyway, so the
  refusal arrived only after the request had been created and parked in the
  review queue as an unapprovable pending row. Publish now checks the name
  locally, before any credential or publish traffic, and names the exact
  `[pack].name` edit that fixes it. It also refuses a scope that is not the
  lowercased GitHub owner of the source repository, which the registry rejects
  with no override.

  Upgrading: a publisher of a grandfathered bare name must add
  `--allow-unscoped-name` to keep publishing under it — the registry still
  accepts a bare name it already holds a claim for, and a local preflight
  cannot see the claim table. New packs must set
  `[pack].name = "<github-owner>/<pack>"` in `pack.toml`. `--name` no longer
  stands in for a missing `[pack].name`, and it can no longer rename a pack at
  publish time: the registry byte-compares it with `[pack].name`, so it can
  only restate the name `pack.toml` already declares.

- **`gc bd` now refuses a `--metadata` body it cannot validate before the
  write, on `new` as well as `create` and `update`.** `gc bd` validates
  rig-qualified metadata (`lease_owner`, `routed_to`) ahead of the write so a
  create naming a rig this city does not configure is stopped before it mints a
  stranded bead. That guard admitted `create` and `update` but not `new` — the
  alias `bd` itself registers for `create` — so the same command spelled `gc bd
  new` skipped validation entirely. It now normalizes the alias and applies the
  identical check.

  Upgrading: `gc bd new --metadata @file.json` now exits 1, on every city,
  split or not. The `@file.json` spelling states its object in a file rather
  than in argv, so `gc bd` cannot read the rig qualification before `bd`
  resolves the file and mints from it — the one spelling where a refusal is the
  only fail-closed answer. Pass the JSON inline (`--metadata '{"routed_to":
  "rig/agent"}'`) instead. A malformed inline body is likewise refused by name
  rather than forwarded. No in-repo caller uses the `@file.json` spelling.

- **`gc bd` on a split city now decides ownership by RESIDENCE, so a
  reserved-prefix id the relocated class binding does not hold falls through to
  the work ledger instead of being refused on its prefix.** The binding is the
  authority for its reserved namespaces, not their only lawful holder:
  `config.ValidateRigs` deliberately admits a rig prefix inside a reserved
  namespace (`ReservedPrefixWarnings` only advises) and `gc storage migrate`
  preserved ids in the other direction, so such a rig mints work beads carrying
  ids the binding has never held. Every read and every write addressed at one —
  including the step-completion write a worked bead ends with — died at this one
  door, while the HTTP API and `classRoutedStoreForID` already served the same
  rows. `gc bd create --deps <reserved-id>` and `--parent <reserved-id>` now
  execute against the work ledger on a clean binding miss, where they were
  previously refused; a subject the binding actually holds is still refused with
  the routing diagnostic, and every addressed id is probed, so an unserved
  `dep add <miss> <resident>` is refused in either argv order. A binding that
  cannot ANSWER — unopenable, refusing, or faulting — is still a hard error and
  never read as absence.

  One diagnostic is lost, by design: a truncated `gcg-…` id no longer gets the
  "class stores resolve ids exactly (no substring match)" hint and instead falls
  through to `bd`'s own substring not-found. Distinguishing a typo from a
  shadow-prefixed rig's real bead requires knowing the namespace has one lawful
  minter, which is the premise this change retires (ga-8w5c7).

### Fixed

- **The reaper's stale-issue auto-close works again when an open bead
  depends on a wisp or external bead.** Such a dependency has no
  `depends_on_issue_id`, and that NULL emptied the active-dependency exclusion,
  so no stale issue in the store was ever closed.

- **The Dolt compactor no longer rewrites adopted or shared history.** The
  default-on `mol-dog-compactor` (`gc dolt compact`) flattened any managed
  database over 2000 commits back to its root commit and, if the database had
  a remote, force-pushed the result. History adopted with `gc rig add
  --adopt`, `bd bootstrap` or a `dolt clone` of a team remote was squashed
  beyond recovery, and every other clone of that remote was rewritten with it.
  See the Upgrading Notes above and "History protection" in
  `docs/troubleshooting/dolt-bloat-recovery.md` (#5958, #6554).

- **Native Dolt store metadata writes no longer silently undo concurrent
  updates.** `SetMetadata` and `SetMetadataBatch` read, merged and wrote back
  the whole metadata map outside a transaction, so a concurrent update to other
  keys was lost; separately, route recovery could stamp an unclaimable bare
  route over a fenced graph step. Metadata writes now compare-and-swap on the
  row version and re-merge on conflict. Under sustained contention on one bead
  (3 refused swaps in about 75 ms) they return an error wrapping
  `beads.ErrVersionMismatch` instead of reporting success after overwriting the
  other writer. Route recovery skips fenced beads and sets `gc.routed_to` with a
  key-level compare-and-set. Known residual: two reconciler writes that ignore
  their error after stopping a runtime can now more often leave a hot session
  bead marked awake or draining with no runtime until a later tick repairs it
  (ga-l03ij). The `bd`-subprocess store path is unchanged (#6222, #6221,
  #6233).

- **Claude sessions in untrusted directories start again instead of
  answering the trust dialog "No, exit".** Current Claude Code builds put the
  cursor on "No, exit", and v1.4.2 confirmed it with a bare Enter, killing
  every managed Claude session that started in an untrusted directory. gc now
  sends movement keys, re-reads the screen, and presses Enter only after two
  consecutive frames show the cursor on "Yes, I trust this folder". If it
  cannot get there it leaves the dialog up and warns on stderr rather than
  guessing (#6531, #5796, #6121, #6547).

- **Unaliased pool workers no longer loop on claim and close.** A pre-release
  regression claimed work under one identity and closed it under another, so
  `bd` rejected every close with "assignee mismatch" and the worker retried
  forever. Claim, close and the runtime actor now agree on the session bead
  id. `gc hook --claim` re-stamps a bead held under the old spelling, the bd
  pack's stale-DB dog closes its wisp under the claim identity, and `gc sling`
  recognises a pool's bead-id claims (#5716, #6324, #6597, #6632, #6640).

- **Unaliased pool workers are named `<template>-<beadID>` again.**
  Pre-release builds named the first worker with the bare template name (for
  example `claude`), so a runtime name no longer led to its session bead. The
  fix for the Kubernetes pod leak that caused this change is kept: a failed
  create now stops its runtime before the bead closes (#6549).

- **`Ready` on a remote native Dolt store is fast again.** A pre-release
  regression read each candidate's dependencies in its own transaction, so
  controller ticks on remote stores took minutes. The blocker check is now one
  batched read (#6491, #6497).

- **Retries inside ralph/review loops get the right attempt numbers and a
  full retry budget in every iteration.** Pre-release builds carried a retry's
  own number in `gc.attempt` from iteration 2 on, so reviewer prompts and
  `attempt-{attempt}` artifact paths pointed at the wrong or stale
  `attempt-N` directory. v1.4.2 started a retry reached in a later iteration
  at the iteration number, so it could be out of budget before its first
  retry. `gc.iteration` and `{iteration}` are unchanged, and the new
  `{retry_attempt}` placeholder names a step's own retry. In-flight molecules
  keep working across the upgrade (#6548).

- **Upgraded cities register `startup-health-episode` again, so crash-loop
  protection works.** An upgraded store kept its old type list in bd's
  `custom_types` table, so creating the new bead type failed with `invalid
  issue type: startup-health-episode` and startup-health protection was
  silently off. gc now merges its required types into both the config row and
  the table on start without removing custom types, and `gc doctor --fix`
  preserves types that exist only in the table (#6495, #6557).

- **A nudge no longer closes the shared store on a city that relocates the
  `nudges` class.** The first nudge maintenance pass closed the storage
  routes' shared engine, and every later class read in the controller failed
  with `sqlite store: bead store closed` until restart; a 7.6-hour outage was
  seen (#5979, #5980).

- **Managed Dolt is no longer killed about 15 seconds after the session that
  restarted it closes.** `gc dolt restart` from an agent shell stamped the
  Dolt watchdog with that session's id, so the orphan sweep killed it when the
  session ended. Managed Dolt and the detached supervisor no longer inherit
  session identity, and the sweep skips city infrastructure (#6316, #6546).

- **A single bad line in `secrets.env` no longer drops every credential.** A
  multi-line JSON or PEM paste made the parser reject the whole file, and a
  quoted multi-line script value leaked its inner lines as stray variables.
  Parsing now skips only the bad line or quoted block and names it by line
  number (#5982, #6022, #6043).

- **Formula steps no longer erase notes on the work bead.** Closing,
  committing and reporting steps in the core formulas replaced the work bead's
  notes, losing coordinator, human and orphan-sweep notes; they now append
  (#6412).

- **The stale-issue reaper no longer closes active Slack room groups.**
  Durable external-messaging records looked stale to the reaper and were
  closed, freezing inbound transcripts for live Slack rooms; they are now
  excluded (#6380, #6385).

- **A stale or relative ambient `BD_BIN` no longer takes every `gc bd`
  offline.** A pre-release regression made an ambient `BD_BIN` that was
  relative or not executable a hard error for every `gc bd` and bd-backed
  command; it is now ignored with a warning (#6550).

- **`gc bd show --watch` works on proxied cities.** bd v1.3.0 refuses watch
  mode under the default proxied beads transport, so `gc bd show <id> --watch`
  (also `view` and `--current`) now polls the bead every 2 seconds and redraws
  it when it changes. With the pinned bd v1.3.1, plain `bd show --watch` works
  on proxied cities too (#6681).

- **The tutorials match the `gc` pack binding.** Tutorial 01 and the
  quickstart now show `[imports.gc]`, as `gc init` writes it (#6676).

- **`gc init` works again below a directory that holds a bd workspace.** A
  city, or a `gc rig add` rig that is not a git repo, created anywhere under a
  beads repo or a home with `~/.beads` failed with
  `workspace already initialized as database "ws00"`. gc now pins bd to the
  new scope's own `.beads` before `bd init` (#6701).

- **gc-owned proxied city and rig stores no longer move into bd's host-wide
  shared server.** When the user-level bd config set
  `dolt.shared-server: true`, gc-owned proxied stores were silently moved into
  `~/.beads/shared-server`, which could merge two cities' `hq` databases. gc
  now pins the mode off for every bd it spawns, for agent sessions, and in
  each gc-owned scope's `.beads/config.yaml` (written at init and on every
  `gc start`). A new `gc doctor` check, `proxied-shared-server`, reports and
  fixes scopes that are unpinned or bound to the shared server. Beads a scope
  wrote while bound to the shared server stay there and are not migrated;
  `gc doctor` warns while that database exists (#6697).

- **Retry attempts that pass no longer abort their scope or fail their
  workflow.** A spawned retry or ralph attempt was not recognised as
  retry-managed, and an attempt's `gc.outcome=fail` was read as a terminal
  `on_fail=abort_scope` failure before the retry controller could act, so
  retries on such steps were inert (#6534).

- **Secret session metadata no longer appears in process argv.** The local
  tmux `SetMeta` backend (`tmux set-environment`) and the ssh provider's
  `SetMeta` put the value on the command line, visible in `ps` and
  `/proc/*/cmdline` on shared hosts. Secret values now go through a private
  0600 command file (tmux) or are staged over stdin (ssh); if staging fails,
  the write is refused instead of falling back to argv (#6537).

- **Session-bead cleanup no longer stops a running session.** Closing the
  unassigned session bead of a reconfigured, suspended or orphaned session
  used to stop its runtime first; the bead now stays open until the runtime
  has stopped (#6596).

- **The orphan sweep no longer kills a live worker on one transient read.**
  The process-table sweep terminated any untracked runtime whose single store
  read said closed or not found, and `BdStore.Get` reported not found for
  transient bd or Dolt errors. A kill now needs the store and the tick's
  session snapshot to agree, and only bead-level misses count as not found
  (#6649).

- **A new pool worker that holds a live claim is no longer drained as
  orphaned.** On a fresh city the first pool worker was often drained seconds
  after claiming its first step, because the claim hid its demand from the
  reconciler. The reconciler now reads the session's current claim live before
  an orphaned or idle drain, and cancels an in-flight orphaned drain once the
  claim is visible (#6665, #6666).

- **The controller no longer crashes on a tmux state-cache race.** Evicting a
  stopped session deleted it from the published tmux snapshot in place while
  status readers could still be reading that map, a fatal concurrent map read
  and write that `recover()` cannot catch. Eviction now publishes a copy
  (#6735).

- **A pending create whose command drifted no longer holds its alias
  forever.** When the command a start was prepared with differed from the
  persisted one and a retry could not converge, the row stayed in `creating`
  and retried every tick, so no replacement could be built. It is now rolled
  back (the bead closes as `failed-create`, releasing its claim and alias) and
  recreated on the next tick. The stale-create bound no longer renews on each
  retry, and `gc session reset` rolls back a stuck create that is past its
  start lease with no running runtime; `--json` now reports
  `mode: restart|rollback` (#6744).

- **A codex prompt swallowed during a large paste is submitted again.** A busy
  codex TUI still ingesting a large pasted prompt ate every submit in the
  confirm window, leaving the draft staged and the seat idle while holding its
  claim. gc now re-sends the submit, at most 8 times and only while the live
  composer still shows the staged draft and the pane is idle; it never sends
  into an attached session (#6739).

- **`gc session kill` no longer races the reconciler.** The kill stopped the
  runtime before marking the session asleep, so a controller tick in between
  could treat it as a crash: resetting the conversation instead of resuming
  it, closing the bead, or reaping it as a `dead-runtime` corpse. The kill now
  writes a kill-pending marker first; while it is fresh the reconciler and the
  corpse cleaner leave the row alone, queued nudges stay pending, and
  attach/send return HTTP 409 (#6749).

- **Killed and dormant sessions are no longer closed as `dead-runtime`.** The
  corpse cleaner closed any open row whose tmux pane had died, so a session
  stopped with `gc session kill` (or drained, suspended, quarantined or
  archived) could never be woken again. Only rows that claim a live runtime
  are closed now, and only when the dead pane belongs to the row's current
  incarnation, so a mid-restart row is left alone (#6752).

- **A condition-triggered order whose check passes now dispatches on the tick
  that observes it, instead of queueing behind the per-tick dispatch budget.**
  `orders.max_dispatches_per_tick` (default 4) capped every trigger type and
  advanced a rotation cursor only when it spent a slot, so on a city with ~40
  orders and a tick period longer than every sweep's interval the cooldown
  sweeps were due at every tick and spent the whole budget before the rotation
  reached anything else — each order got a turn roughly every ten ticks. A
  condition order is a wake demand rather than a request for a turn: its check
  reports that work is pending right now, and it is true only for as long as
  that work waits. On maintainer-city the merge-queue order went 11 h without a
  dispatch while merge beads sat ready, with no gate error, no suppression
  event and nothing in the journal to show for it. Due condition orders now
  fire outside the budget (still single-flighted by the open-tracking and
  open-work gates, and still bounded by their own check), the budget and its
  rotation are unchanged for cooldown, cron and event orders, and a tick that
  spends its budget logs one line naming the orders it did not reach.

- **A closed binding row now supersedes its retained frozen twin in the
  one-live-workflow-per-source-bead guard, so a converged city stops refusing a
  sling whose only live root is gone.** A storage migration copies rows into the
  class binding with ids preserved and deletes nothing, so a workflow root
  relocated into the binding and later closed there still exists as an OPEN copy
  in the retained work ledger. The guard unioned every leg's live roots, reported
  that copy as live, named it in `blocking_workflow_ids`, and refused. The
  collector now lets the binding's row win on a shared root id — live or closed —
  asking the binding directly about the ids the work legs reported, with a
  bounded per-id probe rather than a full closed scan. Only a row that is really
  the same root supersedes: ids are unique within a store and store-prefixed ids
  collide across stores, so the binding's row must be a workflow root, for the
  same source bead, naming the same source store whenever both sides name one. A
  probe that faults refuses the sling: a binding fault is an error, never
  absence. A binding row that predates the `gc.source_store_ref` stamp
  supersedes its twin only once it is closed, because a live one is invisible to
  the guard's own scan and dropping its twin would leave the sling unguarded. A
  city that relocates nothing has no binding leg, runs no probe, and enumerates
  exactly what it did before.

- **The one-live-workflow-per-source-bead guard reads the graph binding, so a
  split city stops admitting a second live workflow.** A workflow root is graph
  class, so on a city that relocates the graph class every live root is in the
  binding — and neither sling door enumerated it. The API walked the city store
  plus each rig store; the CLI walked the city and rig *directories*, which a
  binding is not. Both then answered "no conflict" out of stores that
  structurally could not hold the answer, and a second launch was admitted for a
  source bead that already had one (the batch path instead rolled its launch
  back, because the same blindness hid the root it had just created). Both
  enumerators now lead with the relocated binding, under the same
  `graph:<city>` store ref the workflow snapshot scan already mints and parses,
  and the CLI gets it from the one class-binding front door rather than a second
  enumerator. A scan fault on that leg refuses the sling instead of degrading to
  the tolerated non-source-store warning: a binding fault is an error, never
  absence. A city that relocates nothing enumerates exactly what it did before,
  and one root reached through two legs is named once.

- **The work-record close gate asks the repository the bead's OWNER points at,
  not the store it was read through.** A rig's work step that a relocated class
  binding holds has its commits on the rig's checkout, and both close doors
  asked the city's instead — the CLI class door hands its gate the city path,
  and the HTTP door matched the store that answered against the configured rigs,
  which a binding is not. With `GC_WORK_RECORD_ENFORCE` on, a compliant
  `gc.work_outcome=shipped` close of such a bead was refused with "commit is not
  reachable" against a repository that was never the bead's. Both doors now
  resolve the repository through one rule (`workrecord.RepoDirFor`): the bead's
  own `gc.work_dir`, else the scope `gc.root_store_ref` records — a rig owner to
  that rig's checkout, a city or binding owner to the city's. An owner no
  checkout is configured for is "unknown" rather than the city, and the
  reachability clause degrades to a warning there on both doors instead of
  refusing a close neither can judge; a bead with no outcome at all is still
  refused. A bead that records no owner keeps the answer its door already gave,
  so single-store cities are unchanged.

- **The infra-class cutover now carries dependency-edge payloads.** Every
  within-infra edge the copy re-added went in through a writer that clears the
  pair's metadata sidecar, so the binding received those edges with their
  endpoints and type intact and their payloads gone. In production the
  payload-carrying edges are the `waits_for` fanout gates between formula step
  beads, whose payload records the gate kind; an absent payload reads as the
  default, `all-children`, so a gate the formula asked to release on the first
  child waited for every one of them instead. The copy and the recovery path
  now share one edge writer, a destination that cannot carry a payload is
  refused rather than written to without it, and the equality witness compares
  the payload rather than only the edge.

  **Upgrading:** this does not repair a city that already cut over. Its binding
  still holds the payloadless edges, and no command yet detects or repairs
  them. See "If this city cut over before edge payloads were carried" in
  `docs/runbooks/split-storage-classes.md` for what is affected, what is not
  lost, and the destructive re-converge procedure.
- **`gc workflow delete-source` and `gc workflow reopen-source` write the copy
  of the source bead the residency contract owns, not the one the selector
  named.** An explicit `--rig` / `--store-ref` pins the store the sweep works
  in, and both commands were writing the source bead's metadata through that
  same store. On a converged city — infrastructure classes relocated into the
  class binding with ids preserved, the pre-migration copies retained and
  frozen at cutover — an operator naming the city cleared `workflow_id` on the
  frozen twin while the binding's live row went on pointing at the workflow
  that had just been swept. The next resolve answers from the binding, still
  sees a workflow, and refuses the re-sling as already-running against a tree
  that no longer exists; `reopen-source` had the same defect one verb over,
  reopening the twin and leaving the row the city actually reads closed and
  still bound. Both now resolve the owning copy through the by-id residency
  walk (binding-first, a binding fault is an error and never absence), then
  clear any other resident copy's stale `workflow_id` best-effort so the two
  cannot disagree. A city that relocates nothing, and any `--rig` run, writes
  exactly the store it wrote before.

- **A control bead served by a relocated class binding is routed to the
  dispatcher its own `gc.root_store_ref` names.** On a split city every rig's
  control beads live in one class binding. The reconciler dropped rig-rooted
  rows from control-dispatcher demand entirely, because a binding's ref reads
  as city scope and the candidate filter required a rig match; and it
  suppressed city-rooted rows from that same demand, because the route repair
  read the binding's ref (`class:gmnos`) as a rig name, found no dispatcher
  for that pseudo-scope, and logged `no configured control-dispatcher for its
  store scope` once per tick. The binding is now collected for every row it
  serves, and the repair keys the dispatcher on the row's root scope, so rig
  rows keep (or are repaired toward) their rig dispatcher and city rows their
  city dispatcher. The diagnostic names the binding and the owning scope.
  Supersedes #5548 and #5588; fixes #5547 and #5587.

- **Mail archive and delete now expand whitespace-joined message IDs.** Each
  positional argument is split into individual IDs before single-versus-batch
  dispatch, so shell variables containing multiple IDs no longer look like one
  already-handled message.
- **graphv2 retry re-attempts for rig-scoped `lifecycle=one_shot` steps now
  keep their rig qualifier and drop stale session pinning.** A retry
  control that is nested/runtime-minted (as opposed to one decorated at
  compile time by graphroute) never gets `gc.execution_routed_to` stamped,
  only `gc.execution_rig_context` backfilled. `spawnNextAttempt`'s
  `qualifyAttemptTargetWithSourceRoute` derived a rig prefix only from
  `gc.execution_routed_to`, so a re-attempt for a step whose
  `gc.run_target` was a bare rig-template agent name lost its rig
  qualifier — `gc.routed_to` landed unscoped and no pool ever claimed the
  re-attempt. Separately, `applyAttemptStepRoute`'s metadata-only pool
  branch never cleared `gc.session_affinity`/`gc.continuation_group` the
  way `graphroute.ApplyGraphRouteBinding`'s pool-branch affinity clear
  does, so a re-attempt for a one_shot lifecycle agent stayed pinned to
  session affinity for a runtime that had already exited after its
  bounded invocation. Both are fixed: the target qualifier now falls back
  to the step's own execution rig context, and metadata-only pool
  attempts for one_shot agents now clear the stale continuation/affinity
  keys, mirroring `graphroute.ApplyGraphRouteBinding`'s pool branch.

- **A `lifecycle=one_shot` pool session that exits into a freeable sleep
  reason no longer blocks its own runtime name forever.** The session's
  bounded-work exit lands its bead in `state=asleep` via the generic heal
  path, same as any other dead session, but `reusablePoolSessionInfo`
  excluded every asleep session from reuse on the assumption that "the
  reconciler closes orphaned asleep beads" — no such closing exists. The
  bead stayed open holding the identity's `session_name`, and the next
  tick's fresh create failed closed on that same name ("pool session name
  unavailable ... session name already exists") until an operator ran
  `gc session close` by hand. The asleep exclusion is now scoped: a
  `one_shot` session whose sleep reason is already in the freeable
  allow-list (idle, idle-timeout, city-stop, failed-create, runtime-missing,
  provider-terminal-error, max-session-age) is reused through the ordinary
  wake path instead. A `one_shot` exit that still holds an open or
  in-progress assigned work bead under any of its identities is not treated
  as a clean exit — it falls through to fresh-identity creation rather than
  being reused, so the unfinished step is claimed properly instead of being
  pinned `in_progress` on a dead name. Persistent (non-`one_shot`) pools
  are unaffected — a genuine crash still gets a fresh identity.

- **`model` and `effort` pins are no longer silently discarded on launch.**
  Provider model ids were modelled as a closed enum, so any id the builtin
  catalog had not caught up to produced no flag args and the launch path
  emitted no `--model` at all — leaving the agent on whatever the CLI
  defaulted to, with no error. Named-session resolution meanwhile hard-errored
  on the same value, so the two paths disagreed and the launch path failed
  open. `gasburger.refinery` and `gasburger.gorkcats` ran unpinned this way for
  months (ga-fyh); Claude hit the same hole in ra-jbbv0.

  `model` and `effort` are now **open** options across the catalog: the
  declared choices remain the curated suggestion list, and any other value is
  honored by rendering the option's flag template. An unreleased id or a newer
  effort tier reaches the provider CLI to accept or reject, instead of being
  dropped. Specifically:

  - grok gained `grok-4.6` and `grok-4.7` as curated ids.
  - `effort = "max"` on codex and `effort = "xhigh"` / `"max"` on antigravity
    are honored; those enums previously stopped short, so a tier blessed by
    claude's own defaults was not portable.
  - cursor gained a model option; it had none, so every cursor model pin was
    dropped as an unknown key.
  - Effort tiers now come from one canonical vocabulary shared by every
    provider rather than a hand-copied per-provider list.

  A pin that still cannot be honored — an unrecognized value for a genuinely
  closed option such as `permission_mode`, or an option the provider has no
  concept of (13 of 20 providers have no effort flag) — prints a loud startup
  warning naming the agent, the rejected value, and the valid set.

- **`gc import add` of a local in-git pack now locks to HEAD, not the repo's
  latest tag.** Per `gc import add --help`, a local path inside a git
  worktree is documented to be "locked to the current commit," but the
  default version resolution (absent an explicit `--version`) preferred the
  repo's latest semver tag whenever one existed. A pack added to the
  worktree after the last tag was cut resolved to a checkout whose tree
  predates the pack, failing with a misleading "missing pack.toml" error
  even though the pack exists at HEAD. A local-worktree-promoted source now
  always locks to `sha:<HEAD commit>`, matching the documented behavior;
  registry/remote sources are unaffected. Fixes #3659.

- **`gc doctor`'s `order-firing-current` check no longer hard-fails
  `gc doctor` (exit code, `BlockingFailed`) when its own order-history
  lookup times out.** The check races a Dolt-backed order-history query
  against a 15s budget; on timeout it returned `StatusError` with no
  `Severity` set, silently defaulting to `SeverityBlocking` (the zero
  value of `CheckSeverity`) — turning a slow-but-healthy city's doctor run
  red even when scheduled orders were firing normally, since a timed-out
  lookup proves nothing about actual order staleness. The timeout branch
  now explicitly sets `Severity: SeverityAdvisory` and `TimedOut: true`,
  matching how `Doctor.boundedRun`'s own per-check timeout is already
  reported, so callers (including `--json` output) can distinguish
  "confirmed stale" from "the query didn't finish in time." (#4895)

- **Wisp GC now reaps rootless leaf plain-task wisps.** The orphan reaper
  (`reapOrphanedClosedWisps`) previously skipped any closed wisp-tier row
  with no `gc.root_bead_id` pointer outright, and the root-rooted closure
  purge never enumerated it either (it matches none of the root selectors:
  not a molecule, not `gc.kind=wisp`, not a graph.v2 workflow). A closed
  plain-task wisp that never had an owning root therefore accumulated
  uncollected in the wisp tier indefinitely. Such a row now reaps on its
  own closed status when it is a leaf — no parent and no children — since
  it then has no root to check for collectibility and the single-bead
  delete strands nothing. Leaf-ness is tested over both ownership links a
  bead can carry — the `parent_id` column and a `parent-child` dep row —
  since some step beads are joined to their parent by the dep row alone.
  A rootless row that owns a subtree, is itself a subtree member, or is
  not a plain task, remains out of scope, preserving the original safety
  boundary. The leaf-ness probes are bounded per sweep (including in the
  dry-run default, where they are the only backend cost) so the scan never
  does unbounded reads per tick. Fixes #3780.

- **The legacy workspace-identity deprecation warning now caveats that
  following it can silently break packs pinned to an older revision.**
  `city.toml`'s `workspace.name`/`workspace.prefix` deprecation hint told
  operators to move those fields to `.gc/site.toml`, but any installed pack
  still pinned to a revision that reads `workspace.name` directly (rather
  than the newer site-binding-aware resolution) would silently lose its
  identity/routing once the field was removed — reported after one
  deployment lost inbound Discord messages for ~2 days with zero alarms
  before anyone checked delivery receipts. The warning now says so
  explicitly, naming `gc doctor --fix` (which performs the removal) so
  operators check pack compatibility before running it. (#3887)

- **The dashboard's bead dependency graph preserves relation type on inverse
  ("Blocks") edges instead of collapsing every downstream relation to a
  plain, untyped blocker.** `buildBeadGraph`'s inverse-edge pass previously
  stored only the raw dependent bead, discarding the `dependencies[].type`
  (or `needs`) that produced the forward edge; the `BeadDependencies` detail
  view then rendered every downstream relation — `tracks`, `parent-child`,
  or a genuine `blocks` need alike — under the same unlabeled "Blocks"
  heading. A `tracks` relation (e.g. a workflow root tracking a finalizer)
  could therefore read as a second hard dependency. The inverse edge now
  carries the same `kind` as its forward counterpart, and the detail view
  labels it the same way the "Needs" section already labels non-`needs`
  forward edges. (gascity#4365)

- **A named (on-demand) session no longer replays a trigger stamp for a work
  bead that has since been parked.** The pool session path already clears
  `gc.trigger_bead_id` when there is no ready work to route
  (`bindPoolSessionTriggerBead`), but the named path only ever read and
  replayed whatever was already stamped, with no equivalent check. A
  singleton tier re-materializing after its dispatched bead was parked kept
  re-aiming every new seat at the same stale target — one reported case
  produced 16 seats on a single parked bead over ~19 hours, each re-deriving
  the same dead-end analysis. The named path now checks the stamped target's
  live state before resolving its template and clears the stamp (and its
  dependent `gc.brain_parent_sid`) when the target is no longer workable,
  mirroring the pool path's clear semantics. "No longer workable" means
  closed, absent, or dependency-blocked; the blocked case is read off bd's
  `is_blocked` ready-work projection, because every production store folds
  bd's raw `blocked` status into `open`. A target in a store that does not
  publish that projection is left stamped rather than risk a wrong clear, as
  are cross-store targets this reconciler tick cannot reach. (gascity#4373)

- **The work-record close gate now resolves `gc.work_branch` against its
  remote-tracking ref, not the local branch alone.** `gitCommitReachableOnBranch`
  passed the bare branch name (e.g. `main`) straight to
  `git merge-base --is-ancestor`; gitrevisions precedence resolves a bare name
  to the local `refs/heads/<branch>` ahead of any remote-tracking ref. In a
  refinery/polecat topology, merges land via a push from a *different*
  worktree — advancing `refs/remotes/origin/<branch>` but never the local ref
  checked out elsewhere — so a genuinely-landed commit read as unreachable
  until something happened to fast-forward the local branch, which in that
  topology may be never. The gate now checks `refs/remotes/origin/<branch>`
  first when it resolves, and still falls back to the bare branch name — so a
  commit is reachable if it is on either ref. Purely local repos with no
  `origin` remote are unaffected, and a commit that has been committed locally
  but not yet pushed continues to satisfy the gate as it did before.
  (gascity#5037)

- **The dolt pack's `run_bounded` python3 fallback now sends SIGTERM before
  SIGKILL, matching its documented contract.** The fallback (used when
  neither `timeout` nor `gtimeout` is on `PATH`, the default on stock macOS)
  previously called `subprocess.run(..., timeout=...)`, which kills the
  child with SIGKILL immediately on expiry — giving it no chance to run its
  own signal handler, unlike the `timeout --kill-after=2` path it's meant to
  match. `mol-dog-backup.sh` wraps `dolt backup sync` in this helper, and
  `dolt` publishes a backup archive under its final name before writing the
  manifest that references it; a SIGKILL mid-sync left the archive
  permanently unreferenced (`dolt backup` has no prune verb). The fallback
  now uses `Popen` + `terminate()` + a 2s grace `wait()` + `kill()`,
  streaming output instead of buffering it. (gascity#4823)

- **`GET /runs/{id}/steps` returns steps in topological (pipeline) order, not
  arbitrary fold order.** No level of the read chain — the handler, the
  member-bead projection, or the run projection fold — applied any sort, so
  the Runs dashboard's Formula Graph rendered a run's steps in whatever order
  the projection happened to yield, unreadable as a pipeline. Steps are now
  topologically sorted on each member's real dependency edges (`Dependencies`
  and `Needs`), with a deterministic bead-ID tiebreak for independent steps
  and steps carrying no dependency data. (gascity#4699)

- **`check-core-boundary.sh` no longer false-positives on an in-tree Go
  build cache.** The `org_` boundary scan walked the whole working tree
  (`grep -r --exclude-dir=vendor --exclude-dir=testdata`), so any untracked
  in-tree Go module cache tripped false violations on third-party module
  sources — the common case is GitLab CI's canonical
  `$CI_PROJECT_DIR/.cache/go-mod` layout. The scan now runs over
  `git ls-files` instead, so an untracked cache or build-artifact directory
  is invisible to it regardless of name, while vendor/testdata (tracked or
  not) stay excluded as before. (gascity#4479)

- **`gc status` no longer pays a full event-log scan for a cosmetic field.**
  `storehealth.LastMaintenance` now prefers the `TailProvider` backward-scan
  fast path over an unbounded forward `List` when the provider supports it,
  and a new `Filter.MaxScanBytes` bounds that backward scan so a rare or
  never-emitted event type (the common case: a city that has never run store
  maintenance) can no longer force a full-file walk just to populate the
  `Last GC:` status line. Previously this cost two full scans of
  `events.jsonl` on every `gc status` call, dominating latency on large event
  logs and surfacing as a spurious "runtime status probe timed out" warning.

  Operator note: `Last GC:` may now be absent on a busy city even though
  maintenance has run. The tail scan looks back a bounded 8 MiB, and it reads
  only the active `events.jsonl` — never the rotated `.gz` archives — so a
  maintenance event that has aged out of the window, or out of the active file
  entirely, is reported as absent rather than stale. The field is display-only
  and nothing gates on it. `gc maintenance status` is the fallback, with one
  caveat: it reads the supervisor's in-memory run history, so it requires a
  running supervisor and resets when the supervisor restarts. It is a live
  view, not an equivalent durable source; for durable history, query the event
  log for `store.maintenance.*` directly. (gascity#4418)

- **`gc formula cook --attach`'s help text no longer claims a parent-child
  relationship it never creates.** `--attach=<bead-id>` has only ever added
  a `blocks` dependency from the attached bead to the sub-DAG root
  (`ensureFormulaCookAttachDep` / `molecule.Attach` both call
  `store.DepAdd(..., "blocks")`, never setting `ParentID`), but the long
  help described it as creating the sub-DAG "as children of the given
  bead." Since convoy auto-close watches parent-child children, not
  `blocks` dependents, a user following the old description would wrongly
  expect an attached sub-DAG's completion to trigger the attached convoy's
  auto-close — it never does. Help text now describes the actual `blocks`
  -only relationship and says so explicitly (gastownhall/gascity#2392).

- **`gc stop --help` and `gc stop --json` now state what actually happens to
  a supervisor-managed city's registration.** `gc stop` has always
  unregistered a supervisor-managed city as part of stopping it, but neither
  the CLI help nor the `--json` output said so — a user reading "Stop all
  agent sessions in the city" reasonably expected `gc start` to find the
  city again later. Help text now documents the unregister behavior and
  points at `gc register`/`gc unregister` for the split operations; the
  `--json` envelope gained an `unregistered` field reporting whether this
  stop also removed the supervisor registration (gastownhall/gascity#4366).

- **`gc bd` no longer lets bd's own error message steer operators into a
  Dolt-server conflict.** When the managed Dolt server is unreachable, bd
  (with `dolt.auto-start: false`, which gc always sets) tells the operator
  to run `bd dolt start` — but that starts a second, unmanaged Dolt server
  that fights gc's own managed server for the same data directory. `gc bd`
  now detects that suggestion in bd's stderr and appends a corrective hint
  pointing at the actual remedy (`gc start` / `gc dolt restart`) alongside
  bd's original output, without altering bd's own exit code. The hint fires
  only for gc-managed Dolt endpoints, whose lifecycle gc owns; externally
  bound or explicitly configured endpoints keep bd's own output unchanged
  (gastownhall/gascity#1374).

- **`gc init` provider readiness now finds Nix-installed CLIs.** Provider
  readiness probes (`gc init --default-provider gemini` and friends) search a
  deterministic, user-aware set of install directories rather than the
  ambient `$PATH`, so a CLI installed via Nix (`~/.nix-profile/bin` for
  classic `nix-env`, `~/.local/state/nix/profiles/profile/bin` for the newer
  `nix profile install`) was reported as "not installed" even when it was on
  the shell's `PATH`. Both locations are now included, matching the existing
  npm/pnpm/yarn/cargo/nvm handling in `internal/searchpath`. Fixes #3962.

- **ACP activity is now available across process boundaries.** ACP
  `session/update` timestamps are published through an atomic, coalesced
  sidecar, allowing a process other than the session owner to report
  `last_active`. Sidecar I/O runs off the JSON-RPC dispatch loop, and transient
  publication failures are reported and retried. ACP now declares the matching
  activity capability, enabling timed idle policies and the existing opt-in
  `[session] progress_stall_timeout` policy. The declaration also engages two
  paths that are on by default for ACP: a configured named ACP session whose
  config has drifted is no longer deferred as `activity_unknown`, so a
  config-drift tick can now reset it once its last observed activity is older
  than the two-minute named-session activity threshold; and nudge delivery now
  applies the configured quiescence window to ACP instead of taking the
  deliver-without-an-activity-signal fast path. Activity age records only the last
  observed protocol update; it does not by itself diagnose why updates stopped
  or prove that a session is dead. `progress_stall_timeout` remains disabled by
  default.

## [1.4.2] - 2026-09-18

### Fixed

- Restore the 1.4 release line's compatibility with Beads 1.3.0: initialize
  fresh managed workspace version witnesses, align native library and CLI
  pins, and decode decimal-string revision tokens without losing precision.
- Allow graph containers to close without depending on their own finalizer,
  and explicitly close skipped scope members while preserving cleanup.

### Upgrading Notes

- Coordinate clients sharing a Beads 1.2.2 database before running
  `bd migrate schema`. The verified migration moves schema 53 to 66;
  older clients cannot read the migrated schema.
- Workflow dependency fixes apply to newly instantiated graphs and do not
  rewrite already-persisted workflow dependencies.

## [1.4.1] - 2026-08-15

### Changed

- **beads pinned to v1.2.2 everywhere** (go.mod library, `deps.env`
  `BD_VERSION`, CI matrix, SHA-verified archive installer). beads v1.2.2 is
  the recovery re-release of the tested 1.1 line that superseded the
  accidental, untested v1.2.0/v1.2.1. With the library and the Homebrew
  `beads` formula both at 1.2.2, the native store preflight's version-match
  check passes again for `brew install gascity` users, re-activating the
  in-process NativeDoltStore (it had silently fallen back to the per-call
  `bd` CLI store since bd 1.1.2 replaced 1.1.0 as the released version).
  The module bump is code-identical to the previously pinned 1.1 line
  (v1.2.2 is the v1.1.2 tree; no transitive dependency changes).

## [1.4.0] - 2026-07-24

### Upgrading Notes

- **Configure one store-scoped `control-dispatcher` for every graph-owning
  scope.** Formula control beads now route to the dispatcher whose `Dir`
  matches the city or rig store that owns the graph. A rig-owned graph with no
  matching dispatcher fails before instantiation instead of falling back to a
  dispatcher that cannot read its work.
- **Run `gc doctor --fix` after upgrading an existing city.** The current
  doctor converges pack imports, provider catalogs, project identity, retired
  hold labels, and managed beads/Dolt metadata before the orchestrator starts.
- **Upgrading over an older install at a different path may need a manual
  reseed.** If a machine already ran an older `gc` (for example a Homebrew
  binary now replaced by a source build at a new path), `gc start` can keep the
  stale supervisor running and can fail closed on a present-but-invalid
  bundled-pack cache — only an *absent* cache self-heals. Run `gc import
  install` to repopulate the cache, then let `gc start` auto-restart the
  supervisor (or on Linux `systemctl --user restart gascity-supervisor`).
- **An unrelated stale registered city can block `gc start`; fix or unregister
  that city — not the one you are starting.** A pre-1.3 city still registered
  with un-migrated provider config (for example `workspace.provider = "claude"`
  with no `[providers.claude]` block) can fail the registry scan and abort
  startup, with a misleading hint to `gc init` the healthy city you were
  actually starting. Run `gc doctor --fix` inside the offending stale city, or
  `gc unregister <stale-city>` to drop it.
- **macOS: a supervisor left running from a prior version may need a manual
  restart.** macOS cannot resolve a direct (non-launchd) supervisor's
  executable for binary-drift detection, so the automatic post-upgrade restart
  may not complete. Run `gc supervisor stop --wait`, then `gc start`.

### Added

- **A run-centered dashboard and API.** Run detail now combines the formula
  stage ladder, structured transcripts, token rate, and estimated burn rate.
  Session and run reads use typed, paginated API surfaces backed by warm
  projections instead of ad hoc wire shapes.
- **Durable usage and lifecycle observability.** Model, compute, and lifecycle
  facts feed local usage history and OpenTelemetry metrics. End-of-interval
  transcript sweeps keep live pool sessions' token and cost rates current even
  when agents self-drive after their initial claim.
- **Privacy-scoped command-usage metrics in release artifacts.** Before the
  first eligible interactive command is recorded, `gc` shows the complete
  disclosure. Events contain only a canonical command ID, the `gc` release,
  operating system, and an anonymous installation ID—never arguments, paths,
  file contents, or environment values. `gc metrics status`, `example`, `on`,
  and `off` expose the local controls; `DO_NOT_TRACK=1` and
  `GC_DISABLE_USAGE_METRICS=1` provide environment-level opt-outs.
- **Broader runtime composition.** Provider routing, ACP/automatic runtime
  selection, Herdr-backed sessions, and Kubernetes/subprocess/tmux execution
  share the same session lifecycle and worker boundary.
- **Production workflow controls.** Formulas v2 gained stronger retry,
  fan-out, drain, scope, artifact, and finalization behavior, plus better live
  status and event evidence for operators.
- **An experimental OpenClaw bridge proof of concept.** The private package
  under `contrib/openclaw-bridge` explores iMessage and Telegram connectors; it
  is not a supported provider pack or a shipped connector artifact.

### Changed

- **Session lifecycle operations converge through the worker boundary.** Pool
  demand, wake, resume, drain, close, and orphan recovery now reason from
  persisted session/work identity rather than provider-specific shortcuts.
- **Beads remains the persistence boundary while storage becomes more
  resilient.** Native and CLI-backed stores share transactional lifecycle
  semantics, bounded cached reads, store-aware routing, and explicit degraded
  results across city and rig scopes.
- **CI and local verification are sharded and event-driven.** The release gate
  includes fast units, process tests, integration packages, tutorials, real
  inference acceptance, and macOS regressions without one monolithic test
  process.

### Fixed

- **Pool sessions no longer lose or strand work while draining, restarting, or
  reusing capacity.** Claim ownership, wake budgets, slot selection, and
  confirmed-dead cleanup are now fenced against stale or partial observations.
- **Formula control routing retries transient configuration reads.** Attempt
  spawn and fan-out no longer quarantine an in-flight run because of a
  momentary config/include read failure; a successfully loaded configuration
  that lacks the required scoped dispatcher still fails closed.
- **Managed beads and Dolt paths fail more honestly.** Provider health,
  endpoint ownership, lock release, compaction, reindexing, stale data-dir
  cleanup, and partial-store reads now preserve errors instead of silently
  reporting complete state.
- **Events, nudges, waits, and session output remain bounded under load.** The
  CLI drains paginated event windows, request paths avoid unbounded scans, tmux
  sessions keep their shared server, and structured transcripts preserve tool
  and error frames.
- **Live run cost fields populate for long-lived pool sessions.**
  End-of-interval model-usage sweeps account for each transcript window once,
  restoring `tokens/min` and `burn/hr` in run detail (PR #4436).
- **Customer Zero dashboard and claim regressions are closed.** Cross-city
  attention reads now cancel stale requests and recover after startup; Health
  reports per-metric availability with cross-platform sampling instead of
  false zero/NaN values; and hook claims no longer fuzzy-update a vanished
  session record (#4354, #4356, #4361).
- **Release-candidate gates are portable and reproducible.** Bash 3 scripts,
  deep metrics fixtures, reusable pool slots, Tier C pack compatibility, and
  container-tool vulnerability checks now exercise the same bounded behavior
  expected from the shipped artifacts.

## [1.3.0] - 2026-06-18

### Upgrading Notes

- **Run `gc doctor --fix` once per existing city after upgrading.** The 1.3
  doctor owns the breaking migrations for explicit provider catalogs and
  explicit pack imports/locks. The relevant checks are `provider-catalog`,
  `builtin-pack-imports`, and `packv2-import-state`.
- **Provider references must be declared in `[providers]`.** Cities that set
  `workspace.provider` or agent-level `provider` values now need matching
  `[providers.<name>]` entries. `gc doctor --fix` appends missing built-in
  aliases such as `[providers.claude] base = "builtin:claude"`; custom
  providers still require hand-authored provider tables.
- **Built-in and gastown pack composition changed.** Gas City no longer
  relies on implicit built-ins or per-city `.gc/system/packs` materialization.
  Existing `workspace.includes = [".gc/system/packs/..."]`, legacy public
  `gastown`/`maintenance` import sources, superseded bundled pins, and stale
  locks are migrated to explicit pinned imports in `pack.toml` plus
  `packs.lock`.
- **No control-dispatcher named session is generated.** The control dispatcher
  serves entirely via demand-scaling of the core pack's `core.control-dispatcher`
  agent template (controller work routed through `gc.routed_to =
  "core.control-dispatcher"`), so the on-demand `[[named_session]]` alias older
  builds wrote is redundant. `gc init` no longer creates it, and `gc doctor
  --fix` (during a pack-layout migration) drops the stale alias from upgraded
  cities so its "backing template not found … disabled" warning stops firing.
- **Generated configs no longer pin `formula_v2`.** Formula v2 is on by
  default, so `gc init` omits the `[daemon] formula_v2` line instead of writing
  the default value. An explicit `formula_v2 = false` (or the deprecated
  `graph_workflows = false` alias) is still honored and preserved on round-trip.
- **`gc session logs --tail N` no longer renders blank.** Every transcript
  entry that occupies a tail window now prints at least one line — a non-error
  `tool_result` shows `tool_result: ok`, and any otherwise non-rendering entry
  (empty text, thinking, or an unrecognized block) shows `(no displayable
  content)` — so a tail landing on such entries can no longer produce empty
  output.
- **The built-in Claude provider no longer declares a fresh-start
  `session_id_flag`.** Claude remains resume-capable, but Gas City now records
  the provider-created session key after startup instead of passing a
  preselected fresh-session ID.
- **The `gastown` pack is now consumed from
  `github.com/gastownhall/gascity-packs`.** The old checked-in
  `examples/gastown/packs/gastown` tree is gone. Move local customizations
  into an explicitly imported pack instead of editing `.gc/system/packs` or
  the retired vendored example path.
- **Fallback agents were removed.** Packs that previously depended on a
  fallback dog/worker must ship their own worker pool and formulas. Cross-pack
  agent name collisions are now hard errors.
- **Public imports are intentionally small.** Authored `[imports.<binding>]`
  tables expose `source` and optional `version`; older `export`,
  `transitive`, and `shadow` keys remain compatibility-only loader behavior,
  not public schema.
- **Formula v2 targeted executions use `convoy_id`, not `bead_id`.**
  Graph/formula v2 templates no longer receive `{{bead_id}}`; update them to
  use the reserved `{{convoy_id}}` variable for the input convoy. Formula v2
  rejects authored inputs named `convoy_id` or `bead_id`, and `{{issue}}`
  remains only a temporary compatibility alias that should be migrated too.

### Changed

- **Version pins on builtin packs are honored: the binary only pre-seeds
  its embedded content at each pack's canonical pin.** Previously the
  bundled synthetic cache served the running binary's embedded bytes for
  ANY commit pinned on a bundled source — editing the pin changed nothing.
  Now only the canonical pin (the one `gc init` writes) resolves from the
  embedded copy; a bundled source pinned at any other commit behaves
  exactly like a regular remote import: `gc import install` fetches that
  exact commit from git, validation uses the git checkout, and the cache
  slot uses the plain remote key. Cities on canonical pins keep working
  fully offline, including across binary upgrades that keep the pin
  constants; releases that bump a canonical pin migrate existing cities
  via `gc doctor --fix` (superseded canonical pins are rewritten to the
  current one).

- **Builtin packs are no longer materialized into cities; they compose via
  pinned imports resolved from the user-global pack cache.** The per-city
  `.gc/system/packs` tree is retired (and pruned on sight): `gc init` now
  writes pinned `[imports.core]`/`[imports.bd]` entries into pack.toml plus
  a matching packs.lock, and the gc binary self-heals the GC_HOME cache
  (`$GC_HOME/cache/repos`) with its own embedded content so the pins resolve
  offline. The `builtin-pack-includes` doctor check became
  `builtin-pack-imports`: it migrates legacy `workspace.includes =
  [".gc/system/packs/..."]` cities by stripping the includes, upserting the
  pinned imports (creating a minimal pack.toml for legacy cities), and
  refreshing packs.lock and the cache. The bd lifecycle script moved behind
  a stable per-city shim at `.gc/scripts/gc-beads-bd.sh` that execs the
  cache-resolved bundled script; provider normalization still recognizes
  the legacy materialized path. All repo-cache roots (packman install,
  config resolution, doctor) now uniformly resolve via GC_HOME instead of
  mixing `$HOME/.gc` and GC_HOME. `gc rig add --include <builtin>`
  canonicalizes to the bundled remote source and locks it. **Migration:**
  run `gc doctor --fix` once per existing city.

- **The registry `gascity` planning pack is bundled and offered by the init
  wizard.** `gc init` now offers `gascity` as a config template alongside
  minimal/gastown (also via `--template gascity`), wiring the pinned public
  import from gascity-packs the same way the gastown template does. The
  pack is embedded from the `github.com/gastownhall/gascity-packs` module
  root, so the pin resolves offline from the bundled synthetic cache.

- **The bundled gastown pack is now a Go module dependency, not a checked-in
  copy.** `examples/gastown/packs/gastown` is gone; the gc binary embeds the
  pack from `github.com/gastownhall/gascity-packs` (pinned in go.mod to the
  registry release commit), and the example city composes gastown through
  the pinned public registry import plus a committed `packs.lock` — the same
  shape `gc init` writes — resolved offline from the bundled synthetic
  cache. `scripts/update-bundled-gastown-pack` no longer writes a vendored
  tree; it bumps the go.mod pin, the `PublicGastownPack*` constants, and the
  example pins from the latest registry release, and `--check` verifies the
  pinned module content against the registry hash. The gastown integration
  tests in `examples/gastown` now run against the module-embedded bytes, so
  a runtime/pack mismatch fails in gascity CI.

- **The bundled maintenance pack was folded into the core pack, and builtin
  packs compose only through explicit pinned imports.** The bundled `core`
  pack carries the gc-* skills, default worker prompts, core formulas, the
  mechanical housekeeping orders that used to ship in the maintenance pack
  (gate-sweep, orphan-sweep, cross-rig-deps, order-tracking-sweep,
  spawn-storm-detect, prune-branches, wisp-compact, nudge-mail-sweep,
  nudge-on-route, cascade-nudge-on-blocker-close), the check-binaries doctor
  check, and the per-provider hook overlays. Config load no longer splices
  builtin packs into composition: `gc init` writes explicit `[imports.core]`
  and, for default bd-provider cities, `[imports.bd]` entries into
  `pack.toml`, plus a matching `packs.lock`. The fixable
  `builtin-pack-imports` doctor check repairs missing imports and migrates
  legacy `workspace.includes = [".gc/system/packs/..."]` cities by stripping
  those includes, adding the pinned imports, and pruning stale
  `.gc/system/packs` materialization. **Migration:** run `gc doctor --fix`
  once per existing city.
- **The implicit fallback dog is gone, and the `fallback` agent field was
  removed.** The gastown pack now owns its dog pool outright
  (`agents/dog/`, themed, with `mol-shutdown-dance`), and the dolt pack
  keeps its own dolt dog for Dolt maintenance formulas. The
  fallback-agent resolution mechanism (`fallback = true`, non-fallback
  wins, first-loaded wins) was removed: cross-pack agent name collisions
  are now hard errors, and a stale `fallback` key in a V2
  `agents/<name>/agent.toml` is ignored while a V1 inline `[[agent]]`
  entry fails the pack's unknown-key gate. External packs that relied on
  the bundled fallback dog must define their own worker pool (or route
  work to a pool they ship themselves).

### Added

- **Early access: the public `gascity-packs` collection.** v1.3.0 ships the
  first early-access release of
  [`gascity-packs`](https://github.com/gastownhall/gascity-packs) — an opt-in
  collection of Gas City packs composed via `pack.toml` `[imports]`. Add one
  with e.g.
  `gc import add --name gc https://github.com/gastownhall/gascity-packs.git//gascity`.
  Featured packs:
  - **`gascity`** — planning & implementation workflow pack; bundled in the
    release and the default `gc init` template (also `--template gascity`).
  - **`gastown`** — multi-agent orchestration / default coding workflow pack;
    bundled and offered by `gc init` (`--template gastown`).
  - **`compound-engineering`** (`compound-build`) — Every Inc.'s Compound
    Engineering methodology as a build factory: brainstorm/plan → persona-panel
    plan review → implement → wide reviewer-persona fanout → resolution.
  - **`gstack`** (`gstack-build`) — garrytan/gstack founder-style sprint:
    office-hours intake → multi-perspective plan review → staff review → QA →
    security → release readiness.
  - **`superpowers`** (`superpowers-build`) — Jesse Vincent's Superpowers skill
    library as a build factory: brainstorm → written-spec approval → per-task
    TDD → spec-compliance then code-quality review.
  `gascity` and `gastown` are in the supported registry; the three
  build-methodology packs (`compound-engineering`, `gstack`, `superpowers`) are
  early access and each import `gascity` as `gc`. See the gascity-packs README
  for the full list and import instructions.

- **Formulas v2 and `drain` are the supported path for new graph
  workflows.** The v2 compiler emits flat workflow graphs with
  controller-owned control/finalize beads, and `drain` is now the canonical
  fan-out primitive for scattering convoy members into per-item formula runs.
  The bundled `gascity` planning pack ships graph.v2 build and implementation
  formulas, including the mayor skill's documented `gc sling ... --on
  <formula>` launch flow and drain-based `implement` workflow, so new Gas City
  methodology workflows no longer need the legacy `gc.output_json`/tally fan-out
  pattern.

- Proxy-process workspace services now receive `GC_SERVICE_SECRETS_DIR`
  (`<GC_SERVICE_STATE_ROOT>/secrets`) in their environment, alongside the
  existing `GC_SERVICE_*` variables. The directory is scaffolded at `0700`
  by the service state-root setup and is the sanctioned home for
  pack-managed credentials (bot tokens etc.), so pack services can rely on
  the explicit contract instead of deriving the path from
  `GC_SERVICE_STATE_ROOT`. See #3429.
- `gc nudge drain --inject` now prepends a one-line current-time stamp
  (operator-local + UTC + epoch) to its `UserPromptSubmit` hook output, giving
  agents a live clock in context every turn. The local zone follows the host
  (`time.Local`/`$TZ`) or the `GC_OPERATOR_TZ` override; disable with
  `GC_INJECT_CLOCK=0`. Folded into the existing nudge inject, so it adds zero
  extra hook subprocesses per turn. See #3036.
- The supervisor now merges a machine-local secrets file
  (`${GC_HOME}/secrets.env`, dotenv syntax) into the launchd plist / systemd
  unit environment on every service-file regeneration. This fixes provider
  credentials being dropped when `gc start` runs from a shell that did not
  export them (e.g. at login or after a reboot), which previously caused
  silent provider auth failures. Only keys already eligible for the supervisor
  environment are merged (provider credentials plus `GC_SUPERVISOR_ENV`
  opt-ins); a value exported in the calling shell still takes precedence, and
  `GC_SUPERVISOR_OMIT_PROVIDER_CREDS=1` suppresses provider credentials from
  both sources.
- `GC_DOLT_SYNC_PUSH_TIMEOUT_SECS` configures the SQL-mode push wall-clock
  ceiling for `gc dolt sync` (default 1800s, replacing the prior fixed 120s
  that SIGKILLed large first pushes). Metadata queries keep their own 120s
  bound.
- **ENOSPC pre-flight for managed Dolt** (`GC_DOLT_MIN_FREE_BYTES`,
  `GC_DOLT_WARN_FREE_BYTES`): managed-Dolt startup and the store-maintenance
  compaction loop now check container free space via `statvfs(2)` before
  performing disk-growing operations. Below the critical floor (default
  500 MiB) startup is refused and compaction is skipped; below the soft floor
  (default 2 GiB) a `gc.store.disk_warn` event is emitted and operations
  proceed. Fails open on probe error and is disabled entirely when
  `GC_DOLT_MIN_FREE_BYTES=0`. Uses `f_bavail` (APFS-safe — excludes purgeable
  space). Addresses the root trigger of the 2026-06-01 fleet-drain incident.

### Fixed

- The synthetic bundled-pack cache key now folds in the running binary's
  embedded-pack content hash, so two `gc` binaries with different bundled-pack
  content resolve to different cache directories instead of fighting over one.
  Previously the cache directory was keyed only on namespace+source+commit, so a
  version-skewed deploy (controller and agents on different `gc` builds) left
  both binaries materializing one shared directory in turn: each `gc import
  install` was promptly clobbered by the other binary, re-wedging every `gc bd`
  citywide with "bundled pack cache content hash does not match current binary"
  roughly hourly. With the content hash in the key, `gc import install` for a
  given binary sticks for that binary regardless of other versions running.
  Note: deploying a binary with changed bundled-pack content still requires a
  one-time `gc import install` (or bootstrap materialize) to populate the new
  cache directory; that install is now durable rather than transient (ga-s9p).

- Pool respawn after `gc runtime drain-ack` no longer waits up to a full patrol
  interval (default 60 s) before the replacement session starts. The async kill
  goroutine now pokes the controller once after the session is gone so Phase 2
  (finalize bead + spawn replacement) runs on the next event tick. Fixes the
  `TestLifecycle_DrainAckResponsiveRespawn/prequeued_respawn_2364` Tier B
  nightly regression (ga-ryhnhd, #2364, #2251).

- `gc dolt sync` now emits per-mode diagnostics on push failure instead of a
  generic "push failed": a TIMEOUT message naming the ceiling and
  `GC_DOLT_SYNC_PUSH_TIMEOUT_SECS` on exit 124, the underlying exit code on
  other failures, and the underlying dolt stderr. The replayed stderr cannot
  leak `GC_DOLT_PASSWORD`: the password reaches dolt via the `DOLT_CLI_PASSWORD`
  environment variable, never as an argv flag. `GC_DOLT_SYNC_PUSH_TIMEOUT_SECS`
  rejects every numeric-zero form (`0`, `00`, `000`, ...) -- not just the
  literal `0` -- because GNU `timeout` treats a zero duration as "disable the
  timeout", which would push unbounded. A failure to create the stderr-capture
  temp file now degrades to a per-database error rather than aborting the whole
  run.
- Interactive `gc session new` tmux sessions now scroll tmux scrollback on the
  mouse wheel instead of leaking the wheel to the focused TUI (Claude Code's own
  history, a pager, or the shell). The gastown pack binds `WheelUpPane`→copy-mode
  and `WheelDownPane`→passthrough, and the runtime resolves interactive sessions
  to mouse-on across every create seam so tmux preserves the `mouse on` set at
  session create: the `gc session new` CLI — both the managed-deferred reconciler
  start (`templateParamsToConfig`, for `session_origin=manual` sessions) and the
  unmanaged direct start (`workerSessionCreateHints`) — plus the API
  provider/named paths (`sessionCreateHints`). Resume keeps mouse-on too
  (`sessionResumeHints`), so the wheel survives suspend/restart. Headless agent
  sessions stay mouse-off (controller-poll safety) — they resolve `MouseOn` from
  the agent template path (`cfgAgent.MouseModeOn()`), which is unchanged and has
  neither the `manual`/`named` interactive marker. Replaces the portharbour
  po-vtg2 city-local `set-hook` stopgap with the in-source fix. Refs: ga-c4w.

### Troubleshooting (packs, imports, registry)

v1.3.0 changed pack composition: built-in/gastown packs are now consumed via
explicit pinned `[imports]` in `pack.toml` + `packs.lock`, served from a
content-hashed cache under `~/.gc/cache/repos/` (nothing is materialized into
`.gc/system/packs` anymore). Most upgrade issues are fixed by one command — run
it once per existing city after upgrading:

```
gc doctor --fix
```

It owns the mechanical migrations (`provider-catalog`, `builtin-pack-imports`,
`packv2-import-state`): it adds missing pinned imports, strips legacy
`workspace.includes` / `[packs]` surfaces, re-pins superseded canonical
versions, refreshes `packs.lock` + cache, and prunes leftover `.gc/system/packs`.

| Symptom | Cause | Fix |
| --- | --- | --- |
| `does not import required builtin pack(s) core; run "gc doctor --fix"` | City predates explicit `[imports]`. | `gc doctor --fix` |
| `workspace.includes is deprecated in v2; use [imports]` / `[packs] is deprecated` / `unsupported PackV1` | Legacy v1 composition surfaces. | `gc doctor --fix` (a fragment-authored `[packs]` may need a manual edit) |
| `remote import <src> is not installed (missing packs.lock); run "gc import install"` | Declared import lacks a lock pin, or its cache checkout is absent. | `gc import install` (diagnose with `gc import check` / `gc import status`) |
| `synthetic cache is invalid at <dir>: missing bundled pack cache marker` | Bundled synthetic cache present but invalid (an *absent* cache self-heals offline). | `gc import install` |
| `N bundled import(s) pinned at a superseded canonical version` | Stale `packs.lock` from an older `gc`. | `gc doctor --fix` (offline re-pin) |
| `durable import(s) use command-time registry selectors` | A `registry:` selector was written into `pack.toml`. | Manual edit — replace with the concrete source (`gc pack registry show <pack>`) |
| `gc start` prints `FATAL: pack schema 2 not supported` | A stale supervisor still on the old binary. | let `gc start` auto-restart, or `systemctl --user restart gascity-supervisor` |

**See also:** `docs/getting-started/troubleshooting.md`,
`docs/reference/system-packs.md`, `docs/guides/understanding-packs.md`,
`docs/guides/shareable-packs.md`, `docs/guides/registry-showcase.md`,
`docs/troubleshooting/gc-start-walkthrough.mdx`, and the `gc doctor` /
`gc import` / `gc pack registry` references in `docs/reference/cli.md`.

## [1.2.1] - 2026-05-31

### Fixed

- Built-in pack auto-includes now skip packs already reachable from rig pack
  graphs, preventing duplicate maintenance agents when a rig pack imports a
  built-in pack transitively.
- CI, docs, the managed minimum check, and install helpers now pin Dolt 2.1.0
  so hotfix validation and runtime dependency checks use the same Dolt floor.

## [1.2.0] - 2026-05-25

### Added

- Claude Opus 4.8 is now listed in built-in Claude model choices and default
  pricing. The `opus` model choice targets `claude-opus-4-8`; `opus-4-7`
  remains available for cities that need the prior Opus target. Anthropic's
  published regular-usage pricing is unchanged from Opus 4.7: $5/MTok input
  and $25/MTok output.
- `[daemon].dolt_start_address_in_use_retry_window` configures how long the
  managed dolt start path waits on the originally requested port when bind
  fails with "address already in use" before falling back to a higher port.
  Defaults to `30s`, which roughly covers half of Linux's default TCP
  TIME_WAIT and prevents an external `kill -TERM` / supervisor restart / OOM
  kill of the dolt subprocess from perpetuating a rebound orphan on a
  non-canonical port. Each port gets at most one wait per
  `startManagedDoltProcessWithOptions` invocation, so the worst-case wall
  time per startup is bounded by `(retry_window + per-attempt-startup) ×
  min(5, distinct-ports-tried)` rather than `retry_window × 5`. Set to `0s`
  to disable the retry (legacy fall-back-immediately behavior). Operators
  with a recovery-latency monitor may want to raise their alert threshold
  by ~30s to absorb the new wait under contended port conditions; the
  worst-case per startup remains well under one minute at defaults.
  During a same-port retry the managed-dolt state file briefly reports
  `Running:false, PID:0` for up to `retry_window` while the wait elapses;
  state-readers (`gc dolt-state status`, rig endpoint port projection,
  order routing) treat this as transient not-running and recover on the
  next successful bind. The provider-op timeout for `start` remains `120s`;
  an operator who raises `dolt_start_address_in_use_retry_window` materially
  above the default should also raise that timeout to keep headroom for the
  5-attempt cap.
- `[daemon].dolt_stop_timeout` typos are now caught by `ValidateDurations`
  at config load (previously only `ValidateNonNegativeDurations` covered it,
  so an invalid string like `"30sec"` silently collapsed to zero).

### Fixed

- `gc mail reply` and `gc handoff` now store created mail in the wisp tier,
  matching `gc mail send`. Operators should use `gc mail` commands or
  explicit both-tier/wisp-aware bead queries for mail visibility; default
  issue-tier `bd list` output and git sync do not include wisp-tier messages.
- Built-in pack auto-include graph traversal now avoids redundant pack reads
  while preserving non-transitive import boundaries and later transitive
  expansion of shallow-seen packs.

## [1.2.0] - 2026-05-25

### Added

- `gc mail inbox`, `gc mail read`, `gc mail peek`, `gc mail thread`,
  and `gc mail count` now accept `--json` and emit schema-versioned result
  envelopes for script and dashboard consumers. `gc mail inbox --json` and
  `gc mail count --json` always include the resolved `recipients` array,
  including single-recipient targets.
- Native `bd` store selection now links the upstream Beads/Dolt Go library
  stack into `gc` when the default beads provider is built. This intentionally
  increases binary size and supply-chain surface through the Dolt/Vitess and
  cloud-provider SDK dependency closure; deployments that do not want that
  path can keep using `GC_BEADS_FORCE_FALLBACK=1` or `GC_BEADS=file`. CI now
  runs `make check-native-dependency-surface` to fail on unreviewed native
  dependency-family growth or `gc` binary-size growth.

### Fixed

- `gc runtime drain-ack` now pokes the city controller socket after setting
  the drain-ack flag, so the reconciler stops and respawns a drained pool
  worker on the current patrol tick instead of waiting up to four ticks
  (~120 s/step → ~30–90 s/step). Closes #2364 (pre-queued work) and #2251
  (cold-pool arrival after drain-ack), which shared the same missing-poke
  root cause.
- `gc --json-schema` manifest output no longer includes the removed
  `transport` field. Consumers should use each role schema's `x-gc-jsonl`
  extension, when present, to determine JSONL record-count behavior.
- `gc session attach` now re-applies `session_live` hooks (status-bar theme,
  keybindings) when it recreates a session whose tmux runtime had exited.
  Previously the resume path in `resolvedWorkerRuntimeWithConfigAndMetadata`
  built the runtime `Hints` without `SessionLive`, so `runSessionLive`
  early-returned on the empty list and attach-recreated sessions came up
  unthemed while reconciler-started sessions did not. The setup context is
  built via the reconciler's own `sessionSetupContextForAgent` so
  `session_live` templates referencing `{{.Rig}}`/`{{.RigRoot}}`/`{{.AgentBase}}`
  expand correctly on the resume path.
- Managed bd provider startup now detects a bd-standalone dolt server running
  against the same `.beads/dolt` database before invoking the managed-bd
  lifecycle script, and refuses with a message naming `bd dolt stop` as the
  unblock. This covers `gc start`, `gc init`, and `gc rig add` provider
  convergence paths. Previously, running `bd dolt start` while a city was
  registered at the same path would leave the standalone dolt holding the
  exclusive write lock; the city-managed dolt could not acquire it and startup
  failed with a generic "dolt server could not start via gc helper" error that
  did not point at the lock holder. Stale `.beads/dolt-server.pid` files and
  live PIDs that do not look like `dolt sql-server` are ignored so leftover
  files and PID reuse do not block startup.
- Default bead-backed pool-demand counts now use the same routed target
  resolution as worker claim queries and exclude epic-routed beads, matching
  the default worker `work_query` behavior. Custom `scale_check` overrides are
  unchanged.
- Empty JSON result collections for `gc mail thread`, `gc trace status`, and
  `gc trace show` now encode as `[]` instead of `null`; `gc trace show` also
  reports a concise no-records message in the default text mode.
- `events.FileRecorder.Record` no longer blocks indefinitely on `flock` when
  a prior `gc event emit` process died holding the lock. Acquisition now
  uses non-blocking `LOCK_EX|LOCK_NB` retried at a 5 ms cadence for up to
  250 ms total, then logs `events: lock: timed out after 250ms waiting on
  flock at <path>` to stderr and returns without recording. The deferred
  `LOCK_UN` still runs after a successful acquire; the happy path and
  non-`EWOULDBLOCK` flock-error path are unchanged. Operators previously
  saw hundreds of stuck `gc event emit` processes after a `SIGKILL` of the
  holder; the new bounded wait drops the stuck event recorder instead of
  stacking processes.
- Kiro provider launch behavior is now explicit in release notes and provider
  docs: the built-in Kiro provider starts `kiro-cli` with `chat`,
  `--no-interactive`, `--agent gascity`, and `--trust-all-tools` by default.
  Operators who do not want unrestricted tool trust can replace the full
  default argv with an explicit `[providers.kiro].args` list in `city.toml`.
- Tmux and runtime provider-overlay staging now surface nonfatal preservation
  warnings on stderr, including the Kiro `AGENTS.md` preservation notice when
  project instructions already exist.
- `jsonl-export.sh` no longer mis-classifies a bead database with an empty
  `issues` table as a failed export. `dolt sql -r json` returns `{}` (not
  `{"rows":[]}`) when a queried table is empty; `validate_exported_issues` now
  treats the bare-object form as zero rows so the database lands in the
  success path with an `issues.jsonl` committed to the archive instead of
  appearing in the `failed:` summary.
- The built-in `control-dispatcher` trace now defaults to
  `${GC_CITY_RUNTIME_DIR}/control-dispatcher-trace.log` (falling back to
  `${GC_CITY}/.gc/runtime/control-dispatcher-trace.log`) instead of writing at
  city root. This keeps workflow-trace appends inside the controller's
  watcher-excluded runtime subtree, avoiding continuous `config-changed`
  reconciliations. After upgrading, operators tailing the default trace should
  switch to `.gc/runtime/control-dispatcher-trace.log`; the old
  `${GC_CITY}/control-dispatcher-trace.log` file becomes stale and can be
  removed. After upgrading, restart or recycle existing `control-dispatcher`
  sessions so they pick up the new trace path; otherwise they keep their
  previous trace target and can continue retriggering reconciles. Validation
  currently covers watcher exclusion, dispatcher warning routing, and the
  graph-workflow integration shard; there is not yet a dedicated patrol-cadence
  stress test.
- `proxy_process` services now receive a `GC_SERVICE_URL_PREFIX` that the
  supervisor's public listener actually routes. Previously the prefix was
  the per-city-relative `/svc/<name>`, so any service that composed
  `CallbackURL = $GC_API_BASE_URL + $GC_SERVICE_URL_PREFIX` (the documented
  shape for adapter self-registration) would 404 on inbound calls. The
  prefix is now the full `/v0/city/<cityName>/svc/<svcName>` path. The
  per-city router contract (`config.Service.MountPathOrDefault`) is
  unchanged.
- `gc session reset` now documents its named-session circuit-breaker behavior:
  when the target is a named session, reset clears a tripped respawn breaker
  before requesting a fresh restart.

### Changed

- `gc converge status --json` returns the convergence metadata object with
  `ok: true` injected. `gc converge list --json` returns an object with
  `ok: true` and `entries`. These converge JSON outputs do not include a
  `schema_version` field.
- `gc runtime drain-check --json` now emits a JSON result when the target
  session is not draining, with `ok: true`, `draining: false`, and the
  existing shell-condition exit code of 1.
- `gc sling --json` now emits one JSONL result record, matching its checked-in
  result schema; earlier JSON support emitted an indented multi-line object.
- `gc trace status` and `gc trace show` now default to human-readable output;
  scripts that need machine-readable trace data should pass `--json`. The
  `--json` result shapes are also envelope objects now: `gc trace status
  --json` uses `active_arms` instead of `arms` and includes
  `schema_version`, `as_of`, `controller_running`, and `controller_pid`;
  `gc trace show --json` returns `schema_version`, `city_path`, `count`, and
  `records` instead of a bare record array. See
  `schemas/trace/status/result.schema.json` and
  `schemas/trace/show/result.schema.json` for the exact contracts.
  During rolling upgrades, trace controller socket status replies include the
  legacy `arms` alias and upgraded CLIs still accept `arms` from older
  controllers.
- Pack import cache validation now requires commit abbreviations in
  `packs.lock` to be at least seven characters long. Shorter abbreviations
  should be refreshed with `gc import install`.
- City discovery now treats a `city.toml` at `$HOME` or an explicit
  `GC_CEILING_DIRECTORIES` entry as a valid city. The ceiling directory is
  searched but never crossed, so existing stray `$HOME/city.toml` files may now
  be discovered from subdirectories where they were previously ignored.
- `gc import migrate` is now a hidden, deprecated guidance shim that exits
  non-zero after pointing operators to `gc doctor` and `gc doctor --fix`.
  Update any scripts that treated `gc import migrate` as a successful
  compatibility migration step.
- `gc rig add --include` now writes canonical `rig.Imports`, which are
  processed alphabetically by binding rather than in legacy declaration order.
- `examples/swarm` now inherits the system-maintenance `dog` agent, so the
  example city has the same fallback agent as other maintenance-enabled
  cities.
- ACP, subprocess, and Kubernetes session staging now apply pack and agent
  overlays through the provider-aware `per-provider/<provider>/` contract.
  Custom ACP overlays that previously expected a literal `per-provider/`
  subtree in the session workdir should move provider-specific files under the
  matching provider slot so they are flattened at launch.
- The review-quorum durable contract now documents that synthesized
  `findings_count` is deduplicated, top-level `mutations_delta` is reserved for
  synthesis-created changes, lane mutation deltas remain under their lane
  records, lane-scoped finalizer failures use
  `lane=<lane_id> reason=<stable_reason>` entries, and unknown lane verdict
  values are hard contract failures. Reviewer lane prompts now require durable
  `lane_id`, `provider`, and `model` fields, and the finalizer rejects blank
  lane IDs without merging contract-invalid lane findings, evidence, or usage
  into the synthesized summary.
- `[[orders.overrides]]` rig matching is stricter and clearer. A rigless
  override (`rig` unset) still matches **only** city-level orders; if the
  named order exists only as per-rig instances, the error now names every
  matching rig so it's obvious what to type. `rig = "*"` is a new wildcard
  that targets every instance of the named order (city-level + per-rig).
  The literal `"*"` is reserved and rejected as a real rig name by config
  validation.
- Managed Dolt config now emits listener backlog and connection-timeout keys.
  Existing managed cities may see a `dolt-config` doctor warning until
  `gc dolt restart` or the next managed server start regenerates
  `dolt-config.yaml`.
- In bead-backed pool reconciliation, `scale_check` output is now documented
  and enforced as additive new-session demand. Assigned work is resumed
  separately; custom checks that previously returned total desired sessions
  should return only new unassigned demand.
- Session bead reconciliation now stops suspended and orphaned runtimes before
  closing their beads; resuming one of those sessions starts a fresh lifecycle
  instead of continuing the previous runtime process.
- `gc hook --inject` is now silent legacy compatibility for already-installed
  Stop/session-end hooks. Fresh managed hook configs no longer install it;
  routed work pickup should happen through the SessionStart claim protocol or
  an explicit non-inject `gc hook` call.
- The built-in Claude provider's `model = "opus"` option now emits
  `claude-opus-4-7`. Cities that rely on the `opus` alias should expect the
  new model target after upgrading.

### Fixed

- Linux systemd supervisor service restarts now preserve managed tmux sessions
  for re-adoption. Linux users should rerun `gc supervisor install` after
  upgrading so the user unit is regenerated with `KillMode=process` and the
  preserve-on-signal environment. If the currently active Linux supervisor
  predates the preserve-on-signal environment, `gc supervisor install` now
  refuses the warm refresh before sending a signal and tells operators to stop
  or drain agents intentionally with `gc supervisor stop --wait`, then rerun the
  install. Once the active supervisor already supports preserve mode, Linux warm
  refresh sends the main supervisor PID `SIGTERM` first so preserve-mode
  shutdown can close workspace services and flush traces, with a bounded
  `SIGKILL` fallback if the process does not exit. The Linux refresh also stops
  orphan-prone workspace service process groups owned by registered cities
  before starting the replacement supervisor; supervisor startup repeats the
  same owned-service cleanup after crashes. Service-managed `SIGTERM` preserves
  sessions for re-adoption, while `SIGINT` remains a destructive escalation
  path. Preserve mode intentionally leaves the beads provider running so
  preserved sessions can keep using the store; the bundled managed-Dolt start
  path is idempotent when it finds an already-running server, but custom exec
  providers must make `start` reattach or no-op safely after preserve-mode
  restarts. macOS launchd upgrades still use launchd unload/load rather than the
  Linux main-PID refresh path; macOS supervisor startup now warns that automatic
  orphaned workspace-service cleanup is Linux-only, lists the registered
  `GC_SERVICE_STATE_ROOT` roots to inspect, and tells operators to stop stale
  workspace-service processes before restarting affected cities after
  non-graceful exits.

## [1.0.0] - 2026-04-21

First stable release. Between `v0.15.1` and `v1.0.0` the project received 610
commits across 1,273 files (+303,902 / −46,437) from the core team and 12
community contributors. See the GitHub release page for the full narrative.

### Added

- `gc reload [path]` — structured live config reload. Failures keep the previous
  runtime config active instead of silently degrading.
- `gc prime --strict` — turns silent prompt/agent fallback paths into explicit
  CLI failures for debugging.
- `rig adopt` — adopt existing rigs without a full rebuild.
- Provider-native MCP projection for Claude, Codex, and Gemini, with multi-layer
  catalog resolution and projected-only `gc mcp list`.
- Per-agent `append_fragments` so prompt layering is configurable through the
  supported config and migration paths.
- Wave 1 pass over orders and dispatch runtime — store resolution, dispatch
  surfaces, rig-aware execution, and verifier coverage.

### Changed

- **Session model unified.** Declarative `[[agent]]` policy/config is now
  cleanly separated from runtime session identity; session beads are the
  canonical runtime projection.
- **Pack V2 is the active layout.** Bundled packs use `[imports.<name>]`;
  builtin formulas, prompts, hooks, and orders come from the builtin `core`
  pack. V1-era city-local seeding is retired.
- `gc init` is back on the pack-first scaffold contract. Agent and named
  sessions belong in `pack.toml`; machine-local identity stays in
  `.gc/site.toml`; `city.toml` keeps workspace/provider state.
- `gc import install` is now the explicit bootstrap path for importable packs.
- `gc session logs --tail N` returns the last `N` entries (matches Unix `tail`
  convention) instead of the old compaction-oriented behavior.
- Supervisor API migrated to Huma/OpenAPI; Go client regenerated; dashboard SPA
  restored.
- Order "gates" renamed to **triggers**.

### Fixed

- Startup proofs for hook-enabled providers — correct startup prompt delivery,
  no duplicate `SessionStart` hook context, no replay of startup prompts on
  resumed sessions.
- Managed Dolt hardening: recovery, transient failures, health probes,
  runtime-state validation, and late-cycle macOS portability fixes (start-lock
  FD inheritance, path canonicalization, `lsof` reachability, PID confirmation,
  portable `sed` parsing).
- Pack V2 tmux startup regression where large prompt launches could silently
  fall back to the known-broken inline path.
- Custom provider option defaults now fail early instead of silently degrading.
- Beads storage core quality pass — cache recovery, close-all fallback
  semantics, watchdog reconciliation cadence, dirty-cache fallback reads.
- Long tail of session lifecycle, wake-budget, and pool identity fixes.

[Unreleased]: https://github.com/gastownhall/gascity/compare/v1.4.0...HEAD
[1.4.0]: https://github.com/gastownhall/gascity/releases/tag/v1.4.0
[1.3.0]: https://github.com/gastownhall/gascity/compare/v1.2.1...v1.3.0
[1.2.1]: https://github.com/gastownhall/gascity/compare/v1.2.0...v1.2.1
[1.2.0]: https://github.com/gastownhall/gascity/releases/tag/v1.2.0
[1.0.0]: https://github.com/gastownhall/gascity/releases/tag/v1.0.0
