---
title: Contributor Response and Attribution Conventions
description: What a contributor is owed when they file, and how credit is recorded when we supersede a PR, adopt and finish one, or close a duplicate.
---

## Why this exists

Maintainer attention is the scarce resource in this repo, and rationing it
silently is what breaks contribution. Measured on 2026-08-30 against the live
GitHub API:

- 191 of 634 open issues (30%) have no comment of any kind. That is a lower
  bound on the unanswered set, since it does not count issues whose only
  replies are from bots or from the filer.
- 412 of 446 open pull requests carry no review decision. 33 sit at
  `CHANGES_REQUESTED` and 1 at `APPROVED`.
- A classification pass over 425 of those PRs put 96 in "the underlying problem
  is real and unfixed on main, but the branch has drifted too far to rebase
  cheaply."

Reproduce the first two with:

```
gh api --paginate 'repos/gastownhall/gascity/issues?state=open&per_page=100' \
  --jq '.[] | select(.pull_request == null) | [.number, .comments] | @tsv'
gh pr list --repo gastownhall/gascity --state open --limit 600 --json number,reviewDecision
```

The cost of that lands somewhere specific. Four separate contributors opened
PRs against the same gap in Darwin `pidutil` fail-closed coverage (#5254,
#5291, #5345, #5746), three of them landing on materially the same PID-1 probe,
because none of the earlier ones got an answer saying the gap was already
covered.

None of that is a review-throughput problem. Reviewing every PR is not
achievable and is not the goal. Answering is. A contributor told "not this one,
and here is why" has been treated well. One whose PR goes quiet for four months
has not, and neither has the next person who rewrites it.

## Who this binds, and what an agent may do

The rules below describe what a **maintainer** owes a contributor. An agent
seat may draft any of these replies and may gather the evidence a verdict rests
on, but posting a comment, closing an issue or PR, pushing to someone else's
branch, and merging are external actions that reach a contributor. They require
explicit per-action authorization from the active user, repository, or
orchestrator instructions; a verdict existing is not authorization to publish
it. This is distinct from the session-close push in `AGENTS.md`, which governs
an agent's own work branch and not someone else's PR.

Where an acknowledgement is agent-drafted and not yet reviewed by a human, it
says so rather than implying a person has read the code.

## What a contributor is owed on filing

One reply that states a decision or names what the decision is waiting on. It
does not have to be a full review, and it should not wait for one.

- **Who owes it:** any maintainer. This is deliberately not assigned to a
  specific person, because assignment is how it stops happening.
- **By when:** within 14 days of filing. An item older than that with no reply
  is in violation of this convention and is a valid thing for anyone to raise.
- **An issue** gets a reply stating whether the described behavior is one we
  agree is a bug, and what would move it forward. "We agree, nobody is on it, a
  PR would be welcome" is a complete answer.
- **A PR** gets a reply stating whether the direction is wanted, before any
  line-level review. A contributor who learns in week one that the approach is
  wrong has lost an afternoon. One who learns it in month four has lost the
  afternoon plus every rebase since.
- If the answer needs a decision we cannot make yet, say that, and say what it
  is waiting on.

Silence is the one response this convention forbids.

## The five dispositions

Every close or merge resolves to one of these. The attribution owed depends on
which.

| Disposition | Meaning | Attribution owed |
|---|---|---|
| MERGE | Ships as authored. | Normal commit authorship. |
| ADOPT_AND_FIX | We finish it ourselves and it ships under their name. | See below. |
| SUPERSEDE_WITH_CREDIT | A better implementation of the same idea landed, or the real fix is at another layer. | See below. |
| DUPLICATE | Another PR addresses the same defect. | Name the survivor. |
| DECLINE | Not wanted at all. | A stated technical reason. |

ADOPT_AND_FIX is the default for a defect that is reproducible, localized, and
fixable without input only the author has. Bouncing such a defect back as a
change request is the failure this default exists to prevent.

## Attribution on supersede

The code is not what we are taking when we supersede. The diagnosis is, and the
note says so. A supersede note contains three things:

1. The contributor's handle and what specifically their work contributed. "You
   identified the writer race" is attribution. "Thanks for the PR" is not.
2. The technical reason this branch is not the one being carried forward,
   stated as a property of the code rather than of the contributor.
3. A link to the superseding work, by number.

Wrong-layer PRs are where this most often goes wrong. When a contributor
patches a symptom and the real defect is a layer down, their analysis is
usually right and their patch is usually still not what should land. Close it
in favor of the root-cause fix, credit the analysis by name, and explain why
the workaround is no longer needed. #3458 and #3616 are the worked examples:
both closing notes identify the contribution, explain the layer, and link the
replacement.

## Attribution on adopt-and-fix

The contribution stays theirs in the record. Two mechanisms, in order of
preference:

1. **Push the fixup onto their branch and merge their PR.** History and the
   merged PR both carry their name, and the review conversation stays in one
   place.
2. **When we cannot push to their branch**, open a replacement PR that
   preserves their commits and authorship rather than reimplementing the
   change. Link both directions and say in the replacement's body whose work it
   carries. The common cause is a fork opened without maintainer edits enabled,
   which is the contributor's setting to make and not something to argue about.
   #1732 and #1742 are the worked examples of preserving authorship through a
   replacement: #1732's closing note names maintainer edits being disabled, and
   #1742's names being unable to push commits to the branch.

Never reimplement the change as fresh work and close theirs as a duplicate of
our own.

The fixup or replacement comment states what we changed and why, at the level
of detail the contributor needs in order to agree with it. Open with warmth
rather than with a verdict-shaped judgment of their work; a thank-you that
names what was useful is doing real work, and a bare "thanks" is not.

## Attribution on duplicate close

Name the survivor, always, by number. A duplicate close that does not say what
survived reads as a rejection.

Say plainly that the duplication is a timing artifact rather than a quality
judgment when that is true. It usually is: three people fixing the same bug in
one week means the bug was easy to find and nobody was told it was covered,
which is our failure and not theirs.

When several PRs cluster on one defect, pick the survivor on properties of the
code, not on who filed first:

1. **Eligibility, not a tiebreaker:** a behavior change carries regression
   coverage. `AGENTS.md` and `CONTRIBUTING.md` both require it. An untested
   diff does not become the survivor by being smaller; if the only tested
   candidate is the larger one, that one survives, and if none is tested the
   cluster is not ready to resolve.
2. Among eligible candidates, prefer the smallest correct diff.
3. Then prefer the one with existing review activity, since that conversation
   is worth keeping.

A shared subject line is a hypothesis, not a finding. Confirm two PRs address
the same defect by reading both diffs before closing either. A stack of related
PRs sharing a base branch is not a duplicate cluster, and neither is a pair of
PRs that touch adjacent lines of one file for unrelated reasons.

## What not to do

- Close without a reason. A bare close with no comment is worse than leaving it
  open, because it converts an open question into a decision nobody can see the
  basis for.
- Close a duplicate without naming the survivor.
- Let a PR go quiet as a de facto decline. If the answer is no, the answer is
  no in writing. A stale PR everyone privately knows will not merge still costs
  its author every rebase they do.
- Credit in the merge commit only. Merge commits are not read; the comment the
  contributor receives is.
- Open with agreement performance. "Great catch" and "you're absolutely right"
  ahead of a technical verdict read as filler. State the verdict; the
  thank-you carries the warmth.
