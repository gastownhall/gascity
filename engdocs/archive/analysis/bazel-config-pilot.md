# Bazel config pilot: experiment 3 results

## Decision

Keep Go and Make the authoritative required build/test paths. Land the Bazel
workflow only as a separate, non-blocking canary and continue the graph in
small, explicitly reviewed slices. The pilot demonstrates a useful warm
incremental accelerator, but it does not yet demonstrate package parity,
reliable changed-target selection, or a safe replacement for the required
lanes.

## Scope and graph boundary

This experiment overlays a small Bzlmod/rules_go graph on the existing Go
repository. It adds explicit BUILD files rather than running repository-wide
Gazelle generation. The current config slice contains two hermetic probes:

```text
//internal/config:config_envname_test
//internal/config:config_diagnostic_locations_test
```

Both use `internal/config:config_hermetic`, whose production sources are
`envname.go` and `diagnostic_locations.go`. The surrounding BUILD files retain
the bounded dependency closure used by the earlier contract pilot. The full
`internal/config` package has about 48 production files and 85 test files;
`go list -test -deps ./internal/config` reaches about 32 internal packages.
Repository-wide Gazelle generation would create roughly 195 BUILD files and was
therefore rejected for this experiment.

The complete config package remains Go-authoritative. The Bazel slice does not
claim parity for identity tests, testdata or embed behavior, build-tag variants,
filesystem/process-sensitive tests, tmux, Dolt, CGO, or integration tests.
Those boundaries stay in the existing Go and integration lanes until a
real-boundary proof exists.

## Toolchain and method

The measurements used Bazel 9.2.0, rules_go 0.63.0, Gazelle 0.53.0, and the
pinned Go SDK 1.26.6 on a Linux host (`go1.26.6`, 192 logical CPUs). The
runner is `scripts/bazel-config-backtest.sh`.

Each current-revision run used 20 alternating samples of:

| Scenario | Meaning |
| --- | --- |
| `cold` | A new Bazel output base and clean action graph. |
| `forced` | A warm output base with test execution forced. |
| `no-op` | A warm output base with test-result caching enabled. |
| `source-edit` | One production-source edit followed by a warm test. |

The harness also supports `test-edit`, `unrelated-edit`, and `go-mod` probes.
`go-mod` is an invalidation probe, not a normal speed sample. Every run records
wall time, client user time/RSS, BEP action/cache counts, analysis time, and
Bazel-reported CPU time. The selected-target count is intentionally reported as
`unknown`: the current BEP parser sees transitive actions but does not yet
correlate them reliably to the requested target. The `unrelated-edit` and
`go-mod` probes are not included in the timing tables below; they are reserved
for invalidation/selection evidence.

“Cold” here means a clean output-base/action-graph sample. The Bazel binary,
JDK, and external module/download state are shared, so these are not fully
machine-cold startup measurements.

## Current revision: 20 samples

The current branch's Go baseline was one diagnostic invocation:
`go test -count=1 ./internal/config` in 18.036 s. It is not a Go p95
distribution. Bazel results below are the 20-sample distributions:

| Scenario | Bazel p50 | Bazel p95 | Actions (p50) | Executed (p50) | Cache hits/misses (p50) | Analysis p50 | BEP CPU p50 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| cold | 52.158 s | 53.921 s | 57 | 21 | 0 / 21 | 2.155 s | 166.1 s |
| forced | 0.549 s | 0.834 s | 5 | 2 | 4.5 / 2 | 0.160 s | 2.1 s |
| no-op | 0.485 s | 0.640 s | 10 | 1 | 10 / 1 | 0.180 s | 2.1 s |
| source-edit | 0.475 s | 0.744 s | 5 | 2 | 4.5 / 2 | 0.086 s | 2.3 s |

Relative to the 18.036 s Go invocation, the warm p50s are about 33x faster
(`forced`), 37x faster (`no-op`), and 38x faster (`source-edit`). The clean
output-base p50 is about 2.9x slower than that Go invocation. Client-only RSS
was approximately 10.9 MiB; it excludes the Bazel server and action processes
and should not be treated as total resource use.

An illustrative focused session makes the amortization visible. Using the
current p50s and one cold initialization, the estimated cumulative wall time
for *n* source-edit iterations is:

| Focused iterations | Repeated Go (`n × 18.036 s`) | Bazel (`52.158 s + (n−1) × 0.475 s`) |
| ---: | ---: | ---: |
| 1 | 18.0 s | 52.2 s |
| 3 | 54.1 s | 53.1 s |
| 10 | 180.4 s | 56.4 s |

