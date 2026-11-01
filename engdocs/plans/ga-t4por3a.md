# Plan: honor the final create-file flag

Root: `ga-t4por3a`  
Goal class: bug  
Generated: 2026-11-01  
Base examined: `7472f2967c2f80481146693c2a69fc61451d52e6` (PR #8608)

A split-city operator who supplies repeated `--graph` flags can be refused
because the early create classifier reads the first path while bd and the graph
judge read the last. The repair makes the guards select the payload bd will
consume, preserving the existing class protections.

## Scope and acceptance

| Command tail | Required observable result |
| --- | --- |
| `--graph infra.json --graph work.json` | Forward the work-only plan and point bd at a private copy of the final work.json bytes. |
| `--graph work.json --graph infra.json` | Exit 1 before a write; the sessions-class refusal identifies the disallowed node. |
| `--graph infra.json --graph= --title x` | Judge an argv-shaped single create; the superseded file does not select its storage class. |

The file-form classifier honors the final occurrence and consumes separated
flag values as values, including flag-like values. Existing manifest-aware
spellings, missing/malformed input behavior, single-store behavior and
private-copy protections retain their contracts.

## Work package

| Bead | Routing | Deliverable |
| --- | --- | --- |
| `ga-ln0grpv` | `needs-tests` → validator → same bead to builder | Independently establish RED for the three doBd paths, then implement the specified classifier/refusal repair and demonstrate GREEN. |

This is one atomic test-first scope. The same bead carries its `Build:` scope;
there is no second implementation successor, design step or rollup. The normal
review/deploy path produces one small separate PR from a fresh fix branch.
PR #8608 is already merged and is not modified or re-reviewed.

## Dependencies

`ga-ln0grpv` is blocked by the final PM handoff step `ga-eun52vl` until this plan
is committed and the handoff is verified. Both ends live in the gascity store.
Its `discovered-from:ga-t4por3a` edge preserves the MPR origin. The graph is
acyclic; no implementation successor waits behind a tests bead of the same scope.

## Validation and handoff

Regression evidence must exercise `doBd` in
`cmd/gc/bd_split_graph_judge_test.go`, including private-copy selection and the
refused node, rather than only testing the helper. The prescribed behavior in
`cmd/gc/bd_relocated_classes.go` comes from the source report. Use Bazel and the
owning repository instructions; record the initial failure and actual named
PASS results, then run the normal project checks. PM has written no code or
tests and claims no implementation result.

## Evidence and bounds

The full three-case request is in the local MPR executor report:
`.gc/maintainer-pr-review/gastownhall-gascity/pr-8608/runs/20261101T003102Z/fix-executor-output.md`,
lines 21–29 of the gc-management checkout. The routed root description was
truncated before the cases; the report resolves that omission.

No tracker import script is installed, so tracker import was a no-op. The remote
main ref was independently checked with `git ls-remote` and matched the examined
base after the worktree helper's initial fetch timed out. No new architecture,
storage syntax, policy, API, UI or runtime activation is needed.
