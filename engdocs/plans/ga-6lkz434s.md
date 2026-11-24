# Plan: give the gc binary-size guard a measured budget and owner

Root: `ga-6lkz434s` · priority P3 · 2026-11-24.
Condition record: `ga-cc90uc6u`.
Planning base: `42f005d69334630c326bb3eac685543a57f47ccf`.

The required CI guard has limited size headroom, while the local Makefile
command already rejects pristine main using a different build and cap. This
plan assigns the sizing decision, separates the two implementation scopes,
and keeps the condition tracked until a repair lands. PM selects no numeric
cap, build mechanism or size-reduction algorithm.

## Evidence and its limits

| Path | Artifact and cap | Evidence at the planning base |
|---|---|---|
| CI: `//scripts:check_native_dependency_surface_test` | Hermetic cgo `//cmd/gc`; 230,000,000 bytes | PM read a cached Bazel PASS: 222,496,208 bytes; 7,503,792 bytes remain. |
| Local: `make check-native-dependency-surface` | Script builds `CGO_ENABLED=0 go build -trimpath ./cmd/gc`; 190,000,000 bytes | Reviewer recorded 204,196,879 bytes and exit 1 on pristine main; PM did not rebuild it. |

PM ran `bazel test //scripts:check_native_dependency_surface_test --test_output=all`,
invocation `41e53b00-e974-4e4c-9124-f1d24029cb5b`.
It returned cached evidence for the current inputs; this was not a newly
executed measurement. Module counts were 524 total, 25 AWS, 9 Azure,
13 DoltHub and 1 Google API. CI is passing at this base.

The script records about 210 KB/day for an older no-cgo release-build
window. `scripts/BUILD.bazel` records about 394 KB/day for a different cgo
CI window. Neither establishes today's CI slope. The architect must obtain
comparable dated main measurements and explain toolchain or dependency
steps before proposing a cap or a runway estimate. The mayor's approximate
five-week estimate is a prioritization signal, not a measured deadline.

## Work packages

| Scope | Bead | Intake | Acceptance prerequisite |
|---|---|---|---|
| Measured sizing decision and authoritative guard contract | `ga-amvyim69` | `needs-architecture` → architect | None |
| CI budget, provenance and enforcement | `ga-3k51z31j` | `needs-tests` → validator → builder, same bead | `ga-amvyim69` approved |
| Local parity or retirement and caller migration | `ga-8r4uxodt` | `needs-tests` → validator → builder, same bead | `ga-amvyim69` approved |

All three carry `plan:ga-6lkz434s`, priority P3, and a `discovered-from`
edge to the mayor's root. The two implementation beads each have a real
same-store blocking dependency on the architecture bead. There is no
test-bead/build-bead pair for either scope and no blocking edge to the
record-only tracker.

The architect posts approved Build and finite RED/GREEN contracts on both
implementation beads before completing the prerequisite. Shared changes
must have one owner and a safe transition that lets the scopes land
independently. If the work proves inseparable, amend one existing bead to
own implementation and explicitly no-op the unused scope; create no
duplicate implementation work.

## Decision and acceptance

1. Measure the canonical gc artifact at current main and at least two dated
   historical main points using the same build shape. Preserve source SHAs,
   dates, toolchain/config, commands, byte outputs, elapsed days and the
   growth calculation. Identify cached, historical and newly executed
   evidence separately. Mark missing comparability unknown.
2. Choose a measured re-baseline and justified headroom horizon, or identify
   unwanted growth with a bounded correction contract. Changes outside that
   contract return to PM for scope approval. Preserve size enforcement.
3. Either retain the contributor command with the same canonical artifact
   and effective policy as CI, or retire it with active callers and
   references migrated to the supported Bazel command. Specify one
   authoritative policy source, overrides, diagnostics and coverage
   semantics. Preserve module-family and product-metrics testhook checks.
4. Verify size-boundary and oversize outcomes, preserved non-size failures,
   and local/CI parity or retirement. Use bounded fixtures under Bazel and
   run the real canonical gc guard on the final tree. A numeric-only
   re-baseline needs measured artifact evidence and proven guard behavior;
   do not manufacture a RED claim for an unchanged predicate.
5. Record a durable maintenance owner, provenance location and next review
   trigger. Existing review ownership for `scripts/` and `Makefile` is
   `@gastownhall/gascity-admin` in `.github/CODEOWNERS`; the contract must
   confirm budget maintenance ownership.

Implementation follows `TESTING.md`, the owning `AGENTS.md` rules and the
applicable Bazel gates. A changed invariant updates its owning `AGENTS.md`.
Complexity must show **zero growth against the pinned base** under the full
pack rule, rather than merely reporting the baseline check's exit status.
Neither implementation may edit `engdocs/complexity-baseline.json`.

## Landing and condition closure

The local-repair bead carries `gc.fixes_tracker=ga-cc90uc6u` at creation.
It passes that lineage and its evidence through normal review/deploy.
The deployer or mayor confirms the repair commit is on target main and
records the supported command's result and canonical guard evidence before
closing `ga-cc90uc6u`. A built or closed work bead does not prove landing.
The CI-budget change also passes its own ordinary review and landing.

The tracker remains open, unrouted and record-only throughout planning and
implementation. PM closes the root only to record completed packaging and
a committed plan; that does not claim either repair shipped. There are two
implementation scopes, so no ADR-scale rollup shipment is required. No
fleet installation, restart or deployment operation is authorized here.

Tracker import is a no-op: this session has neither an installed importer
nor a tracker-sync manifest. The root, existing condition record and child
beads are the sources of work.
