# Finish canonical-path sweeps before CI enforcement

Planning bead: `ga-8t9qo7jr`

Owning architecture: `ga-4bqjjn`, decomposed by `ga-iawy13`

Prepared: 2026-11-20 UTC

Inventory base: `441dfd3e25bfc04e1577442fd7563aa5c3d36faf`

## Outcome

The strict guard `ga-iawy13.9` must follow the production sweeps. Every
remaining direct `filepath.EvalSymlinks` call is assigned to an existing
landing or a scoped follow-up below. The mayor chose this ordering; a
shrink-only guard allowlist is not approved.

The existing rule remains authoritative: comparison preparation uses
`internal/pathutil` at its ingest boundary; a deliberate existence or
resolvability check retains its direct call only with a specific adjacent
`canonical-path-exception` justification. The final guard must pass on the
actual target tree before it ships. Closing a build or review handoff does
not prove its change landed.

## Work packages and dependencies

| Bead | Scope | Intake | Prerequisites |
| --- | --- | --- | --- |
| `ga-mdl2a7` | Ten remaining `cmd/gc` calls | `needs-tests` → validator, then the same bead → builder | None |
| `ga-8kd2jcn9` | Eleven runtime/storage/infrastructure calls and three acceptance-support calls | `needs-tests` → validator, then the same bead → builder | Architecture decision `ga-540a8rdj` |
| `ga-dky8ikkx` | Ten sessionlog calls, reconciling the incomplete `.5` sweep | `needs-tests` → validator, then the same bead → builder | `ga-540a8rdj` and existing Kimi fix review `ga-31wjwgdk` |
| `ga-540a8rdj` | Classify native Kimi key and tmux socket boundaries against the existing rule | `needs-architecture` → architect | None |

`ga-mdl2a7` was the existing, unassigned next-batch inventory. This plan
activates and narrows it under the mayor's current request, preserving its
lineage. The other implementation scopes have separate beads; no duplicate
test-versus-build bead is created for any scope.

`ga-iawy13.9` directly blocks on all three sweep beads. Its existing
dependencies on infrastructure deploy `ga-3rkhtqsq`, fixture deploy
`ga-1nh67zro`, the original `.1`–`.8` records, and this planning bead are
preserved. The architecture and Kimi review prerequisites are transitive
through the affected sweeps. The sessionlog package must verify that the
existing Kimi fix actually reached its target base before starting; a review
close alone is insufficient.

## Complete inventory at the pinned base

The inventory scans tracked Go files for direct `filepath.EvalSymlinks(`
calls, excluding `_test.go`, `vendor/`, and `internal/pathutil/`. It includes
all platform files, regardless of the planning host's build tags. Function
names identify sites when line numbers move. These are source-inspection
results, not a run of the future guard.

| Coverage | Production calls | Acceptance-support calls |
| --- | ---: | ---: |
| Existing justified calls | 7 | 0 |
| Existing `.7` sweep, waiting on `ga-3rkhtqsq` | 10 | 0 |
| CLI follow-up `ga-mdl2a7` | 10 | 0 |
| Infrastructure follow-up `ga-8kd2jcn9` | 11 | 3 |
| Sessionlog follow-up `ga-dky8ikkx` | 10 | 0 |
| Total | 48 | 3 |

All 51 calls have an owner or existing justification. The 41 unmarked
production calls are covered by the pending `.7` landing and the three
follow-ups. The three non-`_test.go` acceptance helpers are recorded
separately and assigned explicitly so they cannot become an accidental
gap in a whole-tree scanner. This plan does not change the guard's agreed
scope. Seven existing justifications also need to be accepted by the real
guard; a fixed four-line comment-window heuristic is not that verification.

### CLI follow-up: `ga-mdl2a7`

All paths in this table are under `cmd/gc/`.

