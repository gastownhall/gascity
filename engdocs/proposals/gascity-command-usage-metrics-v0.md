---
plan_slug: gascity-command-usage-metrics-v0
phase: design
rig: gascity
rig_root: /data/projects/gascity
artifact_root: /data/projects/gascity/engdocs/proposals
requirements_file: null
requirements_source: "Direct maintainer request, 2026-07-10"
status: draft
created_at: 2026-07-11T00:12:23Z
updated_at: 2026-07-14T00:00:00Z
implementation_status: stage-1a-client-core
---

# Design: Privacy-Limited Gas City Command Usage Metrics

> **Stage 1a core scope (maintainer update, 2026-07-14).** The generic
> go/packages/SSA self-exec analyzer and asset-by-asset/service-manager
> hardening described as prospective enforcement below are not required for
> the endpoint-empty client PR. Recursive suppression is enforced with the
> GC-only `internal/execenv` helper, managed-agent environment pinning, and
> focused child-process/content tests. Activation gates and the privacy, wire,
> opt-out, queue, and backend contracts are unchanged.

## Summary

Gas City should record one product-usage event for each eligible top-level
`gc` invocation, backed by a durable local queue and asynchronous first-party
delivery. Official releases are enabled by default only after a one-time,
human-visible notice. Users can inspect the exact wire format and deterministic
encoded example, then opt out with a single city-independent command.

Beads is a reference for the successful user experience and queueing pattern,
not part of this design's implementation scope. Gas City owns its own consent,
`app=gascity` event namespace, command classification, queue, endpoint
contract, and reports. This proposal does not change, disable, or otherwise
manage Beads telemetry.

The design deliberately hardens several edges before default-on rollout:

| Area | Decision |
|---|---|
| Coverage | One centralized wrapper covers the constructed Cobra tree; no per-handler metrics calls. |
| Identity | Random, resettable installation UUID committed atomically with notice acceptance; never derive it from the OS machine ID. |
| Command value | Finite canonical executable-leaf ID; all user-defined pack commands collapse to `pack-command`. |
| Payload | Command ID, official release version, GOOS, UTC hour, event ID, and installation ID only. |
| Explicit omissions | No args, flags, paths, names, content, output, error text, exact time, duration, outcome, model, tokens, or cost. |
| Activation | First eligible non-automation TTY invocation fully writes the notice, then atomically commits notice acceptance plus ID; collection starts with the next invocation. |
| Opt-out | `gc metrics off` is linearizable: it durably disables, blocks re-enable during cleanup, proves uploader quiescence, purges all generations, and removes the ID before reporting success. |
| Storage | Owner-private, root-global queue capped by count, bytes, age, and bounded cleanup work, using a new fd-relative durable-storage adapter rather than existing path-based helpers. |
| Upload | Detached, attempt-bound, throttled single-uploader process on supported official platforms, with a closed production constructor, direct strict HTTPS, typed acknowledgements, and a signed kill-switch response. |
| Architecture | New `internal/productmetrics` package, separate from operational events/export, OTel, and local usage-cost facts. |
| Backend | Gas City-owned ingestion and app-separated storage/reporting; deletion requires an exact full-batch acknowledgement after durable HMAC-and-discard capture. |
| Rollout | Ship controls default-off first; default-on waits for a release-mode manifest, backend evidence, canary, and kill-switch gates. |

This is best described as **privacy-limited, pseudonymous usage metrics**, not
anonymous telemetry. The HTTPS receiver necessarily observes the source IP
while serving a request, even though the client does not put an IP address in
the event.

## Goals

1. Answer a deliberately small set of product questions:
   - Which canonical built-in `gc` executable commands are used?
   - Which released Gas City versions and operating systems are active?
   - How many pseudonymous Gas City installations are active by day or month?
2. Make coverage structural so a newly added command cannot silently omit
   instrumentation.
3. Give users a clear, inspectable, reversible, user-global control.
4. Bound foreground latency, disk, processes, retries, retention, and network
   work.
5. Prove that arguments and user-authored names never enter the spool or the
   final HTTP request.
6. Keep product metrics independent from Gas City's other observation systems.

## Non-goals

- This is not `internal/events`. It must not create an operational event type,
  write to `.gc/events.jsonl`, appear in `gc events`, or drive an order.
- It is not `cmd/gc/event_export.go`/`pkg/eventexport`. Product metrics never
  enter the configured redacted operational-event export or its cursor.
- This is not `internal/telemetry`. It must not share the operator-controlled
  `GC_OTEL_*` / `OTEL_*` activation or export path.
- This is not `internal/usage`, `.gc/usage.jsonl`, or `gc costs`. No run,
  session, model, token, or cost fact leaves the local usage subsystem.
- This is not audit, security, billing, licensing, entitlement, or abuse
  enforcement data. The unauthenticated client payload is fabricable.
- It does not report API requests, supervisor reconciliation, agent activity,
  prompts, work results, or pack-specific behavior.
- It does not alter or coordinate Beads telemetry.
- It does not create a general analytics SDK or a public extension API.

## Threat model and supported platforms

The privacy and concurrency guarantees apply to unmodified first-party Gas
City processes running under an honest same-user environment. Product metrics
defends against accidental capture, malformed local state, crashes, ordinary
concurrency, hostile network responses, and untrusted event contents. It does
not defend against a malicious same-UID process or executable pack code: such
code can read or replace user-owned files, ignore advisory locks, inspect the
local ID, alter process environments, or run a modified `gc` binary. Packs that
execute code therefore share the user's local trust boundary. "One event per
invocation" and uploader-exclusion claims below mean one event among
cooperating first-party processes, not enforcement against hostile local code.

Gas City's official v0 release targets are Linux and Darwin. Product metrics
implements and tests fd-relative no-follow storage, advisory locking, durable
directory operations, and detached upload on those platforms. Builds for
Windows or any other unsupported platform compile a stub that is always
`fail-closed`: no notice acceptance, ID, queue, spawn, or network operation.
Full Windows reparse-safe storage, locking, deletion, and detachment would
require a separately reviewed design and is not a v0 promise.

## Current System

### Reference behavior

The Beads work visible in tmux window 25 demonstrates the desired product
shape: a database-independent metrics control command, a friendly one-time
notice, disk-before-network queueing, a detached uploader, stable event IDs,
first-party storage, Grafana reporting, retention, and backup.

The Gas City proposal keeps that product shape while avoiding implementation
choices that should not be repeated:

- manual instrumentation distributed across command handlers;
- a stable non-resettable identifier derived from the OS machine ID;
- collection before a suppressed notice has ever been shown;
- a user example that is not the final network body;
- opt-out that leaves queued data or an already-enabled process able to flush;
- an unbounded queue and one detached spawn attempt per command;
- raw user-defined command labels.

No Beads source or preference is changed by this proposal.

### Gas City's existing observation systems

Gas City currently has four nearby but incompatible observation/egress
systems:

1. `internal/events` contains contentful per-city operational events. They are
   persisted, streamed, user-visible, and may drive orchestration.
2. `internal/telemetry` emits operator opt-in OTel metrics and logs with rich
   operational dimensions.
3. `internal/usage` stores local run/session/model/token/cost facts for
   `gc costs`.
4. `cmd/gc/event_export.go` plus `internal/eventfeed` and `pkg/eventexport`
   form an operator-configured redacted export rail over operational events.
   It can carry actor/run/session correlation and is enabled by machine-wide
   `[events.export]` configuration in
   `$GC_HOME/supervisor.toml`.

Product command counts have a different consent, minimization, persistence,
retention, and egress contract. Neither the event bus nor the redacted exporter
can satisfy the closed command-only schema, user-global notice gate, bounded
private spool, or `gc metrics off` contract. Mixing them would make
`gc metrics off` ambiguous. The new package is therefore a fifth, narrow
boundary; this does not weaken the repository rule that operational domain
activity uses the event bus.

### Central CLI seam

`cmd/gc/main.go:run` already owns root construction, argument injection, JSON
schema/contract early paths, Cobra execution, output buffering, and final exit
selection. It currently calls `Execute`; this design changes that call to
`ExecuteC` so the lifecycle can retain the resolved command. `newRootCmd`
registers every built-in command and then adds pack-discovered commands.
`cmd/gc/json_schema.go:commandPathWords` is the existing canonical-path helper
and `cmd/gc/cmd_commands.go:docgenSkipAnnotation` currently marks discovered
namespace roots; the metrics classifier reuses those seams while adding its
own annotation to every discovered intermediate/leaf rather than inventing a
second argv parser.

That provides a central coverage seam:

1. Snapshot and annotate the finite built-in tree before pack registration.
2. Mark every later discovered namespace/leaf as `pack-command`.
3. Recursively wrap the constructed tree once.
4. Use one per-invocation `RecordOnce` guard.
5. Cover pre-Run and early-return paths around `ExecuteC`.

The eager and lazy discovered-command paths currently call `os.Exit` in
`cmd/gc/cmd_commands.go` and become typed `exitForCode` errors.
`cmd/gc/providers.go:newSessionProviderFromContext` also has a normal-path
`os.Exit(1)`; callers switch to/propagate the existing
`newSessionProviderFromContextWithError` seam (or an equivalent
`(runtime.Provider, error)` result). Thus the central lifecycle remains
testable and cleanup is not bypassed.

### Backend prerequisite

The window-25 ClickHouse raw table contains an `app_name` column, but the
existing Beads rollups do not group by it, and the public tee has Beads-specific
forwarding and acknowledgement behavior. Sending Gas City traffic to that
route unchanged would conflate products and could acknowledge the client
before owned capture is durable.

The preferred Gas City path is a dedicated ingestion route and Gas City table.
A shared CLI endpoint is acceptable only if every raw table, rollup, dashboard,
alert, retention job, backup, restore, and forwarding decision is separated by
required `app`. Default-on client traffic is blocked until one of those
contracts exists.

## User Contract

### States and precedence

| Effective state | Behavior |
|---|---|
| `pending-notice` | Default for an official release with no preference. No ID, queue, event, or upload. |
| `notice-update-required` | A prior notice is stale. The ID may be retained, but there is no active spool generation, enqueue, or upload until the revised notice is accepted. |
| `enabled` | Eligible invocations enqueue and upload. |
| `disabled` | Persisted opt-out. No ID, enqueue, or upload. |
| `disabled-cleanup-pending` | Opt-out is durable but uploader quiescence or purge is not yet proven. No enable, enqueue, or new upload may start. |
| `environment-disabled` | `GC_DISABLE_USAGE_METRICS` or `DO_NOT_TRACK` disables this process only, using the truthiness rules below. |
| `fail-closed` | Invalid config, unstable home, unsupported build, permissions failure, or missing endpoint disables collection. |
| `server-paused` | The endpoint returned a valid signed pause-through-epoch envelope. Covered generations are immediately non-uploadable and purged (or reported cleanup-pending); accepted notice and the local ID are retained. |

`config.toml` stores a persisted preference (`unset`, `enabled`, or
`disabled`), the accepted notice version, an optional installation ID, a
monotonic generation, required-notice floor, typed cleanup state, and scalar
`paused_through_metrics_epoch`. Effective state is derived from that one
committed record plus process/build inputs; signed pause applies
`max(existing, signed_epoch)`. Release strings are audit metadata, not a
control key, and `server-paused` is not an opt-out.

Disable precedence is monotonic for one invocation:

1. unsupported development/test/CI build, missing production endpoint, or
   unstable/unreadable home;
2. truthy `DO_NOT_TRACK`;
3. truthy `GC_DISABLE_USAGE_METRICS`;
4. persisted disabled or cleanup pending;
5. pending or stale notice;
6. a signed pause marker covering the current metrics epoch;
7. persisted enabled state.

`GC_DISABLE_USAGE_METRICS` uses the explicit truthy set `1`, `true`, `yes`,
and `on`. `DO_NOT_TRACK` follows the broader DNT convention: any non-empty
value except `0`, `false`, `no`, and `off` opts out. A value such as
`GC_DISABLE_USAGE_METRICS=0` or `DO_NOT_TRACK=0` never forces collection on and
never overrides a saved opt-out. There is no environment force-enable.

