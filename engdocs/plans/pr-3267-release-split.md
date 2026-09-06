# PR #3267 Release Split Remediation

Root bead: `ga-rle1j4`
Source review bead: `ga-rws585`
Original PR: https://github.com/gastownhall/gascity/pull/3267
Original branch: `builder/ga-f43cns`
Reviewed commit: `c70de2044888c5c34e9d5789115839676c739bdf`

## Goal

Remediate the deploy gate failure by replacing the multi-theme PR #3267 with
single-theme release PRs. The deployer failed criterion 7 because the original
PR bundled emergency/API relay, supervisor doctor check, BeadsLibStore, and
compact script concurrency changes.

The original PR must remain unmerged until it is superseded by scoped
replacement PRs.

## Child Beads

| Bead | Target | Purpose |
| --- | --- | --- |
| `ga-rle1j4.1` | `gascity/builder` | Map PR #3267 into single-theme split work. |
| `ga-rle1j4.2` | `gascity/builder` | Ship emergency/API relay as its own PR. |
| `ga-rle1j4.3` | `gascity/builder` | Ship supervisor HTTP doctor check as its own PR. |
| `ga-rle1j4.4` | `gascity/builder` | Ship BeadsLibStore as its own PR. |
| `ga-rle1j4.5` | `gascity/builder` | Ship compact script concurrency fix as its own PR. |

## Acceptance

Each replacement PR must link PR #3267, `ga-rle1j4`, and `ga-rws585`, and state
which portion of the original PR it supersedes.

Each replacement PR must contain only one release theme unless the split-map
bead documents an unavoidable dependency. If any scope cannot be split cleanly
without changing product or architecture intent, builder should create and
route a follow-up escalation bead before changing the scope.

Relevant tests and standard quality gates must pass for each scoped PR, or the
failure must be recorded with command output and a routed follow-up bead.

Builder must create and route the normal review bead for each replacement PR.
No rig agent should merge PR #3267 or any replacement PR directly.

## Dependencies

`ga-rle1j4.1` is the prerequisite split-map bead.

`ga-rle1j4.2`, `ga-rle1j4.3`, `ga-rle1j4.4`, and `ga-rle1j4.5` are blocked on
`ga-rle1j4.1`.

The four scoped PR beads can proceed independently after the split map is
recorded, unless builder documents a concrete dependency in the split-map bead.

## Risks

There may be hidden cross-theme coupling in the original branch because PR
#3267 was already reviewed as a bundle. The split-map bead is the control point
for finding that before replacement PRs are opened.

The deploy gate should not be retried against `builder/ga-f43cns` until the
single-theme replacement PRs are reviewed and ready for deploy.
