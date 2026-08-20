# Messgo production hotspot audit

Audited 2026-08-20 against commit
`1895c64e02b3f759d8ce4c73864aa340a14e5fbc` (`HEAD`). This is a read-only
assessment: it changes no production source.

## Scope and method

The production population is the 604 Go files currently present below
`internal/` and `cmd/gc/`, excluding `*_test.go`. This includes the generated
production API client at `internal/api/genclient/client_gen.go`; its generated
status is called out below because a large finding count there should not be
interpreted like hand-written code. The scan deliberately excludes all test
files and the `test/`, `examples/`, `contrib/`, `scripts/`, and generator-command
trees. Those paths are test, example, contributor/support, or code-generation
inputs rather than the compiled `gc` command and its internal runtime.

Messgo was version 0.2.0. The requested `design` and `codesize` rulesets are
built-ins and were run directly. `unused` is **not** a Messgo 0.2.0 ruleset:
the tool reports `unknown ruleset or file "unused"`. Its built-in equivalent is
named `unusedcode`, which was run and is labelled that way throughout this
report. Thus the unavailable requested spelling is recorded rather than
silently substituted.

| Ruleset requested | Commanded Messgo ruleset | Violations | Leading rule |
| --- | --- | ---: | --- |
| `design` | `design` | 127 | `CountInLoopExpression` (43) |
| `codesize` | `codesize` | 1,994 | `CyclomaticComplexity` (975) |
| `unused` | unavailable; closest built-in `unusedcode` | 2,370 | `UnusedFormalParameter` (2,225) |
| **Total** | — | **4,491** | — |

The rule-family totals are counts reported by Messgo, not a measure of defects
or a prioritisation recommendation. In particular, the unused-code family is
mostly formal-parameter findings and the generated client accounts for 1,831
of its 2,370 findings.

## Violation concentration

The table ranks current production files by the sum of all three executed
rulesets. Columns are independently countable Messgo violations; ties would
be ordered by path, although none occur in this excerpt.

| Rank | File | Design | Code size | Unused code | Total |
| ---: | --- | ---: | ---: | ---: | ---: |
| 1 | `internal/api/genclient/client_gen.go` (generated) | 4 | 146 | 1,831 | 1,981 |
| 2 | `internal/extmsg/types.go` | 0 | 0 | 81 | 81 |
| 3 | `internal/runtime/runtime.go` | 0 | 1 | 49 | 50 |
| 4 | `cmd/gc/session_lifecycle_parallel.go` | 2 | 38 | 1 | 41 |
| 5 | `internal/runtime/tmux/tmux.go` | 3 | 28 | 9 | 40 |
| 6 | `internal/runtime/tmux/adapter.go` | 0 | 12 | 26 | 38 |
| 7 | `internal/beads/beads.go` | 0 | 0 | 33 | 33 |
| 8 | `internal/convergence/handler.go` | 0 | 10 | 22 | 32 |
| 9 | `internal/api/state.go` | 0 | 0 | 31 | 31 |
| 10 | `cmd/gc/city_runtime.go` | 2 | 22 | 5 | 29 |
| 11 | `internal/dispatch/ralph.go` | 1 | 28 | 0 | 29 |
| 12 | `internal/doctor/checks.go` | 1 | 27 | 0 | 28 |
| 13 | `internal/session/manager.go` | 1 | 23 | 4 | 28 |
| 14 | `cmd/gc/cmd_sling.go` | 0 | 26 | 0 | 26 |
| 15 | `internal/config/config.go` | 9 | 17 | 0 | 26 |

## File-change concentration and correlation

For each current production file, commit concentration is the number of
distinct commits in the reachable `HEAD` history whose name-only diff lists
that path. It is a change-frequency proxy, not ownership or causality. The
history has 2,782 reachable commits. The table shows the leading files by that
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
below the top twelve change-frequency paths. Conversely, the generated client
is the dominant Messgo hotspot but is not a leading change-frequency hotspot;
that difference is why it should be triaged separately.

## Commits associated with leading violation hotspots

These are the three most recent commits whose path history includes each of
the six leading violation files. They establish association only: the audit
does **not** claim that any listed commit introduced a violation or caused a
hotspot.

