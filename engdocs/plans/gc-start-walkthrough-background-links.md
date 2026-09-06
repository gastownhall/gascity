# Plan: gc start walkthrough Background links (`ga-x5v5.3`)

> PM owner: `gascity/pm`
> Source bead: `ga-x5v5.3`
> Child bead: `ga-x5v5.3.1`
> Route: `gascity/builder`

## Goal

The `gc start` troubleshooting walkthrough must not present bead-style IDs as
GitHub issue links. The Background references are informational, but they still
need to be trustworthy: either real GitHub issue URLs, local documentation
links, or removed references.

## Context

Release review found that `docs/troubleshooting/gc-start-walkthrough.mdx`
contains Background links shaped like GitHub issues URLs while using bead-style
IDs. The target page exists on the closed `ga-x5v5.1` / `ga-x5v5.2` builder
branches; this PM worktree does not currently contain the page, so the builder
should apply the fix against the release or main branch that includes those
artifacts.

Observed bad links on `origin/builder/ga-x5v5.2`:

| Link text | Bad URL |
| --- | --- |
| `PRD ga-sn06` | `https://github.com/gastownhall/gascity/issues/sn06` |
| `PRD ga-7kwr` | `https://github.com/gastownhall/gascity/issues/7kwr` |
| `PRD ga-ytx2` | `https://github.com/gastownhall/gascity/issues/ytx2` |
| `PRD ga-qpbe` | `https://github.com/gastownhall/gascity/issues/qpbe` |

The final "Still stuck?" link to
`https://github.com/gastownhall/gascity/issues` is valid repository navigation
and is not part of this bug unless the builder finds a better equivalent.

## Work Package

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-x5v5.3.1` | As an operator, I can trust gc start walkthrough Background links | `ready-to-build` -> `gascity/builder` | none |

## Acceptance Criteria

`ga-x5v5.3.1` is complete when:

1. `docs/troubleshooting/gc-start-walkthrough.mdx` has no GitHub issue links
   whose final path segment is a bead-style ID such as `sn06`, `7kwr`, `ytx2`,
   or `qpbe`.
2. Remaining Background references either resolve to real GitHub issue URLs
   with numeric issue IDs, link to an existing local docs page, or are removed.
3. The "Still stuck?" repository issue-list link is preserved or replaced only
   with an equivalent valid repository issues URL.
4. `make check-docs` passes.
5. The PR or closure note references `ga-x5v5.3` and `ga-x5v5.3.1`.

## Out Of Scope

- Changing walkthrough anchors, symptom order, FATAL URL constants, or page
  routing.
- Rewriting operator-resolution prose beyond the small text edits needed after
  removing or replacing Background references.
- Adding new troubleshooting coverage.

## Risk

The only coordination risk is branch state: the PM worktree does not contain
the target `.mdx` file, while the closed builder branches do. Builder should
start from the branch or release line that has the walkthrough artifacts, then
apply this docs-quality cleanup.
