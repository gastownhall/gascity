# Messgo production hotspot audit

Audited 2026-08-20 against commit
`a260035aa1e93b3aeb6e7eaa190ee4b8e7901e87` (`HEAD`). This is a read-only
assessment: it changes no production source.

## Scope and method

The audited production population is the 603 hand-written Go files below
`internal/` and `cmd/gc/`, excluding `*_test.go`. The scan deliberately
excludes all test files and the `test/`, `examples/`, `contrib/`, `scripts/`,
and generator-command trees. Those paths are test, example,
contributor/support, or code-generation inputs rather than the compiled `gc`
command and its internal runtime.

Generated source is excluded before Messgo runs, rather than merely omitted
from presentation. The reproducible boundary is every in-scope non-test Go
file whose contents match Go's canonical generated-file header,
`^// Code generated .* DO NOT EDIT\.$`. At this revision that finds exactly
one file, `internal/api/genclient/client_gen.go`. The command in
[Reproduction](#reproduction) passes that discovered path to Messgo's
`--exclude` option; the same header test removes it from the current-file list
used for Git rankings. Consequently it is absent from the population, every
ruleset total, and every table and prioritisation below. The pre-exclusion
population was 604 files.

Messgo was version 0.2.0. The requested `design` and `codesize` rulesets are
built-ins and were run directly. `unused` is **not** a Messgo 0.2.0 ruleset:
the tool reports `unknown ruleset or file "unused"`. Its built-in equivalent is
named `unusedcode`, which was run and is labelled that way throughout this
report. Thus the unavailable requested spelling is recorded rather than
silently substituted.

| Ruleset requested | Commanded Messgo ruleset | Violations | Leading rule |
| --- | --- | ---: | --- |
| `design` | `design` | 123 | `CountInLoopExpression` (43) |
| `codesize` | `codesize` | 1,848 | `CyclomaticComplexity` (920) |
| `unused` | unavailable; closest built-in `unusedcode` | 539 | `UnusedFormalParameter` (394) |
| **Total** | — | **2,510** | — |

The rule-family totals are counts reported by Messgo, not a measure of defects
or a prioritisation recommendation. They describe hand-written production code
only under the inclusion boundary above.

## Violation concentration

The table ranks hand-written production files by the sum of all three executed
rulesets. Columns are independently countable Messgo violations; ties are
ordered by path.

| Rank | File | Design | Code size | Unused code | Total |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1 | `internal/extmsg/types.go` | 0 | 0 | 81 | 81 |
| 2 | `internal/runtime/runtime.go` | 0 | 1 | 49 | 50 |
| 3 | `cmd/gc/session_lifecycle_parallel.go` | 2 | 38 | 1 | 41 |
| 4 | `internal/runtime/tmux/tmux.go` | 3 | 28 | 9 | 40 |
| 5 | `internal/runtime/tmux/adapter.go` | 0 | 12 | 26 | 38 |
| 6 | `internal/beads/beads.go` | 0 | 0 | 33 | 33 |
| 7 | `internal/convergence/handler.go` | 0 | 10 | 22 | 32 |
| 8 | `internal/api/state.go` | 0 | 0 | 31 | 31 |
| 9 | `cmd/gc/city_runtime.go` | 2 | 22 | 5 | 29 |
| 10 | `internal/dispatch/ralph.go` | 1 | 28 | 0 | 29 |
| 11 | `internal/doctor/checks.go` | 1 | 27 | 0 | 28 |
| 12 | `internal/session/manager.go` | 1 | 23 | 4 | 28 |
| 13 | `cmd/gc/cmd_sling.go` | 0 | 26 | 0 | 26 |
| 14 | `internal/config/config.go` | 9 | 17 | 0 | 26 |
| 15 | `internal/extmsg/binding_service.go` | 1 | 16 | 9 | 26 |

## File-change concentration and correlation

For each current hand-written production file, commit concentration is the
number of distinct commits in the reachable `HEAD` history whose name-only diff lists
that path. It is a change-frequency proxy, not ownership or causality. The
history has 2,784 reachable commits. The table shows the leading files by that
metric and their Messgo total to make overlap observable.

| Rank | File | Commits touching file | Messgo total |
| ---: | --- | ---: | ---: |
| 1 | `cmd/gc/cmd_start.go` | 235 | 18 |
| 2 | `internal/config/config.go` | 215 | 26 |
| 3 | `cmd/gc/city_runtime.go` | 161 | 29 |
| 4 | `cmd/gc/main.go` | 137 | 8 |
| 5 | `cmd/gc/cmd_sling.go` | 116 | 26 |
| 6 | `cmd/gc/controller.go` | 116 | 17 |
| 7 | `cmd/gc/session_reconciler.go` | 111 | 16 |
| 8 | `cmd/gc/build_desired_state.go` | 104 | 16 |
| 9 | `cmd/gc/pool.go` | 104 | 6 |
| 10 | `cmd/gc/cmd_session.go` | 99 | 18 |
| 11 | `cmd/gc/cmd_supervisor.go` | 99 | 14 |
| 12 | `cmd/gc/cmd_agent.go` | 94 | 6 |

The clearest overlap between the leading lists is `internal/config/config.go`
(215 commits, 26 violations), `cmd/gc/city_runtime.go` (161, 29), and
`cmd/gc/cmd_sling.go` (116, 26). `cmd/gc/session_lifecycle_parallel.go` is
also a notable mixed hotspot (65 commits, 41 violations), although it sits
below the top twelve change-frequency paths.

## Commits associated with leading violation hotspots

These are the three most recent commits whose path history includes each of
the six leading violation files. They establish association only: the audit
does **not** claim that any listed commit introduced a violation or caused a
hotspot.

| File | Associated commits (newest first) |
| --- | --- |
| `internal/extmsg/types.go` | `51893e360` (2026-04-02, feat(extmsg): wire extmsg API endpoints into controller (#251)); `079496a6a` (2026-03-25, fix: resolve all golangci-lint warnings (#126)); `398ce6d71` (2026-03-23, Add transcript-backed external messaging threads) |
| `internal/runtime/runtime.go` | `1a6ed739c` (2026-04-22, fix: tighten ACP transport capability checks); `de95cc488` (2026-04-22, fix: propagate ACP MCP session config); `514bd283a` (2026-04-19, Merge pull request #927 from quad341/fix/744-handoff-named-session-no-restart) |
| `cmd/gc/session_lifecycle_parallel.go` | `1895c64e0` (2026-08-19, fix(pool): align BEADS_ACTOR claims with runtime session identity (#9)); `d57a64dfe` (2026-04-28, fix: guard async session start commits); `283d65880` (2026-04-27, perf: enqueue session starts asynchronously) |
| `internal/runtime/tmux/tmux.go` | `fd4642ff1` (2026-08-19, fix: fail closed when nudge submit Enter is not confirmed (#6)); `d25e715a0` (2026-04-30, ci: stabilize and parallelize Blacksmith proof); `5ee3b885f` (2026-04-19, fix: restore worker branch ci regressions) |
| `internal/runtime/tmux/adapter.go` | `f3e589373` (2026-04-30, Implement auto-respawn on crash using tmux pane-died hook (gascity-12f)); `bd825715a` (2026-04-20, fix(tmux): clean up prompt temp files); `f5231d503` (2026-04-20, fix(tmux): eliminate silent inline fallback for large prompts (#1037)) |
| `internal/beads/beads.go` | `0a79e9b85` (2026-04-16, Session model unification phases 0-2 (#666)); `e66614aa3` (2026-04-14, Stabilize remaining track1 workflow tests); `0cb08cf37` (2026-04-10, fix(beads): Ready() excludes infrastructure types to match bd ready) |

## Reproduction

Run from the repository root at the revision stated above. `--ignore-tests`
is a second safeguard in addition to the explicit production path boundary;
`--ignore-violations-on-exit` makes the command useful as a data collection
step when violations are expected.

```sh
# Confirm the tool's supported names and version.
messgo --version
messgo --help

# Discover exactly the generated in-scope Go files, using the Go convention.
generated_exclude=$(find internal cmd/gc -type f -name '*.go' ! -name '*_test.go' \
  -exec grep -Il '^// Code generated .* DO NOT EDIT\.$' {} + | sort | paste -sd, -)
test -n "$generated_exclude"

# The requested spelling is unavailable in Messgo 0.2.0 (expected error).
messgo internal,cmd/gc json unused --exclude "$generated_exclude" --ignore-tests \
  --ignore-violations-on-exit

# Capture only hand-written production results.
messgo internal,cmd/gc json design --exclude "$generated_exclude" --ignore-tests \
  --ignore-violations-on-exit > /tmp/messgo-handwritten-design.json
messgo internal,cmd/gc json codesize --exclude "$generated_exclude" --ignore-tests \
  --ignore-violations-on-exit > /tmp/messgo-handwritten-codesize.json
messgo internal,cmd/gc json unusedcode --exclude "$generated_exclude" --ignore-tests \
  --ignore-violations-on-exit > /tmp/messgo-handwritten-unusedcode.json

# Rank each already-excluded JSON report by its file-level violation count.
jq -r '.files[] | [.file, (.violations | length)] | @tsv' \
  /tmp/messgo-handwritten-codesize.json | sort -k2,2nr

# Build the current hand-written production file list and count distinct
# historical commits whose name-only diff includes each path.
find internal cmd/gc -type f -name '*.go' ! -name '*_test.go' \
  -exec grep -L '^// Code generated .* DO NOT EDIT\.$' {} + | sort \
  > /tmp/current-handwritten-production-files.txt
git log --format='@@%H' --name-only -- internal cmd/gc | \
  awk 'NR==FNR { current[$0]=1; next }
       /^@@/ { commit=$0; sub(/^@@/, "", commit); next }
       /\.go$/ && current[$0] {
         key=commit SUBSEP $0; if (!seen[key]++) count[$0]++
       }
       END { for (file in count) print count[file] "\t" file }' \
  /tmp/current-handwritten-production-files.txt - | sort -rn -k1,1

# Inspect association for any file; this supplies the commits listed above.
git log --format='%h%x09%as%x09%s' -n 3 -- internal/runtime/tmux/tmux.go
```

For an exact re-run, regenerate all three JSON files at one revision, then
join their per-file counts by path before adding the commit-count column. Do
not compare counts across revisions without recording the revision because
both the source and its history can change.
