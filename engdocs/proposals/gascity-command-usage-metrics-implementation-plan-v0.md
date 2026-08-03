---
plan_slug: gascity-command-usage-metrics-v0
phase: implementation
rig: gascity
rig_root: /data/projects/gascity-command-metrics
design_file: /data/projects/gascity-command-metrics/engdocs/proposals/gascity-command-usage-metrics-v0.md
status: core-pr-scope-approved
scope: stage-1a-client-core-endpoint-empty-default-off
created_at: 2026-07-11T05:20:00Z
updated_at: 2026-07-14T00:00:00Z
council_verdict: clear
council_reviewed_at: 2026-07-11T05:58:24Z
---

# Implementation Plan: Gas City Command Usage Metrics

> **Maintainer course correction (2026-07-14).** The current Stage 1a PR is
> the endpoint-empty client core: controls and notice state, minimized event
> contract, durable bounded queue, strict uploader, centralized Cobra coverage,
> and targeted recursive-child suppression. The generic go/packages/SSA
> analyzer, asset-by-asset mutation census, service-manager migration
> hardening, repeated per-commit councils, and the exhaustive S13 closure loop
> are explicitly not prerequisites for this PR. They are superseded below by
> focused behavior/process tests, normal repository gates, and one final
> three-lane council. This scope change does not activate collection or relax
> any privacy, wire, opt-out, storage, or backend gate.

## Outcome and scope

Implement the complete in-repository Stage 1a client described by
`gascity-command-usage-metrics-v0.md`, with every locally built artifact and
every ordinary release remaining fail-closed/default-off. The result includes
the user controls, notice and identity state machine, bounded durable spool,
strict uploader, centralized command census and instrumentation, release
gates, documentation, and blocking privacy/architecture tests.

This plan does not invent the four deployment inputs the design leaves open.
The production endpoint, public privacy/deletion contact, signed-pause key
custody, and canary thresholds/approver remain blocked Stage 0/2 work. No
placeholder endpoint, key, URL, or synthetic approval may enter a production
build. Beads telemetry is independent and unchanged.

## Delivery invariants

Every slice must preserve these invariants:

- `go build ./cmd/gc` produces a development build with no production metrics
  endpoint and no way to collect or upload product metrics.
- No city, pack, repository, environment variable, ordinary linker flag, or
  semver-looking local version can enable production collection or redirect
  its endpoint.
- Product metrics never enter operational events, event export, OTel, local
  usage/cost storage, Huma/OpenAPI/dashboard surfaces, or Beads telemetry.
- Queue and wire values come only from closed typed DTOs. Arguments, flag
  values, paths, names, content, exact time, duration, outcome, model, tokens,
  cost, and free-form errors have no representable field.
- `gc metrics off` is the only opt-out transition and cannot report success
  until durable disable, uploader quiescence, all-generation purge, identity
  deletion, and final verification are complete.
- Incomplete slices stay unreachable behind the development/default-off
  release identity. A slice may add tests and internals, but may not expose a
  partially safe production path.
- Guarantees apply to cooperating, unmodified first-party processes on the
  user's local trust boundary. Hostile same-UID code or executable packs can
  ignore locks and replace user-owned state and are explicitly out of scope.
- Official v0 support is Linux and Darwin. Every other platform compiles a
  fail-closed stub with no notice acceptance, ID, queue, spawn, or network.

## Council and commit gate

The implementation uses one writer and three independent reviewers per slice.
The writer may be a subagent working in the isolated metrics worktree. Before
any commit, a fresh council reviews the exact unstaged/staged diff and test
evidence through these lanes:

1. privacy/security and hostile-input review;
2. state/concurrency/crash-consistency review;
3. CLI/repository/release and regression review.

The main integrator reconciles disagreements against the design, applies every
confirmed P0/P1 finding, reruns the slice verification, and asks the relevant
reviewer to recheck the repair. A commit is allowed only when all lanes report
no open P0/P1. A P2 may be fixed in-slice or captured as a linked bead with an
explicit reason it cannot violate a delivery invariant. Review reports record
the reviewed tree hash, commands run, findings, dispositions, and final
verdict under `.gc/product-metrics-reviews/`; that runtime directory is not a
second task tracker.

Each commit is one rollback-safe slice. The pre-commit hook runs on the staged
change. The integrator inspects `git diff --cached`, scans for credentials and
production endpoint material, and records the council verdict in the commit
body.

### P0 council record

The durable `design-review` frontend created source bead `ga-sta1wa`, but the
workflow could not launch: the installed `gc` was one store migration behind
and the current workflows formula used an engine-minted `gc.kind=fanout`
field rejected by the checked-out engine. The source bead was closed with that
exact launch failure and no workflow verdict was claimed.

The fallback council used three independent read-only lanes over the proposal,
plan, and `origin/main`: CLI integration/testability, repository/release
grounding, and privacy/state/transport hardening. It ran an initial critique,
a repair pass, and targeted final rechecks. Confirmed findings changed the
documents to include:

- Linux/Darwin v0 support with fail-closed stubs elsewhere, an explicit
  same-user trust boundary, fd-relative storage, and exact home predicates;
- entropy-free disable/pause, one root-global quota and cleanup budget,
  sparse-file convergence, random spawn tokens, and lock-owner rather than
  impossible process-count guarantees;
- outcome-first dispatcher recording, a generated built-in membership table,
  injected-argv root construction, additive provider-exit migration, and an
  early neutral child-env seam;
- endpoint-empty Stage 1a separated from policy/backend/key/canary activation,
  complete artifact-channel guards, and draft→attest→verify→publish ordering.

All three final rechecks returned clear with no open P0/P1. P2 findings must
still follow the per-slice disposition rule above.

## Dependency graph