Official releases are default-on in this precise sense: the first eligible
human TTY invocation may advance `pending-notice` to `enabled` only after the
entire notice is written and the notice/ID transaction commits. That invocation
is never recorded; collection begins with the following eligible invocation.
An invocation is not human-eligible when it runs inside a known Gas City
managed agent/session/runtime context (`GC_SESSION_ID`, `GC_SESSION_NAME`,
`GC_AGENT`, `GC_TEMPLATE`, `GC_MANAGED_SESSION_HOOK`, `GC_HOOK_EVENT_NAME`,
or `BEADS_ACTOR` set by a managed agent), even if stderr is a TTY. Managed
runtimes also set `GC_DISABLE_USAGE_METRICS=1` on child environments as defense
in depth, but classification tests enforce marker-based exclusion even when
that variable is accidentally absent.

Unversioned, development, test, and CI builds default to fail-closed and cannot
use the production endpoint. Tests enable an injected service against an
`httptest` server.

### First-run notice

The first eligible interactive invocation prints this plain-text notice to TTY
stderr:

> Gas City command usage metrics will start after this notice is saved.
> Sent: fixed `app=gascity` and schema version, canonical Gas City command
> (never arguments), release version, OS, UTC hour, a new cryptographically
> random event ID for this command, and one cryptographically random
> installation ID that remains stable until you reset it.
> Not sent: paths, city/rig/agent/bead names, prompts, output, error text, or
> credentials. The HTTPS receiver sees the source IP in access logs retained
> for at most 7 days; it is not copied into metrics. Raw event retention is
> 90 days. Aggregated pseudonymous counts linked by a
> hashed installation ID are retained for 13 months.
> Run `gc metrics off` to stop future collection and delete the local ID plus
> unsent local events. It makes no server request and does not delete accepted
> uploads. Before opting out, save the ID with
> `gc metrics status --show-installation-id` if you may request targeted
> deletion. Privacy and deletion contact: `<compiled privacy URL>`.
> Run `gc metrics example` to inspect the exact request.

While holding `state.lock`, the process reloads state and prints only if the
notice is still pending/stale. "Prints" means a complete successful write of
the fixed notice bytes to the intended TTY; a short or failed write commits
nothing, creates no final ID, records nothing, and may produce the notice again
on a later eligible invocation. After a successful write, one atomic
`config.toml` replacement commits `preference=enabled`, the notice version, a
cryptographically random installation UUID, a new state generation, and a
random local spool generation together. A failed commit leaves the prior
record intact and no final installation-ID artifact exists when the atomic
replacement was not applied. The storage boundary distinguishes that outcome
from an applied replacement whose parent-directory sync reported failure. If
the latter reads back byte-for-byte as the complete new record, it is the
logical activation point: the process retries the directory sync, reports
activation rather than an ordinary failure, and still excludes the activating
invocation. A crash may conservatively lose that opt-in, but cannot expose a
partial record or a separately authoritative ID. This applied-but-sync-pending
rule is limited to notice acceptance and greater-epoch resume. Durable disable,
signed pause, and cleanup completion do not report success until their sync is
proven; their visible applied state is already fail-closed while retry remains
required.

Each process snapshots recording eligibility before waiting on `state.lock`.
That per-invocation snapshot is sticky: a pending invocation remains
unrecordable even if another process enables metrics while it is waiting.
After acquiring the lock it reloads state and suppresses the notice if another
process already committed it. Thus two concurrent first invocations produce at
most one complete notice, one accepted transition, one ID, and zero events.

A notice-version bump is mandatory when fields, receiver behavior, retention,
or policy materially changes. An existing enabled record with an old notice
is atomically invalidated before any permit is issued: the process persists the
compiled monotonic `required_notice_version`, increments state generation,
removes the active spool generation, and enters `notice-update-required`. It
may retain its ID but cannot enqueue or upload. Every v0+ loader compares the
persisted floor to its compiled maximum and fails closed if the floor is newer,
so an older binary cannot resume the superseded generation. Re-acceptance CASes
that exact state, creates a fresh spool generation, and preserves the ID unless
the notice explicitly changes identity/retention semantics. Automatic and
explicit stale-notice acceptance are two-phase: they first durably raise the
floor and clear the old spool, then reread the exact installed record before
notice output, entropy, or acceptance. A short/failed notice write, entropy
failure, or not-applied acceptance replacement therefore leaves the old
generation inactive. Superseded
generations are cleanup-only and can never upload. Headless installations
remain paused until an eligible TTY invocation shows the revised notice.
If the generation or cleanup epoch is terminal-adjacent, invalidation instead
durably advances the counter namespace, resets the numeric counters, preserves
the ID and cleanup kind, raises the notice floor, and clears the active spool.
Any cleanup owner is reissued in the new namespace, so the old owner cannot
clear the new barrier and the new owner remains completable.

Notice invalidation retains the exact record observed before its lock attempt.
If another valid mutation replaces that record first, the invalidator does not
unlock into a resume window: while holding the same `state.lock`, it raises any
still-older floor on the currently named enabled record and clears its spool.
An equal floor is already converged and preserves a peer's newly accepted
spool; a newer floor fails closed.

The notice is neither printed nor treated as delivered for non-TTY output or
any machine/protocol/service context. The closed census includes:

- version output and user-facing or hidden completion;
- JSON, JSONL, JSON schema, JSON contract, and `--format json` output,
  including JSONL-by-default `gc events`;
- `event emit`;
- `hook`, provider hook environments, `prime --hook`,
  `handoff --auto`, and provider/hook-formatted output;
- `bd` and `beads ... --format` passthrough;
- `supervisor run` and other service-process entrypoints;
- `perf`;
- discovered pack commands;
- Git credential-helper, every hidden command without a reviewed exception,
  private bridge/watchdog/uploader modes, and `metrics` itself.

Before activation all of those remain uncollected. After activation, only
entries declared "recordable after activation" by the census may record
silently: version, user-facing completion generation, JSON/schema inspection,
`bd`, `beads`, `events`, `supervisor run`, the outer `perf` command, and the
outer `pack-command`. These are recording-excluded in every state:

- any managed-agent/provider-hook marker context;
- `event emit`, `hook`, `prime --hook`, `handoff --auto`, and hook/provider
  format modes;
- Git credential-helper, Cobra's hidden completion RPC, hidden commands without
  an explicit reviewed recordable exception, and all private process modes;
- `metrics` control commands.

Every discovered pack command sets `GC_DISABLE_USAGE_METRICS=1` for child
`gc` processes, including the lazy fallback, so the parent yields at most one
`pack-command` event. This does not set Beads telemetry variables or otherwise
alter telemetry of `bd` or any non-`gc` child. Metrics never write to ordinary
command stdout or stderr after activation.

Notice exclusion detection runs before `ExecuteC` and uses a closed matrix:

| Category | Pre-`ExecuteC` detection |
|---|---|
| Non-TTY or managed-agent invocation | `isatty(stderr)` false, or managed-agent env marker present. |
| JSON/JSONL/schema/contract output | Root early handlers plus literal JSON/schema/contract flags recognized by the existing pre-scan helpers; commands with JSON-by-default must register an exclusion annotation. |
| Hidden completion protocol | Cobra completion RPC command path such as `__complete`; user-facing `gc completion <shell>` is excluded from notice delivery but remains recordable after activation. |
| Hooks/provider output | Command path `gc hook ...`, provider hook env markers, `prime --hook`, `handoff --auto`/hook formats, or `gc hook run -- <gc args>` wrapper and child. |
| Other machine/service output | Explicit annotations and narrow pre-scan for version, `event emit`, `bd`/`beads`, `events`, `supervisor run`, `perf`, and discovered pack commands. |
| Credential helper | Command path `gc git-credential`. |
| Hidden/internal/private modes | Registered hidden command annotation or private argv sentinel before root construction. |
| Metrics commands | Command path `gc metrics ...`. |

Any command that can produce machine-readable output before `ExecuteC` must
add one row to this matrix or fail the command census. Detection generalizes
the existing `startOutputIsTerminal` TTY helper and the JSON pre-scan; it does
not infer TTY status from the char-device bit alone.

### Commands

`gc metrics` is DB-, city-, pack-, and supervisor-independent.
Ordinary metrics control commands retain the existing operator-controlled OTel
startup/shutdown behavior; they neither add product events nor change OTel
configuration. Only the private uploader sentinel bypasses normal OTel setup.

- `gc metrics` and `gc metrics status` show effective state/reason, config
  path, endpoint hostname, installation-ID presence (not the raw ID), queue
  count/bytes/age, cleanup-pending state, last upload attempt/success, exact
  fields, retention link, and the independence of OTel and `gc costs`.
  `status` is strictly read-only: it never creates directories, repairs state,
  retries cleanup, changes consent, or starts/waits for an uploader. A
  deliberately noisy `status --show-installation-id` prints a warning before
  the raw value: it is a stable linkable pseudonym, should not enter public
  logs, `off` deletes it, there is no automatic remote-delete command, and a
  targeted request may become impossible after deletion. Default and JSON
  status always redact it.
- `gc metrics on` prints the full disclosure, enables, and creates an
  installation ID in the same atomic config transaction. v0 requires verified
  TTY stderr and a complete notice write; there is no non-TTY/fleet
  force-enable flag or environment override. Fleet policy can disable metrics,
  but cannot accept the notice for a user.
  Re-running `on` while already enabled is idempotent and does not rotate the
  ID; `off` followed by `on` is the ID-reset operation. `on` exits nonzero
  without changing state while a disable environment, unsupported build,
  missing endpoint, cleanup barrier, or signed pause for this metrics epoch is
  effective. The control invocation is not recorded.
  Resetting affects only future events; it cannot unlink or delete facts already
  accepted under the prior pseudonym.
- `gc metrics off` durably removes the ID and disables first, then proves
  uploader quiescence and purges every queue/inflight generation. It remains
  available without TTY, city, supervisor, DB, or network under every overlay
  and is not recorded.
- `gc metrics example` renders an inert deterministic fixture through the
  production encoder. It uses fixed marked placeholders and must not open
  state, read the live ID, clock, or random source, create files, or perform
  network work.
- `gc metrics example --json` writes only that example JSON to stdout.

Successful opt-out text should be explicit:

> Gas City command usage metrics are disabled. Removed 12 queued events
> (8.4 KiB) and deleted this installation ID. Data accepted before or while
> this command waited was not deleted: raw events expire within 90 days and
> pseudonymous aggregate facts within 13 months. This command made no server
> request; use the published deletion contact with an ID you saved before
> opt-out for a targeted request. Gas City OTel, redacted event export, local
> cost records, and Beads telemetry were not changed.

`gc metrics off` has an explicit result contract:

| Result | Exit | State and output |
|---|---:|---|
| Already disabled and clean | 0 | Still performs the full quiescence/recheck handshake, then prints already-disabled; no ID, queue, or inflight files remain. |
| Full success | 0 | `disabled` is durable, queue/inflight are empty, ID is absent, no uploader can still send. |
| Durable-disable failure | nonzero | Previous state remains; stderr names `disable-write-failed`; retry guidance printed. |
| Cleanup incomplete after durable disable | nonzero | State remains `disabled-cleanup-pending`; stderr names the incomplete phase. A later `off` retries bounded automatic cleanup; ambiguous non-authorizing INTENT-plus-temp residue instead gets explicit same-UID manual-cleanup guidance. `status` only reports it. |
| Uploader quiescence timeout | nonzero | State remains `disabled-cleanup-pending`; no new enqueue/upload may start; stderr names `uploader-quiescence-timeout`. |
| Concurrent control conflict | nonzero | `on`, activation, or final `off` verification lost its generation/CAS race; stderr names `state-changed-concurrently` and never claims success. |
| Corrupt but safely writable state | 0 after full barrier | `off` replaces it with a fresh disabled schema, purges, and reports recovery; corrupt input is never treated as enabled. |
| Unsafe root or permission failure | nonzero | Fails closed; stderr names a bounded class and does not expose paths beyond the metrics root. |

At a declared control basename such as `status.toml` or `spawn-throttle`, a
non-authorizing filesystem shape is preserved and classified as unrecognized
root residue for same-UID manual-cleanup guidance. Transient I/O and
replacement failures remain retry-only.

Any nonzero result after the disable linearization point explicitly says that
collection and new uploads are already disabled; the retry is only to prove
uploader quiescence and finish local deletion. It never tells the user that
opt-out was wholly lost when the durable preference says otherwise.

Once `disabled` is durable, every future enqueue and uploader path must drop,
even if queue/inflight files remain until cleanup finishes. No exit-0 path,
including already-disabled, is allowed without crossing the uploader-lock
barrier and proving final state under `state.lock`.

