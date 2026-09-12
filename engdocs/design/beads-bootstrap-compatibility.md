---
title: "Beads Bootstrap Compatibility"
---

# Proposed companion design: Beads bootstrap compatibility

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-08-30 |
| Author(s) | [@vishnujayvel](https://github.com/vishnujayvel) |
| Primary audience | Gas City and Beads maintainers and contributors |
| Companion to | [Beads and Dolt contract redesign](beads-dolt-contract-redesign.md) |
| Related design | [Beads–Gas City cross-version contract-test system](beads-gascity-contract-test-system.md) |
| Defect home | [Gas City issue #5348](https://github.com/gastownhall/gascity/issues/5348) |

## Decision summary

For a Gas City bootstrap operation on a resolved Beads scope, Gas City must identify the exact external `bd` initializer and prove that it cannot advance the main schema beyond the migration ceiling known by the Beads library linked into `gc` before running `bd init`.

> Resolve the scope through the accepted contract, identify the exact initializer artifact, compare its main-schema migration ceiling with the linked reader, and refuse before any bootstrap publication when the proof fails.

This companion design does not define a new general policy for commands labeled read or write. It does not change endpoint authority or Beads' existing migration gates.

## Relationship to the accepted design

The accepted Beads–Dolt contract redesign already owns:

- canonical store identity;
- endpoint and database resolution;
- configuration provenance and environment projection;
- lifecycle ownership;
- the topology and migration journal;
- explicit repair paths and postconditions.

This companion design reuses those decisions. It adds one missing precondition to the existing bootstrap path: the selected initializer must not advance the main schema beyond the linked reader's known main migration ceiling.

## Verified failure

[Gas City issue #5348](https://github.com/gastownhall/gascity/issues/5348) reports this sequence:

1. Gas City selected an external `bd` for managed-city bootstrap.
2. That executable created schema v65.
3. The Beads library compiled into `gc` supported migrations only through v59.
4. Gas City selected the new server store as canonical.
5. The linked native reader could not open it and Gas City continued through a CLI fallback.

The issue reports no observed record loss: the earlier embedded store remained populated but inactive. The defect is still serious because Gas City published a canonical store that one of its own implementations could not read.

## Current related work

| Work                                                                                                                            | Current state        | Relationship                                                                                                |
| ------------------------------------------------------------------------------------------------------------------------------- | -------------------- | ----------------------------------------------------------------------------------------------------------- |
| [Accepted Beads–Dolt redesign](beads-dolt-contract-redesign.md) | Accepted | Owns scope, topology, provenance, journal, and lifecycle authority. |
| [Beads–Gas City cross-version contract-test system](beads-gascity-contract-test-system.md) | Proposed | Defines broader CLI/wire compatibility matrices and schema canaries. It does not define the pre-`bd init` migration-ceiling proof or the no-publication invariant in this companion design. |
| [Gas City PR #5421](https://github.com/gastownhall/gascity/pull/5421)                                                           | Open and conflicting | Includes a related `BD_BIN` projection change. An exact path is necessary but does not prove compatibility. |
| [Gas City PR #5518](https://github.com/gastownhall/gascity/pull/5518)                                                           | Open and conflicting | Proposes a schema-v65-capable linked dependency. It repairs one snapshot but does not prevent later skew.   |
| [Beads PR #6048](https://github.com/gastownhall/beads/pull/6048)                                                                | Merged               | Existing shared SQL-server stores refuse version-bump migration without explicit consent.                   |
| [Beads PR #6055](https://github.com/gastownhall/beads/pull/6055)                                                                | Merged               | Applies the migration gate to the proxied-server open path.                                                 |

The merged Beads changes deliberately allow fresh database creation. In Beads, creation is consent for that database's schema. Gas City #5348 is different: Gas City chooses an external initializer and then expects a separately linked reader to use the resulting store.

## Who is affected

This problem crosses installation method, process lifetime, and storage
topology. These are deployment and use-path cohorts, not separate product
tiers:

| Developer cohort | How implementations can diverge | Potential impact |
|---|---|---|
| Packaged-release and Homebrew users | Packaged `gc` and separately installed `bd` can advance on different release schedules. | A managed bootstrap can create a store that the linked reader cannot open. |
| Source builders and contributors | A source-built `gc` links one Beads revision while `PATH` selects a `bd` from another revision or package manager. | Local testing can pass on one execution path and fail on another. |
| Upgrade operators | A newer CLI, an older supervisor, or another long-running process can remain active during an upgrade. | One implementation can advance a store beyond another process's supported schema. |
| Developers using Beads both directly and through Gas City | Direct `bd` commands and Gas City can operate on the same resolved scope through different implementations. | Store behavior depends on which path performs bootstrap or migration. |
| Pack and automation authors | Lifecycle scripts or providers can invoke an external `bd` independently of the library compiled into `gc`. | Automation can create a schema that later native operations reject. |
| Managed-local, explicit-external, and remote-Dolt operators | The endpoint location changes where the failure appears, not whether implementation skew is possible. | Diagnostics may look like connectivity or fallback problems even when schema compatibility is the blocker. |

The cohort boundaries are largely grounded in existing reports, not inferred
from the endpoint in #5348 alone: [#3632](https://github.com/gastownhall/gascity/issues/3632)
tracks a packaged `gc` versus Homebrew `bd` mismatch;
[#4362](https://github.com/gastownhall/gascity/issues/4362) tracks a source-built
upgrade with stale-process blockers; [#5349](https://github.com/gastownhall/gascity/issues/5349)
tracks release alignment; and [#5135](https://github.com/gastownhall/gascity/issues/5135)
tracks native-open failure continuing as a partially working system. The
direct-use and automation cohorts are mechanism-derived exposure paths from
#5348 and the accepted contract, not claims that those cohorts have each
reported this exact failure.

Independent Beads users are not governed by this proposal unless Gas City owns
the bootstrap operation on the same store. The proposal does not require one
Beads version across unrelated projects.

Endpoint location does not grant Gas City new authority. The accepted design remains authoritative for whether Gas City owns lifecycle actions on managed-local or explicit external endpoints.

Out of scope:

- independent Beads projects outside a Gas City lifecycle operation;
- a new general compatibility policy for all reads and writes;
- changes to Beads' merged shared-store migration behavior;
- a global requirement that every Beads project use one version.

## Broader execution-context follow-up

This companion design closes one bootstrap invariant; it does not close the broader
developer-experience problem. A separate follow-up should make these facts
observable for each Gas City-managed Beads operation:

- the effective configuration source and resolved store identity;
- whether the operation uses the Beads library linked into `gc`, an external
  `bd`, or another provider path;
- the implementation and schema-compatibility evidence used for the decision;
- the active CLI and supervisor versions during upgrades;
- any fallback or degraded mode, with a safe next action;
- coverage across packaged installs, source builds, mixed-version upgrades,
  direct `bd` use, automation, and supported storage topologies.

Those concerns should build on the accepted resolver, provenance, and journal
contracts rather than introduce another configuration authority. Each
reproducible bypass should remain a focused issue and regression test; this
document should not expand to speculate about unverified execution paths.
Coordinate the broader scenario matrix with the proposed
[Beads–Gas City cross-version contract-test system](beads-gascity-contract-test-system.md)
instead of creating a second compatibility harness.

## First-principles model

Bootstrap depends on three facts that must not be collapsed:

1. **Scope identity:** Which exact Beads scope is Gas City initializing?
2. **Initializer identity:** Which exact external `bd` will run `init`?
3. **Bootstrap ceiling:** Can the initializer advance the main schema beyond the linked reader's known main migration ceiling?

Correct scope identity does not prove the bootstrap ceiling. An executable path is a locator, not a stable artifact identity or a storage-behavior proof. A semantic version is useful evidence, but backports, pseudo-versions, and source builds prevent it from serving as the compatibility contract.

## Proposed contract

### Beads exposes the main-schema migration ceiling

Beads should expose a stable, database-independent, read-only value for the latest main-schema migration bundled into the selected `bd`.

One candidate is an additive `main_schema_migration_ceiling` field in `bd version --json`; maintainers should choose the final name and compatibility promise. Reading the witness must not open or mutate a database.

The linked-library side already has a [public `schema.LatestVersion()` wrapper](https://github.com/gastownhall/beads/blob/cbfc505e39a60514c57dcdb5afe155c8659647ba/schema/schema.go). The external JSON witness and public library wrapper must describe the same main migration series.

The numeric comparison is meaningful only within the canonical, append-only
Beads main-migration series. This first slice does not support forks that reuse
migration numbers for different schema definitions; Gas City must not treat
such an artifact as proven compatible. A richer series-identity witness remains
future work if canonical build provenance cannot establish that assumption.

This first witness does not prove complete fresh-store readability. Beads has
additional initialization state beyond the main migration series. It proves
only the #5348 invariant: the external initializer cannot advance the main
schema beyond the linked reader's known main ceiling. A broader fresh-init
compatibility witness remains future work if another reproducible gap requires
it.

### Gas City gates bootstrap before side effects

The existing bootstrap path should:

1. Resolve the scope through the accepted Beads–Dolt contract.
2. Resolve symlinks for the selected `bd` and capture a stable artifact identity, such as a content digest plus platform file identity.
3. Query the database-independent witness from that attested artifact.
4. Compare the external main-schema migration ceiling with the linked reader's public `schema.LatestVersion()`.
5. Refuse when the external initializer ceiling is newer than the linked reader ceiling.
6. Bind the identity recheck to the `bd init` launch. If the artifact changed, restart at step 2—including witness and comparison—for the new artifact, or refuse. Never launch an artifact that did not produce the proof being applied.
7. Continue through the existing topology/migration journal and postconditions only after the proof passes and `bd init` succeeds.

The gate is intentionally asymmetric. It prevents the verified v65/v59 main-schema failure without claiming whole-store compatibility or requiring ceiling equality for every operation.

The implementation must document any remaining time-of-check/time-of-use limitation on platforms that cannot bind execution directly to the attested artifact. An absolute path alone is not sufficient evidence because a file or symlink can change between witness and launch.

The bootstrap path must not publish or select a new canonical identity until the compatibility gate and existing accepted postconditions complete. This proposal does not promise to roll back an external database if another actor creates one independently.

## Diagnostic

The refusal should report:

- the Gas City scope;
- the resolved `bd` path, artifact fingerprint, and non-secret build identity;
- the external main-schema migration ceiling;
- the linked reader's main-schema migration ceiling;
- the blocked bootstrap operation;
- the fact that Gas City did not publish or select a new canonical identity;
- one safe next action.

Do not print credentials, tokens, passwords, or secret-bearing connection strings.

`gc doctor` should expose the same redacted identities and compatibility result
through a read-only diagnostic so bootstrap refusal and later diagnosis cannot
disagree about the selected implementation or the failed proof.

## Regression proof

Use a fake or fixture `bd` that reports a main-schema migration ceiling of v65 while the linked-reader fixture reports v59.

Assert that the mismatch reaches none of these side effects:

- external initializer;
- accepted topology/migration journal publication;
- canonical selector;
- admission of work against the replacement store.

Add a separate artifact-identity test that replaces or retargets the selected executable between witness and init. Assert that Gas City restarts the full identity, witness, and comparison proof or refuses instead of launching the changed artifact.

The linked-reader ceiling must be injected through a test seam instead of read
from the live `schema.LatestVersion()` constant; otherwise the v65/v59
regression silently stops testing the mismatch when the real dependency
advances. Add a positive-path fixture where the external ceiling is equal to or
below the linked ceiling and assert that bootstrap reaches the initializer.

The Beads repository should own witness-format and value tests. Gas City should
own the managed-bootstrap ordering and no-side-effect regression. Reuse fixture
or corpus infrastructure from the cross-version contract-test system if it
lands first, but do not make that broader proposal a prerequisite for this
gate.

The test should fail when the gate is removed. It must use a disposable scope and must not target an existing developer city or database.

## Important non-decision: command labels

This companion design does not say that reads and ordinary writes always proceed. The merged Beads work demonstrates why: a user-visible read can reach version-bump reconciliation, and a read-labeled proxied path can create and migrate a fresh database.

Classify future policy by the storage action actually attempted, not by the CLI command label. Beads' existing migration gates remain authoritative. Any broader reader, writer, migration, repair, or feature protocol requires separate evidence and maintainer design.

## Rollout

1. **Witness:** Beads exposes and tests one database-independent main-schema migration ceiling.
2. **Gate:** Gas City attests the initializer artifact, compares its main ceiling with the linked reader, and rechecks artifact identity before `bd init`.
3. **No-side-effect proof:** The #5348 regression verifies that mismatch cannot publish or select a replacement identity.
4. **Diagnostics:** Bootstrap and doctor report the same redacted identities and compatibility result.
5. **Stop:** Evaluate another lifecycle path only after a reproducible bypass is found.

## Alternatives considered

### Pin `BD_BIN` only

An exact path prevents `PATH` drift but does not prove what schema the selected initializer will create.

### Align release versions

Release alignment reduces exposure, but source builds, stale processes, replacement modules, and separate package managers can still diverge.

### Compare semantic versions

Version strings do not prove storage behavior across backports, pseudo-versions, or source builds.

### Require one Beads version everywhere

This would unnecessarily govern independent Beads projects and still would not prove which code executed.

## Maintainer decisions

1. Should the external witness live in `bd version --json` or a dedicated
   database-independent command?
2. What should the external main-schema migration-ceiling field be called?
3. Should an unavailable witness warn for one release or fail closed
   immediately for Gas City bootstrap?
4. Which `bd` build should Gas City recommend when the selected initializer is
   too new?

A warn-only transition preserves the #5348 exposure for the mixed-version
cohorts described above; fail-closed is the safer default unless maintainers
explicitly accept that rollout risk.

## Not doing

- No second store resolver.
- No new configuration source.
- No general read/write compatibility policy.
- No change to Beads' merged migration gates.
- No new endpoint or server authority.
- No automatic downgrade, rollback, or migration-commit reversal.
- No broad schema-feature protocol in the first slice.
- No new issue while #5348 remains the exact defect home.
