# Bazel config pilot: Experiment 4c results

## Decision

Keep Go and Make as the authoritative required build and test paths. Use Bazel
as an opt-in local accelerator and as a separate, non-blocking evidence canary.
Experiment 4c extends the bounded config graph to five explicitly owned,
hermetic test targets, including the session-setup path seam.

The measured shape is useful for an agent that makes several focused edits in
one session: a clean Bazel output base costs about 51 seconds on this host,
while a mapped source or test edit is about 1.06 seconds at the pooled p50.
The comparable Go test -count=1 package baseline is 11.963 seconds at the
pooled warm p50. The simple amortization model crosses at about 4.6 iterations, so
the first integer win is the fifth focused iteration. This is evidence for a
local workflow choice, not evidence for replacing the required Go/Make lanes.

This report supersedes the earlier three- and four-target tables as the
current result. Those earlier runs are not mixed into the numbers below
because their graphs and cache/setup protocols differ.

## Scope and ownership

The experiment branch is experiment/bazel-config-golden at
84e956f208ca5c7601e7d664182cbc5905d909a6 (84e956f208). The recorded
experiment merge-base/worktree base is ac6c9c6853fcfc3b7cde4be1847f2431d3f93865
(ac6c9c6853); `origin/main` advanced after the worktree was created. Historical source revisions
predate this graph; the harness copied the graph into disposable worktrees,
so those rows measure source behavior under a fixed graph rather than native
Bazel support.

The bounded graph has five canonical labels, in deterministic order:

~~~text
//internal/config:config_diagnostic_locations_test
//internal/config:config_envname_test
//internal/config:config_identity_seam_test
//internal/config:config_session_setup_path_test
//internal/config:config_storage_endpoint_test
~~~

internal/config/BUILD.bazel provides one shared config_hermetic library over
the five production seam files and five go_test targets. Ownership is kept
explicit; this is not a repository-wide Gazelle conversion.

| Target | Production source | Test/fixture ownership |
| --- | --- | --- |
| config_diagnostic_locations_test | diagnostic_locations.go | diagnostic_locations_test.go, diagnostic_locations_fixture_bazel_test.go, testdata/diagnostic_locator.toml |
| config_envname_test | envname.go | config_envname_bazel_test.go |
| config_identity_seam_test | identity_seam.go | identity_seam_bazel_test.go |
| config_session_setup_path_test | session_setup_path.go | session_setup_path_test.go |
| config_storage_endpoint_test | storage_endpoint.go | storage_endpoint_bazel_test.go |

The session-setup target exercises source-directory, city-root (double-slash),
legacy fallback, absolute, and empty-path behavior. The diagnostic fixture
remains a test-only embed proof. The full internal/config package, production
PackFS/bootstrap embeds, other testdata and golden files, build-tag variants,
and filesystem-, process-, tmux-, Dolt-, CGO-, and integration-sensitive tests
remain Go-authoritative until each has its own real-boundary proof.

## Changed-file selection and BEP contract

scripts/bazel_target_resolver.py is deliberately fail-closed. Its exact mapped
ownership is:

| Changed path | Selected target |
| --- | --- |
| internal/config/diagnostic_locations.go, internal/config/diagnostic_locations_test.go, internal/config/diagnostic_locations_fixture_bazel_test.go, internal/config/testdata/diagnostic_locator.toml | config_diagnostic_locations_test |
| internal/config/envname.go, internal/config/config_envname_bazel_test.go | config_envname_test |
| internal/config/identity_seam.go, internal/config/identity_seam_bazel_test.go | config_identity_seam_test |
| internal/config/session_setup_path.go, internal/config/session_setup_path_test.go | config_session_setup_path_test |
| internal/config/storage_endpoint.go, internal/config/storage_endpoint_bazel_test.go | config_storage_endpoint_test |