### Preference location

State lives only under the effective Gas City home:

```text
<GC_HOME>/product-usage/
  config.toml
  quota.toml
  .pm-root-temp-journal/<root-temp-basename>  # local INTENT/BOUND record
  queue/<spool-generation>/
  inflight/<spool-generation>/
  state.lock
  uploader.lock
  spawn-throttle
  status.toml
```

The normal path is `~/.gc/product-usage`. The existing
`internal/gchome.Default` loses whether it returned a stable home or its
process-unique temporary fallback, so implementation adds a provenance-returning
side-effect-free resolver (or injects an equivalent resolved result) while
preserving existing `Default()` behavior. Metrics requires an absolute clean
home and applies this exact trust predicate while walking from `/`: every
existing component is a non-symlink directory owned by UID 0 or the effective
UID and is not group/world-writable, except a UID-0-owned sticky ancestor such
as `/tmp` is allowed only above a later effective-UID-owned private home. The
effective Gas City home and `product-usage` root themselves must be owned by
the effective UID with no group/other permission bits (0700-equivalent);
metrics files must be effective-UID-owned 0600-equivalent regular files.
Missing home/product components are created fd-relatively as 0700 only beneath
the nearest trusted existing ancestor, then revalidated by descriptor. A
wrong-owner, broader-mode, symlinked, or unstatable component fails closed; the
resolver never physically resolves a symlink and continues.

CGO-free Linux/Darwin builds cannot portably prove that every administrator-
managed named ACL grants no additional principal. v0 treats such ACL grants as
an explicit expansion of the local trust boundary, like same-UID access, not
as protection this feature can defeat. Creation always requests and rechecks
0700/0600 mode, and any ACL/default-ACL effect reflected in group/other mode
bits fails closed. Platform tests cover root-owned ancestors, sticky ancestors,
group-writable parents, wrong-owner homes, and inherited ACL/mode effects; the
privacy documentation states the named-ACL limitation. The mutating storage
open validates/creates through retained directory descriptors. `gc metrics status`
always prints the effective path and stability reason without creating it. A
relative explicit `GC_HOME`, temporary/fallback home, or unverifiable path
fails closed; path-prefix guessing is not accepted as provenance.

City TOML, pack TOML, repository files, imported configuration, and project
configuration cannot set consent or the endpoint. The endpoint is compiled
into official builds and cannot be redirected by a production runtime
environment variable. Tests inject it through Go options.

Configuration and accounting files are mode 0600 under a mode 0700 directory.
Writes use temp-file, fsync, atomic rename, and parent-directory fsync.
Loaders reject symlinks and fail closed on parse/schema/permission errors.

`config.toml` is the single atomic preference/identity record. It contains
`state_schema`, a private monotonic `counter_namespace`, monotonic
`state_generation`, preference,
`required_notice_version`, accepted notice version, optional installation ID,
optional random `spool_generation`, typed `cleanup_kind` and monotonic
`cleanup_epoch`, and scalar `paused_through_metrics_epoch`. The numeric triple
(`counter_namespace`, `state_generation`, `cleanup_epoch`) plus a retained
lease on the exact validated atomic config record is the non-secret cleanup
ownership token; disable and signed pause never require entropy. The private
namespace and exact-record incarnation are also part of every activation
basis, recording permit, and mutation CAS, but are never exposed by status.
Enabled state is invalid
unless the same committed record contains a valid ID and, when the current
notice/metrics epoch is uploadable, a spool generation; initial pending and
disabled records contain neither. Stale-notice and server-paused records may
retain an ID but cannot contain an active spool generation. There is no
separately authoritative ID file and therefore no crash window where failed
activation leaves a final ID behind.

Every ordinary state mutation increments `state_generation`, and cleanup
ownership advances `cleanup_epoch`. Their reserved terminal value is invalid
on load, so an exhausted record is fail-closed rather than recordable. If the
next disable, signed-pause, notice-invalidation, or cleanup mutation would
enter that terminal value, the atomic mutation advances `counter_namespace`
before resetting the numeric counters. Safely writable corrupt-state recovery
advances a decodable current-schema namespace; otherwise it may use the final
namespace as a fail-closed numeric placement hint. Recovery never treats a
scalar decoded from corrupt bytes as authority: while holding `state.lock`, it
must still match the retained descriptor for that exact corrupt record and
issues cleanup ownership only from the post-replacement record. Disable clears
the ID and spool, while pause and notice invalidation retain the ID but clear
the spool. Exact-record permit and owner matching therefore invalidates all
prior authority even when the complete low numeric tuple repeats. A recording
permit captures a lease on the exact atomic record plus the counter namespace,
generation, installation ID, spool generation, release version, and metrics
release epoch. Callers close the permit after the invocation; copied permits
share one idempotently closed lease. `RecordOnce`
reloads under `state.lock` and writes only when every value still exactly
matches; a permit obtained before `off`, `off`/`on` rotation, notice
invalidation, upgrade, or server pause is a silent drop. The final namespace
cannot advance or wrap and cannot contain an active spool; it is a durable
fail-closed fallback. Opt-out and cleanup completion remain available even
when its ordinary counters are exhausted by atomically replacing the record
without incrementing them; the new exact-record incarnation prevents stale
authority from crossing that replacement. A later `on` from a non-terminal
clean-disabled namespace always creates a fresh spool generation. Old
generation directories are cleanup-only and can never become uploadable again.

The complete `RecordOnce` allow predicate is conjunctive: official supported
build with compiled endpoint; no disable environment or managed-automation
marker; recordable census entry; preference enabled; current accepted notice;
`cleanup_kind=none`; current epoch above any signed pause; valid ID and
spool generation; and an exact permit match. False, unknown, corrupt, timed-out,
or unreadable at any term means silent drop before queue/spawn/network work.

`on`, first-run activation, stale-notice activation, signed server pause, and
`off` serialize through `state.lock` and commit only against the counter
namespace and generation they observed. An enable attempt based on a
pre-disable version loses: if it
commits first, disable supersedes it; if disable commits first, the stale
enable CAS fails. An enable based on the final clean-disabled generation is a
later transition even if wall-clock calls overlap.
A signed pause response may mutate or purge only if its counter namespace,
state generation, ID, spool generation, release version, and metrics epoch
still match the batch that elicited it.

`spawn-throttle` is a bounded record containing a cryptographically random
UUIDv4 attempt token and attempted instant; `status.toml` contains bounded
diagnostics. Neither is evidence that a process is alive. A parent reserves a
fresh token under `state.lock`, passes it to the child, and the child may
proceed after taking `uploader.lock` only if the record still names that exact
token. A token is never recovered or reset from a counter, so corrupt-record
recovery cannot create ABA with a delayed child. Malformed or future values are
durably replaced once with a fresh-token record under the lock rather than
extending suppression on every invocation. Entropy failure skips spawning and
does not affect durable enqueue, disable, or signed pause.
Live ownership and quiescence come only from kernel-released advisory locks,
consistent with the repository's no-status-files-for-liveness rule. Unknown
state fields, schemas, preference values, required-notice floors, cleanup kinds,
or higher persisted metrics epochs fail closed so an older binary cannot
ignore newer privacy state.

## Event Contract

### Exact v0 network body

The client owns a closed DTO. The queue event and HTTP event are the same type;
batching may not add hidden transport fields. The DTO must not contain
`map[string]any`, `interface{}`, `json.RawMessage`, exported extension fields,
or free-text escape hatches.

```json
{
  "schema_version": 1,
  "events": [
    {
      "event_id": "8c4f4128-a6e8-4f66-bd1b-1fcf1298b124",
      "installation_id": "3cf9fd4e-3337-4c29-a0ab-2858cd8a1f21",
      "app": "gascity",
      "release_version": "0.31.0",
      "os": "linux",
      "occurred_hour_utc": "2026-07-11T00:00:00Z",
      "command_id": "help"
    }
  ]
}
```

| Field | Rule |
|---|---|
| `schema_version` | Literal `1`; unknown versions are rejected. |
| `event_id` | Cryptographically random UUIDv4 per invocation; server dedupe key. |
| `installation_id` | Cryptographically random UUIDv4 created only after disclosure; deleted on opt-out. |
| `app` | Literal `gascity`. |
| `release_version` | Official semver only; development builds do not collect. |
| `os` | Bounded `runtime.GOOS` enum. No architecture or kernel version. |
| `occurred_hour_utc` | Invocation start truncated to UTC hour. |
| `command_id` | Closed `CommandID` member, maximum 64 ASCII bytes. |

There is intentionally no exact timestamp, end time, duration, outcome, exit
code, upload/session ID, arbitrary attribute map, or metric map in v0. Adding
a field requires a schema version, notice-version review, documentation
update, and captured-final-wire tests.

`CommandID` is a closed domain generated from a committed command-census
manifest. The manifest maps every built-in production Cobra node to exactly one
canonical executable-leaf ID, explicit sentinel (`help`, `version`, `unknown`,
or the synthetic wildcard `pack-command`), or reviewed exclusion reason.
Runtime-created pack nodes are not enumerated by user-authored name; structural
tests require every post-snapshot object to carry the wildcard annotation. The
encoder validates
membership, length, and ASCII shape at write time; a non-member event is
dropped rather than truncated, hashed, coerced to `unknown`, or spooled.
Adding a command does not automatically expand the domain: CI fails until the
manifest and notice classification are reviewed.

The client must never read or encode:

- argv beyond in-memory canonical command resolution;
- flag or positional values;
- cwd, HOME, city path/name, rig path/name, pack/import name;
- agent, session, bead, convoy, formula, order, repository, branch, or remote;
- prompt, mail, event payload, stdout, stderr, error, or stack trace;
- username, hostname, MAC, OS machine ID, provider identity, or credential;
- model, token, duration, performance, cost, or API data.

The receiver necessarily sees network metadata while serving HTTPS. Its policy
must say that request bodies are never logged, raw source IP and edge metadata
are not copied into analytics tables, and edge access logs expire within seven
days. The server stores only a Gas City-scoped HMAC of
`installation_id` and discards the client value.

### Command classification

Root construction first calls Cobra's `InitDefaultHelpCmd` and
`InitDefaultCompletionCmd` so its lazy built-ins exist. Before pack commands
are registered, it snapshots the built-in tree and attaches the private ID
declared in the census. Normal executable-leaf IDs match the canonical path
from `commandPathWords` without `gc`, joined by `-`; the policy is leaf-level,
not coarse family-level.

| Invocation | `command_id` |
|---|---|
| `gc session peek abc` | `session-peek` |
| Alias of `session peek` | `session-peek` |
| `gc`, `gc help session peek`, or `gc session peek --help` | `help` |
| `gc version` | `version` |
| `gc completion bash` | `completion` |
| Any pack-contributed command | `pack-command` |
| Unknown root or nested input | `unknown` |
| Recognized command with invalid args/flags | Recognized canonical ID |

Raw pack binding, pack name, discovered command path, Cobra `Use` text, and
arguments never become an ID, even hashed. All discovered intermediate and
leaf nodes receive an explicit `gc.productmetrics.class=pack-command`
annotation alongside `docgenSkipAnnotation` where applicable. The lazy pack
fallback returns a typed lifecycle result containing `pack-command` rather
than asking the root wrapper to infer a label from raw argv. Cobra resolution
may inspect argv in memory to select an annotated command or the literal
`unknown` sentinel, but no token is copied into the event, status, queue,
error class, or log.

Recording exclusions are the census entries described in the notice contract,
including non-user/protocol/control invocations:

- `gc metrics ...`;
- Cobra's hidden completion RPC such as `__complete`, while user-facing
  `gc completion <shell>` remains included;
- managed-agent contexts, `gc event emit`, hooks, provider-formatted modes,
  `prime --hook`, and `handoff --auto`;
- `gc git-credential`;
- hidden `gc internal`, `bd-store-bridge`, `dolt-config`, and `dolt-state`
  helpers;
- private metrics-uploader and managed-Dolt watchdog process modes;
- Gas City-owned recursive `gc` implementation details carrying the disable
  environment.

Help, version, JSON/schema requests, recognized usage errors, unknown input,
pack-command success/failure, long-running `supervisor run`, and all ordinary
user-visible built-ins are in scope after activation unless the committed
census explicitly excludes the mode. Abrupt termination before command
identity is resolved cannot be recorded.

