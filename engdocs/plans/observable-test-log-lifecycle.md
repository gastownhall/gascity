# Observable test log lifecycle (ga-kpq7ib)

## Goal and urgency

Passing `scripts/go-test-observable` runs leave their default JSON action log in
shared `/var/tmp`. The reviewer measured 321 files using 14 GB on October 2;
the mayor measured 393 files using 10.7 GB on October 3 and raised this to P2.
Counts and sizes vary as the host changes, but the runner has no owner or expiry
for a default passing log. The fleet scratch cleaner is disabled because of
gm-7baceg. Reviewer and deployer gates read named PASS events after a run, so
their evidence window must survive the change.

This plan separates future log ownership from treatment of existing files.
Nobody should remove shared-host logs merely because a run has passed: some
belong to active gates or are cited as evidence.

## Work packages

| Order | Bead | Route | Acceptance outcome |
| --- | --- | --- | --- |
| 1 | ga-4oym3q | architect (`needs-architecture`) | Decide and record a bounded retention and ownership contract for default and explicit logs, failure diagnostics, gate consumers, and legacy eligibility. |
| 2 | ga-rqq0vq | validator (`needs-tests`) | Write isolated failing tests for that contract, including named PASS evidence, failures, timing artifacts, concurrency, and abnormal exits. |
| 3 | ga-kjxikg | builder (`ready-to-build`) | Make passing runs follow the contract without changing test results or losing reviewer/deployer evidence; demonstrate that repeated runs stop growing unmanaged shared scratch files. |
| 4 | ga-5w6zfy | builder (`ready-to-build`) | Deliver a dry-run inventory and narrowly scoped reclaim procedure for the old backlog. A live cleanup follows only the approved policy and mayor's disk-pressure trigger. |

Dependency graph: ga-4oym3q → ga-rqq0vq → ga-kjxikg → ga-5w6zfy.
The legacy-log package also depends directly on ga-4oym3q. Every package has a
`discovered-from:ga-kpq7ib` edge so its admission origin stays visible.

## Decision boundary and risks

The architect chooses whether a passing log is compressed, placed in a caller
owned directory, retained only for an explicit consumer, or handled another
way. PM acceptance criteria state the observable result, not the storage
mechanism. The design must account for the runner's current `OBSERVABLE_TEST_LOG`
path and for the reviewer/deployer recipes that inspect PASS actions by name.

Two failures would make a nominal disk fix unsafe: deleting logs before gate
consumers finish, and reclaiming a legacy file that still belongs to an active
run or a cited gate. The separate last package and its direct dependency on the
architecture decision keep those risks explicit. No package re-enables the
disabled fleet scratch cleaner as a shortcut.

## Source checks

- `scripts/go-test-observable` creates a unique default log under
  `${TMPDIR:-/var/tmp}` and prints its path on PASS and FAIL; it removes only a
  caller supplied `OBSERVABLE_TEST_LOG` before use.
- `scripts/go_test_observable_test.go` tests unique default paths and explicit
  log and timing behavior; the new contract needs a test-first update.
- `Makefile` and `scripts/test-go-test-shard` call the wrapper for local and
  sharded test gates.
- Reviewer report ga-kpq7ib and the mayor's October 3 note supply the host
  measurements and the rule against speculative hand deletion.
