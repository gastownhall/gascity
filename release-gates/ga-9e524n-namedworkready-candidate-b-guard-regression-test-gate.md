# Release Gate: namedWorkReady Candidate-B guard regression test

- Bead: ga-9e524n
- Source builder bead: ga-7x9khs
- Source review bead: ga-mojprz
- Final branch: deploy/ga-9e524n-namedworkready-candidate-b-guard-clean
- Base: origin/main at 4189caf4fcac0d7b199a143346c2c1b704674994
- Candidate commit: 14a9fba2be5c7bf3b764c244b6c4d16ebb71ab74
- Original reviewed commit: 5de45c92488a5ee40d847faaf5ce1398e3e0fd57
- Evaluated: 2026-07-05T21:54:30Z

## Summary

PASS. Prerequisite PR #3865 is merged, so this deploy branch was evaluated as
a clean single-commit branch from current origin/main. The candidate only adds
the regression test for namedWorkReady Candidate-B behavior when an
expanded-identity named session has no canonical session bead.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-mojprz` is closed with reason `pass` and notes include `REVIEW: PASS (gascity/reviewer)`. |
| 2 | Acceptance criteria met | PASS | `bd show ga-7x9khs` asks for one self-contained in-memory test in `cmd/gc/build_desired_state_ga80pen8_test.go`, matching sibling test style, with gofmt/go vet/fast-unit coverage clean and a doc comment referencing ga-tpe9od. The candidate adds only `TestNamedWorkReady_ExpandedIdentityTemplate_NoCanonicalBead_DoesNotMaterialize`. |
| 3 | Tests pass | PASS | `TMPDIR=/var/tmp/gc-ga-9e524n-test make test-fast-parallel` passed all fast shards. `TMPDIR=/var/tmp/gc-ga-9e524n-test go vet ./...` passed. `git diff --check origin/main..HEAD` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes have no open HIGH findings; the review marks security N/A because this is a pure in-memory Go unit test with no external input, auth, serialization, or injection surface. |
| 5 | Final branch is clean | PASS | `git status --short` was empty before writing this gate file. The only release-only change is this gate checklist, committed as the branch tip. |
| 6 | Branch diverges cleanly from main | PASS | The deploy branch is based directly on current `origin/main`; `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` reports a clean merge. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and one purpose: one `cmd/gc` desired-state regression test for namedWorkReady Candidate-B behavior. |

## Diff Scope

```text
cmd/gc/build_desired_state_ga80pen8_test.go | 77 +++++++++++++++++++++++++++++
1 file changed, 77 insertions(+)
```

## Commits

```text
14a9fba2b test(reconciler): pin down namedWorkReady Candidate-B guard vs. no-canonical crash recovery
```

## Notes

The original reviewed branch commit was `5de45c924` on
`builder/ga-7x9khs-namedworkready-nocanonical-test`. That branch was stacked on
the pre-squash prerequisite for PR #3865, so it is not suitable as a PR head
after #3865 merged. This deploy branch keeps the reviewed test-only change and
drops the obsolete prerequisite stack from the review surface.
