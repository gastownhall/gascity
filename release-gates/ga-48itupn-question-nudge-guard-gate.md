# Release gate: pending question nudge guard

**Verdict:** **PASS**

Evaluated: 2026-10-28T16:31:12+00:00. Deploy bead: ga-48itupn; build: ga-61fduec; review: ga-8d8q93a.
Reviewed source: `cb1a29a5ca55749337c9ba241240c36ca07646ce`. Deploy source remains that exact commit;
the only added deployment commit contains this record.

`deploy_mode: remote` · `base_ref: origin/main`

merge_state: clean (origin/main@b52c9105bad9aba447cb8220ecff352223592336)
gate_base_kind: current-tip
gate_base_sha: b52c9105bad9aba447cb8220ecff352223592336
merge_validation: canonical materialized merge 6637c016a3834a5e5808216c83bbdbeb2157621d, tree dd8ca0e378bcede966bea9c497661b0188aed792; all targets build and full nogo/vet pass. MPR verifies and merges the exact cleared PR head; later base movement remains subject to the merge-time guarantee.

The gate ran on the canonical merged tree in `/var/tmp/gc-merge-validate.y1yd1jup`. The reviewed
source was never rebased or reset. Preflight found no associated PR or already-merged
target. Ancestry contains four labelled commits for the same build bead; the scope
guard accepted ga-48itupn and ga-61fduec. No stack or unrelated feature was included.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS present | PASS | Closed review ga-8d8q93a records PASS at the resolved source above; no carryover or reviewer-added commit substitution. |
| 2. Acceptance criteria met | PASS | R1–R13 checked against the merged code and the named tests below. Immediate, queued, backstop, drain, session and runtime paths refuse or hold pending questions as specified. |
| 3. Tests pass | PASS | Canonical full-scope five-phase farm suite, mandatory native floor, all 24 changed tests PASS by name, zero FAIL. Policy/drift/OpenAPI fast lane passed first. Detailed execution evidence below. |
| 4. No high-severity review findings open | PASS | Zero unresolved HIGH, major or blocker findings. Supported-capture and dashboard limitations are disclosed below and in the PR. |
| 5. Final branch is clean | PASS | Reviewed source and canonical scratch clean before adding this record; publication guard requires a gate-only commit with the reviewed source as parent and a clean worktree before push. |
| 6. Merge state and build/vet recorded | PASS | Clean canonical merge above; `bazel build --config=remote-exec --noremote_local_fallback --keep_going //...` built all 1,385 targets; full `make vet NOGO_BUILD_FLAGS='--config=remote-exec --noremote_local_fallback --keep_going --output_groups=nogo_fix'` passed. Normal merge hooks ran. |
| 7. Single feature theme | PASS | One pending-question delivery safety feature across its runtime/session/CLI paths and supporting tests/docs. No independent feature, internal operational doc contamination, new primitive or production worker contract. |

## Acceptance evidence

- R1–R2: one existing pending-interaction error contract, kind-aware terminal guidance,
  unchanged approval wording; no production worker change. CLI, session, runtime and
  worker refusal tests below passed.
- R3–R6: a shared single capture per look detects the live supported question frame,
  rejects idle classification, fails closed on capture errors, preserves approval
  precedence, and ignores answered scrollback. Pending exposes `kind: question`, a
  question request ID and empty options; Respond refuses with its typed error and
  sends no keys.
- R7–R9: pre-checks hold the backstop before reserve and drain before marker write,
  log once per hold transition, and queue with the existing retry delay without
  charging attempts. Live-only send, FollowUp and InterruptNow refuse questions;
  all dismisser negative cases send no keys. The poller test models the provider
  refusal explicitly; the tmux tests independently prove that refusal.
- R10–R13: SESSION-RUNTIME-021 records the behavior; existing pending-lane/watch and
  reconciler contracts are unchanged. API reference documents terminal answers and
  `409 invalid_interaction`. The API kind remains an unconstrained string; API,
  OpenAPI spec, dashboard and hidden-attached delivery code are outside this diff.
  Generated BUILD files and the derived tmux test census are in sync.

## Criterion 3 execution