The resolver unions mapped labels and sorts them canonically. Any other
internal/config path, config rename/delete/copy, build-graph file (BUILD*,
.bzl, MODULE.bazel*, .bazelrc, .bazelversion), go.mod, go.sum,
canary/resolver input, empty or malformed diff, or unavailable resolver input
selects all five labels conservatively. A confidently unrelated path selects
no labels and is skipped. The canary persists the normalized selection before
toolchain setup and consumes that exact file during execution; it does not
recompute selection after setup.

The BEP parser requires the requested pattern set to equal the requested
labels, exactly one targetConfigured and one completed event per requested
label, and equal configured and completed sets. Duplicate, missing, mismatched,
or malformed events are errors. Target counts are the authoritative
selection/correlation evidence. Bazel action, cache, analysis, and CPU
figures are graph-wide metrics and are reported separately; they are not
selected-target counts.

## Revisions, tooling, and artifacts

The backtest used these recent PR revisions:

| PR reference | Revision |
| --- | --- |
| #5246/#5258 | 7c33f3f7f1 |
| #5164/#5252 | 128bd64033 |
| #5193 | a784438ce0 |
| #5215 | 58a47d6bdc |

The host ran Bazel 9.2.0, rules_go 0.63.0, Gazelle 0.53.0, and the pinned Go
SDK 1.26.6 on Linux with 192 logical CPUs. Primary artifacts are retained at:

- cold rows: `/tmp/gascity-bazel-experiment2-20260831/cold/<ref>/artifacts/results.tsv`
- cold stdout streams: `/tmp/gascity-bazel-experiment2-20260831/cold/<ref>/stdout.tsv`
- warm rows: `/tmp/gascity-bazel-experiment2-20260831/warm/<ref>/artifacts/results.tsv`
- warm stdout streams: `/tmp/gascity-bazel-experiment2-20260831/warm/<ref>/stdout.tsv`

Each artifact directory also contains per-run logs, BEP payloads, and prime
logs needed to audit a row. The total retained artifact footprint is about
44 MB at report time (596 KB for cold rows and 43 MB for warm rows).

## Measurement design

The cold campaign used BACKTEST_SAMPLES=1: one Go `go test -count=1
./internal/config` row (test-result cache bypassed, build cache primed) and one
Bazel cold row per revision.
Each cold row received a new Bazel output base and action graph. The four cold
rows are therefore descriptive setup observations (n=1 per revision), not a
basis for a cold p95, tail estimate, or SLO.

The warm campaign used 20 samples per revision for Go and for each of forced,
no-op, source-edit, test-edit, unrelated-edit, and go-mod. The warm invocation
created a separate output base and performed a warm Bazel prime before its
recorded scenarios; that prime is not included as a result row. Forced
disables test-result caching, no-op permits it, mapped edits change one owned
source/test file, go.mod is a conservative graph invalidation, and an
unrelated edit is resolved without invoking Bazel. Scenario order rotates to
reduce positional bias, while each scenario restores the pristine worktree
before it runs.

The cold campaign contains 8 rows total (4 Go and 4 Bazel). The warm campaign
contains 560 rows (80 Go plus 480 scenario rows), including 80 recorded
unrelated skips. All successful Bazel rows have an empty BEP error and exact
requested/configured/completed label equality.

Do not add a cold row to a warm row as though they were one continuous cache
sequence: the separate warm invocation has its own unrecorded prime. The
iteration model below makes the setup cost explicit instead.

### Methodology finding: abandoned parallel matrix

An early run at /tmp/gascity-bazel-matrix-kBKMTc launched 20 samples for all
four revisions concurrently. By samples 3–4, every cold sample was creating a
fresh output base and recompiling the Go toolchain; the tree peaked at about
7.4 GB and orphaned Bazel servers remained. We terminated only processes
belonging to the exact experiment paths and removed that disposable tree after
preserving its artifact/size note. Its incomplete rows are not evidence.
The final campaign serialized revision work and kept output bases and worktrees
isolated, which makes the reported matrix complete and auditable.

## Cold results (one sample per revision)

All four rows passed with empty BEP errors and exact five-target 5/5
correlation. The action figures are created/executed/cache-hits/cache-misses
and are graph-wide.

