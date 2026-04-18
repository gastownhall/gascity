# Dolt Regression Audit

## Scope

This audit covers the Dolt-related GitHub items for `gastownhall/gascity`
as of `2026-04-14`, plus the historical Dolt regressions that already drove
this redesign but are not currently labeled on GitHub.

Sources used for the live inventory:

- `gh issue list -R gastownhall/gascity --state all --label dolt`
- `gh pr list -R gastownhall/gascity --state all --label dolt`

This document is branch-local to `feature/beads-dolt-contract`. Its purpose
is to answer two questions for every Dolt-related item:

1. What exact regression test covers the failure?
2. Why does this branch prevent that failure from recurring?

## GitHub Label Snapshot

### Currently labeled Dolt issues

- `#245` `bd/gc dolt port env var mismatch: GC_DOLT_PORT vs BEADS_DOLT_PORT`
- `#323` `Dolt/beads reliability: journal corruption prevention, port pinning, boundary scan fixes`
- `#525` `bug: dolt server port drift and stale runtime state cause bd connection failures`
- `#560` `bug: gc dolt sync double-restarts dolt via start + ensure-ready race`
- `#630` `Orphaned dolt sql-server holding deleted inodes serves stale snapshot silently`
- `#684` `bug: gc-beads-bd exec provider missing CRUD operations — sessions cannot query beads`
- `#696` `bug: GC_BEADS=exec:gc-beads-bd silently no-ops all bead data operations in managed sessions`

### Currently labeled Dolt PRs

- `#454` `[bug] DoltServerCheck trusts stale GC_DOLT_PORT env var over current port file (ga-egq)`
- `#455` `[bug] DoltServerCheck trusts stale GC_DOLT_PORT env var over current port file (ga-egq v2)`
- `#459` `Shell scripts use stale GC_DOLT_PORT with no port file fallback — breaks after any dolt restart (ga-bys)`
- `#479` `fix(k8s): inject BEADS_DOLT_SERVER_HOST/PORT into pod env`
- `#554` `fix: strip all BEADS_* vars by prefix in mergeRuntimeEnv and gc bd`
- `#680` `fix: add PID-port coherence check and clean up stale dolt state (#525)`
- `#683` `fix: preserve rig-owned dolt port during city sync`
- `#685` `fix: exec beads provider CRUD passthrough to bd`
- `#686` `fix: route rig dolt env to scale_check regardless of city provider`
- `#687` `fix: route agent-session GC_BEADS to raw provider (#647)`

### Historical Dolt regressions still covered here

These drove the redesign and still deserve traceability even though they are
not in the current live `dolt` label snapshot:

- `#506` `gc doctor` subprocess port propagation drift
- `#541` ambient `BEADS_*` env leakage
- `#561` unusable canonical `.beads/` bootstrap state

## Summary

### Issues

| Item | Disposition | Branch status | Primary coverage |
|---|---|---|---|
| `#245` | `fixes: #245` | fixed | env projection + `gc bd` regression tests |
| `#323` | `fixes: #323` | fixed for the in-scope Dolt reliability symptoms | port pinning, canonical bootstrap, boundary-scan tests |
| `#506` | `fixes: #506` | fixed | doctor uses contract-resolved targets |
| `#525` | `fixes: #525` | fixed | stale runtime / stale port-file rejection |
| `#541` | `fixes: #541` | fixed | sanitize-and-populate env projection tests |
| `#560` | `fixes: #560` | fixed | idempotent start + transient probe no-restart tests |
| `#561` | `fixes: #561` | fixed | canonical bootstrap / normalization / deferred init tests |
| `#630` | `fixes: #630` | fixed | stale deleted-inode local server restart regression |
| `#684` | `fixes: #684` | fixed | `exec:gc-beads-bd` CRUD/session/mail regressions |
| `#696` | `fixes: #696` | fixed | managed-session `exec:gc-beads-bd` no-op regressions |

### PRs

