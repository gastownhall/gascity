---
title: Early-exit readers under pipefail
description: Why `printf ... | grep -q` and `cmd | head -1` flip results under `set -o pipefail`, which sites the guard checks, and the one-token rewrites.
---

`TestShellNoEarlyExitReaderUnderPipefail` (in `scripts/`) fails the build when a
reader that can stop before its input ends sits at the end of a pipe under
`set -o pipefail`. This page is what every finding points at.

## The mechanism

```bash
set -o pipefail
if printf '%s\n' "$list" | grep -q "$name"; then   # BUG
```

1. `printf` writes `$list` into the pipe in chunks.
2. `grep -q` finds a match, exits 0, and closes its end of the pipe.
3. `printf` writes its next chunk to a closed pipe, gets `SIGPIPE`, and exits
   141.
4. `pipefail` makes the pipeline's status the rightmost non-zero status: 141.
   The `if` sees failure. A match became a miss.

It only happens when the producer still has data to write after the reader has
decided. Small inputs fit in the pipe buffer in one write, so the test passes
and the bug stays invisible until the input grows.

Measured on bash 5.3, `printf '%s\n' "$v" | grep -q MARK` with the match on the
first line, 300 runs per size:

| payload | false misses |
|---|---|
| 1 KiB | 0 / 300 |
| 16 KiB | 0 / 300 |
| 64 KiB | 162 / 300 |
| 128 KiB and up | 300 / 300 |

The onset sits at the pipe buffer size (64 KiB on Linux), and past it the
result is a coin flip and then a certainty. Retrying a flaky test hides this
for a while. The same shape has already cost this repo three incidents: the pre-push
Go-change scan (#6866), the orphan-sweep membership checks (#6714) and an earlier
fix (#3796). The CI gate `bd init --help | grep -q -- --proxied-server` flipped
0 to 2 times in 300 on the host it was measured on.

Readers that exit early:

- the grep family with `-q`/`--quiet`/`--silent`, `-m N`/`--max-count`, or
  `-l`/`--files-with-matches`
- `head`, in every form except a negative count (`-n -N`, `-c -N`), which reads
  everything
- `sed` with a `q` or `Q` command
- `awk` with an `exit` outside an `END { }` block

`cmp -s`, `diff -q` and `sort -c` are not readers in this sense: their own early
exit is non-zero, so `pipefail` reports it truthfully.

ShellCheck does not catch this. Nothing in the pipeline is wrong in isolation,
and the failure needs a particular input size.

## What the guard checks

The test parses every tracked shell source with `mvdan.cc/sh/v3/syntax` (parse
only, nothing is executed) and flags a reader in any pipeline stage after the
first, for `|` and `|&`, through `command`, `builtin`, `env`, `timeout` and
`exec` wrappers, wherever a pipeline can appear (`if`, `!`, `&&`/`||`, `$(...)`,
functions, multi-line pipelines).

Sources are found by content, never by a directory list: a first line `#!`
naming sh, bash, dash, ash or ksh (directly or through `env`), a `*.sh` or
`*.bash` name, and every `run:` block of `.github/workflows/*.yml`.

Scope:

- **Bash is always checked**, with or without `set -o pipefail`. A library
  inherits its sourcer's options, so removing `pipefail` must never silence the
  guard.
- **POSIX sh** is checked only when the file sets `pipefail` itself, or is
  sourced (`source X` / `. X`, resolved by the trailing path of `X`,
  transitively) from a file in scope. Otherwise `pipefail` is not portable
  there and a bash-only rewrite would be wrong.
- **Workflow `run:` blocks** are checked when the step's shell is `bash` (an
  explicit `shell: bash` runs as `bash -eo pipefail`) or the block sets
  `pipefail`. An unspecified shell is `bash -e` and has no `pipefail`.
- `set +o pipefail` inside a subshell or command substitution is honored.

A file that does not parse fails the test and names the file and line. It is
never skipped. A scan set below the source floor, or a canary directory with no
sources, fails too, so the guard cannot quietly check less than it claims.

Known gap: `read` and loops that `break` (`cmd | while read ...; do break; done`)
are not flagged.

## No exemptions

There is no allow-comment, no baseline file and no directory exclusion list. Each
finding has a one-token rewrite, and an escape hatch is what rots. Do not fix a
finding by removing `pipefail`, adding `|| true`, or reordering stages.

## The rewrites

| Situation | bash | POSIX sh (in-scope libraries) |
|---|---|---|
| Variable-fed reader (`echo "$v"` or `printf '%s\n' "$v"` into any reader) | here-string: `grep -q P <<<"$v"`, `head -1 <<<"$v"`, `awk '...exit' <<<"$v"`. No producer process exists to receive `SIGPIPE`. | drain form fed by the same `printf` |
| Command-fed test | drain: `cmd \| grep P >/dev/null` (`pipefail` still reports a real `cmd` failure), or capture first, `out=$(cmd) \|\| handle; grep -q P <<<"$out"`, when the failure needs its own branch | drain only |
| `cmd \| head -N` | `cmd \| sed -n '1,Np'` | same |
| `head -c N` on possibly large input | `head -c N < <(cmd)` (the producer status is deliberately unobserved) | write to a temp file first |
| `sed ...q` or `awk ...exit` | a program that reads to the end: `awk '!seen && P { print $1; seen=1 }'` | same |
| Deliberate early exit from an endless or streaming producer | `grep -q P < <(cmd)` | restructure |

Keep the polarity and short-circuit shape: `! producer | grep -q P` becomes
`! grep -q P <<<"$v"` or `! producer | grep P >/dev/null`. Keep `2>/dev/null` on
the producer. A here-string adds one trailing newline, which does not matter for
line matching; check any byte-exact consumer by hand.

## Running the guard

```bash
bazel test //scripts:scripts_test --test_filter='Shell|Pipefail' \
  --test_env=GO_TEST_WRAP_TESTV=1 --test_output=all
```

The whole-repo test lists every finding, sorted, and fails once. The rules tests
(`scripts/shell_pipefail_early_exit_rules_test.go`) hold the positive and
negative fixtures and the historical incidents verbatim, so the guard cannot rot
into matching nothing.
