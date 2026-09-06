# Plan: Actual deployer management-pack release

> Owner: `gascity/pm` · Created: 2026-05-28
> Source beads: `ga-1qhwmc`, `ga-5tfosh`, `ga-tqaejv`, `ga-z5ioe1`

## Why this work exists

Three reviewed deployer prompt commits passed review, but failed the
gascity deploy gate because they are not deployable as separate
single-bead PRs. The commits live in the HQ management repository
(`/home/jaword/projects/gc-management`), not the gascity rig
repository, and they form one coupled prompt-template update.

A fourth bead, `ga-z5ioe1`, covers a defect in the same HQ prompt file:
the single-bead technical FAIL path still shows `bd update
--metadata-field`, which is not accepted by current `bd update`.

## Routing decision

Route this as an HQ management-pack package to `pack-author.pack-author`.
Do not route it back to `gascity/builder` or `gascity/deployer`; the
referenced file is outside the gascity repo and the HQ repo has no
configured remote.

Target:

- Repository: `/home/jaword/projects/gc-management`
- File: `packs/actual/deployer/prompts/deployer.md`
- Release path: local management-pack release outside the gascity PR path
- Existing reviewed commits: `c8f307d`, `56a5cf3`, `79b9d44`

## Package bead

| Bead | Title | Priority | Routes to | Gate |
|------|-------|----------|-----------|------|
| `gm-*` | Package actual deployer prompt gate updates as HQ management-pack release | P2 | `pack-author.pack-author` | `ready-to-build` |

## Acceptance criteria

1. The HQ prompt file includes the reviewed deployer gate behavior from
   `c8f307d`, `56a5cf3`, and `79b9d44` as one coherent release unit.
2. The single-bead technical FAIL path in
   `packs/actual/deployer/prompts/deployer.md` uses a metadata flag
   accepted by current `bd update`.
3. No `bd update --metadata-field` routing example remains in the
   deployer prompt.
4. Verification includes a command showing the invalid flag is absent
   and the replacement flag is supported by `bd update`.
5. The package notes state that this is an HQ local management-pack
   release, not a gascity repo PR, unless a remote/branch target is
   configured later.

## Dependencies

The package depends on the reviewed source commits already present in
the HQ repo and on the pre-existing bug report `ga-z5ioe1`. Cross-rig
dependency edges are not encoded in Beads; the HQ package bead lists
the gascity source beads explicitly in its notes.

## Risks

- The HQ repository has no configured git remote, so normal deployer
  push/PR handoff is unavailable.
- The existing reviewed commits are already on HQ `main`; the package
  owner must treat the final result as a local release unless a remote
  is configured before handoff.
- Sending this work back to gascity implementation agents would strand
  it again because the file is not in the gascity worktree.
