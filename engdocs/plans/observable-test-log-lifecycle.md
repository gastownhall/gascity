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

| Stage | Bead | Route | Acceptance outcome |
| --- | --- | --- | --- |
| 1 | ga-4oym3q | architect (`needs-architecture`) | Decide and record a bounded retention and ownership contract for default and explicit logs, failure diagnostics, gate consumers, and legacy eligibility. |
| 2 | ga-rqq0vq | validator (`needs-tests`) | Write isolated failing tests for that contract, including named PASS evidence, failures, timing artifacts, concurrency, and abnormal exits. |
| 3 | ga-kjxikg | builder (`ready-to-build`) | Make passing runs follow the contract without changing test results or losing reviewer/deployer evidence; demonstrate that repeated runs stop growing unmanaged shared scratch files. |
| After 1, parallel with 2–3 | ga-5w6zfy | builder (`ready-to-build`) | Deliver a dry-run inventory and narrowly scoped reclaim procedure for the old backlog. Live compression follows only the approved policy and mayor's disk-pressure trigger. |

Dependency graph: ga-4oym3q → ga-rqq0vq → ga-kjxikg, and ga-4oym3q →
ga-5w6zfy. The architect ruled that the legacy reclaim procedure is independent
of the wrapper change, so PM removed its ga-kjxikg blocker on October 3. The
reclaim package can proceed in parallel with tests and implementation. Every
package has a `discovered-from:ga-kpq7ib` edge so its admission origin stays
visible.

## Decision boundary and risks

Architect decision ga-4oym3q sets the implementation contract: default logs
are wrapper-owned in a dedicated 0700 directory under the scratch root, stay
readable for at least 72 hours after their last write, and are age-pruned by
later wrapper runs. An explicit `OBSERVABLE_TEST_LOG` remains caller-owned.
Legacy files use a separate dry-run-first, fail-closed, compress-only procedure;
the disabled scratch cleaner stays untouched. See the decision bead for the
complete safety guards and test cases.

Two failures would make a nominal disk fix unsafe: deleting logs before gate
consumers finish, and reclaiming a legacy file that still belongs to an active
run or a cited gate. The separate legacy package and its direct dependency on
the architecture decision keep those risks explicit. At the October 3 recheck,
`/` had 58 GB free, above the mayor's under-30-GB trigger for live relief. No
package re-enables the disabled fleet scratch cleaner as a shortcut.

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
