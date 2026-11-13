# Preserve legitimate retention in the rig data-loss check

PM intake: `ga-7o0tki05`. Build carrier: `ga-5pnyxkuk`.

An operator can legitimately prune every issue in a rig when all issues are
closed. A populated export can remain until the next export cycle. PR #3732
currently treats that state as blocking data loss and recommends restoration.
The requested repair makes the all-closed case advisory while retaining the
blocking signal when exported issues include a non-closed issue.

The operator asked the fleet to make this change on bourgois's existing PR.
The mayor owns landing it on that PR, preserving the contributor's authorship.

## Exact base and deliverable

| Item | Contract |
|---|---|
| Contributor PR | `gastownhall/gascity#3732`, bourgois, `Voxist/gascity` |
| Base ref | `voxist/gc/vp-pt73.3` |
| Pinned parent | `c50634e6094521742265fd8849ddddf3786b74d5` |
| Build carrier | `ga-5pnyxkuk`, P2, `ready-to-build`, `source:actual-pm` |
| Origin edge | `ga-5pnyxkuk` discovered from `ga-7o0tki05` in the gascity store |
| Build method | `mol-tdd-build`, explicit `base_branch=voxist/gc/vp-pt73.3`, `commit_scope=fix` |
| Landing interlock | Build metadata `gc.deploy_ref=voxist/gc/vp-pt73.3` |
| Output | Exactly one new fleet-authored fix commit whose parent is the pinned SHA |
| Review result | PASS creates `needs-fork-land`; mayor receives SHA and available branch |

Re-fetch the contributor branch before launch and before building. If its head
has moved, stop and tell the mayor. Preserve all four contributor commits without
rebasing them. The plan has its own `docs/ga-7o0tki05-pruned-rig-advisory` branch;
the repair's complete acceptance contract also lives in the build bead.

## One scope through test, build, and review

The requested behavior and implementation boundary are already fixed by the MPR
synthesis. One build carrier owns the regression and repair through
`mol-tdd-build`. Creating separate test and implementation beads would duplicate
this scope. There are no independent child blockers or architecture decisions.

The existing streamed reader must carry both issue count and non-closed issue
count into the check, including the archive oracle. The synthesis specifies
`CountJSONLRecords`, a compatible `CountJSONLLines` wrapper, and the archive
oracle's four-value result. Expected edits are the data-presence check and its
tests, the archive adapter, and tests whose oracle signature changes.

| Evidence and live state | Required outcome |
|---|---|
| Zero live rows; populated local export with all issues closed | Warning / Advisory; retention may explain it; confirm before restoring |
| Zero live rows; populated archived export with all issues closed | Same advisory result |
| Zero live rows; any exported issue not closed | Error / Blocking remains |
| Zero live rows; exported issue has no status | Conservative blocking remains |
| Zero live rows; no export history | Existing fresh-rig pass remains |
| Unreadable export or archive | Existing advisory result remains |
| Nonzero live count below export count | Existing advisory deficit remains |

Keep local-export precedence, active archive selection, streamed large-record
support, issue versus memory filtering, legacy `rows` snapshots, legacy empty
objects, and store preflight gating covered. The all-closed rule applies to
counted issues, not unrelated records in an export.

## Acceptance evidence

1. The new retention regression reports an assertion failure against the pinned
   implementation before the repair, then a real PASS afterwards. Record both
   runs. Include local and archived all-closed, mixed/non-closed, absent-status,
   and legacy snapshot cases. Skips, empty filters, and compile failures do not
   establish RED or GREEN.
2. At the final commit, run `bazel test //internal/doctor:doctor_test`,
   `bazel test //internal/testpolicy/...`, and the doctor/archive/preflight/JSONL
   tests in `//cmd/gc:gc_test`. The selection from the review is
   `ArchiveExportRows|Doctor|doctor|Preflight|JsonlArchive|JSONLArchive`.
   Run `//examples/gastown:gastown_test` filtered to `TestMigration`.
   Filtered evidence includes `--test_env=GO_TEST_WRAP_TESTV=1 --test_output=all`
   and inspection of the named PASS lines. Bazel is the authoritative test runner.
3. The parent also requests `go build ./...` and
   `go vet ./cmd/gc/ ./internal/doctor/ ./examples/gastown/`. Run these and the
   repository's required `make check` gates at the repair commit. Follow the
   pinned tree's `AGENTS.md` and `TESTING.md`.
4. The final range contains exactly one new commit, and its parent equals
   `c50634e6094521742265fd8849ddddf3786b74d5`. Preserve RED/GREEN logs when
   consolidating only the fleet's new TDD commits. Review the final one-commit
   head. Record its SHA and branch before handoff.
5. Complexity has zero growth against that pinned parent, following
   `packs/actual/all/template-fragments/complexity-zero-growth.md.tmpl` in the
   city checkout. A green base requires a green `make complexity-check` at the
   repair head. For a red base, quote base/head reports with no worse existing
   rows, and a successfully measured `COMPLEXITY_BASE_REF=<pinned SHA> make
   complexity-diff` has no `new:`, `regressed:`, or `deleted:` for a function
   still present. Leave `engdocs/complexity-baseline.json` unchanged.

The requested repair excludes rebasing onto main, recounting constants for
main's drift, migration changes, widening the archive evidence window, and
optimizing the live row count. Mechanically required BUILD or test-resource
updates must arise from this repair's own changes, be regenerated at the pinned
base, and carry an explanation. Attribute unrelated base failures under the
existing gate protocol.

## Dispatch boundary

Direct `gc formula cook --attach` was refused on this split city before creating
an attachment. Its error names explicit `gc sling --on` as the supported launch
through the graph binding. The mayor confirmed the concrete launch in
`gcg-7871489`, replying to PM's verified request `gcg-7870465`.

Launch with the explicit base, fix scope, and a Bazel
`TestRigDataPresence` test command. Verify the build carrier's route, its actual
claimability or holder, the formula's recorded base variable, and the fork ref
read by `deploy-ref.sh`. Preserve any admission refusal and ask the mayor about
it; a successful dry run is historical admission evidence only.

The builder sends the exact commit through review. A PASS must produce a
`needs-fork-land` record without upstream deployment routing. The mayor alone
handles the eventual Voxist push or contributor PR comment. PM's handoff reports
planning and dispatch evidence; it does not claim implementation or shipment.

## Source evidence

The operator request, authority, exact base, and authorship constraint are in
`ga-7o0tki05` and mayor mail `gcg-7864321`. The MPR run is
`.gc/maintainer-pr-review/gastownhall-gascity/pr-3732/runs/20261113T130240Z`
under the city checkout: `synthesis-output.md`, `intake/codex-review.md`, and
`intake/claude-review.md`. The synthesis's high finding fixes the required
semantics. The parent's exact-head instruction supersedes the review's earlier
rebase proposal; the review's verification names remain applicable.

Tracker import is a no-op: no tracker skill is installed in this session.