```text
P0 hardened plan
 |
 v
S1 closed contract + inert release identity
 |
 v
S2a side-effect-free home provenance
 |
 v
S2b durable root + locks
 |
 v
S3 consent/notice/state machine
 |
 +------------------+
 |                  |
 v                  v
S4 bounded spool    S5 strict transport + signed pause codec
 |                  |
 +--------+---------+
          v
S6 uploader transactions + linearizable opt-out
          |
          v
S7 detached uploader process + private entrypoint
          +--------------------------+
                                     |
P0 -> H1 child-env policy -> E1 pack exit propagation
                                     +-> S8 city-independent controls
                                           |
                                           v
                                      S9 command census + classifier
                                           |
P0 -> E2a compatibility API -> E2b1/b2 bounded read/session waves
      -> E2c1/c2/c3 bounded mutation waves -> E2d remove exiting API/ratchet
                                           +-> S10 centralized invocation lifecycle
                                         |
                                         v
                                    S11b-d remaining recursive-gc closure
                                         |
                         +---------------+---------------+
                         |               |               |
                         v               v               v
                    S12a guards     S12b tech docs
                         |               |
                         v               |
                    S12c inert release gates
                         \               /
                          +-------------+
                                         v
S13 adversarial end-to-end, race, fuzz, and performance closure
          |
          v
Stage 1a endpoint-empty/default-off release candidate

External prerequisites (parallel, never guessed in this repository):
B1 policy/privacy approval ─┐
B2 dedicated GC backend ───┼─> R2 explicit-opt-in manifest -> R3 canary -> R4 default-on
B3 pause-key custody ──────┤
B4 canary criteria/owner ──┘
```

## Slice P0 — Harden and approve this plan

Write the repo-grounded plan, run the multi-persona implementation-plan
council, reconcile every confirmed finding, and create the implementation
epic and slice beads from the hardened dependency graph.

Evidence and acceptance:

- Every design requirement maps to at least one slice and verification.
- External deployment work is represented but cannot unblock a client build
  by guessing values.
- File ownership avoids concurrent writers on the same files.
- The bead DAG matches this document and records blocked Stage 0/2/3 work.
- The reviewed proposal and this plan are committed together before code.
- The isolated feature branch is based directly on the current `origin/main`,
  not the unrelated dirty `refactor/sdo-wi6-w2` canonical checkout.

Verification: local Markdown links, task-payload dry run, council verdict, and
manual bidirectional traceability from design definition-of-done to slices.

## Slice S1 — Closed event contract and inert release identity

Add the additive `internal/productmetrics` package with closed enums and DTOs,
strict shape validation/encoding, permanent sentinel IDs, and an inert release
identity. The deterministic public example uses the permanent `help` sentinel,
so no temporary hand-written built-in command allowlist is needed. No
filesystem or network code lands in this slice.

Primary files:

- `internal/productmetrics/event.go`
- `internal/productmetrics/event_test.go`
- `internal/productmetrics/release.go`
- `internal/productmetrics/release_test.go`
- `internal/productmetrics/testdata/example-v1.json` and low-level sentinel
  wire vectors

TDD sequence and acceptance:

- First commit failing tests for exact one- and multi-event bytes using the
  permanent sentinels, UUIDv4, official semver, bounded GOOS, UTC-hour, and
  non-forgeable `CommandID` shape.
- Prove unknown/duplicate JSON fields and duplicate keys are rejected by the
  strict decoder used at disk and network edges.
- Prove the DTO contains no map, interface, raw JSON, time duration, error, or
  extension field by reflection/source guard.
- Prove the example uses fixed marked values plus the permanent `help` ID and
  requires no home, clock, random source, state, or network. Provide the typed
  hook S9's generated membership table will extend. Until S9, production
  accepts only permanent sentinels; tests inject any other closed membership
  through package-private dependencies.
- Prove default build identity is development, endpoint-empty, epoch zero,
  rollout default-off, and cannot be promoted from runtime inputs.

Focused verification: `go test ./internal/productmetrics` and normal/release
cross-build symbol inspection for the inert defaults.

## Slice S2a — Side-effect-free metrics home provenance

Extend `internal/gchome` without changing existing `Default()` callers so
product metrics receives explicit stable-vs-fallback provenance and can inspect
status without creating a fallback directory.

Primary files: `internal/gchome/gchome.go` and its tests.

TDD sequence and acceptance:

- Distinguish explicit `GC_HOME`, real user home, `MkdirTemp` fallback, and
  last-resort fallback without path-prefix guessing.
- The metrics resolver is side-effect-free, requires an absolute clean home,
  and implements the design's exact UID/mode/sticky-ancestor predicate. The
  effective home/product root must be effective-UID-owned 0700-equivalent;
  trusted ancestors are UID 0/effective UID and non-writable by group/other,
  with the narrow root-owned-sticky exception. It does not weaken existing
  non-metrics `gchome.Default()` behavior.
- Read-only resolution does not create any directory, including when user-home
  lookup fails.
- Tests cover root-owned and sticky ancestors, group-writable parents,
  wrong-owner homes, missing-component creation policy, and inherited
  ACL/default-ACL effects reflected in mode. Named ACL grants not visible to a
  CGO-free mode check are documented as expansion of the local trust boundary.

Focused verification: `go test ./internal/gchome` plus focused compatibility
tests for existing `Default()` callers.

## Slice S2b — Durable storage root and bounded advisory locks

Implement the private metrics root, fd-relative no-follow path operations,
durable atomic writes/deletes/renames, directory synchronization, and bounded
advisory locks on Linux and Darwin, consuming the S2a resolved-home contract.

Primary files:

- `internal/productmetrics/storage.go`
- `internal/productmetrics/storage_unix.go`
- `internal/productmetrics/lock_unix.go`
- `internal/productmetrics/platform_unsupported.go`
- corresponding package tests

TDD sequence and acceptance:

- Red tests cover 0700/0600 modes, file and parent fsync, temp cleanup,
  rename/parent-sync failure injection, stable lock inode handling, context
  timeout, and kernel release after process death.
- Hold validated directory descriptors and use relative no-follow operations
  so component swaps cannot redirect a write, claim, restore, or purge. Reject
  symlinks, non-regular files, hard-link surprises, ownership/mode drift, and
  unsafe parent components.
- Read-only opens do not create or repair the metrics root.
- Linux and Darwin behavior tests and unsupported-platform fail-closed compile
  checks are blocking.

