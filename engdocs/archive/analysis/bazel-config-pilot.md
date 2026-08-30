# Bazel config pilot: experiment 4b results

## Decision

Keep Go and Make as the authoritative required build and test paths. Use Bazel
as an opt-in local accelerator and as a separate, non-blocking evidence canary.
The identity slice makes changed-file selection and strict target correlation
credible for four small hermetic targets, but it does not establish parity for
the complete `internal/config` package or justify a required-lane migration.

The useful result is the shape of the measurement: after one clean Bazel
initialization, a focused edit is generally sub-second on this host. That
benefit is conditional on a warm output base and on the changed code belonging
to a proven hermetic target.

## What this slice proves

The bounded graph now has four canonical test labels, in deterministic order:

```text
//internal/config:config_diagnostic_locations_test
//internal/config:config_envname_test
//internal/config:config_identity_seam_test
//internal/config:config_storage_endpoint_test
```

`internal/config/BUILD.bazel` contains one shared `config_hermetic` library and
four `go_test` targets. The production sources in that library are the four
small, explicitly owned seams (`diagnostic_locations.go`, `envname.go`,
`identity_seam.go`, and `storage_endpoint.go`). The graph is hand-bounded; no
repository-wide Gazelle conversion is part of this experiment.

The identity target exercises the pure helpers in `identity_seam.go` through
`identity_seam_bazel_test.go`. The existing `config.go` Agent method path still
belongs to the full Go package. In other words, the identity target is a
hermetic seam proof, not a claim that Bazel now covers every identity test or
the complete production package. A broad or unproven config change therefore
falls back to all four targets.

The full `internal/config` package remains Go-authoritative. Testdata/embed,
build-tag variants, filesystem- and process-sensitive tests, tmux, Dolt, CGO,
and integration tests are intentionally outside this graph until each has a
real-boundary proof.

## Changed-file selection contract

`scripts/bazel_target_resolver.py` is deliberately fail-closed. It maps only
these source/test pairs:

| Files | Target |
| --- | --- |
| `envname.go`, `config_envname_bazel_test.go` | `config_envname_test` |
| `diagnostic_locations.go`, `diagnostic_locations_test.go` | `config_diagnostic_locations_test` |
| `identity_seam.go`, `identity_seam_bazel_test.go` | `config_identity_seam_test` |
| `storage_endpoint.go`, `storage_endpoint_bazel_test.go` | `config_storage_endpoint_test` |

The resolver unions mapped labels and sorts them canonically. Its decisions are:

| Input | Selection | Conservative? |
| --- | --- | ---: |
| One or more mapped files | The owning label(s) | No |
| Mixed mapped files | Sorted union of owning labels | No |
| Other `internal/config` file | All four (`config-unmapped`) | Yes |
| Config rename, delete, or copy | All four (`config-unmapped`) | Yes |
| `BUILD*`, `.bzl`, `MODULE.bazel*`, `.bazelrc`, `.bazelversion`, `go.mod`, `go.sum`, or canary/resolver files | All four (`shared-build-graph`) | Yes |
| Empty, malformed, unreadable, or unavailable input | All four (`unavailable`) | Yes |
| Confidently unrelated path | No targets (`unrelated`) | No |

The canary writes a normalized selection during its resolve phase and consumes
that file during execution; it does not recompute the selection after toolchain
setup. This prevents a changed checkout or a transient resolver failure from
silently changing what was requested. Unknown and rename/delete/copy cases are
intentionally over-inclusive because under-testing is the more dangerous error.

## BEP and canary contract

The canary and backtest use the same four-label allowlist. The BEP parser now
correlates requested target patterns strictly: it requires the pattern set to
equal the requested labels, exactly one configured event and one completed
event per requested label, and equal configured/completed sets. Duplicate,
missing, mismatched, or malformed events are errors. The four-target replay
reported numeric `4/4` correlation for broad runs and `1/1` for the mapped
source-edit runs; no row had a BEP error.

Action, cache, analysis, and CPU figures are graph-wide metrics from Bazel's
build metrics, not target-specific counts. The resolver's target counts are the
authoritative configured/completed correlation; graph-wide action metrics are
reported separately so they are not mistaken for selected-target work.

`.github/workflows/bazel-canary.yml` remains separate from required `ci.yml` and
uses `continue-on-error: true`. It runs the equivalent Go config parity first,
uses runner-temporary repository/output caches with remote caching disabled,
bounds commands and the job (`25` minutes), shuts down only its known Bazel
output base, and uploads BEP/profile/log/summary artifacts with `if: always()`.
The Sol review council approved the resolver-driven identity slice at
`2152144708` (building on `0967b13d43` and `625b1fe227`). That approval is for
bounded evidence collection, not for making the canary required.

## Toolchain and measurement method

Measurements used Bazel 9.2.0, rules_go 0.63.0, Gazelle 0.53.0, and the pinned
Go SDK 1.26.6 on a Linux host with 192 logical CPUs. The runner is
`scripts/bazel-config-backtest.sh`. A `cold` sample gets a new Bazel output
base and action graph; `forced` reuses a warm graph but executes tests;
`no-op` enables test-result caching; `source-edit` changes one production file
and runs the warm graph. The harness also supports `test-edit`,
`unrelated-edit`, and `go-mod` invalidation probes.

Cold means a clean output base/action graph, not a fully machine-cold startup:
the Bazel binary, JDK, and external module downloads are shared. Every row
records wall time, client user time/RSS, BEP action/cache metrics, analysis
time, and Bazel CPU time. Historical revisions predate the BUILD graph, so the
harness copied the graph from the pilot checkout into disposable worktrees.
Those rows measure source behavior under a fixed graph; they do not claim that
the historical commits were natively Bazel-buildable.