| Item | Disposition | Branch status | Primary coverage |
|---|---|---|---|
| `#454` | `supersedes: #454` | superseded by broader contract fix | doctor uses canonical target, not ambient env |
| `#455` | `supersedes: #455` | superseded by broader contract fix | doctor uses canonical target, not ambient env |
| `#459` | `supersedes: #459` | superseded by broader contract fix | shell-facing env uses resolved projection |
| `#479` | `supersedes: #479` | superseded by broader contract fix | K8s projects canonical `GC_DOLT_*` then mirrors `BEADS_*` |
| `#554` | `supersedes: #554` | superseded by broader contract fix | sanitize-and-populate env projection |
| `#680` | `supersedes: #680` | superseded by broader contract fix | stale-state rejection + runtime-required managed resolution |
| `#683` | `supersedes: #683` | superseded by canonical endpoint ownership | explicit rig endpoint preserved over city sync |
| `#685` | `supersedes: #685` | superseded by exec-store bridge | `exec:gc-beads-bd` implements real data ops |
| `#686` | `supersedes: #686` | superseded by rig-scoped projection | scale-check gets rig Dolt env from resolved target |
| `#687` | `supersedes: #687` | superseded by broader session/store fix | session data ops work even through `exec:gc-beads-bd` |

## Detailed Issue Entries

### `fixes: #245` `GC_DOLT_PORT` versus `BEADS_DOLT_PORT` mismatch

- Historical failure:
  different callers projected different Dolt env families, so raw `bd`,
  `gc bd`, projected shells, and adapter code could connect to different
  servers.
- Regression tests:
  - `cmd/gc/bd_env_test.go`: `TestCityRuntimeProcessEnvStripsAmbientGCDolt`
  - `cmd/gc/bd_env_test.go`: `TestBdRuntimeEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig`
  - `cmd/gc/bd_env_test.go`: `TestSessionDoltEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig`
  - `cmd/gc/cmd_bd_test.go`: `TestGcBdUsesProjectionNotAmbientEnv`
  - `internal/beads/exec/exec_test.go`: `TestRunSanitizesAmbientLegacyAndStoreTargetEnv`
- Why this branch closes it:
  `cmd/gc/bd_env.go` is now the projection owner for GC-native Dolt env,
  and the compatibility mirror is derived from that same resolved target.
  Ambient `GC_DOLT_*` / `BEADS_*` state is explicitly stripped or ignored,
  so `gc bd`, sessions, and exec backends all see the same host/port/user.

### `fixes: #323` Dolt/beads reliability: journal corruption prevention, port pinning, boundary scan fixes

- Historical failure:
  this was a broad umbrella issue. The concrete Dolt failures in scope for
  this branch were unstable managed port discovery, stale compatibility
  port-file reuse, non-canonical runtime-state scanning, and partial
  canonical `.beads/` bootstrap state.
- Regression tests:
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentManagedDoltPortIgnoresNonCanonicalPackState`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentManagedDoltPortUsesCanonicalPackStateOnly`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentDoltPortPrefersRuntimeState`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentDoltPortIgnoresReachablePortFileWithoutManagedState`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestNormalizeCanonicalBdScopeFilesRepairsCityAndRigScopeFiles`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestNormalizeCanonicalBdScopeFilesMaterializesMissingMetadata`
  - `cmd/gc/cmd_rig_test.go`: `TestDoRigAdd_DoesNotWriteConfigWhenCanonicalBdNormalizationFails`
- Why this branch closes it:
  managed mode now has one canonical runtime publication path,
  compatibility port files are mirrors only, and canonical `.beads/` files
  are normalized or repaired through explicit GC-owned flows. Startup and
  `gc rig add` no longer leave partial authoritative state behind when
  normalization fails.

### `fixes: #506` `gc doctor` fails to propagate Dolt port to `bd` subprocesses

- Historical failure:
  doctor relied on ad hoc port/env discovery, so managed and external
  scopes could be diagnosed against the wrong server target.
