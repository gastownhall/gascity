# Plan: minimal city docs upstream tracking

> PM owner: `gascity/pm`
> Source bead: `ga-1izs99`
> External issue: gastownhall/gascity#3089

## Decision

Do not create local builder or designer beads for the "Complete Minimal City"
example page right now.

## Rationale

The triager handoff identifies GitHub issue #3089 as Issue 7 of @esciara's own
User Docs Overhaul work plan. The supporting design document is on
`docs/user-docs-overhaul-plan`, not merged to `main`, and @esciara is already
driving the effort.

Creating local implementation beads now would duplicate in-flight docs work and
risk conflicting information architecture or tutorial-content decisions. The
right PM action is to track the upstream issue and wait for either a merged
design baseline or an explicit request to contribute content.

## No Downstream Work Packages

No child beads were created.

## Reopen Criteria

Create local docs work only if one of these becomes true:

- Mayor asks Gas City agents to contribute content to #3089.
- The User Docs Overhaul design lands on `main` and leaves a clear local gap.
- A follow-up bead requests a specific review, copyedit, or implementation slice
  that does not duplicate @esciara's in-flight work.

## Out Of Scope

- GitHub comments or issue edits.
- Selecting the final docs IA location for `minimal-city.md`.
- Writing the example page before the upstream docs plan settles.