## Proposed Design

### Boundary and data flow

```text
cmd/gc/run + one command-tree wrapper
                  |
                  | one closed, bounded Event
                  v
        internal/productmetrics
          | consent + notice
          | RecordOnce
          | bounded file spool
          | spawn throttle + uploader locks
                  v
       Gas City first-party ingest
          | validate + dedupe
          | Gas City-scoped ID HMAC
                  v
       Gas City raw table -> rollups/dashboard
```

`internal/productmetrics` must not import, directly or transitively,
`internal/events`, `internal/telemetry`, `internal/usage`, `internal/extmsg`,
`internal/eventfeed`, `pkg/eventexport`, API packages, dashboard packages, or
any future external-egress helper. A `go list -deps`/`go/packages` boundary
test pins a positive allowlist of permitted production dependencies. Test
packages may import neighboring systems only to assert isolation.

`cmd/gc` glue is guarded separately because `main.go` legitimately hosts
events, OTel, usage, event export, and product metrics in one package.
Production integration is confined to a tiny
`cmd/gc/productmetrics_adapter.go` with an explicit positive list of allowed
imports and callees. A `go/packages` + SSA reachable-call-graph test starts at
every adapter/metrics command entrypoint and rejects any direct or indirect
path to event emitters/recorders, `events.KnownEventTypes`, telemetry lifecycle
or recording, usage/cost sinks, `startEventExport`, `internal/eventfeed`,
`pkg/eventexport`, extmsg, API/dashboard helpers, or
`.gc/events.jsonl` writers. File-level AST checks remain a fast canary, not the
completeness proof.

Route/source guards separately prove that product metrics adds no Huma route,
raw supervisor mount, `internal/api/dashboardbff` route, dashboard request
path, OpenAPI/generated-client shape, event payload variant, or `gc events`
projection. The first-party ingestion service is deployment infrastructure,
not a supervisor/dashboard endpoint in this repository.

Production and test construction are intentionally different APIs. Official
code cannot name an endpoint or HTTP client:

```go
type ProductionOptions struct {
    Home           string
    Release        ReleaseIdentity
}

type Service struct { /* private */ }

func OpenProduction(ProductionOptions) (*Service, error)
func (s *Service) RecordingPermit(InvocationContext) RecordingPermit
func (p RecordingPermit) Close() error
func (s *Service) MaybeActivateNotice(InvocationContext, io.Writer) NoticeResult
func (s *Service) RecordOnce(RecordingPermit, CommandID) RecordResult
func (s *Service) DisableAndPurge(context.Context) (PurgeResult, error)
func (s *Service) Enable(context.Context) error
func (s *Service) Status(context.Context) Status
func (s *Service) Example() Batch
func FlushProductionMain(context.Context, ProductionOptions) int
```

`OpenProduction` validates immutable dependencies and returns a lazy service;
it does not create the root, open a writable file, repair state, or start a
process. Read-only `Status` uses no-create opens throughout. Mutating methods
open/create only after their own effective-state and operation gates require
it, so merely preparing an invocation cannot violate disabled/pending or
read-only behavior.

The compiled endpoint, direct transport, trust policy, random source, and real
clock are private production dependencies selected from `ReleaseIdentity`.
Same-package unit tests use an unexported dependency constructor. Process tests
compile a `productmetrics_testhook`-tagged adapter that alone exposes loopback
endpoint/client/clock/random injection; release CI rejects that build tag and
proves the symbols are absent from normal binaries. Names may change; the
closed production surface and test-only boundary should not.

### File ownership

| File | Responsibility |
|---|---|
| `internal/productmetrics/config.go` | State machine, validation, atomic persistence, precedence. |
| `internal/productmetrics/event.go` | Closed v0 DTO and strict encoder/decoder. |
| `internal/productmetrics/spool.go` | Atomic enqueue, bounds, pruning, claim/restore/delete. |
| `internal/productmetrics/upload.go` | HTTPS, batching, response policy, deadline. |
| `internal/productmetrics/spawn.go` | Lease, recursion guard, detach, minimal environment. |
| `internal/productmetrics/release.go` | Runtime-unoverrideable build identity, endpoint, metrics epoch, and rollout mode. |
| `internal/productmetrics/lock_*.go` | Cross-platform state and uploader locking. |
| `cmd/gc/productmetrics_adapter.go` | Sole allowlisted same-package bridge into product metrics. |
| `cmd/gc/metrics_lifecycle.go` | Census load, built-in snapshot, annotations, recursive wrapping, `RecordOnce`. |
| `cmd/gc/cmd_metrics.go` | Status/on/off/example user surface. |
| `cmd/gc/main.go` | Early uploader mode, service setup, notice gate, `ExecuteC` funnel. |
| `cmd/gc/json_schema.go` | Reuse canonical command-path and typed early JSON outcomes. |
| `cmd/gc/cmd_commands.go` / `cmd_pack_commands.go` | Pack annotations, typed fallback outcomes, child disable, and no normal-path exit. |
| `cmd/gc/providers.go` | Propagate provider-construction errors instead of exiting. |
| `internal/gchome/gchome.go` | Return stable-versus-temporary home provenance. |
| `internal/execenv` plus `cmd/gc/productmetrics_child_env.go` and self-exec call sites | Neutral GC-only disable policy, CLI adapter, and census without upward imports. |
| `cmd/gc/cmd_version.go`, `Makefile`, `.goreleaser.yml` | Build/release identity inputs and official constants. |
| `.github/workflows/release.yml` and new canary workflow | Pre-build manifest/evidence gate and post-build artifact attestation. |
| `engdocs/evidence/product-metrics/**` / `scripts/check-product-metrics-release` | Typed evidence/release schemas, manifests, hashes, and CI checker. |

No Huma/API/dashboard route or operational event registration changes are
allowed.

### Invocation lifecycle

1. Approved private sentinels (`main` or existing watchdog `init` paths)
   recognize uploader/watchdog modes before Cobra construction, pack discovery,
   OTel initialization, or normal metrics setup. The metrics uploader has one
   dedicated sentinel and recursion marker.
2. `run` captures one immutable `InvocationContext` immediately: invocation
   start rounded to UTC hour, build identity, environment/TTY classification,
   and the sticky recording permit. Product-metrics open failure means
   fail-closed and cannot change command output or exit behavior.
3. A narrow literal pre-scan over injected `run(args)` selects root
   construction; it never consults ambient `os.Args`. It understands the exact
   persistent-flag grammar (`--city value`/`--city=value`,
   `--rig value`/`--rig=value`, and `--` termination) and recognizes the
   built-in `metrics` token without consuming arbitrary values. `gc metrics`
   skips city resolution and pack discovery entirely, so
   status/on/off/example work with broken city config, unavailable
   DB/supervisor, or held pack-cache locks. Ordinary commands keep existing
   construction. The widely used `newRootCmd(stdout, stderr)` remains a
   compatibility wrapper over `newRootCmdWithOptions`; production `run` passes
   its injected argv and pack-discovery option explicitly, and
   `registerPackCommands` no longer inspects ambient `os.Args`.
4. Root construction registers Gas City built-ins, calls
   `InitDefaultHelpCmd` and `InitDefaultCompletionCmd`, then snapshots and
   annotates the complete built-in tree from the committed census.
5. Pack discovery runs afterward; every discovered namespace, intermediate
   node, leaf, and lazy fallback is annotated `pack-command`. Existing
   `installArgUsageErrors` and `installFlagGroupUsageErrors` remain in their
   current order. One final recursive installer wraps every normal executable
   leaf. Dispatcher handlers whose identity is known only after their body
   runs—the root fallback, manual help/group dispatchers, and lazy pack
   fallback—are explicitly annotated `deferred-outcome` and own one typed
   outcome callback instead of an immediate wrapper.
6. Notice evaluation uses the immutable context. A pending invocation remains
   unrecordable even if it successfully becomes the notice printer.
7. JSON/schema pre-scan returns a typed `earlyOutcome` containing handled,
   exit code, annotated classification or `unknown`, and exclusion reason.
   Handled outcomes pass through the same invocation recorder; helpers never
   call the product-metrics service independently.
8. Other paths run through `ExecuteC`. A normal-leaf handler wrapper calls
   `RecordOnce` immediately before the original handler. A deferred dispatcher
   first resolves its typed outcome, then makes its single attempt; otherwise
   an eager root wrapper could permanently record `unknown` before discovering
   help or `pack-command`. If no handler began because help, validation,
   `PreRun(E)`, or resolution failed, the final funnel uses the returned/
   resolved command plus typed lifecycle outcome to record `help`, the
   canonical leaf, or `unknown`. It never classifies from Cobra error text.
9. Eager and lazy pack fallback return a structured lifecycle outcome with
   classification, handled state, and `exitForCode` error. They never call
   `os.Exit` or expose the pack token to the recorder.
10. One invocation-scoped, first-writer-wins guard owns all paths. Its first
    attempted classification suppresses later attempts even when enqueue
    fails. `RecordOnce` validates the recording permit's state/ID/spool
    generation under `state.lock`, durably reserves quota and writes at most
    one event, releases the lock, and may attempt the throttled detached spawn.
    A failure is a silent metrics drop, never a reclassification opportunity.

The captured occurrence hour represents invocation start, while the file is
stored at the earliest point where a canonical identity is known—not command
completion. This covers long-running supervisor commands and handler exits
without collecting duration or outcome. A process that terminates before any
identity can be resolved is intentionally unrecordable.

The exit-bypass ratchet allows `os.Exit` only in `main`, the named
watchdog/private entrypoints, and the documented emergency supervisor hard
exit. Both discovered-command sites and
`providers.go:newSessionProviderFromContext` return typed errors. Production
`log.Fatal*` and `runtime.Goexit` remain forbidden unless added to the same
reviewed allowlist.

One neutral `internal/execenv` primitive and its CLI adapter inject
`GC_DISABLE_USAGE_METRICS=1` into every Gas City-owned recursive `gc` spawn:
hook children, supervisor start/restart/service
`ExecStart`, drift restart, perf, prompt-to-sling, nudge pollers, GitHub repair,
managed agent templates, and pack-launched child `gc`. An AST/spawn-site census
fails when a new `os.Executable`, `GC_BIN`, literal
`exec.Command("gc", ...)`/`exec.CommandContext("gc", ...)`, generated service
`ExecStart`, or indirect
self-executable argument lacks the helper or a reviewed lower-layer template
content guard. The outer `gc perf` and pack command
may record after activation; their children do not. The helper never sets or
translates `BD_DISABLE_METRICS` and does not change Beads or any other child
tool's independent telemetry.

## Persistence and Concurrency

### Durable storage and spool limits

Product metrics uses a dedicated durable-storage adapter. Reusing
`internal/fsys.WriteFileAtomic` alone is insufficient because v0 requires
file fsync and parent-directory fsync; its path-based component checks and
`internal/fsys.RemoveAll` also cannot prevent component-swap races. On Linux
and Darwin the adapter therefore walks from an already-validated root through
directory file descriptors and uses no-follow, relative operations throughout.
The adapter contract is:

- create temp files with mode 0600 in owner-private directories;
- write, fsync, close, atomic rename, and fsync the parent directory;
- use a strict two-state journal for every root-level atomic temp. Under
  retained descriptors, first create an owner-only, single-link regular marker
  with `O_EXCL`; its `INTENT` representation is exactly zero bytes and grants
  no deletion authority. Fsync the marker, sync the fixed owner-private
  `.pm-root-temp-journal`, and root-sync a newly linked journal before creating
  the matching empty 0600 root temp with `O_EXCL`. Capture the temp's nonzero
  device and inode without writing payload bytes;
- before writing any payload byte, durably transition that exact marker to the
  one closed `BOUND` representation. It is exactly `32+N` bytes, where
  `N <= 128` is the basename byte length: bytes `[0:8]` are ASCII `GCPMRTJ1`;
  byte `[8]` is `0x02`; byte `[9]` is `uint8(N)`; bytes `[10:16]` are zero;
  bytes `[16:24]` and `[24:32]` are respectively the device and inode as
  big-endian unsigned 64-bit integers; and bytes `[32:]` are the exact
  canonical root-temp basename. Total length is at most 160 bytes. Parsing
  requires exact length, magic, state, zero reserved bytes, nonzero identity
  fields, canonical basename, and basename equality with the marker and temp;
