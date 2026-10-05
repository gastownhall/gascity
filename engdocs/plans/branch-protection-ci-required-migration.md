# Plan: branch protection CI / required migration

Owner: `gascity/pm`
Created: 2026-06-19
Source beads: `ga-uqxysq`, `ga-m133uw`

## Goal

Replace the narrow branch-protection requirement on `Check` with the
comprehensive `CI / required` gate for `gastownhall/gascity` ruleset
`14017226`, while preserving contributor clarity and avoiding a surprise
block on open PRs.

The architect ADR `ga-m133uw` settles the policy: replace `Check` with
`CI / required`, keep the four CodeQL checks, keep
`strict_required_status_checks_policy` false, and do not add
`Integration / review-formulas` while that workflow is red and separately
tracked. The designer handoff `ga-uqxysq` adds the transition sequence,
open-PR communication, and CONTRIBUTING.md guidance.

## Work Packages

| Bead | Title | Route | Gate |
| --- | --- | --- | --- |
| `ga-uqxysq.1` | As a maintainer, I know CI / required is green on main before changing protection | `gascity/builder` | `ready-to-build` |
| `ga-uqxysq.2` | As a maintainer, I know which open PRs the gate change will affect | `gascity/builder` | `ready-to-build` |
| `ga-uqxysq.3` | As an operator, I can replace Check with CI / required in ruleset 14017226 | `gascity/builder` | `ready-to-build` |
| `ga-uqxysq.4` | As a maintainer, I can verify branch protection requires the intended five contexts | `gascity/builder` | `ready-to-build` |
| `ga-uqxysq.5` | As a PR author, I receive notice when the new gate may affect my open PR | `gascity/builder` | `ready-to-build` |
| `ga-uqxysq.6` | As a contributor, I can understand the new CI / required merge gate in CONTRIBUTING | `gascity/builder` | `ready-to-build` |
| `ga-uqxysq.7` | As a maintainer, I can confirm new PRs experience the CI / required gate correctly | `gascity/builder` | `ready-to-build` |

## Dependency Graph

```text
ga-uqxysq.1 (green-main pre-flight)
  -> ga-uqxysq.3 (apply ruleset)
    -> ga-uqxysq.4 (verify ruleset)
      -> ga-uqxysq.5 (post affected-PR comms)
      -> ga-uqxysq.6 (update CONTRIBUTING.md)
        -> ga-uqxysq.7 (monitor post-change PRs)

ga-uqxysq.2 (open-PR audit)
  -> ga-uqxysq.5 (post affected-PR comms)
```

The pre-flight green-main check and open-PR audit can start in parallel. The
ruleset mutation waits for the pre-flight. Post-apply communication waits for
both the audit and verification. Monitoring waits for verification, PR comms,
and the docs update.

## Acceptance Summary

`ga-uqxysq.1` is complete when builder records the current `main` commit,
confirms `CI / required` is completed and successful on that commit, and
confirms the current ruleset still has the expected pre-migration shape. A red
or missing `CI / required` result blocks the apply bead.

`ga-uqxysq.2` is complete when builder records the current open PR list and
identifies any PRs where `CI / required` is missing, pending, or failing. It
must not comment on or modify PRs.

`ga-uqxysq.3` is complete when builder applies the ADR-approved API payload to
ruleset `14017226`, preserving the four CodeQL checks, keeping
`strict_required_status_checks_policy` false, and avoiding
`Integration / review-formulas` or unrelated required contexts.

`ga-uqxysq.4` is complete when builder verifies the required status contexts
are exactly `CI / required`, `Analyze (actions)`, `Analyze (go)`,
`Analyze (javascript-typescript)`, and `Analyze (python)`, with `Check`
removed and strict status checks still false.

`ga-uqxysq.5` is complete when builder posts the designer-approved notice only
on affected open PRs, or records evidence that no PR comments are needed. It
must not merge, close, rerun, or modify PR branches.

`ga-uqxysq.6` is complete when CONTRIBUTING.md explains the new required gate
in plain language: code-changing PRs may take roughly 20-40 minutes, docs-only
or unaffected-path PRs remain fast because path-gated jobs skip automatically,
and authors can use GitHub's checks panel to identify the failing sub-job.

`ga-uqxysq.7` is complete when builder monitors the next 2-3 relevant PRs, or
current open PRs after rerun, and records whether merge blocking, CodeQL, and
path-gated skip behavior match the ADR and design expectations. If behavior
differs, builder files or routes a follow-up with evidence instead of loosening
branch protection.

## Handoff Notes

All child beads route to `gascity/builder` because the design phase is
complete. No bead should route back to designer. Builder should treat the
ruleset change as an operator-side GitHub API mutation, not a workflow-file
edit. The only repository file change requested by this plan is the
CONTRIBUTING.md update in `ga-uqxysq.6`.

If the ruleset has drifted from ADR assumptions or GitHub permissions prevent
the mutation, builder should stop and route exact evidence back to PM. Do not
invent a different branch-protection policy in the implementation step.

## Risks

The main risk is applying the ruleset while `CI / required` is red on `main`,
which would immediately block all PRs. The pre-flight bead exists to prevent
that. The second risk is surprising authors with newly blocked open PRs; the
audit and targeted comms beads address that. The third risk is accidentally
adding `Integration / review-formulas` or enabling strict up-to-date checks;
both are explicitly out of scope by ADR.
