# GraphOnlyReadyStore Rollup Deploy

Owner: `gascity/pm`
Created: 2026-07-01
Implementation root: `ga-ifavnc`
Failed deploy beads: `ga-oeu196`, `ga-oz3ow5`, `ga-82huzt`, `ga-v93648`, `ga-wpxh4d`
Replacement deploy bead: `ga-ifavnc.5`
Clean candidate: `deploy/ga-oz3ow5.1-graphonlyready-clean` at `5affa0e82ed1075beab7074d674e4ff98ffe4114`

## Goal

Ship the complete GraphOnlyReadyStore implementation as one clean reviewed
release unit after the implementation stack is green.

The earlier deploy gate, `ga-oeu196`, targeted the contract-test commit
`370ef0b49e3fe9fdab9cbebe17c5e0d428615809`. Deployer rejected that candidate
because the tests are intentionally red until `ga-ifavnc.2`, `ga-ifavnc.3`,
and `ga-ifavnc.4` land, and because the branch contained prior production
commits outside the reviewed contract-test scope.

The later deploy gate, `ga-oz3ow5`, targeted reviewed head
`4e1e9f272b61bb07c91c9a530f8790ca29e0645f`. Deployer rejected that candidate
because it bundled the reviewed GraphOnlyReadyStore store-layer chain with
older hook, reconciler, and docsync commits. The reviewer PASS in `ga-l4ya3q`
stands; the release unit is the blocker.

Three follow-up deploy beads repeated the same release-unit problem:
`ga-82huzt` targeted `local/integration` for beadPolicyStore at `4e1e9f272`,
`ga-v93648` targeted the DoltLite blocked-dependency fix at `9422ebee`, and
`ga-wpxh4d` targeted CachingStore delegation at `4fcb5b1`. Each review PASS
stands, but none is a valid deploy unit by itself because the selected branch
also carries unrelated changes. Fold those reviewed changes into the clean
candidate instead of opening independent deploy lanes.

## Decision

Do not deploy the contract-tests-only branch.

Queue one replacement deploy lane for the complete GraphOnlyReadyStore stack.
The deploy bead remains blocked on `ga-ifavnc.4`, which already depends on
`ga-ifavnc.3`, which depends on `ga-ifavnc.2`, which depends on the closed
contract-coverage bead `ga-ifavnc.1`.

After `ga-oz3ow5`, the deploy lane also waits on a clean-candidate builder
package. The builder package is not a new feature scope; it is release
packaging work to produce an `origin/main`-based candidate that preserves
`ga-ifavnc.1` through `ga-ifavnc.4` and excludes unrelated commits.

As of 2026-07-01, `ga-oz3ow5.1` is closed with a clean candidate branch:
`deploy/ga-oz3ow5.1-graphonlyready-clean` at
`5affa0e82ed1075beab7074d674e4ff98ffe4114`. The deploy lane is unblocked and
should run against that candidate only.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-oz3ow5.1` | `gascity/builder` | `ready-to-build` | Assemble a clean `origin/main`-based GraphOnlyReadyStore deploy candidate, record branch/SHA/included changes, run the required native build/test/vet checks, and leave PR/deploy/merge out of scope. |
| `ga-ifavnc.5` | `gascity/deployer` | `needs-deploy` | Gate a clean, reviewed rollup branch containing the contract coverage plus the DoltliteReadStore, CachingStore, and beadPolicyStore implementations; open or update a scoped PR only on PASS. |

## Dependency Graph

```text
ga-ifavnc.1
  -> ga-ifavnc.2
      -> ga-ifavnc.3
          -> ga-ifavnc.4
              -> ga-oz3ow5.1
                  -> ga-ifavnc.5

ga-l4ya3q
  -> ga-oz3ow5.1
```

`ga-ifavnc.5` is unblocked by the clean-candidate bead and should route to
deployer for the standard gate.

## Builder Acceptance For Clean Candidate

- Start from current `origin/main`; do not reuse reviewed head `4e1e9f272` as
  the release branch as-is.
- Include only the GraphOnlyReadyStore release scope from `ga-ifavnc.1` through
  `ga-ifavnc.4`: contract coverage, DoltliteReadStore graph-only ready,
  CachingStore capability propagation, and beadPolicyStore policy wrapping.
- Preserve the reviewed DoltLite blocked-dependency fix from `ga-b5j6av` /
  `ga-51hrx9` (`9422ebee`) so graph-only ready reads exclude wisps blocked by
  open dependencies.
- Preserve the reviewed CachingStore delegation behavior from `ga-hdiar1`
  (`4fcb5b1`) and the reviewed beadPolicyStore policy wrapper behavior from
  `ga-kgvw46` (`4e1e9f272`), or record an equivalent included change list.
- Exclude the unrelated commits named by the failed gate:
  `2978967f4`, `db33a4310`, `1d13f841e`, `da7a80872`, `d5636d2fc`, and
  `f4010daa4`, plus stale release-gate artifacts.
- If the scoped candidate cannot pass without an adjacent controller,
  reconciler, or hook change, stop and route back to PM/architect with the
  exact blocker instead of broadening the release unit silently.
- Record branch name, final SHA, included commit list or equivalent change
  list, and gate results in `ga-oz3ow5.1` before closing it.

## Deployer Acceptance

- Start only after the full implementation stack has reviewer PASS for the
  complete GraphOnlyReadyStore scope.
- Do not reuse `370ef0b49e3fe9fdab9cbebe17c5e0d428615809` as the deploy unit.
- Do not reuse `local/integration` or
  `builder/ga-ifavnc.1-graph-only-ready-store-contract-coverage` as the deploy
  unit unless the branch has first been rebuilt as the clean candidate recorded
  by `ga-oz3ow5.1`.
- Gate a clean candidate based on current `origin/main`.
- Confirm the candidate includes the contract coverage and all three
  implementation layers, with no unrelated prior production commits or stale
  release-gate artifacts.
- Run the standard deploy gate, including the native build/test/vet checks
  required by `ga-ifavnc.4`.
- On PASS, open or update a GraphOnlyReadyStore-scoped PR and route merge
  authority to mayor/mpr only.
- On FAIL, append the failed criteria and artifact path to `ga-ifavnc.5`, then
  route back to PM with the exact blocker.

## Out Of Scope

- PM-authored implementation, tests, branch surgery, PR approval, or merge.
- Deploying contract coverage before the implementation stack is green.
- Bundling unrelated production changes into the GraphOnlyReadyStore deploy
  candidate.