- at every applicable authority boundary—before temp creation, before `BOUND`
  becomes authoritative, before payload write, and before cleanup mutation—
  revalidate the retained root and journal, the named marker incarnation, and,
  once created, the named temp incarnation. Root, journal, marker, and temp
  must remain private, same-device objects, and the temp's exact device/inode
  must equal `BOUND`. Replacement, mismatch, or a cross-device object fails
  closed. After payload write, fsync, target rename, and root sync are durable,
  marker-retirement failure does not downgrade the installed target; it leaves
  conservative journal cleanup for a later bounded sweep. This INTENT/BOUND
  codec is local disk state only and changes neither the metrics HTTP wire
  format nor the server contract;
- open and retain validated parent directory descriptors before writing;
- read, rename, and remove with fd-relative no-follow semantics and reject
  non-regular files;
- normalize or reject existing lax permissions;
- reject symlinks, Unix hard-link surprises, ownership/mode drift, and
  component replacement; unsupported platforms use the fail-closed stub;
- bootstrap/open the metrics root and every descendant with no-follow,
  owner/type/link-count checks rather than validating once and reopening by
  path. Every ordinary descendant and event-file open also checks the retained
  parent-device relationship before opening or reading below the boundary;
  only the metrics root may differ from its lexical `GC_HOME` parent;
- treat the retained metrics root as the destructive-cleanup filesystem
  boundary: before opening any enumerated descendant for cleanup, revalidate
  its exact device/inode/type and require the child's device to match its
  retained parent. Preserve a cross-device child without opening, renaming, or
  unlinking it, and fail the clean-tree proof. The metrics root itself may be
  on a different device from its lexical `GC_HOME` parent. Same-device bind
  mounts remain inside the same-UID local trust boundary unless a portable
  mount-ID or no-cross-mount primitive is added;
- expose injectable failures for lock, write, fsync, rename, parent sync,
  enumerate, claim, delete, restore, entropy, and timeout tests.

- Directory mode 0700; file mode 0600.
- One event per atomic file beneath the active random spool generation; batches
  are assembled only by the uploader.
- Opaque event-ID filename; never a command or installation ID.
- Temp write, fsync, close, rename, and directory fsync.
- Upload oldest first with a stable filename tie-break.
- Root-global hard cap across every generation's queue, inflight, and event
  temp files: 4 MiB or 5,000 events, whichever is reached first.
- Maximum client age: 7 days.
- Maximum event file: 4 KiB.
- Maximum request: 64 KiB and 25 events.
- `quota.toml` is a conservative durable reservation counter for event count
  and bytes across queue, inflight, and event temp files in every generation.
  Enqueue reserves before writing; any crash window can overcount and drop
  future metrics but can never undercount past a cap. A notice update, pause,
  or ID rotation cannot reset quota while old event-bearing generations exist.
- Foreground enqueue performs no directory scan. The uploader reconciles quota
  with a scan bounded at the declared count/byte caps plus one overflow marker;
  unexpected external overflow fails closed and is pruned in bounded chunks.
- Every local control file is size-capped before decode (`config.toml` at
  16 KiB; `quota.toml`, `status.toml`, and `spawn-throttle` at 4 KiB). Names
  are ASCII and at most 128 bytes, nesting is exactly the declared layout,
  directory enumeration stops at 5,001 event-bearing entries plus a bounded
  metadata allowance, and all count/byte/time arithmetic is overflow-checked.
- Prune expired then oldest events after enqueue and before flush, within the
  bounded work budget.
- Delete malformed, oversized, symlinked, or schema-invalid files without
  uploading; a poison file never blocks later work. One cleanup invocation has
  one root-global budget across all generations and tree levels: at most 6,000
  directory entries, 512 opened directories, 5 MiB of bytes actually read, and
  1 MiB of names. Oversized/sparse files are charged an entry/name cost and
  unlinked without reading their contents; declared size can never exhaust the
  budget forever. Enumeration is streaming/fd-relative; empty and malformed
  generation directories consume the same global budget, and traversal stops
  everywhere when any dimension is exhausted. `off` invalidates consent first and
  returns nonzero cleanup-pending until repeated `off` calls finish an
  adversarially large tree; it never trades bounded work for a false success.
- Root-journal draining and final root enumeration spend that same cleanup
  meter. A marker basename has the exact atomic-writer form
  `.pm-tmp-<canonical-lower-hex-pid>-<canonical-lower-hex-sequence>`; both
  components are positive, have no leading zero, and exactly round-trip their
  lowercase encoding. A marker read is capped and charged to the shared read
  budget by reserving the 161-byte maximum-plus-one envelope before the read;
  the 160-byte maximum plus one overflow byte is sufficient to classify exact,
  truncated, and oversized records without a second unmetered read. One pass
  handles at most 64 marker entries and then performs an
  explicitly entry/name-charged 65th `Next` as an overflow sentinel; only EOF
  at that sentinel proves the bounded namespace complete.
- Only a strict `BOUND` record authorizes deletion of its matching root temp.
  Cleanup revalidates the journal's named incarnation after enumeration, the
  marker's enumerated/opened/named incarnation and same-device relationship,
  and the temp's effective-UID ownership, owner-only mode, single-link regular
  type, root-device membership, canonical name, and exact recorded nonzero
  device/inode immediately before unlink. It never reads the temp, including
  sparse contents. Temp unlink plus root sync—or root sync followed by
  rechecked absence—must finish before exact marker unlink plus journal sync.
  The same checks apply in the mutating drain and the read-only main/peer clean
  proofs.
- Zero-byte `INTENT` is deliberately non-authorizing. If its mapped temp is
  absent, synced/rechecked absence can settle or retire the non-sensitive
  marker. If the temp exists, cleanup preserves both and reports manual
  cleanup pending. A crash after `O_EXCL` temp creation but before durable
  `BOUND` can therefore require same-UID manual removal, but the temp is
  guaranteed empty because no sensitive payload byte may be written before
  `BOUND`. Malformed, overlong, noncanonical, mismatched, replaced, or
  cross-device marker evidence never authorizes deletion of a mapped root
  entry and cannot certify cleanup. A valid `BOUND` marker whose temp absence
  is durably synced and rechecked is non-sensitive settled crash evidence and
  may remain if exact marker retirement fails.
- Outside declared root names and the journal-derived authority above, every
  entry—including an unjournaled canonical-looking `.pm-tmp-*` file—is
  preserved and never descended into. Its presence or exhaustion before a
  complete scan leaves cleanup pending rather than broadening deletion
  authority. Repeated `off` is convergence-guaranteed for journaled temps and
  metrics-owned spool/control trees, not for an adversarial population of
  preserved unknown root entries or an ambiguous INTENT-plus-temp crash. The
  root-clean allowlist is ownership-driven, not a reservation of future names:
  `status.toml` and `spawn-throttle` remain preserved unknown residue until
  their S8 and S7 handlers respectively exist and add exact cleanup/proof
  semantics. Stage 1a is unshipped, so no published artifact predates this
  INTENT/temp/BOUND-before-payload protocol; if that release assertion changes,
  a separate migration design is required.
- Never log event bodies or file contents.

Foreground enqueue targets 20 ms. The 50 ms number is a decision budget, not a
wall-clock promise: file/directory fsync, rename, and process creation are not
cancellable once entered. If lock acquisition, path validation, quota
reservation, or remaining-budget checks cannot justify starting the next
uncancellable step, the event or spawn is skipped. A spawn is attempted only
after the durable file exists and decision budget remains; slow `StartProcess`
can still exceed the target and is measured separately. Contention, disk full,
permissions, slow storage, or I/O failure drops the event and leaves command
bytes and exit behavior untouched. Deterministic injected-delay tests enforce
the decision points; non-flaky benchmarks report the real latency distribution.

Spool transitions are normative:

| Transition | Lock | Operation | Crash recovery |
|---|---|---|---|
| reserve -> temp -> queue | `state.lock` | durably reserve quota, then event write+rename+parent sync | a crash may leave safe over-reservation; orphan temps are deleted on a bounded sweep. |
| queue -> inflight | `uploader.lock`, then `state.lock` | verify current state/ID/spool generation and bounded oldest-first rename claim | pre-existing current-generation inflight files are restored before new claims; non-current generations are cleanup-only. |
| inflight -> delete | same uploader owns claim | delete only after a complete typed durable acknowledgement, sync directory, then decrement quota | missing file is success; a crash before quota update safely overcounts. |
| inflight -> queue | same uploader owns claim | restore on retryable response or timeout | restored files keep original mtime/order key. For an exact destination collision, identity-leased, byte-exact, parent-device proof precedes an atomic exchange that installs the claimed inflight inode at the canonical queue name. Only a durably synced exchange may authorize identity-bound deletion of the displaced destination now named in inflight; unsupported, not-applied, ambiguous, or sync-pending exchange preserves both authorities and returns a conservative settlement error. |
| queue/inflight -> opt-out purge | `uploader.lock` then `state.lock` | spend one root-global cleanup budget streaming across generations and sync affected directories | idempotent; the cleanup epoch remains pending until all deletion and quota reset are durable. |
| queue/inflight -> pause purge | same uploader, then `state.lock` CAS | after a valid signed envelope, invalidate active generation and purge epochs at or below the signed pause epoch | stale responses cannot touch a newer state; a later approved epoch creates a fresh generation. |

### Opt-out race

`state.lock` protects consent, ID, generation, quota, queue, inflight claims,
and server-paused state. It is an OS advisory lock that releases on process death;
existence-only PID/status lock files are forbidden. `uploader.lock` is also an
OS advisory lock and is held for one uploader's bounded lifetime, including
HTTP. Uploader paths acquire `uploader.lock` before `state.lock`; `off` releases
`state.lock` before waiting for `uploader.lock`, so future code must not invert
that order.

The uploader:

1. acquires the single-uploader lock;
2. under state lock, rechecks the exact state generation, ID, spool generation,
   release/metrics epoch, and claims a same-release bounded batch, then releases
   state while it prepares the already-claimed bytes;
3. reacquires state in uploader-then-state order for the final permit check and
   calls the sender's split `Start` phase while that exact state lock is still
   held. `Start` must synchronously cross the one request-initiation boundary;
   it cannot merely schedule a later un-ordered send. The HTTP attempt keeps
   its existing five-second total deadline. A failed start sends nothing and
   restores the claim under the same ordering;
4. releases state and runs the sender's `Wait` phase outside `state.lock` while
   retaining `uploader.lock`. Thus state readers are not blocked on network,
   while `off` still cannot pass the uploader barrier before the attempt and
   its local settlement finish;
5. after `Wait` returns, creates a fixed 12-second settlement context from
   `context.Background()`, independent of caller cancellation, and reacquires
   state in uploader-then-state order. A still-current permit applies the typed
   response: exact acknowledgement deletes, and every sender error—including
   cancellation or deadline—restores. If the permit is stale, the
   disabled/non-current claim is cleanup-only. The claim cannot be abandoned
   merely because the initiating caller's context ended after `Start`;
6. rechecks consent before every next batch.

`gc metrics off`:

1. opens one exact mutable metrics-root descriptor before its initial state
   observation, derives any pending-cleanup token through that descriptor, and
   retains the root through step 4. Under that same root's `state.lock`,
   atomically commits preference `disabled`, increments
   `state_generation` and monotonic `cleanup_epoch`, removes the ID and active
   spool generation, and sets `cleanup_kind=disable`. The committed
   (`state_generation`, `cleanup_epoch`) tuple is the cleanup owner and requires
   no randomness. This is the disable linearization point; every
   enqueue/uploader/`on` path now drops or conflicts. An `off` observing an
   existing disable cleanup reuses its exact tuple without mutating it. An
   already-disabled clean `off` advances the cleanup epoch so it cannot
   fast-return around the barrier. Entropy failure can block enablement or drop
   a new event, but can never block durable disable or signed pause. A corrupt
   config in a verified owner-private, safely writable root is replaced by a
   fresh disabled record; an unsafe or unwritable root remains fail-closed and
   returns nonzero. A path-component replacement after the descriptor is
   retained cannot redirect `off` to another root's uploader lock, cleanup
   tree, pending owner, or predictable peer-successor record;
2. releases `state.lock` and tries to acquire `uploader.lock` for up to 12
   seconds, exceeding the cooperative child budget while still treating a
   stuck filesystem operation conservatively;
