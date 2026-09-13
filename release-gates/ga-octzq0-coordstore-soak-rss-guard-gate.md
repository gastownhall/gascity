# Release Gate: Coordstore soak RSS guard

Bead: ga-octzq0 - needs-deploy: coordstore soak RSS guard (from:ga-tcey6d)
Source bead: ga-pzgem
Review bead: ga-tcey6d
Feature branch: builder/ga-pzgem-soak-mem-ceiling
Head under gate: 145980743c8de6e51ce457fad1eb7b99ed7a0549
Base checked: origin/main f1365b85d5510f38622034f9cb333ca5fd553f31

Note: docs/PROJECT_MANIFEST.md is not present in this checkout. This gate uses
the deployer release criteria and the repository testing guidance in TESTING.md.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-tcey6d is closed with `REVIEWER VERDICT: PASS (rev2)` for commit 145980743. |
| 2 | Acceptance criteria met | PASS | Source bead ga-pzgem required the coordstore soak harness to abort and report memory leaks instead of running to OOM. The branch adds RSS ceiling and growth-rate guards, first-class `Scorecard.LeakAborted` / `LeakFinding` reporting, default soak guard activation, and deterministic guard tests. |
| 3 | Tests pass | PASS | `go test ./internal/benchmarks/coordstore` passed in 21.571s. `make test-fast-parallel` passed all fast shards. `go vet ./...` passed with no output. |
| 4 | No high-severity review findings open | PASS | Prior ga-kg17no BLOCKER/HIGH/MEDIUM findings were all resolved in 145980743 and re-reviewed as PASS in ga-tcey6d. No new findings were recorded by the PASS review. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` showed the branch clean at `builder/ga-pzgem-soak-mem-ceiling...origin/builder/ga-pzgem-soak-mem-ceiling`. The only deployer change is this release-gate file, to be committed as the gate evidence. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree 39a29f033653208b611692f8683f95ae0da0eae6. The branch is 2 commits ahead and 2 commits behind origin/main, with no merge conflicts. |
| 7 | Single feature theme | PASS | The feature diff from the merge-base touches only `internal/benchmarks/coordstore/{runner.go,runner_leak_test.go,scorecard.go,suite_test.go,workload.go}`. The commit set is one coordstore soak memory-guard theme. |

## Acceptance Evidence

- `internal/benchmarks/coordstore/workload.go` adds `MaxRSSBytes` and
  `MaxRSSGrowthBytesPerSec` to workload and soak configuration.
- `internal/benchmarks/coordstore/runner.go` starts the memory sampler with
  the configured guard values, cancels the run on a guard breach, records the
  leak finding on the scorecard, and checks `rss > warmUpRSS` before subtracting.
- `internal/benchmarks/coordstore/suite_test.go` enables production defaults
  for real soak runs: 8 GiB RSS ceiling and 50 MiB/s RSS growth-rate ceiling,
  both overrideable by typed environment variables.
- `internal/benchmarks/coordstore/runner_leak_test.go` covers ceiling abort,
  growth-rate abort, and runner-level scorecard abort behavior.

## Test Log

```text
$ go test ./internal/benchmarks/coordstore
ok  	github.com/gastownhall/gascity/internal/benchmarks/coordstore	21.571s

$ make test-fast-parallel
All fast jobs passed

$ go vet ./...
PASS (no output)
```