| PR / revision | Go wall (s) | Bazel cold wall (s) | Actions created / executed / hits / misses | BEP |
| --- | ---: | ---: | ---: | ---: |
| #5246/#5258 · 7c33f3f7f1 | 11.210 | 50.482 | 97 / 61 / 0 / 61 | 5/5 |
| #5164/#5252 · 128bd64033 | 11.628 | 51.056 | 97 / 61 / 0 / 61 | 5/5 |
| #5193 · a784438ce0 | 11.004 | 51.669 | 97 / 61 / 0 / 61 | 5/5 |
| #5215 · 58a47d6bdc | 11.837 | 51.344 | 97 / 61 / 0 / 61 | 5/5 |

Across the four descriptive cold rows, the Bazel p50 is 51.200 seconds
(range 50.482–51.669) and the Go p50 is 11.419 seconds. There is no
meaningful cold p95 with one observation per revision.

## Warm results (20 samples per revision)

The cells below are wall-time p50/p95 in seconds. Each cell has 20/20
successful rows; p95 is a descriptive quantile of this controlled sample, not
a production SLO.

| PR / revision | Go | forced | no-op | source edit | test edit | go.mod |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| #5246/#5258 · 7c33f3f7f1 | 11.843 / 12.406 | 0.774 / 1.078 | 0.782 / 0.946 | 1.046 / 1.149 | 1.054 / 1.136 | 0.840 / 1.045 |
| #5164/#5252 · 128bd64033 | 11.719 / 12.376 | 0.816 / 0.943 | 0.843 / 0.962 | 1.074 / 1.138 | 1.062 / 1.211 | 0.818 / 1.083 |
| #5193 · a784438ce0 | 12.038 / 13.379 | 0.835 / 1.123 | 0.786 / 0.975 | 1.096 / 1.197 | 1.051 / 1.138 | 0.905 / 1.103 |
| #5215 · 58a47d6bdc | 12.169 / 13.417 | 0.787 / 0.932 | 0.811 / 1.041 | 1.054 / 1.231 | 1.063 / 1.131 | 0.879 / 1.160 |

Pooled across the four revisions (n=80 per scenario):

| Scenario | p50 (s) | p95 (s) | Rows / validity |
| --- | ---: | ---: | --- |
| Go package | 11.963 | 13.350 | 80/80 success |
| forced | 0.811 | 1.085 | 80/80 success; 5/5 BEP |
| no-op | 0.800 | 1.038 | 80/80 success; 5/5 BEP |
| source edit | 1.062 | 1.210 | 80/80 success; 1/1 BEP |
| test edit | 1.058 | 1.166 | 80/80 success; 1/1 BEP |
| go.mod edit | 0.863 | 1.103 | 80/80 success; 5/5 BEP |

The unrelated-edit scenario produced 80/80 not-run rows with
selection_reason=unrelated and zero Bazel invocations. Every executed
scenario had an empty BEP error and exact requested/configured/completed label
sets. Thus the result is both a timing matrix and a changed-file selection
check, rather than a benchmark that happens to run fewer tests.

### Graph-wide action and cache shape

The following are medians across applicable rows; ranges are included where
invalidation or scheduler state changes the count:

| Scenario | Actions created | Executed | Cache hits | Cache misses |
| --- | ---: | ---: | ---: | ---: |
| cold (n=4) | 97 | 61 | 0 | 61 |
| forced (n=80) | 51 | 6 (6–9) | 45 (42–45) | 6 (6–9) |
| no-op (n=80) | 51 | 2.5 (1–4) | 48.5 (47–50) | 2.5 (1–4) |
| source edit (n=80) | 11 | 5 | 6 | 5 |
| test edit (n=80) | 11 | 5 | 6 | 5 |
| go.mod edit (n=80) | 51 | 7.5 (6–9) | 43.5 (42–45) | 7.5 (6–9) |

Fractional values are medians of integer action counts. These figures include
transitive toolchain and graph actions; they must not be read as “five
selected targets did five actions.”