| File | Associated commits (newest first) |
| --- | --- |
| `internal/api/genclient/client_gen.go` | `b9c798af1` (2026-04-27, perf(orders): cache order check read model (follow-up) (#1387)); `60c49c61d` (2026-04-22, fix: complete provider patch and mcp fallback wiring); `8c74c50cf` (2026-04-22, fix: recover provider resume contracts) |
| `internal/extmsg/types.go` | `51893e360` (2026-04-02, feat(extmsg): wire extmsg API endpoints into controller (#251)); `079496a6a` (2026-03-25, fix: resolve all golangci-lint warnings (#126)); `398ce6d71` (2026-03-23, Add transcript-backed external messaging threads) |
| `internal/runtime/runtime.go` | `1a6ed739c` (2026-04-22, fix: tighten ACP transport capability checks); `de95cc488` (2026-04-22, fix: propagate ACP MCP session config); `514bd283a` (2026-04-19, Merge pull request #927 from quad341/fix/744-handoff-named-session-no-restart) |
| `cmd/gc/session_lifecycle_parallel.go` | `1895c64e0` (2026-08-19, fix(pool): align BEADS_ACTOR claims with runtime session identity (#9)); `d57a64dfe` (2026-04-28, fix: guard async session start commits); `283d65880` (2026-04-27, perf: enqueue session starts asynchronously) |
| `internal/runtime/tmux/tmux.go` | `fd4642ff1` (2026-08-19, fix: fail closed when nudge submit Enter is not confirmed (#6)); `d25e715a0` (2026-04-30, ci: stabilize and parallelize Blacksmith proof); `5ee3b885f` (2026-04-19, fix: restore worker branch ci regressions) |
| `internal/runtime/tmux/adapter.go` | `f3e589373` (2026-04-30, Implement auto-respawn on crash using tmux pane-died hook (gascity-12f)); `bd825715a` (2026-04-20, fix(tmux): clean up prompt temp files); `f5231d503` (2026-04-20, fix(tmux): eliminate silent inline fallback for large prompts (#1037)) |

## Reproduction

Run from the repository root at the revision stated above. `--ignore-tests`
is a second safeguard in addition to the explicit production path boundary;
`--ignore-violations-on-exit` makes the command useful as a data collection
step when violations are expected.

```sh
# Confirm the tool's supported names and version.
messgo --version
messgo --help

# The requested spelling is unavailable in Messgo 0.2.0 (expected error).
messgo internal,cmd/gc json unused --ignore-tests --ignore-violations-on-exit

# Capture all executed rule-family results.
messgo internal,cmd/gc json design --ignore-tests --ignore-violations-on-exit \
  > /tmp/messgo-production-design.json
messgo internal,cmd/gc json codesize --ignore-tests --ignore-violations-on-exit \
  > /tmp/messgo-production-codesize.json
messgo internal,cmd/gc json unusedcode --ignore-tests --ignore-violations-on-exit \
  > /tmp/messgo-production-unusedcode.json

# Rank each JSON report by its file-level violation count.
jq -r '.files[] | [.file, (.violations | length)] | @tsv' \
  /tmp/messgo-production-codesize.json | sort -k2,2nr

# Build the current production file list and count distinct historical commits
# whose name-only diff includes each path.
find internal cmd/gc -type f -name '*.go' ! -name '*_test.go' | sort \
  > /tmp/current-production-files.txt
git log --format='@@%H' --name-only -- internal cmd/gc | \
  awk 'NR==FNR { current[$0]=1; next }
       /^@@/ { commit=$0; sub(/^@@/, "", commit); next }
       /\\.go$/ && $0 !~ /_test\\.go$/ && current[$0] {
         key=commit SUBSEP $0; if (!seen[key]++) count[$0]++
       }
       END { for (file in count) print count[file] "\\t" file }' \
  /tmp/current-production-files.txt - | sort -rn -k1,1

# Inspect association for any file; this supplies the commits listed above.
git log --format='%h%x09%as%x09%s' -n 3 -- internal/runtime/tmux/tmux.go
```

For an exact re-run, regenerate all three JSON files at one revision, then
join their per-file counts by path before adding the commit-count column. Do
not compare counts across revisions without recording the revision because
both the source and its history can change.