- Regression tests:
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ManagedCityUsesRuntimeState`
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ManagedCityReportsStartHint`
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ExternalCityUsesCanonicalTarget`
  - `internal/doctor/checks_test.go`: `TestRigDoltServerCheck_ExplicitRigUsesCanonicalTarget`
  - `internal/doctor/checks_test.go`: `TestRigDoltServerCheck_InheritedRigDriftIsError`
- Why this branch closes it:
  doctor no longer invents its own Dolt target resolution. It consumes the
  same canonical contract used by the runtime and env projection layers, so
  managed scopes use runtime publication and external scopes use canonical
  `config.yaml` targets with the right fix hint class.

### `fixes: #525` port drift and stale runtime state break `bd` connectivity

- Historical failure:
  stale `.beads/dolt-server.port` files and dead runtime-state entries could
  remain reachable enough to trick callers into connecting to the wrong
  managed server.
- Regression tests:
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentDoltPortIgnoresReachablePortFileWithoutManagedState`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentDoltPortIgnoresDeadRuntimeStateAndPrunesDeadPortFile`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentDoltPortIgnoresReachablePortFileWhenManagedStateIsStopped`
  - `cmd/gc/bd_env_test.go`: `TestBdRuntimeEnvDoesNotUseStalePortFileWithoutManagedRuntimeState`
  - `internal/beads/contract/connection_test.go`: `TestResolveDoltConnectionTargetRequiresRuntimeForManagedScopes`
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ManagedCityReportsStartHint`
- Why this branch closes it:
  managed resolution is now runtime-state-first and runtime-state-required.
  The compatibility port file is diagnostic only. Dead or orphaned managed
  publications are rejected and pruned instead of becoming authority.

### `fixes: #541` environment sanitization leaks stale `BEADS_*` state

- Historical failure:
  callers inherited ambient `BEADS_*` / `GC_DOLT_*` values and overlaid new
  values incompletely, so sessions and helpers could silently talk to the
  wrong server.
- Regression tests:
  - `cmd/gc/bd_env_test.go`: `TestCityRuntimeProcessEnvStripsAmbientGCDolt`
  - `cmd/gc/bd_env_test.go`: `TestBdRuntimeEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig`
  - `cmd/gc/bd_env_test.go`: `TestSessionDoltEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig`
  - `internal/beads/exec/exec_test.go`: `TestRunSanitizesAmbientLegacyAndStoreTargetEnv`
  - `cmd/gc/cmd_bd_test.go`: `TestGcBdWarnsOnExternalOverrideDrift`
- Why this branch closes it:
  projection is now sanitize-and-populate, not merge-and-hope. GC-native
  code consumes resolved `GC_DOLT_*` values, and the `BEADS_*` mirror is a
  compatibility output derived from the same target after ambient drift has
  been cleared.

### `fixes: #560` duplicate lifecycle actions cause Dolt restart races

- Historical failure:
  concurrent or repeated lifecycle paths could restart Dolt unnecessarily,
  including the sync/start versus ensure-ready race.
- Regression tests:
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestGcBeadsBdStartIsIdempotentWhenAlreadyRunning`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestGcBeadsBdEnsureReadyDoesNotRestartAfterTransientTCPProbeFailure`
- Why this branch closes it:
  `gc-beads-bd` now fences startup with a lock, reuses a healthy existing
  managed server, and only restarts when the existing process is actually
  unusable. A transient probe miss no longer forces a second launch.

### `fixes: #561` bootstrap sync leaves unusable `.beads` state in fresh worktrees

- Historical failure:
  bootstrap and adoption could leave partially normalized canonical files,
  wrong `dolt_database` identity, or misleading success output on deferred
  init paths.
- Regression tests:
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestNormalizeCanonicalBdScopeFilesRepairsCityAndRigScopeFiles`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestNormalizeCanonicalBdScopeFilesMaterializesMissingMetadata`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestGcBeadsBdInitRepairsWrongDoltDatabaseFromExplicitCanonicalIdentity`
  - `cmd/gc/cmd_rig_test.go`: `TestDoRigAdd_DoesNotWriteConfigWhenCanonicalBdNormalizationFails`
  - `cmd/gc/cmd_rig_test.go`: `TestDoRigAdd_SkipDoltReportsDeferredInit`
  - `cmd/gc/lifecycle_coordination_test.go`: `TestSeedDeferredManagedBeadsUsesCompatCityExternalBeforeStartup`
  - `cmd/gc/lifecycle_coordination_test.go`: `TestSeedDeferredManagedBeadsUsesCompatExplicitRigEndpointBeforeStartup`
- Why this branch closes it:
  canonical `.beads/config.yaml` and `metadata.json` are now owned and
  normalized by GC. Bootstrap paths preserve pinned database identity,
  materialize missing canonical files deterministically, and fail before
  persisting partial authoritative state.

### `fixes: #630` orphaned Dolt SQL server holding deleted inodes serves stale snapshot silently