Focused verification: productmetrics storage tests, subprocess lock tests with
repository-standard race timeouts, real Darwin CI coverage, and
`GOOS=windows go test -c` proving only the unsupported stub is reachable.

## Slice S3 — Consent, notice, identity, and state machine

Implement `config.toml`, strict TOML loading, effective-state precedence,
notice activation, `on`, status projection, recording permits, notice-version
invalidation, pause/resume state, and crash-safe CAS mutations. This slice does
not enqueue or upload.

Primary files:

- `internal/productmetrics/config.go`
- `internal/productmetrics/service.go`
- `internal/productmetrics/notice.go`
- state/notice/service tests

TDD sequence and acceptance:

- Start with the full state matrix: absent, pending, enabled, disabled,
  cleanup-pending, stale notice, server-paused, greater-epoch resume,
  environment-disabled, unsupported build, unstable home, and every corrupt
  or newer-schema input.
- `DO_NOT_TRACK` and `GC_DISABLE_USAGE_METRICS` implement the exact truth sets;
  neither can enable.
- No installation ID exists before a complete verified-TTY notice write and
  the one-record notice+ID+spool-generation commit.
- For pending or disabled activation, short/failed notice writes and every
  atomic-write `not-applied` failure leave the prior record and no final ID
  artifact. Stale-notice activation first durably raises the notice floor and
  clears the old spool, rereads that exact record, and only then performs
  notice output, entropy, and acceptance; every second-phase failure retains
  the inactive floor barrier. An applied, byte-exact record with
  parent-directory sync pending is the logical activation, is retried, and is
  visible as enabled to a separately opened peer; the activating invocation
  remains unrecordable. Disable, signed pause, and cleanup success remain
  durability-strict.
- Concurrent activation produces at most one complete notice, one ID, and no
  recording permit for either first invocation.
- `on` is TTY-only, idempotent while enabled, blocked by cleanup/pause/build/
  environment gates, and rotates identity only after `off`.
- Disable and signed-pause cleanup ownership uses a private persisted monotonic
  counter namespace plus monotonic state/cleanup epochs, never randomness, and
  retains a lease on the exact validated atomic config record. The namespace
  and exact-record incarnation participate in activation bases, cleanup
  owners, recording permits, and mutation CAS but not exported status. A
  mutation reloads the newly named record under the same `state.lock` and
  issues authority only from that post-write lease. A barrier,
  notice-floor invalidation, or cleanup completion that resets
  terminal-adjacent numeric counters first advances the namespace; in the last
  namespace, opt-out and completion use a same-namespace numeric reset plus
  atomic record replacement. Stale bases/tokens must lose even when the full
  numeric tuple repeats, including after corrupt recovery. The last namespace is a
  non-wrapping inactive durable fallback, and terminal/overflow shapes load
  fail-closed. `RecordingPermit.Close` releases the shared idempotent lease;
  read-only and rejected paths close leases internally. Injected entropy
  failure can block `on` or drop an event but cannot block durable opt-out or
  pause.
- Notice invalidation monotonically rebases under the same lock when its
  observed record was concurrently replaced but the currently named enabled
  record still has an older floor. It preserves ID/pause/cleanup state,
  reissues cleanup ownership on the replacement, treats an equal floor as
  converged, and never unlocks into an old-notice greater-epoch resume window.
- `OpenProduction` is lazy and non-creating. Preparing an invocation or reading
  status cannot create the metrics root, repair state, or start a process.
- Status is a pure projection over an already-open read-only view; byte-level
  root snapshots prove it never writes or repairs.

Focused verification: package tests plus `go test -race` for state/notice CAS
tests.

## Slice S4 — Bounded generation-scoped spool and `RecordOnce`

Add quota reservation, one-event files, generation isolation, pruning,
claim/restore/delete primitives, poison cleanup, and permit-checked
`RecordOnce`. No HTTP request or process spawn lands yet.

Primary files:

- `internal/productmetrics/spool.go`
- `internal/productmetrics/quota.go`
- spool/quota/record tests and fuzz corpus

TDD sequence and acceptance:

- Enforce root-global 4 MiB/5,000-event accounting across queue, inflight, and
  temp files in every generation, plus seven-day, 4 KiB event, 25-event batch,
  and 64 KiB request limits at every boundary.
- Reservation-before-write crash windows may overcount only; reconciliation
  is bounded and cannot undercount past a cap.
- Foreground enqueue performs no directory scan and drops before starting an
  uncancellable step when its injected decision budget is spent.
- Queue/inflight operations preserve oldest-first ordering, filename/body ID
  equality, uniqueness, original retry order, and directory-sync durability.
- Malformed, oversized, symlinked, duplicate-ID, non-current-generation, and
  schema-invalid files are cleanup-only and never enter a batch.
- Cap config at 16 KiB; quota/status/throttle at 4 KiB; names at 128 ASCII
  bytes; nesting at the exact declared layout; enumeration at 5,001
  event-bearing entries plus bounded metadata; and overflow-check every
  counter/time calculation. Cleanup uses one streaming root-global per-call
  budget—6,000 entries, 512 directories, 5 MiB bytes actually read, and 1 MiB
  names—shared by all generations and tree levels. Oversized/sparse files cost
  one entry/name and are unlinked without reading content, so declared size
  cannot prevent convergence. Empty/malformed directories
  consume it too, so an adversarial tree yields cleanup-pending rather than a
  per-generation multiplier, unbounded work, or false success.
- `RecordOnce` is first-attempt-wins and revalidates every permit component
  under the state lock; stale permits silently drop.

Focused verification: package/race tests, cap/property tests, and benchmark
report for enqueue separate from process spawn.

## Slice S5 — Strict transport, acknowledgement, and signed-pause codec

Implement pure batch construction, the dedicated HTTPS client, strict response
decoding, exact acknowledgement set equality, and Ed25519 signed-pause
verification. Response application to state/spool remains in S6.

Primary files:

- `internal/productmetrics/upload.go`
- `internal/productmetrics/pause.go`
- transport/pause tests and shared vectors