| File and line | Function |
| --- | --- |
| `api_state.go:2846` | `symlinkAwareContainment` |
| `bd_inventory_trust.go:92` | `statBdBinary` |
| `cmd_bd.go:217` | `resolveBdInvokingGCBinary` |
| `cmd_hook_claim.go:2999` | `hookClaimPathOutside` |
| `doctor_bd_inventory.go:94` | inventory check's `Run` method |
| `dolt_handoff_projection.go:136` | `committedBeadsHandoffOwnsScope` |
| `storage_reverse.go:2049` | `reverseCityTOMLPath` |
| `storage_snapshot_city.go:344` | `cityTOMLFile` |
| `storage_store_identity.go:156` | `canonicalIdentityPath` |
| `supervisor_binary_mismatch.go:256` | `resolveExecutablePath` |

The strict executable, store-identity, trust and confinement checks must
retain their required errors. The established pathutil missing-path
fallback cannot silently replace a security-relevant existence check.

### Infrastructure follow-up: `ga-8kd2jcn9`

| File and line | Function |
| --- | --- |
| `internal/beads/proxyendpoint/record.go:401` | `RootID` |
| `internal/citylayout/export.go:276` | `WorkDirExportEntries` |
| `internal/platform/filelock/filelock_windows.go:99` | `openDirectoryLock` |
| `internal/runtime/unix_listener.go:63` | `unixSocketPathCandidates` |
| `internal/runtime/tmux/server_socket_probe.go:270,274` | `socketBaseDir` |
| `internal/storebinding/beadsworkspace/provider.go:300` | `canonicalCityRoot` |
| `internal/storebinding/migration_guard.go:140` | `validateCanonicalMigrationGuardDirectory` |
| `internal/storebinding/sqlite/graph_inspection.go:114` | `canonicalPath` |
| `internal/worktree/worktree.go:817` | `canonicalCommonDir` |
| `internal/worktree/worktree.go:875` | `canonicalPathAllowMissing` |
| `test/acceptance/helpers/beads_topology.go:1217` | `sameTopologyScope` |
| `test/acceptance/helpers/rig_add_recovery.go:194` | `rigRoots` |
| `test/acceptance/helpers/sentinel_dolt.go:146` | `NewSentinelDolt` |

The last three rows are test support. Preserve the native tmux socket
contract, Windows locking identity, listener path limits, store ownership
and fail-closed durable-scope behavior. Platform-specific changes require
evidence from the platform they affect; a Linux skip does not establish
Windows behavior.

### Sessionlog follow-up: `ga-dky8ikkx`

All paths in this table are under `internal/sessionlog/`.

| File and line | Function |
| --- | --- |
| `codex_batch.go:497` | `markCodexBatchRoot` |
| `kimi_code_discovery.go:18` | `kimiCodeWorkDirKey` |
| `kimi_reader.go:201` | `findKimiSessionFilesInVisited` |
| `kimi_reader.go:237` | `findKimiSessionFileByIDInVisited` |
| `kimi_reader.go:286` | `canonicalKimiSessionRoot` |
| `reader.go:872` | `appendCodexRolloutMatch` |
| `reader.go:1103` | `findCodexRolloutBySuffixIn` |
| `reader.go:1199` | `collectCodexCandidatesInDays` |
| `reader.go:1221` | `appendCodexCandidatesFromDir` |
| `reader.go:1281` | `findCodexSessionFileIn` |

Canonical dedup keys and lexical returned paths are distinct contracts.
Preserve the caller's spelling through nested symlinked account roots and
retain cycle protection, broken-link handling and ambiguity checks. The
newer `kimiCodeWorkDirKey` site belongs to this same package, including
compatibility with Kimi Code's persisted key algorithm.

### Existing infrastructure landing: `ga-3rkhtqsq`

These ten raw calls remain covered by the existing `.7` implementation and
its open deploy. They are excluded from the new scopes.

| File and line | Function |
| --- | --- |
| `internal/beads/contract/connection.go:828` | `normalizeScopePathForCompare` |
| `internal/buildimage/context.go:172` | `copyDirFiltered` |
| `internal/doctor/checks.go:1181,1182` | `sameDoctorScope` |
| `internal/doctor/checks_service_secrets.go:138` | `secretsDirs` |
| `internal/doctor/checks_service_secrets.go:174` | `withinCity` |
| `internal/git/git.go:602` | `canonicalWorktreePath` |
| `internal/importsvc/source.go:188` | `canonicalizeLocalGitImportSource` |
| `internal/runtime/k8s/staging.go:353` | `walkTar` |
| `internal/transcriptmeta/transcriptmeta.go:68` | `Write` |