- Historical failure:
  a managed local Dolt process could still answer TCP and simple probes even
  after its underlying data files had been deleted, letting GC reuse a stale
  server instead of restarting it.
- Regression tests:
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestGcBeadsBdStartIsIdempotentWhenAlreadyRunning`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestGcBeadsBdEnsureReadyDoesNotRestartAfterTransientTCPProbeFailure`
- Why this branch closes it:
  `gc-beads-bd` now refuses to reuse a local managed server when the owning
  process has deleted cwd/data-dir inodes open. Healthy servers remain
  idempotent; stale orphaned ones are killed and restarted before GC updates
  runtime publication.

### `fixes: #684` `gc-beads-bd` exec provider missing CRUD operations; sessions cannot query beads

- Historical failure:
  `exec:gc-beads-bd` implemented lifecycle operations but not the exec store
  protocol, so session and mail paths saw empty or invalid bead responses.
- Regression tests:
  - `cmd/gc/cmd_session_test.go`: `TestCmdSessionList_ManagedExecLifecycleProviderReadsSessions`
  - `cmd/gc/cmd_mail_test.go`: `TestCmdMailInbox_ManagedExecLifecycleProviderReadsInbox`
  - `cmd/gc/cmd_bd_test.go`: `TestManagedExecBdRigStoreConsistentAcrossRawBdAndProviderStore`
  - `cmd/gc/cmd_bd_test.go`: `TestInheritedExternalExecBdRigStoreConsistentAcrossRawBdAndProviderStore`
  - `cmd/gc/store_target_exec_test.go`: `TestOpenStoreAtForCityExecBeadsBdProjectsScopedExternalDoltEnv`
- Why this branch closes it:
  `cmd/gc/gc-beads-bd` now implements the exec store protocol by bridging
  CRUD/list/get/update/dep operations through pinned `bd` commands, and the
  exec store opener projects the correct scoped Dolt env for
  `exec:gc-beads-bd`.

### `fixes: #696` `GC_BEADS=exec:gc-beads-bd` silently no-ops bead data operations in managed sessions

- Historical failure:
  managed-session flows could appear successful while all bead lookups were
  effectively no-ops under `exec:gc-beads-bd`.
- Regression tests:
  - `cmd/gc/cmd_session_test.go`: `TestCmdSessionList_ManagedExecLifecycleProviderReadsSessions`
  - `cmd/gc/cmd_mail_test.go`: `TestCmdMailInbox_ManagedExecLifecycleProviderReadsInbox`
  - `cmd/gc/cmd_bd_test.go`: `TestManagedExecBdRigStoreConsistentAcrossRawBdAndProviderStore`
- Why this branch closes it:
  the same store-bridge implementation that fixes `#684` now gives managed
  session and mail flows a real bead store instead of an exec provider that
  only answered lifecycle commands.

## Detailed PR Entries

### `supersedes: #454` stale `GC_DOLT_PORT` in `DoltServerCheck`

- Original PR intent:
  stop doctor from trusting stale ambient `GC_DOLT_PORT` over the current
  managed or canonical target.
- Regression tests:
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ManagedCityUsesRuntimeState`
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ManagedCityReportsStartHint`
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ExternalCityUsesCanonicalTarget`
- Why this branch supersedes it:
  doctor no longer resolves Dolt targets from ambient env. It uses the same
  contract-resolved managed runtime publication or canonical external target
  as the rest of GC.

### `supersedes: #455` stale `GC_DOLT_PORT` in `DoltServerCheck` v2

- Original PR intent:
  same failure class as `#454`, with a second attempt at the same narrow
  fix.