3. while holding `uploader.lock`, acquires `state.lock` in the global order. If
   the exact disable generation/cleanup epoch remains, it spends one
   root-global cleanup budget streaming across queue/inflight trees, syncs
   affected directories, and stops globally when that budget is exhausted.
   It returns cleanup-pending for the next explicit `off`; only once the whole
   tree is empty does it reset quota and
   verify preference disabled, ID/spool generation absent, directories
   empty, the root-temp journal boundedly proven settled, and no unexpected or
   cross-device residue while both locks remain held. Malformed, unsafe,
   live-temp, cross-device, replaced, traversal-error, or budget-exhausted
   journal state is cleanup-pending. Unexpected and
   cross-device entries are never deleted by this cleanup; they return nonzero
   cleanup-pending with disable durable and the cleanup owner retained. It then
   atomically commits clean
   disabled (`cleanup_kind=none`). If another `off` already reached
   that exact clean-disabled postcondition, it is only a candidate peer
   successor: `off` performs the same read-only bounded journal/root proof and
   then reloads and identity-leases the exact expected clean-disabled state
   after that proof. The main completion path likewise reloads the exact final
   state after its final-config journal proof, with that write and proof charged
   to the original sweep meter. Any peer/main field, namespace, generation, or
   record-incarnation mismatch is `state-changed-concurrently`, never silent
   success;
4. releases `state.lock` and then `uploader.lock` and returns the result already
   established under both locks.

A request whose `Start` phase acquires the final state lock before `off`'s
disable transition is ordered before opt-out and may still be accepted; this
command cannot revoke it from the server, so it remains subject to published
retention or a separate targeted-deletion request. If durable disable wins the
state-lock order, final permit revalidation prevents `Start` entirely. In
either order, `off` cannot report success until `Wait` and bounded local
settlement release `uploader.lock`. After successful `off`, no old local event
may be sent. If the
uploader lock cannot be proven free within the bounded wait, `off` exits
nonzero with durable `disabled-cleanup-pending` state; future enqueues,
uploader starts, automatic activation, and `on` remain blocked. A later
explicit `off` resumes cleanup. `status` is observational and never retries it.
Success proves no uploader or event from the disabled generation can send after
the barrier. A later explicit `on` may create a new generation; an `on` that
CASes from the final clean-disabled generation is ordered after `off` even if
their wall-clock calls overlap. Only enable attempts based on a pre-disable
generation are required to lose.

### Signed pause and resume race

After verifying a signed 410 and the batch permit, the uploader first commits
the privacy barrier under `state.lock`: set
`paused_through_metrics_epoch=max(existing, signed_epoch)`, increment state
generation, remove the active spool generation, and set
`cleanup_kind=pause` with an advanced cleanup epoch and covered metrics epoch.
No entropy is required. Only then does it delete and
directory-sync covered generations. Purge failure never rolls back the pause;
`status` reports pause cleanup pending, and those files are permanently
non-uploadable.

The current uploader retries bounded local deletion before exit. Thereafter,
an explicit `off` may supersede pause cleanup with the stronger all-generation
disable cleanup. A later greater-epoch release must also finish the local
pause-cleanup barrier before it can resume; read-only `status` never does so and
ordinary paused invocations do not spawn a network uploader.

Greater-epoch resumption is a CAS transition, not a version-string side effect.
It always reacquires `uploader.lock` then `state.lock` and boundedly reproves the
clean spool tree, zero quota, and root-temp journal before creating a new spool
generation, including when a prior pause-cleanup call made its successor visible
but its final proof failed. Only if preference remains enabled, the required
notice is current, local pause cleanup and that proof are complete, and the
compiled manifest epoch is strictly greater does
the process retain the ID, increment state generation, and create a fresh spool
generation. The invocation performing that transition has no recording permit;
collection begins with the following eligible invocation. Same/older epochs
and downgrades remain paused.

### Detached uploader

- Reserve at most one attempt per effective home per exact 60-second interval
  through the schema-closed, at-most-4-KiB `spawn-throttle` record. Immediately
  after the foreground event is durable and while retaining its root and
  `state.lock`, the parent draws a fresh canonical lowercase UUIDv4 token and
  durably commits `throttle_schema=1`, `attempt_token`, and a canonical UTC
  `attempted_at`. Only an applied-and-directory-synced write authorizes a
  process start. Missing, malformed, oversized, future/clock-rollback, and
  expired records may be replaced once; other read or write uncertainty fails
  conservatively. Every canonical UUIDv4 visible in a bounded malformed record
  participates in the equality guard; a generated token equal to any such
  recoverable prior token does not spawn, so replacement cannot create token
  ABA. Entropy, reservation, capability-close, or start failure never changes
  the already-durable event result.
- Before resolving the executable/environment or calling `Start`, the parent
  closes every queue, generation, config-lease, state-lock, and root
  capability retained by the foreground transaction. Process start is
  suppressed unless every reservation and transaction-capability close
  succeeds. The child then opens one root once, acquires its `uploader.lock`
  once, and retains both through claim, request initiation, and settlement. It
  validates exact throttle-token equality under `state.lock` both before
  claiming and immediately before the upload `Start` boundary. A replaced
  child, including one delayed until after replacement, and a losing-lock child
  exit with zero network work; token
  validation is never followed by an uploader-lock release/reacquisition. A
  marker-valid child whose token is stale or that finds no batch exits
  successfully before constructing the production transport. The 60-second
  timestamp controls parent replacement rather than expiring an otherwise
  exact current token.
- The child has a cooperative 10-second work budget and a hard five-second HTTP
  deadline. Context cancellation cannot guarantee termination during an
  uninterruptible filesystem or kernel wait, so ten seconds is not a hard
  process-lifetime promise. If such a child still owns `uploader.lock`, `off`
  times out nonzero with durable cleanup-pending state rather than claiming
  quiescence.
- Enter only through the exact argv pair
  `__gc-product-metrics-uploader-v1 <canonical-lowercase-UUIDv4>` plus
  `GC_PRODUCT_METRICS_PRIVATE_UPLOADER=1`. Every argv beginning with the
  sentinel is consumed, including malformed forms, before normal CLI setup;
  the marker is checked before storage or network. The child does not build
  Cobra, discover packs, initialize OTel, record an event, write normal command
  streams, or spawn recursively.
- Use the absolute current executable and a new session/process group on Linux
  and Darwin, set cwd to `/`, point all three standard descriptors at the null
  device, and give exactly one asynchronous owner responsibility for `Wait`,
  including a parent-descriptor close failure after successful `Start`. Do not
  use `Process.Release`. Unsupported-platform process entry returns before its
  selected runner can inspect home/root state, resolve executable/environment,
  reserve throttle, or start a process.
- Pass a sorted positive-allowlist environment only: pinned `GC_HOME`, the
  recursion marker, safe absolute `HOME`, `TMPDIR`, and reviewed XDG path
  values, plus `LANG`, `LC_ALL`, and the explicit standard locale-category
  names. Production uploader children do not inherit `PATH`, proxy variables,
  custom CA variables, loader injection, `GODEBUG`, `OTEL_*`, `GC_OTEL_*`,
  `BD_OTEL_*`, usage/cost variables, credentials, or arbitrary parent env.
  Production transport obtains system roots through Go/OS APIs rather than
  inherited CA variables. Tests inject clients and trust roots only through
  the test-tagged constructor, and normal artifacts contain neither those
  constructors nor their endpoint/CA literals.
- The production sender's split `Start` phase returns only after its wrapper
  enters the actual HTTP `RoundTrip` boundary while `state.lock` remains held.
  A pre-entry error or cancellation closes the gate permanently so delayed
  work cannot initiate a request; after entry, the returned `Wait` owns only
  completion and settlement proceeds under the S6 rules.
- Surface only bounded status through `gc metrics status`.

## Transport and Gas City Backend

### Client transport

- Compiled first-party HTTPS URL in official releases.
- Reject userinfo, query, fragment, non-HTTPS schemes, unexpected ports, and
  every redirect, including same-origin redirects.
- Official builds accept only the compiled scheme/host/port. Tests may inject
  loopback HTTP(S) only through a test-only constructor or build-tagged option;
  production has no endpoint or HTTP-client override.
- Production builds construct a dedicated `http.Client`/`Transport`: system
  TLS verification, `InsecureSkipVerify=false`, `Proxy=nil`,
  `DisableCompression=true`, no cookie jar, no `http.DefaultClient` or mutable
  global transport, and `CheckRedirect` that returns before a second request.
  Proxy/custom-CA environment and `Set-Cookie` therefore cannot affect a later
  attempt. A future corporate proxy/custom-CA policy requires a new transport
  and notice review.
- Request body cap is 64 KiB and 25 events. Because compression is disabled and
  no `Accept-Encoding` is sent, the raw response body is capped at 4 KiB before
  decode. Connection timeout is 2 seconds, TLS handshake timeout is
  3 seconds, response-header timeout is 3 seconds, and total request deadline
  is 5 seconds. v0 performs at most one HTTP attempt per uploader child; the
  60-second spawn throttle is the retry pacing. `Retry-After` is recorded only
  as bounded local status and does not extend the child lifetime.
- No cookies, bearer token, URL arguments, or arbitrary headers. Fixed headers
  are `Content-Type: application/json`, `Accept: application/json`, and the
  User-Agent below.
- Fixed User-Agent `gascity-product-metrics/1`.
- Network activity only in the detached child.

| Response | Client action |
|---|---|
| 200-299 with exact complete `accepted` acknowledgement | Delete the acknowledged claimed events. |
| 409 with exact complete `duplicate` acknowledgement | Delete only after proving every requested event was previously captured. |
| Empty, partial, malformed, generic 409, or mismatched acknowledgement | Restore and back off; never delete. |
| 400/413/422 | Restore the validated batch and record a bounded schema error; age caps eventually prune it. |
| 401/403/407 | Restore and back off; never purge. |
| 429, 500-599, network/timeout | Restore and back off within age/size caps. |
| 410 with valid signed pause envelope | CAS `paused_through_metrics_epoch`, invalidate the active spool generation, and purge generations at/below that epoch. |
| Any other status | Restore and back off; never purge. |
| Redirect | Reject and restore; never follow. |

Success and duplicate acknowledgement use a strict closed DTO:

```json
{"schema_version":1,"app":"gascity","action":"accepted","event_ids":["<uuid>"]}
```

`action` is exactly `accepted` for 2xx or `duplicate` for 409, and
`event_ids` must be a duplicate-free set exactly equal to the submitted batch.
Unknown fields, duplicate JSON keys, missing/extra IDs, wrong action/status,
wrong content type, or an oversized body restore the entire claim.
Before sending, the batch builder requires canonical lowercase UUID text,
filename/body event-ID equality, and uniqueness across files. A mismatch or
second local file with the same ID is poison/cleanup-only and never enters a
request, making acknowledgement set equality unambiguous.

Restoring a claim is equally identity-bound. If its original queue name is
already occupied, name equality or matching event ID alone is insufficient:
the implementation leases and revalidates the claimed inflight source and the
existing queue destination, and both must contain the exact canonical bytes of
the immutable claimed event before an atomic exchange installs the claimed
source inode at the queue name. Only after that exchange and both parent syncs
are durable may the displaced destination, now named in inflight, be deleted by
its retained identity. A replacement, read uncertainty, unsupported exchange,
ambiguous outcome, sync uncertainty, or byte mismatch preserves both retained
authorities and returns a conservative settlement error; restore never uses a
replacing rename or deletes the claimed source based only on mutable
destination bytes.

Destructive `410` additionally requires this closed signed envelope:

```json
{
  "schema_version": 1,
  "app": "gascity",
  "action": "pause-through-metrics-epoch",
  "release_version": "0.31.0",
  "metrics_epoch": 7,
  "key_id": "pm-pause-2026-01",
  "signature": "<base64-ed25519>"
}
```

`key_id` selects an embedded 32-byte Ed25519 public key. The signed message is
the ASCII/domain prefix `gascity-product-metrics-pause-v1\0` followed by the
RFC 8785 canonical JSON bytes of the six non-signature fields
(`schema_version`, `app`, `action`, `release_version`, `metrics_epoch`,
`key_id`); `signature` is the 64-byte Ed25519 signature encoded base64url
without padding. The strict decoder rejects duplicate/unknown fields before
canonicalization. Checked-in valid, bit-flipped, reordered, duplicate-key,
wrong-key, and non-canonical test vectors are shared by client and service.