This is a directional model, not a promise about an agent's whole workflow:
it assumes the Bazel server/output base stays warm and that the changed code
fits this tiny hermetic target.

## Historical PR backtest

The harness replayed three samples for four recent PR revisions. The BUILD
graph was copied from the pilot checkout because those revisions predate the
graph; therefore these rows are directional source-change measurements, not
claims that the PRs were natively Bazel-buildable. Each replay explicitly set
the config target, package, production source, test source, and unrelated-file
overrides; it did not use the harness's default contract target. The p95 values
below are descriptive three-sample estimates, not an authoritative tail SLO.

| PR / revision | Go baseline | cold p50 / p95 | forced p50 / p95 | no-op p50 / p95 | source-edit p50 / p95 |
| --- | ---: | ---: | ---: | ---: | ---: |
| #5246/#5258 · `7c33f3f7f1` | 18.799 s | 51.940 / 51.965 s | 0.563 / 1.054 s | 0.566 / 0.734 s | 0.711 / 0.773 s |
| #5252 · `128bd64033` | 18.889 s | 51.694 / 52.379 s | 0.549 / 1.008 s | 0.489 / 0.548 s | 0.708 / 0.733 s |
| #5193 · `a784438ce0` | 18.150 s | 54.756 / 56.084 s | 0.733 / 1.118 s | 0.491 / 0.749 s | 0.746 / 0.760 s |
| #5215 · `58a47d6bdc` | 18.964 s | 52.554 / 54.608 s | 0.704 / 1.085 s | 0.513 / 0.576 s | 0.735 / 0.761 s |

All 48 historical Bazel rows passed. The warm p50s remain in the same
sub-second band across all four revisions, while cold p50s remain around
52–55 s. There is no monotonic timing trend to attribute to the PRs: the
graph, toolchain, and runner were held constant, and the observed differences
are within the expected host/cache variation. The useful result is the stable
warm-vs-cold shape, not a claim that one PR made Bazel faster.

## CI canary

`.github/workflows/bazel-canary.yml` is deliberately separate from required
`ci.yml` and remains `continue-on-error: true`. It runs only the two config
targets, performs equivalent Go config parity first, uses isolated runner-temp
output/base paths, pins the checkout/setup-go/upload-artifact actions and
Bazelisk v1.26.0, disables remote caching with Bazel 9.2's supported empty
`--remote_cache=` value, bounds each command at five minutes, and gives the job
25 minutes for setup, execution, shutdown, and artifact upload. An EXIT trap
shuts down the known Bazel output base; BEP, profiles, logs, and a summary are
uploaded with `if: always()`.

The Sol review council approved the final timeout-adjusted canary at commit
`a37aaf197f`. Approval is for bounded evidence collection only; it is not
approval to make the workflow required or to remove the Go/Make lanes.

## Gaps and promotion gates

Before considering a required-lane migration, the next slices should establish
all of the following:

1. Complete config parity in vertical slices: identity, testdata/embed,
   build-tag variants, and the documented filesystem/process boundaries.
2. A changed-file-to-target resolver and BEP target correlation, with an
   unrelated edit proving zero selected test actions (or an explicit,
   explainable conservative fallback).
3. At least 20 comparable Go and Bazel samples per representative package,
   including Go p50/p95 rather than one baseline point, persistent-cache and
   clean-output-base cells, and failure/timeout samples.
4. Cache isolation and cleanup evidence on shared CI runners, plus coverage for
   process, tmux, Dolt, CGO, and integration-sensitive tests in their owning
   lanes.
5. A promotion review showing realistic warm p95 and total developer wall time
   beat the current Go shards, with a documented fallback to Go when Bazel
   setup or cache state is unavailable.

## Recommendation for the next experiment

Treat this as an opt-in local accelerator and evidence-producing canary. The
next experiment should expand one adjacent config vertical (preferably
testdata/embed or identity) with explicit BUILD ownership and parity tests,
then repeat the same 20-sample matrix plus the unrelated-edit and invalidation
probes. Do not start a repository-wide Gazelle conversion or replace required
Go/Make CI yet. If two or three focused iterations are the common agent
session, the measured warm path is already promising; the engineering question
now is whether that benefit survives realistic package breadth, cache misses,
and the non-hermetic test boundaries—not whether the two-file probe itself is
fast.