- Regression tests:
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ManagedCityUsesRuntimeState`
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ManagedCityReportsStartHint`
  - `internal/doctor/checks_test.go`: `TestDoltServerCheck_ExternalCityUsesCanonicalTarget`
  - `internal/doctor/checks_test.go`: `TestRigDoltServerCheck_ExplicitRigUsesCanonicalTarget`
- Why this branch supersedes it:
  same reason as `#454`, but with broader scope. The fix is no longer a
  doctor-only env precedence tweak; it is shared target resolution for both
  city and rig checks.

### `supersedes: #459` shell scripts use stale `GC_DOLT_PORT` with no port-file fallback

- Original PR intent:
  make shell-facing paths resilient after Dolt restarts instead of leaving
  them stuck on stale ambient port values.
- Regression tests:
  - `cmd/gc/template_resolve_workdir_test.go`: `TestResolveTemplateUsesCityManagedDoltPort`
  - `cmd/gc/cmd_bd_test.go`: `TestGcBdUsesProjectionNotAmbientEnv`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentDoltPortIgnoresReachablePortFileWithoutManagedState`
  - `cmd/gc/bd_env_test.go`: `TestBdRuntimeEnvDoesNotUseStalePortFileWithoutManagedRuntimeState`
- Why this branch supersedes it:
  shell-facing env is now projected from the resolved target. Managed shells
  use runtime publication, external shells use canonical endpoint config,
  and no caller needs to “fall back” from stale ambient env to a port file.

### `supersedes: #479` inject `BEADS_DOLT_SERVER_HOST/PORT` into K8s pods

- Original PR intent:
  make raw `bd` work in pods by ensuring the pod sees a usable server host
  and port.
- Regression tests:
  - `internal/runtime/k8s/provider_test.go`: `TestBuildPodEnvProjectsManagedDoltEndpoint`
  - `internal/runtime/k8s/provider_test.go`: `TestBuildPodEnvMirrorsBeadsEndpointFromProjectedGCDoltVars`
  - `internal/runtime/k8s/provider_test.go`: `TestBuildPodEnvRejectsHostOnlyProjectedTarget`
  - `internal/runtime/k8s/provider_test.go`: `TestBuildPodEnvUsesProviderManagedAlias`
- Why this branch supersedes it:
  K8s now consumes the canonical projected `GC_DOLT_*` target and mirrors
  `BEADS_DOLT_SERVER_*` from that one source. The old K8s-only env contract
  is compatibility-only, not authoritative.

### `supersedes: #554` strip all `BEADS_*` vars by prefix in runtime env merges

- Original PR intent:
  fail closed on inherited `BEADS_*` state instead of letting unknown vars
  leak into `bd` subprocesses.
- Regression tests:
  - `cmd/gc/bd_env_test.go`: `TestCityRuntimeProcessEnvStripsAmbientGCDolt`
  - `cmd/gc/bd_env_test.go`: `TestBdRuntimeEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig`
  - `cmd/gc/bd_env_test.go`: `TestSessionDoltEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig`
  - `internal/beads/exec/exec_test.go`: `TestRunSanitizesAmbientLegacyAndStoreTargetEnv`
  - `cmd/gc/cmd_bd_test.go`: `TestGcBdUsesProjectionNotAmbientEnv`
- Why this branch supersedes it:
  the branch-wide fix is stronger than prefix stripping in one merge helper.
  All GC-native Dolt env comes from one sanitize-and-populate projection
  layer, and the `BEADS_*` mirror is derived after that sanitization.

### `supersedes: #680` PID-port coherence and stale-state cleanup for `#525`

- Original PR intent:
  reduce false trust in stale managed Dolt state by tightening PID/port
  coherence checks and pruning dead state.
- Regression tests:
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentDoltPortIgnoresDeadRuntimeStateAndPrunesDeadPortFile`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestCurrentDoltPortIgnoresReachablePortFileWhenManagedStateIsStopped`
  - `internal/beads/contract/connection_test.go`: `TestResolveDoltConnectionTargetRequiresRuntimeForManagedScopes`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes`
- Why this branch supersedes it:
  the current fix is broader than PID-port coherence. Managed resolution now
  requires canonical runtime publication, prunes stale compatibility state,
  and refuses to reuse a local server that is clearly stale even if TCP
  still answers.

### `supersedes: #683` preserve rig-owned Dolt port during city sync

