# Plan: safe bd init-lock paths

Work: `ga-7i7r5k1w` · Priority: P4 · Date: 2026-11-25

## Outcome and scope

An operator can initialize a store without following a planted lock-file link
or using a default lock directory controlled by another user. Existing default
directories owned by the effective user are tightened to 0700 during upgrade.
Explicitly configured shared lock directories retain their ownership and mode
policy.

The architect's complete specification is in the work bead's notes. That
specification corrects the original request: an owned 0755 default directory
is tightened, not refused. It also records the sticky `/tmp` assumption and the
best-effort symlink check in explicitly shared directories. The predictable
name can still be pre-created to cause a refusal; recovery uses the existing
`GC_DOLT_INIT_LOCK_DIR` override. A fallback directory is excluded because two
initializers must use the same lock identity.

This is one implementation scope and one PR. Keep `ga-7i7r5k1w` as the work
bead; create no separate test, implementation, or rollup successor. After the
landing gate opens, PM relabels this same bead `ready-to-build` and slings it to
the builder, which follows the architect's TDD contract.

## Prerequisite and ownership

The preceding lock-path refresh must reach main first. Its existing deploy is
`ga-2cr5m5yb`, from build `ga-936yiczx`. At the PM intake, fetched main
`77143ecd18ff6f0199dfee3a412825f1fd9fa8a2` still used the TMPDIR-based default.
The deploy was in progress, with its full test suite passed and final required
CI accounting pending. Neither is evidence of landing.

| Record | Purpose | Disposition |
| --- | --- | --- |
| `ga-2cr5m5yb` | Existing refresh deploy | Keep the existing gate and merge chain |
| `ga-8ufhwpnz` | Verify the refresh actually lands | Open, unassigned, unrouted `parent-record`; never sling |
| `ga-7i7r5k1w` | Implement path hardening | Open and routed to PM while the prerequisites remain open |

The hardening bead has same-store `blocks` edges to both the existing deploy
and the landing record. The latter carries `discovered-from:ga-7i7r5k1w` as
provenance, not as a scheduling edge. The scheduling graph is acyclic: both
prerequisites precede the one hardening scope.

Mayor accepted closure ownership in mail `gcg-17120257` and recorded it on the
landing record; deployer was notified to carry it through the existing merge
handoff. Before closing the record, mayor records the refresh PR, main commit
and normal work-record or squash evidence, then verifies the content predicate:

```sh
git show origin/main:examples/bd/assets/scripts/gc-beads-bd.sh \
  | grep -nF 'INIT_LOCK_DIR="/tmp/gc-beads-bd-init-locks-$(id -u)"'
```

The match must be the default assignment in `acquire_init_lock`. Notify PM
with `--notify` after verification. PM re-reads the source bead, main content,
and prerequisite outcomes before handing this same bead to the builder with
`--nudge`. Builder repeats the predicate before making changes; absence
returns the unchanged scope to PM. Keep the build on current main, with no
stacked `base_ref`. If the refresh is abandoned or its contract changes, leave
the landing record open and notify PM to revise the sequence.

## Acceptance carried to the builder

- Default path: fresh directories are 0700; owned legacy directories are
  tightened; symlinked or foreign-owned directories fail with a useful reason.
  Existing foreign-owned default lock files also fail. No alternate lock
  directory is selected automatically.
- Lock-file links are refused in both default and explicit directories,
  including dangling links whose targets must remain absent. Explicit
  directory permissions and caller umask behavior remain as specified.
- All eight architect-specified test rows and the six listed mutation checks
  provide named evidence. Tests use unique fake-uid paths and never alter the
  operator's real per-user lock directory. Root-only limitations are explicit.
- Builder supplies the specified real-filesystem transcript using real flock,
  updates the script header, troubleshooting page and changelog, and follows
  the supplied platform and scope boundaries. Public PR prose carries the
  threat model, transcript and predecessor PR number without internal ids.
- The specified filtered Bazel tests report the named PASS lines; `make check`
  passes and `dash -n` accepts the script. No implementation or test evidence
  is claimed by this PM plan.
- Complexity has zero growth against the immutable pinned base. A green base
  requires a green canonical merge check. For a red base, retain both check
  outputs with no worsened row, and a successful diff measurement with no new,
  regressed, or still-living deleted function. Use the gate's `GATE_BASE_SHA`,
  or the recorded target merge base outside a gate. Never update
  `engdocs/complexity-baseline.json` in this feature.

## PM disposition

The current task is sequencing, not implementation. Preserve P4 and the
architect's one-bead scope. Release PM's claim after recording the dependency
graph, committing this plan and verifying the notifications. Keep the work
open with `needs-pm` so the verified prerequisite landing can return it to PM.
There is no builder admission or claimability receipt while the gate is shut.
Tracker import was a formula-authorized no-op because neither supported
importer path exists; no tracker manifest was written.