TDD sequence and acceptance:

- Capture final HTTP bodies and prove they byte-match queue/example encoders;
  the envelope adds only schema version and events.
- Reject non-HTTPS production URLs, userinfo/query/fragment, unexpected ports,
  every redirect, proxy environment, custom-CA environment, cookies,
  compression, default/global clients, arbitrary headers, and over-limit
  bodies.
- Enforce connect/TLS/header/total deadlines and one HTTP attempt per child.
- Only exact full-batch `accepted` 2xx or `duplicate` 409 acknowledgements are
  deletable; empty, partial, duplicate-key, wrong-content-type, extra-field,
  wrong-action, and mismatched-ID responses are retryable restores.
- Verify canonical signed 410 vectors and reject bit flips, duplicate/unknown
  keys, wrong key/app/release/epoch, bad encoding, replay against a newer
  permit, and oversized/HTML responses.
- Stage 1a production identity embeds an empty approved-key set; vectors use a
  marked test key through the package-private/testhook constructor only.

Focused verification: package tests against `httptest` TLS servers and no
external network.

## Slice S6 — Uploader transactions and linearizable opt-out

Join state, spool, and transport under the fixed lock order. Implement uploader
claim/start/wait/apply/restore, pause invalidation/purge, greater-epoch resume,
and `DisableAndPurge` with the full cleanup-token handshake.

Primary files:

- `internal/productmetrics/uploader.go`
- `internal/productmetrics/control.go`
- `internal/productmetrics/spool.go`
- `internal/productmetrics/storage.go`
- `internal/productmetrics/storage_unix.go`
- concurrency/crash integration tests inside the package
- focused Unix spool and storage hostile-filesystem tests

TDD sequence and acceptance:

- A blocked test uploader proves all documented `off` outcomes, including
  already-disabled, durable-disable failure, quiescence timeout, cleanup
  retry, concurrent state change, corrupt-safe recovery, and unsafe-root
  failure.
- No exit-zero path bypasses `uploader.lock`; `off` opens one exact mutable root
  descriptor before its initial state observation, derives every disable or
  pending-cleanup authority through that descriptor, and retains it across
  disable, uploader quiescence, cleanup, and final proof. A component
  replacement therefore cannot redirect either the barrier or a peer-successor
  proof to another root. Success is established while holding uploader then
  state locks with disabled durable, ID/generation absent, queue/inflight empty
  and synced, quota reset, and cleanup clear.
- Each cleanup call performs bounded work. Durable disable happens first; an
  oversized or corrupt tree spends the one S4 root-global budget and returns
  nonzero cleanup-pending until repeated `off` calls finish; entropy failure
  never prevents the disable point.
- Repeated `off` converges with a multi-gigabyte sparse poison file followed by
  valid/later entries while respecting the global entry/directory/read/name
  limits.
- Hostile-root RED tests drive every production root atomic writer through the
  strict two-state local marker protocol: durable zero-byte, non-authorizing
  INTENT; matching empty 0600 root temp created with `O_EXCL`; then durable
  BOUND before any payload byte. BOUND is exactly `32+N` bytes, where
  `N <= 128` is the basename byte length: `[0:8]` is ASCII `GCPMRTJ1`, `[8]`
  is state `0x02`, `[9]` is `uint8(N)`, `[10:16]` is zero, `[16:24]` and
  `[24:32]` are the big-endian nonzero uint64 device and inode, and `[32:]` is
  the exact canonical basename. Exact length/magic/state/reserved/identity/name
  equality and the 160-byte total cap are blocking. This is a local-storage
  codec only; request, response, event wire, and server behavior do not change.
- Crash tests cover marker/journal/root sync, temp creation, BOUND durability,
  payload, rename, root sync, and marker retirement. A crash after empty temp
  creation but before valid durable BOUND preserves the mapped root entry,
  exposes conservative manual cleanup pending, and proves that it contains no
  sensitive bytes. Marker/journal/temp type, ownership, link count,
  same-device relationship, and exact enumerated/opened/named incarnation are
  revalidated at every authority boundary.
- Cleanup reserves and charges the 161-byte maximum-plus-one marker-read
  envelope to the original shared read meter. It processes at most 64 entries
  and performs a separately entry/name-charged 65th iterator read as the
  overflow sentinel; a present 65th entry cannot be mistaken for EOF.
  Only exact BOUND device/inode authority may unlink its mapped temp.
  INTENT-with-live-temp, malformed, mismatched, replaced, cross-device, or
  over-budget evidence never mutates the mapped root and cannot certify clean.
  Unjournaled lookalikes and arbitrary residue are likewise preserved without
  descent.
- Root clean-name policy follows implemented ownership. S6 explicitly treats
  future `status.toml` and `spawn-throttle` files as unknown preserved residue;
  S8 and S7 may add each name only with its landed handler and exact cleanup/
  proof tests. Once handled, a declared control name with a non-authorizing
  filesystem shape remains preserved but maps to existing unrecognized-root
  manual-cleanup guidance; transient I/O and replacement failures remain
  retry-only.
- Main and peer-successor success run the same bounded, namespace-read-only
  settled-journal/root proof. After that proof, the peer path reloads and
  identity-leases the exact expected clean-disabled successor; the main path
  likewise revalidates its exact final record after the final-config journal
  proof. Any field, namespace, generation, or incarnation change is
  `state-changed-concurrently`. The final state write and proof consume the
  original sweep meter.
- Split sender tests prove request initiation occurs in `Start` under the final
  state lock, `Wait` runs outside state while `uploader.lock` remains held, and
  disable and send are linearly ordered at that start boundary. After a start,
  settlement uses a fixed 12-second `context.Background()`-derived context,
  independent of caller cancellation, while the HTTP attempt keeps its
  five-second deadline and `off` keeps its 12-second uploader wait.