### Existing justified calls

| File and line | Function |
| --- | --- |
| `cmd/gc/controller.go:696` | local `addPath` closure |
| `internal/convergence/artifact.go:41,63` | `ValidateArtifactDir` |
| `internal/convergence/condition.go:429` | `ResolveConditionPath` |
| `internal/convergence/evaluate.go:84` | `ResolveEvaluateStep` |
| `internal/dispatch/retry.go:606,616` | `requiredArtifactTargetInWorktree` |

## Reconciliation of sessionlog sweep `.5`

`ga-iawy13.5` closed after handing `da5dfe29fad` to review. Its deploy
`ga-w7a2xm` is abandoned after the original PR was narrowed during
`ga-ytk4sf`. Replacement `#8943` landed as
`12390fee396bb80e3600236b2d7d1fecd9a7494a`, an ancestor of the pinned base.
That commit touches `kimi_reader.go`, `kimi_reader_test.go` and its release
gate record. It preserves lexical Kimi discovery paths; it does not carry
the original `reader.go` and `codex_batch.go` migrations.

Current main still has ten direct sessionlog calls and no sessionlog
`canonical-path-exception` marker. The four original comparison-preparation
sites remain direct calls. Therefore the whole `.5` scope cannot be marked
shipped from the replacement PR. `ga-dky8ikkx` owns the remaining work;
historical build/deploy outcomes stay as recorded.

The existing Kimi spelling fix `ga-qm1hfa`, at
`e60cc870f9d7dc4b1b1da068537e613074c610c7`, already has regression coverage
and open review `ga-31wjwgdk`. The rework waits on that chain and must
integrate its actual landed result rather than reproduce the same fix.

## Architecture decision before two sweeps

`ga-540a8rdj` resolves three calls with native external contracts. Kimi Code
hashes a resolved workspace spelling into a persisted key. tmux resolves
socket directories using its own `realpath(3)` behavior, including
`link/..`, resolution failures and `/tmp` fallback. Pathutil's Darwin alias
folding or missing-path behavior could differ from those contracts.

This is a source-based compatibility concern, not a claimed failing
platform run. The architect decides how each fits the existing rule and
supplies disposition and focused test expectations. PM does not add an
exception category, change the guard or approve an allowlist.

## Acceptance and handoff

- Each sweep publishes a complete per-site classification and disposition
  matrix, with no unexplained direct call in its scope.
- Changed comparison behavior has focused regression evidence; deliberate
  probes preserve required errors and have specific adjacent justifications.
  Existing coverage is reused for comment-only sites.
- Each scope is one `needs-tests` bead. The validator hands that same bead
  to the builder; there is no duplicate implementation bead.
- Tests follow `TESTING.md`, relevant owning `AGENTS.md` files and Bazel.
  Required package, integration/platform, generated/policy and hook gates
  apply. BUILD files stay synchronized after imports or file changes.
- Implementation complexity has zero growth against its pinned merge base
  under the complexity-zero-growth rule. No sweep edits
  `engdocs/complexity-baseline.json`.
- The guard's named holds are replaced by verified same-store dependency
  coverage. Remove `hold:mayor` only after reading back every required edge;
  preserve all existing landing prerequisites. Finish and close this planning
  bead only after its plan is committed, pushed and verified clean.
- Sweep completion means a confirmed shipped change through normal review
  and deploy. The guard owner re-inventories its actual main and runs the
  strict guard before building or shipping enforcement. New sites that
  appear after this pin require classification; they are not implicitly
  covered by a stale count.

Tracker import is a no-op: no tracker-to-beads skill or sibling tracker skill
is materialized. The earlier plan is available as
`fe92b5a5d:engdocs/plans/canonical-path-at-ingest.md`; it is historical context,
while current bead contracts and this pinned inventory define the remainder.