- Original PR intent:
  stop city-side sync from overwriting a rig’s own Dolt endpoint.
- Regression tests:
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestSyncConfiguredDoltPortFilesPreservesLegacyExplicitRigConfig`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestSyncConfiguredDoltPortFilesPrefersCanonicalExplicitRigEndpointOverCompatConfig`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestSyncConfiguredDoltPortFilesPreservesCanonicalCityAndExplicitRigOverCompatInputs`
  - `cmd/gc/beads_provider_lifecycle_test.go`: `TestValidateCanonicalCompatDoltDriftRejectsInheritedRigCompatOverride`
- Why this branch supersedes it:
  the branch formalizes endpoint ownership. Explicit rig endpoints are
  authoritative; inherited rigs are derived from the city. City sync cannot
  overwrite an explicit rig target because the canonical config makes the
  distinction explicit.

### `supersedes: #685` exec beads provider CRUD passthrough to `bd`

- Original PR intent:
  make `exec:gc-beads-bd` support actual bead CRUD instead of lifecycle-only
  operations.
- Regression tests:
  - `cmd/gc/cmd_session_test.go`: `TestCmdSessionList_ManagedExecLifecycleProviderReadsSessions`
  - `cmd/gc/cmd_mail_test.go`: `TestCmdMailInbox_ManagedExecLifecycleProviderReadsInbox`
  - `cmd/gc/cmd_bd_test.go`: `TestManagedExecBdRigStoreConsistentAcrossRawBdAndProviderStore`
  - `cmd/gc/cmd_bd_test.go`: `TestInheritedExternalExecBdRigStoreConsistentAcrossRawBdAndProviderStore`
  - `cmd/gc/store_target_exec_test.go`: `TestOpenStoreAtForCityExecBeadsBdProjectsScopedExternalDoltEnv`
- Why this branch supersedes it:
  the current branch contains the full exec-store bridge, not just a narrow
  patch for one caller. Session, mail, raw `bd`, and provider-store paths all
  exercise the same bridge.

### `supersedes: #686` route rig Dolt env to `scale_check` regardless of city provider

- Original PR intent:
  make rig-scoped `scale_check` subprocesses see the rig’s Dolt target even
  when the city provider would otherwise short-circuit env setup.
- Regression tests:
  - `cmd/gc/build_desired_state_test.go`: `TestBuildDesiredState_PoolCheckInjectsDoltPortForRigScopedAgent`
  - `cmd/gc/build_desired_state_test.go`: `TestBuildDesiredState_PoolCheckUsesExplicitRigPassword`
  - `cmd/gc/build_desired_state_test.go`: `TestBuildDesiredState_PoolCheckUsesManagedCityDoltPortWhenRigHasNoOverride`
  - `cmd/gc/pool_test.go`: `TestShellScaleCheck_NoBEADS_DOLT_SERVER_PORT_Injection`
- Why this branch supersedes it:
  pool and `scale_check` env now come from the same resolved rig target used
  everywhere else. The fix is no longer “special-case rig even if city is
  file”; it is “always project the right scope target into the caller.”

### `supersedes: #687` route agent-session `GC_BEADS` to raw provider

- Original PR intent:
  avoid crashing session data operations when `GC_BEADS` pointed at the
  lifecycle-only `gc-beads-bd` wrapper.
- Regression tests:
  - `cmd/gc/cmd_session_test.go`: `TestCmdSessionList_ManagedExecLifecycleProviderReadsSessions`
  - `cmd/gc/cmd_mail_test.go`: `TestCmdMailInbox_ManagedExecLifecycleProviderReadsInbox`
  - `cmd/gc/cmd_bd_test.go`: `TestManagedExecBdRigStoreConsistentAcrossRawBdAndProviderStore`
- Why this branch supersedes it:
  this branch removes the lifecycle-only cliff entirely by making
  `exec:gc-beads-bd` a valid data/store provider. Session data paths now work
  even through the wrapper, so correctness no longer depends on that one
  env-routing choice.

