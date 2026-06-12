# Release gate: ga-q3gwx0

**Deploy bead:** ga-q3gwx0 - needs-deploy: gc rig add comment preservation (from:ga-48s4bw)  
**Source review bead:** ga-48s4bw  
**Implementation bead:** ga-un5mg3.1  
**Branch:** `builder/ga-un5mg3.1`  
**PR:** https://github.com/gastownhall/gascity/pull/3414  
**Code HEAD before gate:** `e5f38b77878e5fb74b5ec264db5be16d53cbb323`  
**Base:** `origin/main` at `331f5b23aed65a8c290817e6ec2545bbf919cde0`  
**Verdict:** **PASS**

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-48s4bw` is closed with `VERDICT: PASS`; reviewer notes report the implementation is clean to deploy. |
| 2 | Acceptance criteria met | PASS | The release branch is a clean one-commit PR for the rig-add comment-preservation fix, records PR #3414 / branch / commit in `ga-un5mg3.1`, limits the diff to the three approved files, includes `TestDoRigAdd_PreservesComments`, and does not reuse PR #3402. |
| 3 | Tests pass | PASS | GitHub checks for PR #3414 are green, including required CI, CodeQL, preflight, cmd/gc process shards, integration shards, worker suites, and pack compatibility. Local focused regression, build, and vet passed; see Test Runs for the local fast-baseline environment note. |
| 4 | No high-severity review findings open | PASS | Review notes list only LOW/INFO non-blocking findings; unresolved HIGH count is 0. GitHub PR reviews/comments are empty at gate time. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate file. After this file is committed as the gate commit, the branch should be clean again. |
| 6 | Branch diverges cleanly from main | PASS | `gh pr view 3414` reports `mergeStateStatus: CLEAN`; `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` exits 0. |
| 7 | Single feature theme | PASS | Diff scope is limited to `cmd/gc/cmd_rig.go`, `cmd/gc/cmd_rig_test.go`, and `internal/config/site_binding.go`, all serving one behavior: preserving existing `city.toml` comments on new `gc rig add` writes while keeping site bindings consistent. |

## Test Runs

```
$ go test ./cmd/gc -run '^TestDoRigAdd_PreservesComments$' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	0.436s

$ go build ./cmd/gc
(clean)

$ go vet ./...
(clean)

$ gh pr checks 3414 --watch=false
All required GitHub checks pass, including CI / required, CodeQL, preflight,
cmd/gc process shards, package integration shards, worker suites, and pack
compatibility.
```

Local broad baseline note:

```
$ make test-fast-parallel
FAIL in local cmd/gc shards due host test-environment contamination.
```

The local host has a live `/tmp/.gc` city marker owned by a Dolt sql-server
process. `TestFindCity/not_found` is hard-coded to create under `/tmp`, so it
fails on this host even with a clean `HOME` and short `TMPDIR`. The same
focused no-city/supervisor set also fails on `origin/main`, so this is not
introduced by the PR branch. With clean `HOME=/var/tmp/gchq3` and short
`TMPDIR=/var/tmp/gctq3`, the three standalone-controller supervisor tests and
`TestSupervisorCreatesControllerSocketForManagedCity` pass; only
`TestFindCity/not_found` remains blocked by the live `/tmp/.gc`.

## Diff Scope

```
cmd/gc/cmd_rig.go
cmd/gc/cmd_rig_test.go
internal/config/site_binding.go
```