test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_exec_bep: {"unit":{"results":373,"remote_executed":373,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":85,"remote_executed":85,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}
test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.y1yd1jup --base b52c9105bad9aba447cb8220ecff352223592336 --bead-json /var/tmp/deploy-ga-48itupn.e33__toy/bead-c3-live.json --owned /var/tmp/deploy-ga-48itupn.e33__toy/owned.tsv --run-dir /var/tmp/deploy-ga-48itupn.e33__toy/c3-run/evidence -- bash /var/tmp/deploy-ga-48itupn.e33__toy/local-full.sh
test_log_dir: /var/tmp/deploy-ga-48itupn.e33__toy/c3-run/evidence/remote/logs
test_counts: PASS=131116 FAIL=0 SKIP=373
farm_test_seconds: 10003.484
fallback_reason: none
waiver_ref: none
failure_attribution: none
heavy_mode: none
owned_topup: []

Counts are column-zero result lines summed over retained jobs, not unique test
functions; repeated jobs count repeated results. By-name reader:
`gate-test-evidence.py --logs /var/tmp/deploy-ga-48itupn.e33__toy/c3-run/evidence/remote/logs --owned /var/tmp/deploy-ga-48itupn.e33__toy/owned.tsv`.

All five phases force `--nocache_test_results` and `--noremote_local_fallback`.
`gate-rbe-evidence.py` was independently rerun against each BEP and exact phase
window. Every result has strategy `remote`, attempt 1, neither cached flag, an
in-window start, and a terminal verdict. Cache-credited test results: 0; retries: 0;
local/other strategies: 0. All 994 retained log/XML artifacts match their CAS hashes.

| Phase | Fresh remote results | Farm test seconds | Start–end (epoch ms) | Result |
| --- | ---: | ---: | --- | --- |
| unit | 373 | 2884.343 | 1793203277508–1793203392821 | PASS |
| acceptance | 24 | 1190.415 | 1793203393697–1793203545377 | PASS |
| integration-packages | 85 | 3691.800 | 1793203546938–1793203908436 | PASS |
| integration-full | 14 | 2122.005 | 1793203910347–1793204328684 | PASS |
| integration-latency | 1 | 114.921 | 1793204329395–1793204449748 | PASS |

Coverage: unit `bazel test --config=ci //...` (287 targets), acceptance
`--config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` (7 targets), integration-tagged packages
`--config=integration //test:integration_packages` (36 targets), and full integration
`--config=integration //test/integration:integration_test` (14 shards), followed by
the separate latency leg. The canonical runner's stdout contains every exact
command. `TestTutorial01` literally PASSed in the integration-packages logs under
`GC_FAST_UNIT=0`, covering CI's cmd/gc-process requirement.

latency_leg: measured, one attempt, probe clear after 10 s (load1 46.74,
threshold 50); TestGcBdLatencyBudget PASS, TestGcSeatHookLatencyBudget PASS.
Neither latency test ran in the main integration leg. No budget assertion was retried.

admission: {"class":"remote","floor_gib":25.0,"free_gib":58.924293518066406,"waited_seconds":0.001,"cap":2}
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}

Mayor-enabled remote selection was verified from the actual array-shaped bead
record and source provenance. Remote execution took no host heavy slot/load wait.
The mandatory native floor retained its ordinary local load wait:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=390 wait_timed_out=0 run_start_load=14.66 run_max_load=14.16 run_mean_load=13.42 run_readings=6 wait_first_load=33.27 wait_max_load=33.27 wait_mean_load=21.95 wait_readings=14 read_errors=0
```

load_threshold: 15
load_waited_seconds: 390
load_wait_timed_out: 0

floor_topup: complete; 16 commands, all exit 0. Retained individual
commands and outcomes: `/var/tmp/deploy-ga-48itupn.e33__toy/c3-run/evidence/remote/floor-topup.json`; logs:
`/var/tmp/deploy-ga-48itupn.e33__toy/c3-run/evidence/remote/floor-logs`. This includes beadstest, Darwin compile,
push-gate-lock and local-concurrency self-tests, and the parent test of every
farm environment-absence SKIP, grouped by package. Native SKIPs remain inventoried;
they do not count as changed-test coverage. No farm/local suite was stitched.

skip_inventory: 359 farm SKIP rows / 302 unique
package/test pairs: 71 environment-absence,
288 other. Full names, reasons and log paths remain in
`/var/tmp/deploy-ga-48itupn.e33__toy/c3-run/evidence/remote/skip-inventory.json`. The unchanged skip reasons cover fast-unit/process
separation, optional infrastructure/tools/legacy fixtures, subprocess sentinels,
provider capability/platform cases and existing disabled scenarios. Each skip
site's file is absent from this source diff; the four new test files and two
assertion-only test edits change no skip helper/fixture. No changed test SKIP.
The mandatory floor probes every environment-absence parent; non-owned remaining
SKIPs are inventoried without being promoted to PASS. Audit: `/var/tmp/deploy-ga-48itupn.e33__toy/skip-audit.json`.

diff_tests_executed:

```text
diff_tests_executed:
  TestCLILaneQueuedNote PASS (integration-packages-12697e2ea0d32c278115.log, unit-637c74c578a2b17fc654.log)
  TestDeliverSessionNudgeImmediateRefusesPendingQuestion PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-27b23e708b477fce4dad.log)
  TestDeliverSessionNudgeWaitIdleQueuesBehindPendingQuestion PASS (integration-packages-9326181a736eb30b71ca.log, unit-3d6d3036a2f5902472b5.log)
  TestDrainReminderHoldsWhileQuestionPending PASS (integration-packages-821a75e4a503601af052.log, unit-423c6a0da1986b90e9a7.log)
  TestExecutionBackstopHoldsBeforeReserveWhileQuestionPending PASS (integration-packages-e4245ddf6a37030111bc.log, unit-637c74c578a2b17fc654.log)
  TestPollerHoldsQueuedNudgeWhileQuestionInteractionPending PASS (integration-packages-12697e2ea0d32c278115.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestSessionNudgeLaneQueuedNotesFromThePass PASS (integration-packages-9326181a736eb30b71ca.log, unit-423c6a0da1986b90e9a7.log)
  TestAskUserQuestionRefusalTextIsKindAware PASS (integration-packages-15c091b746137e5c2921.log, unit-4bcc997199c6a7426526.log)
  TestDismissersLeaveAskUserQuestionDialogAlone PASS (integration-packages-5a078b24388ef3c79748.log, unit-ce5f199695f6832daef7.log)
  TestNudgeSessionDeliversPastAnAnsweredAskUserQuestionDialog PASS (integration-packages-15c091b746137e5c2921.log, unit-4bcc997199c6a7426526.log)
  TestNudgeSessionFailsClosedWhenTheQuestionLookCannotReadThePane PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-ef2749222a71266756ae.log)
  TestNudgeSessionSendsNoKeysIntoAskUserQuestionDialog PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-ef2749222a71266756ae.log)
  TestPendingRejectsNonDialogPanes PASS (integration-packages-15c091b746137e5c2921.log, unit-4bcc997199c6a7426526.log)
  TestProviderNudgeRefusesAskUserQuestionDialog PASS (integration-packages-5a078b24388ef3c79748.log, unit-ce5f199695f6832daef7.log)
  TestProviderPendingIgnoresAnsweredAskUserQuestionDialog PASS (integration-packages-15c091b746137e5c2921.log, unit-4bcc997199c6a7426526.log)
  TestProviderPendingPrecedenceBetweenQuestionAndApproval PASS (integration-packages-5a078b24388ef3c79748.log, unit-ce5f199695f6832daef7.log)
  TestProviderPendingReportsAskUserQuestionDialog PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-ef2749222a71266756ae.log)
  TestRespondRefusesAskUserQuestionDialog PASS (integration-packages-5a078b24388ef3c79748.log, unit-ce5f199695f6832daef7.log)
  TestSnapshotPaneIdleRejectsAskUserQuestionDialog PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-ef2749222a71266756ae.log)
  TestPendingInteractionErrorWordingIsKindAware PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestSendLiveOnlyRefusesQuestionInteraction PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestSubmitRefusesQuestionInteraction PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestRuntimeHandleReportsProviderPendingInteractionRefusal PASS (unit-481d1d821b2235b34d5b.log)
  TestSessionHandleLiveOnlyNudgeRefusesQuestionInteraction PASS (unit-481d1d821b2235b34d5b.log)