- Sender errors always restore a current claim, including cancellation and
  deadline failures. A restore destination collision first identity-leases and
  byte-proves both same-device copies, then atomically exchanges the exact
  claimed inflight inode into the canonical queue name. Only a durably synced
  exchange may authorize identity-bound deletion of the displaced destination
  now named in inflight; unsupported, not-applied, ambiguous, or sync-pending
  exchange preserves both authorities and fails conservatively. Every ordinary
  root-descendant open and event-file read rejects a parent-device mismatch
  before work below that boundary; the metrics root alone may differ from its
  lexical `GC_HOME` parent.
- Every greater-epoch resume reacquires uploader then state locks and reproves
  the clean spool tree, zero quota, and settled root journal, including a visible
  pause-cleanup successor left by a failed post-transition proof; uncertainty
  cannot create a new generation.
- Once durable disable lands, stale enqueue permits, upload permits, responses,
  activation, and pre-disable `on` transitions cannot revive or send data.
- A valid 410 persists the pause barrier before purge; purge failure leaves
  cleanup pending and covered generations forever non-uploadable.
- Response/crash replay proves exact-ack deletion and conservative quota
  accounting are idempotent.

Focused verification: package race tests, subprocess contention tests, and the
blocking uploader/off matrix.

## Slice S7 — Detached uploader and private process entrypoint

Add the 60-second spawn-attempt record, single-uploader child, cooperative
ten-second work budget, Linux/Darwin detachment, null stdio, minimal
environment, recursion guard, and the private sentinel handled before Cobra,
packs, OTel, or normal metrics setup.

Primary files:

- `internal/productmetrics/spawn.go`
- `internal/productmetrics/spawn_unix.go`
- `internal/productmetrics/spawn_unsupported.go`
- `cmd/gc/productmetrics_adapter.go`
- `cmd/gc/productmetrics_testhook.go` under the test-only build tag
- minimal early-entry change in `cmd/gc/main.go`
- process tests using the `productmetrics_testhook` build tag

TDD sequence and acceptance:

- In the same root and `state.lock` transaction that just made the event
  durable, reserve the strict `throttle_schema=1`/canonical UUIDv4/canonical
  UTC record within the existing 50-ms decision window. Exactly 60 seconds
  permits replacement. Missing, corrupt, oversized, expired, and
  future/rollback records are replaced once; arbitrary read uncertainty and
  every not-applied or sync-pending atomic outcome fail closed. A generated
  token equal to any canonical UUIDv4 recoverable from the bounded prior bytes
  cannot spawn, including malformed duplicate-key orders. Entropy and every
  reservation, retained-lease-close, or start failure leave the event result
  `RecordStored`.
- The parent closes queue/generation descriptors, the config lease,
  `state.lock`, and the retained root before executable/environment resolution
  or process `Start`; any close error suppresses Start. The child opens one
  root, acquires `uploader.lock` once, and retains both capabilities through
  claim, request initiation, and settlement. It validates the exact token
  under `state.lock` before claim and again immediately before `Start`; it
  never validates and then releases and reacquires `uploader.lock`.
- Prove at most one reserved attempt per 60 seconds and at most one
  `uploader.lock` owner/network-active uploader, not impossible process
  cardinality. Stale/losing children perform zero network work and exit; a
  child stuck in uninterruptible local I/O may overlap as an OS process after
  the lease, but cannot own a second lock. `off` then times out cleanup-pending
  rather than claim success. A valid replaced-token or no-batch child returns
  success before production transport construction and performs zero network
  work. The timestamp is a parent replacement throttle, not a child-token TTL;
  an exact token remains current until replacement.
- Production `Start` handshakes on actual `RoundTrip` entry while the final
  state lock is held. A pre-entry error/cancellation permanently aborts that
  gate; after entry, `Wait` only observes completion outside `state.lock`.
- Linux/Darwin use `Setsid`, cwd `/`, and null stdin/stdout/stderr. Long-lived
  parents call one asynchronous `Wait` owner so completed children are reaped
  without blocking the command; a successful process start followed by a
  parent-descriptor close error still schedules exactly one `Wait`. There is no
  `Process.Release`; short-lived parents may exit and let the OS reparent the
  detached child.
- Snapshot the exact sorted positive-allowlist environment: pinned `GC_HOME`,
  recursion marker, validated absolute HOME/TMPDIR/XDG paths, and only the
  reviewed locale variable names. `PATH`, proxy, CA, loader, `GODEBUG`, OTel,
  Beads OTel, usage/cost, arbitrary parent variables, credentials, and secrets
  are absent.
- Every argv beginning with `__gc-product-metrics-uploader-v1` is consumed
  before Cobra, packs, and OTel, even when malformed. The exact recursion
  marker is required before storage/network. The private child does not emit
  product events or write normal command streams.
- Normal release builds contain no test endpoint/client injection symbols.
- Unsupported-platform private entry returns before selecting a runner or
  inspecting home/root state, and spawn returns before executable/env/throttle
  work. Supported Linux/Darwin targets and unsupported Android/Windows targets
  are compile-checked. iOS selects the unsupported file set and is
  compile-checked when an Apple SDK is available.

Focused verification: package tests, a separately built tagged `gc` process
binary (not ambient testscript re-exec), normal binary symbol scan, real
Linux/Darwin coverage, and unsupported-platform compile checks.

## Slice S8 — City-independent `gc metrics` commands

Add `gc metrics status|on|off|example` and the narrow early argument selector
that lets these commands run without city resolution or pack discovery. This
slice starts after S7 and E1 so injected-argv root selection targets the final
typed pack-discovery API once, without rebasing concurrent edits.

Primary files:

- `cmd/gc/cmd_metrics.go`
- `cmd/gc/productmetrics_adapter.go`
- `cmd/gc/main.go`
- command/unit/testscript coverage

TDD sequence and acceptance:

- Commands work under corrupt city config, unavailable DB/supervisor, and held
  pack-cache locks; persistent `--city`/`--rig` grammar and `--` termination
  cannot trick the selector.
- Preserve `newRootCmd(stdout, stderr)` for existing callers; production uses
  `newRootCmdWithOptions` with injected `run(args)`. Pack discovery and the
  credential-helper check consume injected argv, never ambient `os.Args`.