Official builds embed an allowlisted public-key set and their release
manifest's monotonic `metrics_epoch`. The envelope must match the batch
release/epoch and current state permit. Replay for the same or an older epoch
is intentionally safe; a downgrade cannot clear it, and only a
manifest-approved greater epoch can resume with a fresh spool generation.
Malformed, unsigned, unknown-key, wrong-release/epoch/app, oversized, HTML,
empty, redirected, or otherwise unexpected 410 responses restore and back off.
System TLS remains the upload identity boundary, while the signature prevents
a locally trusted TLS interceptor from causing destructive local purge.

### Required server contract

Before default-on traffic, the Gas City endpoint must:

1. require `app=gascity` and validate the closed schema, enums, lengths, body
   size, batch size, and content type;
2. dedupe by `(app, event_id)` for at least eight days (the seven-day client
   horizon plus 24 hours); dedupe state and durable capture are atomic or
   ordered so a duplicate acknowledgement proves prior durable capture;
3. HMAC the installation ID before any durable write with a Gas City-specific,
   environment-specific, versioned secret, then discard the client ID. A key
   version remains usable for deletion lookup no longer than 13 months after
   last ingestion and is then destroyed. Rotation deliberately starts a new
   pseudonym; versions are never joined, so rare rotations may overcount active
   installs rather than create a cross-key identity map. No key or identity
   namespace is shared with Beads;
4. return an exact `accepted` acknowledgement only after durable first-party
   database capture, or after a documented first-party spool with DB-equivalent
   crash recovery, access isolation, retention, and drain guarantees. Return
   an exact `duplicate` acknowledgement only when the entire named set is
   already durably captured;
5. avoid forwarding Gas City events to Beads vendor/GA or undeclared third
   parties;
6. keep request bodies, raw IPs, User-Agent, edge-ray metadata, and TLS
   fingerprints out of analytics tables. Edge/access logs never contain bodies,
   are not joinable to analytics, and expire within seven days;
7. rate-limit and treat all data as attacker-controlled;
8. enforce 90-day raw-event and 13-month aggregate-fact retention. The
   aggregate fact grain is UTC day, `app`, installation HMAC + key version,
   command ID, release version, and OS with a bounded count; monthly active
   installs are derived from those facts. No raw IP/network identifier enters
   either table;
9. group/filter every raw table, aggregate, query, dashboard, alert, retention
   job, backup, restore, and deletion job by `app=gascity`. If an existing
   physical column is named `app_name`, the ingest mapping `app -> app_name` is
   typed and fixture-tested rather than implied;
10. accept a deletion request only with the user's deliberately revealed
    current installation ID, compute matches under still-retained Gas City key
    versions, and delete matching raw/aggregate facts. `gc metrics off` itself
    does not contact the server; after it destroys the only local ID, targeted
    remote deletion may be impossible and normal retention is the fallback;
11. expose the signed 410 pause envelope and protect its private signing key
    separately from ingestion.

The Gas City reports answer only Gas City product questions, always filter
`app=gascity`, and label installation counts as best-effort estimates because
the unauthenticated payload is fabricable and key rotation intentionally breaks
linkage.

Stage 0 produces a real versioned artifact, not prose:
`engdocs/evidence/product-metrics/backend/v1/evidence.json` validated by
`engdocs/evidence/product-metrics/backend/v1/schema.json`. It records endpoint
owner/origin, service commit and image digest, schema and raw-table fixture
results, app-mapping filters, HMAC-and-discard/key-retirement tests, aggregate
schema, dedupe retention/atomicity, forwarding denylist, backup/restore and
deletion filters, seven-day edge-log policy, retention jobs, signed-pause key
and drill, dashboard/alert queries, approvers, evidence hashes, and expiry.
`scripts/check-product-metrics-release` validates the typed artifact and every
referenced local evidence hash; the release manifest pins its SHA-256. Missing,
expired, stale, or unapproved evidence makes canary/default-on artifact
construction fail.

## Error Handling and Local Status

Except for the exact one-time notice on an eligible human TTY and output from
explicit `gc metrics` commands, product metrics never change command stdout,
stderr, exit code, JSON buffering, or OTel shutdown behavior. Normal commands
do not print metrics failures.

`gc metrics status` exposes bounded local diagnostics:

- effective state and reason;
- notice/config schema version;
- queue count, bytes, oldest age, and drop count;
- cleanup-pending and spool-generation presence, never raw generation values;
- last upload attempt and success hour;
- last error class such as `lock-timeout`, `disk-full`,
  `network-timeout`, `server-5xx`, or `server-paused`;
- spawn-throttle age.

It never stores or prints a rejected request body, arbitrary response body,
argument, command output, or error detail from another `gc` command.

`server-paused` is not opt-out. It preserves the accepted notice and local
installation ID, removes the active spool generation, performs no enqueue or
network work at or below `paused_through_metrics_epoch`, and reports that the
ID is retained. `gc metrics off` remains the only transition that deletes it.
`gc metrics on` cannot override the same or an older epoch. Resumption requires
a strictly greater runtime-unoverrideable metrics epoch authorized by the
release manifest; a version-string change or downgrade is insufficient and a
fresh spool generation is created. If identity policy changed, resumption also
requires a new notice and ID rotation.

## Testing

### Product-metrics package

- State matrix: absent config, first/stale notice, on/off,
  cleanup-pending, signed pause epoch, upgrade/downgrade, disable env
  precedence, `DO_NOT_TRACK`, dev/test/CI build, stable/fallback home, and
  malformed/unreadable/unknown/newer-schema or field config.
- Atomic persistence, modes, symlink rejection, fsync/rename failures, and
  multi-process locking, including file/parent fsync, complete-notice write,
  notice+ID one-record commit, generation/CAS conflicts, and no final ID after
  every injected activation crash point.
- Identity lifecycle: no ID before notice, stable while enabled, absent after
  off, retained-but-inactive for stale notice/server pause, idempotent while
  already enabled, and new after off+on.
- The inert `gc metrics example --json` fixture is a golden production-encoder
  vector and produces identical bytes in every local state without touching
  state/clock/random. Separate one- and multi-event fixed vectors byte-match
  queue decode, uploader batch assembly, and handler-captured HTTP bodies; the
  batch envelope adds only `schema_version` and `events`.
- Root-global count/byte/age caps, oldest pruning, corrupt/oversized files,
  disk full, conservative quota-reservation crash windows, bounded
  reconciliation and cleanup,
  generation isolation, claim/restore, typed-ack/delete crash replay, and
  event dedupe.
- Root-temporary codec goldens prove zero-byte INTENT and the exact
  `GCPMRTJ1`/`0x02`/length/reserved/big-endian-dev/big-endian-ino/basename
  BOUND bytes through the 160-byte limit. Truncation, a 161st byte, bad magic
  or state, nonzero reserved bytes, zero identity, noncanonical or unequal
  names, and recorded-identity mismatch are non-authorizing and read through
  the shared maximum-plus-one budget.
- Root-temporary crash tests cover marker file/journal/root sync ordering,
  `O_EXCL` allocation collisions, every INTENT/temp/BOUND/payload/rename/root-
  sync point, and target-installed marker retirement including the final clean
  state. A crash after empty-temp creation and before durable BOUND leaves no
  payload bytes, is never auto-deleted, and reports manual cleanup pending.
- Journal drain/proof tests cover exactly 64 markers plus the charged 65th
  overflow sentinel, multi-pass shared-meter exhaustion, temp unlink/absence
  replay, marker unlink replay, identical read-only main/peer settled-journal
  proof, and final exact peer-successor state revalidation. Journal, marker,
  and temp device/incarnation replacement, cross-device evidence, malformed or
  INTENT evidence with a live temp, unjournaled canonical lookalikes, arbitrary
  root entries, and pre-handler `status.toml`/`spawn-throttle` residue are
  preserved without mapped-root mutation or descent and cannot certify clean.
- Cross-device cleanup tests cover queue, inflight, control, generation, and
  nested descendants, prove no open/enumeration/mutation below the boundary,
  cover direct post-sweep generation reopens and event-file reads, and
  separately allow a metrics root mounted on a different device from its
  lexical parent.
- Response policy covers empty/partial/generic 409 acknowledgements, exact ID
  set equality, duplicate JSON keys, all redirects, direct proxy policy,
  compression disabled, cookie/global-client isolation, HTTPS limits/deadlines,
  catch-all restore, source-authoritative atomic-exchange collision restore,
  unsupported/replaced/post-exchange/sync-pending uncertainty, and
  valid/invalid/replayed signed 410 envelopes.
- Spawn attempt-ID races, clock rollback/future timestamp normalization,
  recursion guard, cooperative runtime budget, minimal environment, process
  reaping, Linux/Darwin detachment, and fail-closed unsupported-platform stubs.
- Inject entropy failure and prove it may block `on` or drop an event but never
  blocks durable `off` or signed-pause state.
- Block an uploader in an HTTP test and prove every `off` result row. Every
  exit-0 path—including already-disabled—has crossed the uploader-lock barrier,
  disabled is durable, queue/inflight are empty and directory-synced, ID/spool
  generation are absent, cleanup is clear, and no later send occurs. Timeout
  is nonzero and leaves cleanup pending.
- Instrument the split sender and prove request `Start` crosses its final
  permit boundary under `state.lock`, `Wait` runs outside state while retaining
  `uploader.lock`, disable winning the state-lock order prevents start, and a
  started request wins before disable. Caller cancellation after start cannot
  bypass the independent 12-second settlement context: exact acknowledgements
  still delete and every sender error still restores when the permit remains
  current.
- Race concurrent first-run/`on`/two `off` calls/signed pause/greater-epoch
  resume and assert the generation/CAS winner rules, shared disable cleanup
  epoch, main and peer-successor final exact-record revalidation after the
  shared read-only proof, stale-response rejection, failed-pause purge
  persistence, and that notice/resume transition invocations never record.
- Snapshot the entire metrics root before/after `status` and prove the command
  is read-only in clean, corrupt, and cleanup-pending states.

### Command coverage

- Initialize Cobra defaults, then enumerate every executable/group/hidden
  built-in node and assert one census row, stable annotation/exclusion, and
  exactly one recording owner: an immediate wrapper for normal leaves or a
  typed deferred-outcome owner for dispatchers, never both.
- Assert every post-snapshot discovered node resolves only to `pack-command`.
- Assert aliases resolve to canonical IDs.
- Exercise success, handler error, pre-run/flag/arg error, unknown root/nested,
  bare/group/target help, version, both completion forms, JSON/schema/contract,
  JSONL/default-format, panic, long-running start, pack success/nonzero, and
  eager/lazy pack fallback. Each eligible case records exactly once.
- Exercise the full notice/recording matrix: event emit, hooks and hook child,
  prime/handoff modes, bd/beads/events, supervisor run, perf, every hidden
  command, service/private sentinels, managed markers, and pack output.
- AST-ratchet allowed `os.Exit`, `log.Fatal*`, and `runtime.Goexit` sites.
- AST-ratchet every first-party self-exec/`GC_BIN` site through the recursive
  disable helper and prove no `BD_DISABLE_METRICS` mutation.
- Prove `gc metrics` builds/runs without city or pack discovery under corrupt
  config and held repository-cache locks.
- Fail the command census when a new executable lacks classification.

### Privacy and output

- Structural schema tests assert the serialized field set is exactly the v0
  contract with no maps, raw JSON, interfaces, or free-text escape hatches.
  Property/fuzz tests plus committed adversarial corpus seeds generate secrets
  in args, flags, paths, names, environment, output, and errors, and prove none
  appears in a queue file or captured HTTP request.
- Prove raw dynamic command names are absent, including hashed/encoded forms.
- Notice tests cover TTY/non-TTY, JSON/JSONL/schema, completion, hooks,
  credential helper, metrics commands, hidden commands, concurrent first run,
  failed persistence, and notice-version migration.
- Run one blocking byte-for-byte stdout, stderr, exit-code, JSON-buffering, and
  OTel-shutdown comparison matrix across disabled, pending, enabled, and every
  injected metrics failure for all command outcomes above. The only allowed
  delta is the exact golden notice on an eligible human pending invocation;
  every machine-output case remains byte-identical.
- Verify metrics on/off does not alter OTel, local usage, redacted event export,
  operational events, or Beads state/telemetry.
