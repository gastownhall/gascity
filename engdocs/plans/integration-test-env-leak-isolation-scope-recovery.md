# Plan — Integration-test environment-leak isolation scope recovery

**Root bead:** `ga-1xcg0l`
**Review bead:** `ga-ykejub`
**Rejected deploy source:** `8d0e3b120ecd40468ce5961e568d1245eca8460f`
**Scope-gate evidence:** `4a9add3c3a81faf61d4064c016627b0c78c437a9`

## Goal

Publish a single-theme release candidate that prevents integration-test `bd`
subprocesses from inheriting ambient developer or fleet configuration. The
candidate must preserve the behavior that passed review on `ga-ykejub` while
excluding the thirteen independent commits inherited by its source branch.

The deploy gate rejected `8d0e3b120` before release tests or pull-request
creation. Relative to its merge base, that tip carries fourteen commits and
changes fourteen paths across retry, readiness, timing, HOME-isolation, and
environment-golden themes. The reviewed behavior remains valid; the release
unit does not.

No tracker integration skill is materialized in this PM worktree, so external
tracker import is a no-op for this package.

## Scope contract

The clean candidate preserves only the behavior introduced by the final source
commit:

- `pinnedBdStoreCommandRunner` receives the intended isolated environment and
  applies it verbatim to the child process instead of rebuilding from
  `os.Environ()`.
- The runner has bounded execution and keeps stdout separate from diagnostic
  stderr so a valid JSON response cannot be corrupted by interleaved output.
- All three existing runner call sites pass their isolated environment.
- `newIsolatedEnvRoot` pins HOME inside the test root so a developer-level
  beads configuration cannot redirect the test database.
- Resource-census documentation and baselines change only as required by the
  additional subprocess call site.

The source commit changed six paths:

| Area | Expected role in the clean candidate |
| --- | --- |
| `test/integration/integration_test.go` | Hermetic runner and isolated HOME behavior |
| `test/integration/bdstore_batch_delete_test.go` | Pass the isolated environment at the batch-delete call site |
| `test/integration/bdstore_test.go` | Pass the isolated environment at both store call sites |
| `TESTING.md`, `internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml` | Keep the subprocess ledger synchronized |

Same-theme regression coverage may also update the canonical integration shard
so all three call sites run in a durable gate. It must not broaden production
behavior.

## Excluded lineage

The replacement must not carry any earlier commit inherited by `8d0e3b120`:

| Excluded theme | Existing disposition |
| --- | --- |
| Post-init database-visibility retry and 20-second budget | `ga-lvrcyp.3.1.1` |
| Dolt init and readiness deadlines | `ga-l4xwgh`, `ga-biy3ae` |
| Environment-golden add and dedupe | Evidence on `ga-lvrcyp.3.1.2` |
| `internal/beads` HOME isolation | `ga-0l3teb` |
| `cmd/gc` HOME isolation | `ga-us7c35` |
| Stop-test wall-clock margin | `ga-bm7k79` |

`ga-lvrcyp.3.1.2` already owns the live-state inventory for those themes. The
new handoff depends on that bead rather than creating parallel release work.
Both `8d0e3b120` and `builder/ga-ykejub-integration-env-isolation` remain
provenance only and are forbidden review or deploy sources.

## Work packages

| Bead | Route | Outcome |
| --- | --- | --- |
| `ga-1xcg0l.2` | `gascity/validator` | Author durable regression coverage for ambient-variable, HOME, stdout/stderr, and all-call-site isolation |
| `ga-1xcg0l.1` | `gascity/builder` | Publish the clean candidate from current `origin/main`, integrating the accepted tests and only necessary census changes |
| `ga-1xcg0l.3` | `gascity/builder` | After scope prerequisites close, create and verify the exact-SHA review and deploy chain |

Each child carries complete measurable acceptance criteria plus its intake label
and `source:actual-pm`.

## Dependency graph

```text
ga-1xcg0l.2  regression tests
        |
        v
ga-1xcg0l.1  clean candidate --------+
                                      +--> ga-1xcg0l.3  review/deploy handoff
ga-lvrcyp.3.1.2  side-theme inventory +
```

The validator package must land first so the candidate is built against durable
red/green evidence. The handoff remains blocked until both the candidate and
the existing side-theme inventory are complete.

## Release guardrails

- Build from current `origin/main`; never open a review or deploy request from
  the mixed builder tip.
- Require byte-level scope review against the candidate base. The only allowed
  theme is integration `bd` subprocess environment isolation, its regression
  coverage, and necessary resource-census accounting.
- Exercise every runner call site, including
  `TestBdStoreDeleteBatchOrphansExternalDependents`, in a durable gate.
- Run the focused integration tests, resource-census consistency test, gofmt,
  `go build ./...`, `go vet ./...`, and the repository fast push gate.
- A reviewer PASS creates a fresh deploy bead for the exact reviewed SHA. No
  rig agent self-merges; merge authority remains with mayor, mpr, or an
  operator.

## Completion evidence

This recovery is complete when the validator records reproducible red/green
coverage, the builder records a pushed and independently verified isolated SHA,
and the handoff bead records the review and deploy bead IDs, read-back routes,
exact SHA, gate result, and PR URL or explicit failure disposition.
