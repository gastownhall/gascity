# Release Gate: ga-6sdnap wisp_events mail INSERT tripwire

Date: 2026-06-15
Deployer: gascity/deployer
Bead: ga-6sdnap
Source branch: builder/ga-v62dur-mail-integration-test
Base checked: origin/main@85768eeba752bfe2d8e066a6110a29e04f2e6b61
Candidate checked: d0e3d7562bf64732273e0e5ca1fe7861824c95e5

`docs/PROJECT_MANIFEST.md` is not present in this checkout. Release criteria were evaluated against the deployer prompt gate table and the repo testing guidance in `TESTING.md`.

## Summary

This is a single-bead deploy for a test-only regression tripwire around `gc mail` ephemeral-message creation. The candidate adds integration coverage for `wisp_events` INSERTs through both the native Dolt-backed store path and the bd CLI backed store path, then wires the focused check into the bdstore integration shard and a local `make test-mail-wisp-insert` target.

Diff scope against `origin/main`:

- `Makefile`
- `internal/beads/native_dolt_store_integration_test.go`
- `scripts/test-integration-shard`
- `test/integration/bdstore_test.go`

## Gate Checklist

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-4fvqpk` is closed with `REVIEW VERDICT: PASS` from `gascity/reviewer`. Deploy bead `ga-6sdnap` also records reviewed + PASSED evidence for commit `d0e3d7562bf64732273e0e5ca1fe7861824c95e5`. |
| 2 | Acceptance criteria met | PASS | Source bead `ga-v62dur` requires a real beads-backed mail path that exercises `wisp_events` INSERT and fails on the prior `wisp_events.id` NOT NULL/version-skew regression. The candidate adds `TestNativeDoltStoreEphemeralMailSend`, `TestBdStoreMailWispInsert`, bdstore shard wiring, and `make test-mail-wisp-insert`. The reviewer noted one LOW non-blocking gap: no separate `Get(sent.ID)` content assertion; the release-blocking INSERT tripwire objective is covered. |
| 3 | Tests pass | PASS | `make test` passed with observable log `/tmp/gascity-test.jsonl.JQYePa`; `go vet ./...` passed; `go build ./cmd/gc` passed; `make test-mail-wisp-insert` passed both `TestNativeDoltStoreEphemeralMailSend` and `TestBdStoreMailWispInsert`. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-4fvqpk` list no HIGH findings and no security findings. The only recorded gap is LOW/non-blocking. |
| 5 | Final branch is clean | PASS | Before writing this checklist, `git status --short --branch` in the feature worktree showed a clean `builder/ga-v62dur-mail-integration-test...origin/builder/ga-v62dur-mail-integration-test` branch. This checklist is the only deployer-added file and is committed as the branch tip before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` exited 0 against `origin/main@85768eeba752bfe2d8e066a6110a29e04f2e6b61`. The branch is behind current main by `85768eeba chore(config): bump gastown pack pin to release 0.1.6 (#3516)`, but has no merge conflicts with main. |
| 7 | Single feature theme | PASS | The branch contains one candidate commit and touches one subsystem theme: regression coverage for `wisp_events` mail INSERT behavior and the runner wiring for that coverage. No independent feature theme is bundled. |

## Test Evidence

Commands run on `builder/ga-v62dur-mail-integration-test`:

```bash
make test
go vet ./...
go build ./cmd/gc
make test-mail-wisp-insert
```

Focused smoke output included:

```text
--- PASS: TestNativeDoltStoreEphemeralMailSend (0.59s)
ok  	github.com/gastownhall/gascity/internal/beads	0.610s
--- PASS: TestBdStoreMailWispInsert (1.22s)
ok  	github.com/gastownhall/gascity/test/integration	11.908s
```

## Scope Evidence

`git log --oneline --left-right --cherry-pick origin/main...HEAD` before the gate commit:

```text
< 85768eeba chore(config): bump gastown pack pin to release 0.1.6 (#3516)
> d0e3d7562 test(beads): add wisp_events INSERT regression tripwire for gc mail
```

`git diff --stat origin/main...HEAD` before the gate commit:

```text
 Makefile                                           | 10 +++-
 .../beads/native_dolt_store_integration_test.go    | 64 ++++++++++++++++++++
 scripts/test-integration-shard                     |  4 +-
 test/integration/bdstore_test.go                   | 70 ++++++++++++++++++++++
 4 files changed, 145 insertions(+), 3 deletions(-)
```