- Verify no productmetrics imports or schemas appear in `internal/api`,
  Huma or raw/dashboard-BFF routes, OpenAPI, generated dashboard types,
  `events.KnownEventTypes`/payloads, event feed/export, `gc events`, or
  `.gc/events.jsonl`. The allowlisted reachable-call-graph test covers
  same-package indirection.

### Performance

- Ordinary foreground enqueue remains below the 20 ms target in benchmark
  conditions and obeys the 50 ms decision budget by dropping before starting
  uncancellable work when injected slow dependencies consume the budget.
- Report enqueue and process-spawn distributions separately; no test claims an
  uncancellable fsync or `StartProcess` has a hard wall-clock deadline.
- High-concurrency and offline tests prove queue caps, one reserved attempt per
  lease, at most one uploader-lock owner/network-active uploader per home, and
  zero network work by stale/losing children.
- Disabled and pending-notice paths perform no queue or network work.

## Rollout

Release mode is runtime/config-unoverrideable inside a built artifact. This is
an accidental/runtime activation boundary, not authenticity against an OSS
builder who changes source; signed release attestation establishes provenance
for published binaries. Today
`cmd/gc/cmd_version.go`, `Makefile`, `.goreleaser.yml`, and
`.github/workflows/release.yml` inject only version/commit/date; implementation
extends those exact ownership points.

Local source defaults compile as `BuildKind=development`, empty endpoint,
`metrics_epoch=0`, and `rollout=default-off`. The release workflow alone
generates official constants after validating
`engdocs/evidence/product-metrics/releases/v1/<semver>.json`. That typed
manifest pins release version and source commit, build kind, rollout mode
(`default-off`, `canary`, or `default-on`), monotonic metrics epoch, endpoint
origin, compiled privacy URL, notice version/text hash,
wire/example/privacy-doc hashes, backend
evidence SHA-256, signed-pause public-key IDs, canary result, approvers, and
expiry. Environment, city/pack config, ordinary ldflags, or a semver-looking
local version cannot promote a build.

`scripts/check-product-metrics-release` has two non-confusable modes. Its
Stage-1 scaffold validates schemas and deliberately non-production negative
fixtures but can emit no official constants. Its later activation mode requires
the complete manifest/evidence/hash/approval set before emitting private
official constants. Current stable, RC, edge, RC-gate snapshot, and container
artifact paths (`release.yml`, `rc-release.yml`, `gc-edge-publish.yml`,
`rc-gate.yml`, and `container-scan.yml`) are all pinned to endpoint-empty,
default-off identity until activation mode is explicitly wired. The rolling
edge artifact remains a development contract-radar build and is never reused
as the metrics canary.

For activation, release publication changes from the current upload-first
order to build/upload-as-draft, bind the manifest hash and exact artifact
SHA-256 values in a signed attestation, verify that binding with a second
checker, and only then publish; Homebrew waits for publish. Dev, test, CI,
unversioned, and locally built artifacts fail closed and cannot name the
production endpoint. CI also builds without the test hook and rejects the
`productmetrics_testhook` tag/symbols in every artifact channel.

### Stage 0: policy and backend

- Approve the exact schema, first-run copy, privacy page, edge-log policy,
  retention, and deletion contact.
- Build the Gas City ingestion route with durable acknowledgement, Gas City
  tables/rollups, dedupe, HMAC, rate limits, kill switch, dashboard, alerting,
  backup, and restore.
- Verify no Gas City request reaches a Beads-specific forwarding path.
- Commit the backend evidence bundle named in the server contract, including
  HMAC-and-discard, app separation, dedupe atomicity/retention, retention jobs,
  edge-log retention, backup/restore, forwarding isolation, and kill-switch
  tests.
- Commit and validate the versioned release-manifest schema, checker, and
  two-phase artifact attestation before any canary build.

### Stage 1a: inert client controls, default-off

- Ship `gc metrics status|on|off|example`, state/locking, bounded queue,
  uploader, coverage tests, and technical docs.
- Keep every official artifact endpoint-empty and feature-gated off. `on`
  reports unsupported/missing approved endpoint and cannot collect.
- Exercise the full path only through the tagged test constructor and loopback
  service; no published binary can opt in yet.
- Make command census, exit-bypass ratchets, closed schema tests, productmetrics
  boundary tests, and output/exit/OTel invariance tests blocking in CI.
- Make schema/example and production-vs-test constructor drift blocking in CI.
  Final notice/privacy text hashes become blocking only when Stage 0 supplies
  the approved URL, contact, retention policy, endpoint, and evidence.

### Stage 1b: approved explicit opt-in, still default-off

- After Stage 0 policy, endpoint, and pause-key evidence exist, finalize the
  public notice/privacy page and embed the approved endpoint/key set through an
  activation manifest.
- Keep automatic activation off; exercise only deliberate TTY `gc metrics on`
  in a bounded maintainer channel.
- Use the same draft-attest-verify-publish release order required below; this
  stage is deployment work and is not part of the inert in-repository client
  implementation.

### Stage 2: canary

- Add a dedicated maintainer/nightly canary workflow because the existing
  stable release workflow accepts only `vX.Y.Z` and rejects prerelease refs.
  The canary workflow uses its own manifest, endpoint/evidence approval, signed
  attestation, and official canary `BuildKind`; it remains notice-gated and
  cannot publish a stable artifact.
- Drill offline, corrupt queue, concurrent opt-out, endpoint outage, generic
  and signed 410, key rotation, upgrade, and downgrade.
- Validate deduped raw counts against Gas City-filtered aggregates.
- Canary success criteria: bounded queue age, uploader error budget, no
  unexpected purge, dedupe divergence within the approved threshold, dashboard
  separation verified, and an approver record in the manifest.

### Stage 3: official default-on

- Enable only in versioned official artifacts.
- Publish release notes and privacy/retention documentation first.
- Monitor queue age, uploader errors, durable ingest, dedupe rate, and report
  separation.
- Keep both client build/env disables and the signed server 410 kill switch.

### Rollback

The endpoint returns the signed 410 envelope to pause through the current
metrics epoch and trigger generation-scoped local purge. A follow-up release
can compile the feature off or, after review, advance the metrics epoch.
Neither path affects operational events, redacted event export, OTel, local
cost facts, or Beads telemetry.

## Documentation Required with Implementation

- Add `gc metrics` and all flags to the generated command tree, then regenerate
  `docs/reference/cli.md` through the existing doc generator.
- After Stage 0 policy approval, add `docs/reference/usage-metrics.md` (and its
  `docs/docs.json` navigation entry) as the public Gas City
  usage-metrics/privacy page with exact fixture, endpoint owner, pseudonymous
  identity/reset semantics, source-IP and seven-day edge-log handling,
  90-day/13-month retention, deletion limitations/contact, signed pause, and
  opt-out behavior.
- Update `docs/reference/events.md` and
  `docs/reference/trust-boundaries.md` to distinguish operational events,
  redacted operator event export, operator OTel, local cost facts, and product
  metrics, including their independent controls.
- Cross-link and correct historical context in
  `engdocs/archive/backlogs/telemetry-roadmap.md` and
  `engdocs/design/usage-facts-v0.md` without rewriting their historical
  decisions.
- Stage 1a may land the generated CLI reference and technical independence/
  trust-boundary docs, but must not publish placeholder privacy copy, URL, or
  deletion contact.
- Document `GC_DISABLE_USAGE_METRICS` and `DO_NOT_TRACK`.
- Document default-off development/test/CI behavior.
- State in every control page that Gas City opt-out does not change or suppress
  Beads telemetry.

## Alternatives Considered

### Reuse the current Beads client/backend path unchanged

Rejected. Gas City needs a Gas City consent state, random identity, closed
schema, command classifier, queue bounds, durable acknowledgement, and
`app=gascity` reports. Sharing an app-less rollup or Beads-specific forwarding
path would conflate products. This decision does not change Beads itself.

### Put command metrics on the operational event bus

Rejected. That bus is city-scoped, contentful, replayable, user-visible, and
able to drive orchestration. Its trust and retention boundaries are wrong.

### Reuse the redacted operational event exporter

Rejected. `cmd/gc/event_export.go`/`pkg/eventexport` is an operator-configured
projection of city events with actor/run/session correlation, bearer-token
support, a durable cursor, and a different failure/retention contract. It does
not provide user-global notice acceptance, command census, resettable product
identity, bounded private spool, or quiescent opt-out.

### Reuse transitive `eventkit`

Rejected. `eventkit` is currently only transitive through the Beads module and
is not in `go list -deps ./cmd/gc`. Its public event shape includes exact
start/end time, arbitrary attributes/metrics, OS-derived machine identity, and
a differently hardened file queue. It has no Gas City notice state, closed
command schema, count/byte/age cap, generation barrier, or
opt-out/purge/quiescence contract. Importing it would weaken rather than reuse
the required boundary.

### Reuse OTel

Rejected. OTel is operator opt-in and intentionally rich. Combining it with a
notice-gated product signal would make both controls misleading.

### Use the OS machine ID

Rejected. It is stable across reinstall, non-resettable, and unnecessarily
linkable. A random post-notice ID answers the product question with less risk.

### Record exact duration, outcome, or exit code

Rejected for v0. Command counts do not require them, duration can reveal
workload characteristics, and completion loses long-running/crashed commands.
Any future addition needs a demonstrated question and a new schema/notice
review.

### Record raw or hashed pack-command names

Rejected. They are user-authored, potentially sensitive, and unbounded.
Hashing preserves fingerprintability.

### Store preferences in city or pack config

Rejected. A project or imported pack must never enable collection, redirect
the endpoint, or fragment a user opt-out.

## Open Questions

The client/state/transport contracts above are resolved for decomposition.
These deployment inputs remain intentionally unanswered and keep the document
in `draft`:

1. What exact first-party endpoint origin, service repository, and owning team
   will be named in backend evidence?
2. What public privacy URL and deletion contact will ship, and who approves the
   seven-day/90-day/13-month policies?
3. Which signed-pause key IDs and custody process will be approved?
4. Which release channel, observation window, numeric error/dedupe thresholds,
   and human approver define a successful canary?

None may be filled in by an implementer guessing. Inert Stage 1a work can
proceed; explicit opt-in, canary, and default-on cannot.

## Approval Gates

Default-on rollout remains blocked until the versioned release manifest and CI
checks prove:

1. the public privacy/retention URL, deletion contact, and explicit limitation
   after the local ID is destroyed;
2. the exact Gas City endpoint and owning service;
3. request-body logging disabled and edge access-log retention capped at seven
   days, separate from 90-day raw-event retention;
4. dedicated Gas City storage versus a fully app-separated shared CLI store;
5. the daily aggregate-fact schema, HMAC rotation/non-linkage policy, and
   13-month retention implementation;
6. typed backend evidence acceptance and its SHA-256 pinned in the release
   manifest;
7. signed-pause key custody plus valid/invalid/replay/downgrade drill evidence;
8. canary selector, window, success criteria, and approver;
9. notice/schema/privacy-doc/retention drift check passing;
10. command/notice census, exit/self-exec bypass, closed-schema,
    production-constructor, reachable-call-graph, and API/event/export boundary
    ratchets passing;
11. a signed publication attestation binding the approved manifest and exact
    release artifact SHA-256.

Implementation can complete through endpoint-empty/default-off Stage 1a while
those deployment inputs are resolved.

## Definition of Done

- Every eligible top-level invocation by cooperating, unmodified first-party
  `gc` processes after activation records exactly one canonical bounded event;
  every exclusion is explicit.
- No argument or user-defined command name can appear in spool or wire.
- First collection occurs only after a complete human-visible notice and one
  atomic notice/identity/spool-generation commit.
- Every successful `gc metrics off`, including already-disabled, returns only
  after the uploader-lock barrier, durable disable, all-generation purge,
  identifier deletion, and directory fsync; partial cleanup is explicit
  nonzero state.
- Queue, process, latency, retry, and retention limits are enforced.
- The exact final request shape is available as an inert user-inspectable
  fixture and byte-for-byte golden-tested through the wire encoder.
- Gas City data is deleted locally only after complete typed acknowledgement,
  app-separated end to end, protected by a signed destructive pause, and never
  routed through a Beads-specific report or forward.
- Gas City metrics remain independent of operational events, redacted event
  export, OTel, local usage facts, and Beads telemetry.
- Default-on is impossible until release-mode identity, policy, backend,
  canary, kill-switch, notice drift, command census, and boundary gates pass.