## Three-target matrix (reference baseline)

Before the identity slice, the three-target matrix used artifact
`/tmp/gascity-bazel-matrix-20260830211754/stdout.tsv` at
`5a4fab675054`. It is retained as a separately labeled, non-comparable
reference rather than mixed with the four-target numbers below. This run used
20 alternating samples and one uncached Go invocation (`go test -count=1
./internal/config`).

| Scenario | Go/Bazel reference | Bazel p50 | Bazel p95 |
| --- | ---: | ---: | ---: |
| Go package baseline | 16.822 s | — | — |
| cold | — | 50.998 s | 53.524 s |
| forced | — | 0.550 s | 0.766 s |
| no-op | — | 0.474 s | 0.781 s |
| source-edit | — | 0.479 s | 0.793 s |
| test-edit | — | 0.387 s | 0.418 s |
| `go.mod` invalidation probe | — | 0.397 s | 0.492 s |

The unrelated-edit probe was skipped `20/20` times (`reason=unrelated`), with
zero Bazel invocations. The three-target run's median action shape was 77
created/41 executed for cold, 15/4 for forced, 30/1 for no-op, and 5/2 for a
source edit. Those counts are graph-wide and are included only to make the
cache behavior reproducible.

Using the three-target p50s as a directional amortization model, one cold
initialization plus repeated source edits costs approximately
`50.998 + (n-1) * 0.479` seconds, versus `n * 16.822` seconds for repeated Go
invocations. The curves cross at roughly four focused iterations. This is a
model of a warm, hermetic loop—not a promise about an entire agent session.

## Four-target historical replay

The completed replay artifact is
`/tmp/gascity-bazel-four-target-replay-20260830231428/stdout.tsv`. It ran four
recent revisions, three samples per scenario, with `cold`, `forced`, `no-op`,
and `source-edit`. All 48 Bazel rows exited successfully, had an empty BEP
error, and passed strict correlation (36 broad rows were `4/4`; 12 mapped
source-edit rows were `1/1`). The p95 values below are descriptive estimates
from three samples, not tail SLOs.

| PR / revision | Go baseline | cold p50 / p95 | forced p50 / p95 | no-op p50 / p95 | source-edit p50 / p95 |
| --- | ---: | ---: | ---: | ---: | ---: |
| #5246/#5258 · `7c33f3f7f1` | 17.478 s | 50.980 / 52.055 s | 0.749 / 1.077 s | 0.735 / 0.747 s | 0.704 / 0.718 s |
| #5164/#5252 · `128bd64033` | 16.991 s | 50.553 / 50.834 s | 0.721 / 1.068 s | 0.524 / 0.699 s | 0.739 / 0.775 s |
| #5193 · `a784438ce0` | 16.924 s | 52.620 / 57.895 s | 0.791 / 1.235 s | 0.739 / 1.036 s | 0.746 / 1.372 s |
| #5215 · `58a47d6bdc` | 20.994 s | 60.979 / 62.468 s | 0.737 / 1.170 s | 0.754 / 0.763 s | 0.745 / 0.890 s |

The cold spread is host and initialization variation, not a monotonic PR trend.
Warm medians stay below one second across the four revisions, while warm p95s
are roughly 0.7–1.4 seconds. A separate isolated four-target spot check
(artifact `/tmp/bazel-identity-resolver.ozzcV7/`) measured `58.77 s` cold,
`6.29 s` forced-warm, and `0.80 s` cached no-op; all four targets passed strict
`4/4` BEP correlation. Its different cache/setup shape is not combined with
the replay table.

Taking the median of the four replay Go baselines (`17.235 s`), cold p50s
(`51.800 s`), and source-edit p50s (`0.742 s`) gives another directional model:

| Focused iterations | Repeated Go | Four-target Bazel |
| ---: | ---: | ---: |
| 1 | 17.2 s | 51.8 s |
| 3 | 51.7 s | 53.3 s |
| 4 | 68.9 s | 54.0 s |
| 10 | 172.3 s | 58.5 s |

This is why the experiment is promising for agents that make several focused
edits in one session, while still being a poor default for a one-off cold run.

## Gaps and promotion gates

Before proposing any required-lane migration, the next slice should:

1. Prove one `testdata`/embed or build-tag vertical end to end, including its
   exact Go/Bazel dependency closure and resolver ownership. The identity target
   must not be treated as full `config.go` parity.
2. Repeat a 20-sample matrix with the four-target graph, including Go p50/p95,
   clean-output-base and persistent-cache cells, timeout/failure samples, and
   `test-edit`, `go.mod`, and unrelated-edit selection evidence.
3. Exercise the canary on an actual GitHub runner and retain evidence for cache
   isolation, cleanup, timeout behavior, and the persisted-selection path.
4. Keep process, tmux, Dolt, CGO, and integration-sensitive tests in their
   existing Go/integration lanes until equivalent real-boundary proofs exist.
5. Define a Go fallback when Bazel is unavailable or its cache is cold, and
   compare realistic warm p95 and total developer wall time against the
   documented Go shards.

Do not begin a repository-wide Gazelle conversion. Do not replace required
Go/Make CI on the strength of this four-target graph; the graph is valuable as
an additive accelerator and as a way to collect the evidence needed for a
larger, carefully owned slice.

## Recommendation for the next experiment

Proceed with the filed follow-up `ga-22kbk.2`: inventory config testdata/embed
and build-tag consumers, choose one vertical with a demonstrably hermetic
closure, and add only that target. Run the resolver and strict BEP tests before
expanding the allowlist, then repeat the matrix and ask the Sol council for a
fresh slice review. If the next vertical keeps the warm p95 in the same band,
use Bazel locally for focused edits while leaving Go/Make as the required
authority.