## Iterative cost model

For n focused iterations, use:

~~~text
Go = n * G
Bazel = C + (n - 1) * W
~~~

with pooled warm Go G = 11.963 s, cold setup C = 51.200 s, and mapped
source-edit W = 1.0625 s (displayed as 1.062 s above). The continuous
crossover is:

~~~text
(C - W) / (G - W) = 4.599... iterations
~~~

Rounded inputs put the break-even in the 4.60–4.70 range; operationally, the
first whole iteration at which the Bazel setup curve is lower is iteration 5.

| Focused iterations | Repeated Go | Bazel with setup |
| ---: | ---: | ---: |
| 1 | 11.963 s | 51.200 s |
| 3 | 35.889 s | 53.325 s |
| 4 | 47.852 s | 54.388 s |
| 5 | 59.815 s | 55.450 s |
| 10 | 119.630 s | 60.763 s |
| 20 | 239.260 s | 71.388 s |

This maps to likely developer wall-clock savings only for the build/test part
of an iterative session. Editing, model reasoning, downloads, test discovery,
unmapped package tests, and waiting for a CI runner are outside this model.

## Canary and cache evidence

The canary remains a separate, continue-on-error workflow. It runs the Go
parity package first, disables remote caching (--remote_cache=), uses
runner-temporary repository/output caches, bounds the job and commands, shuts
down only its known output base, and uploads logs/BEP/profile/summary files
with if: always().

Local canary evidence is retained at:

- /tmp/gascity-bazel-canary-real-20260831-031651/out/summary.txt: Bazel
  9.2.0, conservative five-target manual selection, cold pass (58.43 s) and
  warm pass (9.35 s), exact 5/5 correlation.
- /tmp/gascity-bazel-canary-mapped-20260831-032051/out-warm-mapped/summary.txt:
  a changed session_setup_path.go selects only
  config_session_setup_path_test, warm pass (8.07 s), exact 1/1 correlation,
  19 cache hits and 2 misses.
- /tmp/gascity-bazel-canary-20260831-030837/summary.txt: manual/no-diff
  fallback records bazel-unavailable while retaining a conservative
  five-target selection.

These are smoke/canary observations, not additional samples in the backtest.
Remote caching was intentionally disabled, so the experiment does not measure
the potential benefit or operational risk of a shared remote cache. A
machine-local output base, a CI runner cache, and a remote cache are separate
deployment choices.

## Gaps and promotion gates

Before considering any required-lane migration:

1. Keep the full go test ./internal/config package and all process, tmux, Dolt,
   CGO, build-tag, and integration-sensitive tests in their existing Go/Make
   lanes.
2. Add one vertical at a time only after proving its real dependency closure,
   testdata/build-tag boundary, resolver ownership, and strict BEP correlation.
3. Repeat a 20-sample matrix for each added vertical with clean-output-base,
   persistent-cache, timeout/failure, mapped-edit, graph-edit, and unrelated
   cases. Record setup separately from warm iterations.
4. Exercise the canary on an actual GitHub runner, including cache isolation,
   cleanup, timeout, unavailable-Bazel fallback, and persisted-selection paths.
5. Define and document the local Go fallback for unavailable Bazel or a cold
   cache; compare realistic warm p95 and total developer wall time against the
   documented Go shards.

Do not begin a repository-wide Gazelle conversion, and do not make Bazel a
required CI lane on this five-target graph. The evidence supports a narrow
workflow rule: when a changed file is one of the proven mappings and the local
output base is warm, run the owning Bazel target(s); otherwise use the
authoritative Go/Make path.

## Recommendation

Adopt the bounded five-target graph as an opt-in local accelerator for proven
focused edits, with the resolver's conservative all-five fallback for unknown
or shared-graph changes. Keep Go/Make required and authoritative, and keep the
canary non-blocking while collecting runner evidence. The next experiment
should add at most one more hermetic vertical, repeat this same measurement
contract, and receive a fresh Sol council review before its mapping is used in
developer tooling or CI.