GATE_TEST_EVIDENCE_RESULT ok: 24 diff-owned tests verified by name
```

## Fast lane and setup

`docs/PROJECT_MANIFEST.md` is absent in this repository. TESTING.md, the Makefile,
Bazel/CI tier definitions and the pack release-criteria protocol supply the checks.

Before suite/runtime setup, separate fresh views passed the pinned changed-format
check, shell policy guards, normal hook ownership, 11 policy/format/docsync Bazel
targets, seven generated-artifact targets, and the base-pinned OpenAPI breaking
check. `make bazel-sync` produced no tracked or untracked drift. Exact commands:
`/var/tmp/deploy-ga-48itupn.e33__toy/fast-lane.sh`; BEP verdicts: `/var/tmp/deploy-ga-48itupn.e33__toy/fast-proof.json`. No CI workflow,
matrix, timeout or required-check list changed (3c not applicable).

The isolation wrapper reported PASS-THROUGH for the scratch worktree; no claim of
namespace isolation is made. Farm tests run hermetically; canonical native-floor
commands scrub live store/session selectors. Host ICU 74 verified without a
library-path override; bd pinned to v1.3.2/9b65b9e19; rootless Podman socket ping
passed with paired Ryuk cleanup settings. Containerhost uses its documented
emulated host. Initial C6 setup lacked scratch-local RBE definitions and began a
local build; only that driver was stopped, logs retained, no result credited.
Sanctioned RBE definitions were installed, then the unchanged canonical tree
completed the actual remote build/vet and this complete fresh suite.

## Review limits

Detection is grounded in the live Claude Code 2.1.280 single-select,
single-question capture. Other layouts, later tabs and status rows below the
footer remain unverified. The existing check-then-type timing gap remains: a
question appearing after a pre-check may spend one attempt before the later
runtime refusal. The dashboard still displays approval controls for a question;
responding returns the typed 409 without sending keys. Terminal answers are the
supported path. No new stuck-dialog timeout is introduced.

Evidence root: `/var/tmp/deploy-ga-48itupn.e33__toy`. Detached driver: `/var/tmp/deploy-ga-48itupn.e33__toy/c3-run`.
Pack evidence revision: `3a5734e69eea6311280d005af4a28d677f7b53ac`.