## Implementation Points That Enforce These Fixes

These regressions are prevented by a small number of shared mechanisms rather
than many one-off patches:

- `internal/beads/contract/connection.go`
  resolves canonical Dolt targets for managed, inherited, and explicit
  scopes.
- `cmd/gc/bd_env.go`
  is the projection owner for GC-native Dolt env.
- `internal/doctor/checks.go`
  consumes the same resolved target instead of ad hoc fallback chains.
- `cmd/gc/beads_provider_lifecycle.go`
  and `cmd/gc/gc-beads-bd`
  own managed lifecycle, runtime publication, canonical drift checks, and
  recovery behavior.
- `cmd/gc/gc-beads-bd`
  now also implements the exec store bridge for `exec:gc-beads-bd`.
- `internal/runtime/k8s/provider.go`
  projects pod env from canonical `GC_DOLT_*` state and mirrors `BEADS_*`
  only as compatibility output.

## Verification Command Set

These focused suites back the entries above:

```bash
go test ./cmd/gc -run 'TestGcBeadsBd(StartIsIdempotentWhenAlreadyRunning|StartRestartsServerHoldingDeletedDataInodes|EnsureReadyDoesNotRestartAfterTransientTCPProbeFailure)|Test(CurrentDoltPortIgnoresReachablePortFileWithoutManagedState|CurrentDoltPortIgnoresDeadRuntimeStateAndPrunesDeadPortFile|CurrentDoltPortIgnoresReachablePortFileWhenManagedStateIsStopped|NormalizeCanonicalBdScopeFilesRepairsCityAndRigScopeFiles|NormalizeCanonicalBdScopeFilesMaterializesMissingMetadata|GcBeadsBdInitRepairsWrongDoltDatabaseFromExplicitCanonicalIdentity)|Test(DoRigAdd_DoesNotWriteConfigWhenCanonicalBdNormalizationFails|DoRigAdd_SkipDoltReportsDeferredInit)|Test(ManagedBdRigStoreConsistentAcrossRawBdGcBdAndProviderStore|ManagedBdCityStoreConsistentAcrossRawBdGcBdAndProviderStore|InheritedExternalBdRigStoreConsistentAcrossRawBdGcBdAndProviderStore|ManagedExecBdRigStoreConsistentAcrossRawBdAndProviderStore|InheritedExternalExecBdRigStoreConsistentAcrossRawBdAndProviderStore|GcBdUsesProjectionNotAmbientEnv|GcBdWarnsOnExternalOverrideDrift)|Test(CmdSessionList_ManagedExecLifecycleProviderReadsSessions|CmdMailInbox_ManagedExecLifecycleProviderReadsInbox)|Test(OpenStoreAtForCityExecBeadsBdProjectsScopedExternalDoltEnv)|Test(BuildDesiredState_PoolCheckInjectsDoltPortForRigScopedAgent|BuildDesiredState_PoolCheckUsesExplicitRigPassword|BuildDesiredState_PoolCheckUsesManagedCityDoltPortWhenRigHasNoOverride)|Test(ResolveTemplateUsesCityManagedDoltPort)' -count=1 -timeout 1200s

go test ./internal/doctor ./internal/beads/contract ./internal/beads/exec ./internal/runtime/k8s -run 'Test(DoltServerCheck_ManagedCityUsesRuntimeState|DoltServerCheck_ManagedCityReportsStartHint|DoltServerCheck_ExternalCityUsesCanonicalTarget|RigDoltServerCheck_ExplicitRigUsesCanonicalTarget|RigDoltServerCheck_InheritedRigDriftIsError|ResolveDoltConnectionTarget|RunSanitizesAmbientLegacyAndStoreTargetEnv|BuildPodEnvProjectsManagedDoltEndpoint|BuildPodEnvMirrorsBeadsEndpointFromProjectedGCDoltVars|BuildPodEnvRejectsHostOnlyProjectedTarget|BuildPodEnvUsesProviderManagedAlias)' -count=1 -timeout 1200s
```