- Status default/JSON redact IDs; `--show-installation-id` is deliberately
  noisy and accurately explains linkability/deletion limits.
- Example is byte-identical in every state and `--json` writes only JSON to
  stdout through the production encoder, not the normal CLI JSON envelope.
- `off` is non-TTY/network independent and its success/failure output matches
  each result row without leaking paths or event bodies.
- Control commands remain excluded from product metrics and retain ordinary
  OTel startup/shutdown behavior.
- A separately built `-tags productmetrics_testhook` binary runs the successful
  vertical control flow against loopback: status/no-create → verified-TTY `on`
  → one testhook-injected `help` event through the real service adapter →
  redacted status → `off` → disabled status, plus exact
  `example --json`. The test simultaneously holds/corrupts city and pack state
  to prove the injected-argv control route is independent. Normal artifacts
  still fail symbol scans for the hook.

Focused verification: `go test ./cmd/gc` focused tests and testscript fixtures.

## Slice S9 — Complete command census and structural classifier

Generate and commit the finite built-in Cobra census after forcing lazy Cobra
help/completion nodes. Annotate built-ins and every discovered namespace/leaf,
canonicalize aliases, and classify help/version/unknown/pack-command and
reviewed exclusions without encoding argv values. S9 begins only after E1; E1
is the sole writer for pack-discovery annotations and S9 only consumes/tests
that surface.

Primary files:

- `cmd/gc/metrics_census.go`
- generated census manifest and generator/checker
- `internal/productmetrics/command_ids_gen.go`
- `cmd/gc/metrics_lifecycle.go`
- minimal annotations in command constructors/discovery
- structural census tests

TDD sequence and acceptance:

- Enumeration fails for every new/missing/duplicate executable, group, hidden
  built-in node, alias, annotation, or notice/recording classification. The
  manifest contains one synthetic wildcard `pack-command`, never runtime pack
  names.
- Discovered intermediate and leaf nodes can produce only `pack-command`;
  binding, pack, command path, and user args are unreachable from the DTO.
- Help, version, completion, JSON/schema/contract, hooks, credentials,
  service/private modes, and managed contexts match the closed matrix.
- Recognized invalid flags/args retain the recognized canonical ID; unknown
  root/nested input becomes only `unknown`.
- Every executable has exactly one recording owner: immediate wrapper for a
  normal leaf or explicit deferred typed outcome for a root/manual-group/lazy
  dispatcher, never both.
- The committed census has a disposition for every deferred dispatcher, and a
  structural registry test proves each `deferred-outcome` annotation names
  exactly one typed resolver/callback. Zero-arg/manual groups have explicit
  help/unknown policy; lazy pack uses E1's pre-resolved action.
- S9 is the sole owner of the production built-in membership table.
  Regeneration feeds the S1 validator directly; no duplicate or temporary
  allowlist remains. The S1 public example remains valid because `help` is a
  permanent sentinel.

Focused verification: focused classifier tests and deterministic manifest
regeneration diff.

## Slice H1 — Shared recursive-child environment policy

Land the small ownership seam needed by pack execution and later recursive
child migration before either touches call sites. Existing `internal/execenv`
owns the canonical variable name and remove-then-append operation; the CLI
adapter owns `cmd/gc/productmetrics_child_env.go` over that primitive.

Acceptance:

- Applying the policy yields exactly one `GC_DISABLE_USAGE_METRICS=1`, is
  idempotent, and never adds/removes/translates `BD_DISABLE_METRICS` or OTel
  variables.
- Slice E1 can use the helper for pack children without creating an ad-hoc
  implementation that S11 later replaces.
- Lower-layer shell/template generators can use the neutral package without an
  upward dependency on `cmd/gc` or on the productmetrics service.

Focused verification: pure env-list/map tests and import-boundary checks.

## Slice E1 — Typed pack outcomes and removal of pack `os.Exit`

Before the central lifecycle can prove error and OTel-shutdown behavior, make
both eager and lazy pack dispatch return typed `{handled, classification,
exitCode}` outcomes and `exitForCode` errors instead of terminating the
process. Split resolution from execution so help, unknown, and pack-command
are known before the single deferred recording attempt.

Primary files: `cmd/gc/cmd_commands.go`, `cmd/gc/cmd_pack_commands.go`, and
their existing/focused tests. E1 also owns all wildcard annotations in pack
discovery; S9 consumes and verifies them but does not edit discovery code.

Acceptance:

- Eager/lazy pack exit 42 returns process code 42 through `run` without killing
  the test process; OTel defers and later lifecycle hooks remain reachable.
- Success, nonzero, and help each produce one typed `pack-command` outcome;
  binding/path/arguments cannot reach the recorder.
- Every discovered namespace, intermediate, and leaf carries the wildcard
  annotation, while pack child environments receive only the GC metrics
  disable variable.

Focused verification: pack command tests plus process cases for exit codes and
output parity.

## Slices E2a–E2d — Propagate provider construction errors

`newSessionProviderFromContext` currently exits and has roughly thirty
production callers. Migrate it before S10 in three independently compilable,
upstreamable commits rather than hiding the breadth in lifecycle glue.

### E2a — Provider factory API and characterization

Add error-returning compatibility variants such as
`newSessionProviderWithError`, `newSessionProviderForCityWithError`, and status
variants while retaining the current exiting wrapper names/signatures. Add
characterization/error tests for both paths; no production caller changes yet,
so this commit remains compilable.

Primary file: `cmd/gc/providers.go` and provider tests.

### E2b1–E2b2 — Session/read/runtime caller family

Migrate callers to propagate a normal contextual error and process exit code
rather than terminate, in two commits of roughly three production files each:

- **E2b1:** `cmd_session.go`, `cmd_session_reset.go`, and
  `session_logs_resolve.go` plus focused tests.
- **E2b2:** `cmd_runtime_drain.go`, `cmd_status.go`, and `cmd_citystatus.go`
  plus focused tests.

### E2c1–E2c3 — Lifecycle/mutation caller family

Migrate the mutation/lifecycle callers in three bounded commits:

- **E2c1:** `cmd_start.go`, `cmd_stop.go`, and `cmd_restart.go`.
- **E2c2:** `cmd_handoff.go`, `cmd_nudge.go`, and `cmd_sling.go`.
- **E2c3:** `cmd_doctor.go`, `session_template_start.go`, and any remaining
  factory caller found by the blocking source census. Each wave owns its
  focused tests and must leave no unhandled error.

### E2d — Remove exiting compatibility wrappers and enable the ratchet

After every caller uses an error-returning variant, delete the exiting
wrappers, rename error-returning variants to the canonical names where that
improves the API, update test seams such as `sessionProviderForStopCity`, and
enable the zero-normal-path-exit source ratchet.

Shared acceptance for E2a–E2d:

- A broken provider returns a normal error/exit 1, preserves JSON failure
  formatting, and executes caller defers.
- No production caller ignores the new error or creates an alternate provider
  path.
- After E2d, the AST ratchet permits `os.Exit` only in `main`, named private/
  watchdog entrypoints, and the reviewed emergency supervisor hard exit.

Focused verification: provider tests, each caller-family test set, and the
exit-bypass source census after every wave.

## Slice S10 — Centralized invocation lifecycle and output invariance

Install one wrapper over the complete tree, convert the execution funnel to
`ExecuteC`, type early JSON outcomes, capture the immutable invocation context,
and call `RecordOnce` at the earliest resolved identity. Normal leaves record
immediately before their handler; root/manual-group/lazy dispatchers record
only after their typed outcome is known. Do not add metrics calls to ordinary
individual command handlers.

Primary files:

- `cmd/gc/metrics_lifecycle.go`
- `cmd/gc/main.go`
- `cmd/gc/json_schema.go`
- focused lifecycle/output tests

TDD sequence and acceptance:

- Exercise success, handler/pre-run/flag/arg errors, unknowns, help paths,
  version, completion, schema/contract, JSONL, panic, long-running start, and
  early outcomes; every eligible invocation attempts exactly once.
- The first-attempt guard suppresses reclassification even when enqueue fails.
- A dispatcher can never record `unknown` before later resolving help or
  `pack-command`; census tests reject double ownership by wrapper and outcome.
- Call-order spies prove every classification/attempt occurs after typed
  resolution but before the dispatcher's first observable output or child
  execution. The implementation may not infer an outcome from returned error
  text after side effects have begun.
- Pending/stale notice invocation snapshots remain unrecordable after they
  activate; a greater-epoch resume invocation is also unrecordable.
- Byte-for-byte stdout, stderr, exit code, JSON buffering, and OTel shutdown
  match disabled baseline for every injected product-metrics failure. The only
  allowed delta is the complete pending-notice TTY text.
- The tagged real-binary flow from S8 is repeated with an ordinary eligible
  `gc help` invocation, proving the centralized lifecycle—not a test-only
  recorder—produces the single captured event.

Focused verification: focused `cmd/gc` lifecycle matrix, race tests, and normal
fast unit shard containing the touched package.

## Slices S11b–S11d — Close remaining recursive-gc suppression holes

Centralize the `GC_DISABLE_USAGE_METRICS=1` child-environment policy at every
Gas City-owned recursive invocation. The neutral env operation lives below
`cmd/gc` where lower-layer shell/template generators need it; Go process sites
use one CLI helper over that primitive. Neither layer may set Beads metrics
variables.

Primary files:

- `cmd/gc/productmetrics_child_env.go`
- direct process sites currently including `cmd_perf.go`, `cmd_prompt.go`,
  `cmd_nudge.go`, `cmd_github.go`,
  `cmd_supervisor_lifecycle.go`, `cmd_start_drift.go`, and
  `cmd_agent_script.go`
- generated/managed sites or reviewed exclusions currently including
  `template_resolve.go`, `bd_env.go`, `store_target_exec.go`, `hooks.go`,
  `mcp_integration.go`, `skill_integration.go`,
  `beads_provider_lifecycle.go`, `internal/config/config.go`,
  `internal/hooks/hooks.go`, and core bootstrap agent TOML
- AST/source census tests

Commit boundaries:

- **S11b direct children:** hook, perf, prompt, GitHub, nudge, and other direct
  `exec.Cmd` sites; a child spy proves the outer invocation may record once and
  each child records zero.
- **S11c supervisor/service children:** supervisor lifecycle, drift restart,
  generated `ExecStart`, and service processes; process/content tests pin every
  generated environment.
- **S11d templates/assets/materialization:** pack execution not already handled
  by E1, agent templates, hooks, MCP/skill materialization, beads lifecycle,
  config strings, and bootstrap TOML; content guards plus the final census pin
  every site or reviewed exclusion.

TDD sequence and acceptance:

- Every first-party `os.Executable`, `GC_BIN`, literal/indirect `gc` exec, and
  generated `ExecStart` site is either routed through the helper or has a
  reviewed non-recursive exclusion.
- Managed runtime templates set the GC-only disable as defense in depth;
  marker classification still excludes them if it is absent.
- Template/string producers in `internal/config`, `internal/hooks`, MCP/skill
  materialization, and bootstrap pack assets are covered without introducing
  an upward import into `cmd/gc`.
- Source ratchets prove no `BD_DISABLE_METRICS` mutation and preserve the E2
  exit allowlist. E1's already-migrated `cmd_commands.go` remains a census
  assertion, not an S11 writer.

Focused verification: the named family spy/content tests after each commit,
then the complete self-exec census after S11d.

## Slice S12a — Architecture, privacy, and exit/self-exec ratchets

Land focused blocking guards after the complete call graph exists. Keep the
expensive `go/packages`/SSA analysis in a dedicated guard package/tool and one
`preflight-static` CI target instead of multiplying it across every `cmd/gc`
test shard.

Primary files: a focused `internal/productmetricsguard` or equivalent tool,
source/AST tests, `Makefile`, `.github/workflows/ci.yml` preflight wiring, and
boundary fixtures. S12a is the sole owner of generic Make/CI preflight wiring;
S12c consumes that target and owns only artifact/release workflows.

Acceptance:

- A positive production dependency allowlist rejects productmetrics imports of
  events, telemetry, usage, extmsg, eventfeed/export, API, or dashboard code.
- Reachability from the adapter and metrics commands rejects event/OTel/usage/
  export/API/dashboard sinks and `.gc/events.jsonl` writers, while allowing
  `main.go` to host independent lifecycle systems outside the adapter graph.
- Route/OpenAPI/event registry/generated-client snapshots are byte-identical.
- Exit and recursive-gc censuses fail on new unreviewed sites and prove no
  Beads metrics variable is changed.

Focused verification: the new static target once, ordinary package tests, and
negative mutation fixtures proving each guard fails.

## Slice S12b — Generated CLI and technical independence docs

Regenerate `docs/reference/cli.md` and update the existing events,
trust-boundary, environment, and historical technical docs to distinguish
product metrics from operational events/export, OTel, local costs, and Beads.
Do not publish `usage-metrics.md`, a privacy URL/contact, final notice golden,
or retention approval before B1.

Acceptance:

- Generated CLI includes every metrics command/flag and remains in sync.
- Technical docs accurately state endpoint-empty/default-off development,
  test, CI, edge, RC, and stable behavior plus the two disable variables.
- No placeholder privacy page is added to `docs/docs.json`; after B1, the real
  page and navigation entry must land together so docsync remains blocking.

Focused verification: command doc generation and `go test ./test/docsync`.

## Slice S12c — Inert evidence schemas and artifact-channel guards

Add versioned backend/release evidence schemas and a two-mode checker whose
Stage-1 path validates negative/non-production fixtures but cannot emit
official constants. Guard every current artifact channel, without requiring
missing B1–B4 evidence merely to publish an endpoint-empty release.

Primary files:

- `engdocs/evidence/product-metrics/**` schemas and unmistakably
  non-production fixtures
- `scripts/check-product-metrics-release` and tests
- `cmd/gc/cmd_version.go`, `.goreleaser.yml`
- `.github/workflows/release.yml`, `rc-release.yml`,
  `gc-edge-publish.yml`, `rc-gate.yml`, and `container-scan.yml`

Acceptance:

- Schema/negative mode rejects malformed/unknown fields and can validate
  checker behavior but cannot emit endpoint, key, privacy URL, or official
  identity constants.
- Activation mode rejects missing, expired, unhashed, unapproved,
  endpoint-mismatched, notice/schema/example/privacy drift, absent backend
  evidence, stale canary, wrong commit/version, key mismatch, and unattested
  artifacts.
- Local, CI, stable, RC, edge, RC-gate snapshot, and container outputs remain
  endpoint-empty/default-off and contain no testhook symbol. Edge remains a
  development contract-radar artifact, not a canary.
- The future activation workflow contract is build/upload-as-draft, bind
  manifest plus exact artifact hashes in an attestation, verify, then publish;
  Homebrew follows publish. It is tested as a schema/state machine but not
  activated without B1–B4.

Focused verification: checker tests, artifact matrix builds/symbol scans,
`goreleaser check`, and deliberately rejected activation fixtures.

## Slice S13 — Adversarial closure and Stage 1a release candidate

Run the cross-slice hostile corpus, fuzz/property/race/process matrices,
performance characterization, and full repository gates. Fix only defects
within this feature; capture unrelated baseline failures with evidence.

Acceptance:

- Adversarial secrets placed in args, flags, environment, cwd, names, output,
  errors, and pack metadata never appear verbatim, hashed, or encoded in queue
  files or captured HTTP requests.
- Concurrent first-run/on/off/pause/resume/record/upload races satisfy the
  exact generation/CAS winner rules under `go test -race` and subprocess tests.
- Offline, corrupt queue/state, disk-full, permissions, clock rollback, crash
  replay, endpoint outage, generic/signed 410, key rotation, upgrade, and
  downgrade drills converge safely.
- Symlink/component swaps, hard links, FIFOs/devices, replaced lock paths,
  relative homes, huge/sparse control files, overflow counters, excessive
  entries, old-generation accumulation, entropy failure, and delayed stale
  spawn attempts fail closed within bounded work.
- Disabled and pending paths perform no queue/network work. Enqueue benchmark
  reports the 20 ms target and decision-budget behavior separately from spawn.
- Fast unit, process-backed CLI shards, integration shards relevant to the
  boundary, vet, pre-commit, docs, Linux and real Darwin behavior, unsupported
  platform compile, and build all pass.
- Final council re-reviews the cumulative diff and verifies every design
  definition-of-done item is either satisfied or explicitly blocked on the
  named external B1–B4 inputs.

## External work and activation handoff

The following work must be created as blocked/human-owned beads, not silently
filled during client implementation:

- approve the exact notice/privacy URL/deletion contact and retention policy;
- choose the endpoint/service owner and implement the dedicated GC ingest,
  atomic dedupe, HMAC-and-discard, app-separated storage/rollups, retention,
  deletion, dashboards, alerts, backup/restore, and rate limits;
- establish signed-pause key custody and run shared-vector drills;
- approve the canary channel, selector, observation window, numeric error and
  dedupe thresholds, and approver;
- produce versioned backend evidence, canary result, release manifest, and
  signed artifact attestation.

Only those artifacts can authorize later council-reviewed slices:

- R2 final notice/privacy page/nav plus official explicit-opt-in manifest and
  constants (depends B1–B3 and S12c);
- R3 a dedicated canary workflow and evidence (depends R2+B4 and must not reuse
  edge publishing);
- R4 stable draft→attest→verify→publish and Homebrew release ordering (depends
  successful R3 evidence).

None is part of endpoint-empty Stage 1a.

## Definition of implementation-plan completion

The original decomposition was complete when its dependency graph and slice
beads were approved. For the maintainer-approved core PR scope above, Stage 1a
client completion instead requires the focused product-metrics suites,
generated-census check, normal repository gates, and one final council with no
open P0/P1. S12a's generic analyzer and S13's exhaustive hardening matrix are
not completion prerequisites. External activation remains blocked until
maintainers supply the four deployment decisions.
