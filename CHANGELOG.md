# Changelog

All notable changes to Gas City will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `gc mail inbox`, `gc mail read`, `gc mail peek`, `gc mail thread`,
  and `gc mail count` now accept `--json` and emit schema-versioned result
  envelopes for script and dashboard consumers. `gc mail inbox --json` and
  `gc mail count --json` always include the resolved `recipients` array,
  including single-recipient targets.

### Fixed

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

[Unreleased]: https://github.com/gastownhall/gascity/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/gastownhall/gascity/releases/tag/v1.0.0
