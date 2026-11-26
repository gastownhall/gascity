# Plan: local drain-ack guard coverage

Work: `ga-82fc08ac` · Priority: P4 · Target: `gastownhall/gascity-packs`

## One coverage scope

Keep the two findings from mayor mail `gcg-17327105` and review `ga-dcai4d6l`
together in one follow-up: strengthen
`tests/test_v2_drain_ack_closes_own_step.py` and fix its Ruff E741 variable
name during the same edit. This is one test-file change and one PR, with
no formula, production, CI-workflow or dependency changes.

The preceding change, reviewed at
`193f748feeb944f3774f882198a7708c3246bb68`, makes a missing own-step close
detectable at all twelve checked acknowledgment sites. It still accepts a
claim resolution or jq step guard donated by an earlier arm of the same
fence: deleting either local component, or corrupting the local step suffix,
is detected at only six of twelve sites. Submit-and-exit acknowledgments 4-9
are the six affected sites. The lint finding is in those same helper lines.

The follow-up requires each acknowledgment's own immediately preceding
resolve/show/status-and-step-guard/fail-closed-close sequence, with consistent
identities and step suffix. Keep the existing work bead through PM,
validator and builder; do not create a separate lint or implementation bead.

## Acceptance

- Re-inventory the checked shipped sites on landed main: twelve at the
  reviewed head, comprising ten submit-and-exit, one notify-close and one
  drain. Unmodified legitimate sequences pass.
- Independently remove each site's own hook resolution, show, jq guard or
  close, and corrupt its local step suffix. The defective site must be
  reported in every case, including all six shared-fence sites. Preserve the
  predecessor's twelve-of-twelve close-removal coverage.
- Neighboring or earlier-arm sequences cannot vouch for a later ack. Two-ack
  and multi-arm fixtures report exactly the defective ack. Preserve rejection
  of wrong-variable, cross-fence, bare, non-fail-closed and ack-before-close
  cases, the array-or-object jq fixture, and every original passing test.
- New regression cases fail on the known predecessor gap before the checker
  repair, then pass with named results and an all-site mutation matrix.
  No new skip or weakened assertion is allowed. A changed shipped inventory
  returns to PM with context instead of silently reducing coverage.
- Rename the ambiguous variable and pass the target file's default Ruff
  rules. Keep this within the same edit, without adding a lint job,
  dependency or unrelated formatting pass.
- Validate in `gascity-packs`: the targeted pytest file and its current full
  CI Python pack-test step pass. Read the command and Python/dependencies
  from that checkout's `.github/workflows/ci.yml`; report actual execution
  and justified skips. The review kit documents the venv and normal-umask
  requirements. This PM plan claims no target-repository test execution.

## Prerequisites and ownership

| Record | Purpose | Disposition |
| --- | --- | --- |
| `ga-2esxku7d` | Existing predecessor deploy | Existing deploy and merge chain; unchanged |
| `ga-0jskfka2` | Verify main landing and authorize the new non-rig workspace | Open, unassigned, unrouted record; never sling |
| `ga-82fc08ac` | One P4 future test/build scope | PM while the prerequisites remain open |

The work has same-store blocking edges to the deploy and prerequisite
record. Provenance is `discovered-from:ga-dcai4d6l` on the work and
`discovered-from:ga-82fc08ac` on the record; neither is a scheduling edge
back to the work. The scheduling graph is acyclic.

Fetched `gascity-packs` main `44b9db7168f6e983d5b6798164724f405d105a4b`
still has the old whole-fence checker. Gate completion or a closed build is
insufficient. Mayor is asked to own the prerequisite record and close it
only with the actual predecessor PR/main squash SHA, normal landing evidence
and inspection of the per-ack close/identity-binding checker on main.

This repo has no rig. The original workspace authorization on `ga-r108z7`
was for that bead. The proposed new workspace is
`/home/jaword/projects/gascity-packs-worktrees/ga-82fc08ac`, branch
`validator/ga-82fc08ac`, created from landed main by validator after admission.
Mayor must record authorization for this workspace or a named equivalent;
authorization does not claim the worktree already exists. The agent session
stays in its gc-provisioned Gas City worktree. The live Discord gateway
checkout at `/home/jaword/projects/gascity-packs` receives no assigned edits,
checkout, stash, reset, clean or fetch-and-merge. Metadata operations use a
safe worktree handle. Do not reuse the predecessor branch or edit worktree.

After both exits, mayor notifies PM with `--notify`, naming the work and
record. PM re-reads main, content, outcomes and workspace authorization, then
relabels this SAME bead `needs-tests` and slings it to validator with
`--nudge`. Validator authors RED and hands the same bead and workspace to
builder for GREEN. Use normal machinery admission, with no stacked base,
predecessor-head amendment or premature worker admission.

## Evidence and PM disposition

The existing review kit is under
`/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-dcai4d6l`:
`README.md`, `helpers/mutate-shipped.py` and `logs/mutate-shipped.log` were
confirmed present. The live beads carry the complete scope, so the kit is
supporting reproduction evidence rather than the only specification.

Publish and verify this plan, then release PM's documentation claim. Keep the
source open, unassigned, `needs-pm` and routed to PM behind its prerequisites.
The PM pointer is deliberately recorded without a self-sling or PM formula;
future validator routing belongs to `gc sling`. The prerequisite record has
no route or worker intake label. No implementation or landing is claimed.
