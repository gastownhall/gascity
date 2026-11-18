package scripts_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

// Rules for the early-exit-reader guard (shell_pipefail_early_exit_test.go):
// what is flagged, what is not, which files are in scope, and the historical
// incidents the guard must keep catching.
//
// Known gap, deliberately not modeled: `read` and loops that `break` are
// consumers that exit early too, but they are not readers in this guard's sense
// and the repo has exactly one harmless site
// (examples/bd/dolt/assets/scripts/runtime.sh: lsof | while read ... break).
// TestPipefailGuardDoesNotFlagSafeShapes pins that a `while read` loop is NOT flagged, so nobody adds
// the rule without also deciding what its rewrite is.

const (
	bashHeader = "#!/usr/bin/env bash\nset -euo pipefail\n"
	// bashHeaderLines is how many lines bashHeader occupies, so the first line
	// of a fixture body is line bashHeaderLines+1.
	bashHeaderLines = 2
	posixHeader     = "#!/bin/sh\nset -eu\nset -o pipefail\n"
)

// analyzeFixture runs the real discovery-by-content and analysis entry points
// over an in-memory file set, the same path the whole-repo test takes.
func analyzeFixture(t *testing.T, files map[string]string) ([]pipefailFinding, error) {
	t.Helper()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var sources []shellSource
	for _, p := range paths {
		data := []byte(files[p])
		if strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml") {
			runs, err := workflowRunSources(p, data)
			if err != nil {
				return nil, err
			}
			sources = append(sources, runs...)
			continue
		}
		if src, ok := newShellSource(p, data); ok {
			sources = append(sources, src)
		}
	}
	return checkShellSources(sources)
}

func mustAnalyze(t *testing.T, files map[string]string) []pipefailFinding {
	t.Helper()
	findings, err := analyzeFixture(t, files)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return findings
}

// want names one expected finding by file, a needle that identifies its line,
// and the reader command.
type want struct {
	file   string
	needle string
	reader string
}

func assertFindings(t *testing.T, files map[string]string, wants []want) {
	t.Helper()
	got := mustAnalyze(t, files)
	type key struct {
		file   string
		line   int
		reader string
	}
	wantKeys := map[key]bool{}
	for _, w := range wants {
		wantKeys[key{w.file, lineOfNeedle(t, files[w.file], w.needle), w.reader}] = true
	}
	gotKeys := map[key]bool{}
	for _, f := range got {
		gotKeys[key{f.File, f.Line, f.Reader}] = true
	}
	if len(got) != len(wants) {
		t.Errorf("got %d findings %s, want %d %+v", len(got), describeFindings(got), len(wants), wants)
	}
	for k := range wantKeys {
		if !gotKeys[k] {
			t.Errorf("missing finding %+v; got %s", k, describeFindings(got))
		}
	}
	for k := range gotKeys {
		if !wantKeys[k] {
			t.Errorf("unexpected finding %+v", k)
		}
	}
}

// bashOneLine is a bash file whose only content after the header is line.
func bashOneLine(line string) map[string]string {
	return map[string]string{"t.sh": bashHeader + line + "\n"}
}

func TestPipefailGuardFlagsEarlyExitReaders(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		reader string
	}{
		{"grep -q", `printf '%s\n' "$v" | grep -q x`, "grep"},
		{"grep -Fxq --", `echo "$v" | grep -Fxq -- "$c"`, "grep"},
		{"grep -qE", `printf x | grep -qE 'a|b'`, "grep"},
		{"grep -E -q split", `printf x | grep -E -q x`, "grep"},
		{"grep -m1", `printf x | grep -m1 x`, "grep"},
		{"grep -m 1", `printf x | grep -m 1 x`, "grep"},
		{"grep --max-count=1", `printf x | grep --max-count=1 x`, "grep"},
		{"grep --max-count 1", `printf x | grep --max-count 1 x`, "grep"},
		{"grep -l", `printf x | grep -l x`, "grep"},
		{"grep -lF cluster", `printf x | grep -lF x`, "grep"},
		{"grep --files-with-matches", `printf x | grep --files-with-matches x`, "grep"},
		{"grep --quiet", `printf x | grep --quiet x`, "grep"},
		{"grep --silent", `printf x | grep --silent x`, "grep"},
		{"egrep -q", `printf x | egrep -q x`, "egrep"},
		{"fgrep -q", `printf x | fgrep -q x`, "fgrep"},
		{"rg -q", `printf x | rg -q x`, "rg"},
		{"grep by path", `printf x | /usr/bin/grep -q x`, "grep"},
		{"grep -e value then -q", `printf x | grep -e pat -q`, "grep"},
		{"head -1", `git log | head -1`, "head"},
		{"head -n 5", `git log | head -n 5`, "head"},
		{"head -n5", `git log | head -n5`, "head"},
		{"head -c 16", `git show HEAD:f | head -c 16`, "head"},
		{"head --lines=5", `git log | head --lines=5`, "head"},
		{"head --bytes 8", `git log | head --bytes 8`, "head"},
		{"sed 1q", `git log | sed 1q`, "sed"},
		{"sed -n {p;q}", `git log | sed -n '/x/{p;q}'`, "sed"},
		{"sed -n 3q", `git log | sed -n '1,3p;3q'`, "sed"},
		{"sed address q", `git log | sed '/x/q'`, "sed"},
		{"sed -e 1q", `git log | sed -e 1q`, "sed"},
		{"sed Q", `git log | sed -n '2{p;Q}'`, "sed"},
		{"awk exit", `git log | awk '{print; exit}'`, "awk"},
		{"awk NR==1 exit", `git log | awk 'NR==1{print;exit}'`, "awk"},
		{"awk exit before END", `git log | awk '/x/ {found=1; exit} END{print found}'`, "awk"},
		{"gawk exit", `git log | gawk '{print; exit}'`, "gawk"},
		{"command wrapper", `printf x | command grep -q x`, "grep"},
		{"timeout wrapper", `printf x | timeout 5 grep -q x`, "grep"},
		{"env wrapper", `printf x | env LC_ALL=C grep -q x`, "grep"},
		{"|& pipe", `git log |& grep -q x`, "grep"},
		{"if condition", `if printf x | grep -q x; then :; fi`, "grep"},
		{"negated", `! printf x | grep -q x`, "grep"},
		{"and list", `printf x | grep -q x && echo y`, "grep"},
		{"or list", `printf x | grep -q x || echo n`, "grep"},
		{"command substitution", `v=$(git log | head -1)`, "head"},
		{"while condition", `while printf x | grep -q x; do break; done`, "grep"},
		{"subshell", `( printf x | grep -q x )`, "grep"},
		{"process substitution body", `cat <( printf x | grep -q x )`, "grep"},
		{"case arm", "case $1 in a) printf x | grep -q x ;; esac", "grep"},
		{"brace group", `{ printf x | grep -q x; }`, "grep"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustAnalyze(t, bashOneLine(tc.line))
			if len(got) != 1 {
				t.Fatalf("want exactly 1 finding for %q, got %d %s", tc.line, len(got), describeFindings(got))
			}
			f := got[0]
			if f.Reader != tc.reader {
				t.Errorf("reader = %q, want %q", f.Reader, tc.reader)
			}
			if f.File != "t.sh" || f.Line != bashHeaderLines+1 {
				t.Errorf("position = %s:%d, want t.sh:%d", f.File, f.Line, bashHeaderLines+1)
			}
		})
	}
}

func TestPipefailGuardFlagsMidPipelineStage(t *testing.T) {
	got := mustAnalyze(t, bashOneLine(`git log | head -1 | tr a b`))
	if len(got) != 1 || got[0].Reader != "head" {
		t.Fatalf("mid-pipeline head must be flagged once, got %s", describeFindings(got))
	}
}

func TestPipefailGuardFindsReadersAcrossLines(t *testing.T) {
	files := map[string]string{"t.sh": bashHeader + `
f() {
  git log |
    grep -q trailing_pipe_needle
}

printf x \
  | head -1 # backslash_needle

if ! git remote \
    | grep -q negated_multiline_needle; then
  :
fi
`}
	assertFindings(t, files, []want{
		{"t.sh", "trailing_pipe_needle", "grep"},
		{"t.sh", "backslash_needle", "head"},
		{"t.sh", "negated_multiline_needle", "grep"},
	})
}

func TestPipefailGuardDoesNotFlagSafeShapes(t *testing.T) {
	cases := []struct{ name, line string }{
		{"here-string", `grep -q x <<<"$v"`},
		{"process substitution", `grep -q x < <(git log)`},
		{"grep -c", `printf x | grep -c x`},
		{"grep drain to /dev/null", `printf x | grep x >/dev/null`},
		{"grep -n", `printf x | grep -n x`},
		{"grep -E -i", `printf x | grep -Ei x`},
		{"grep -A value", `printf x | grep -A 1 x`},
		{"grep -e takes the pattern -q", `printf x | grep -e -q`},
		{"grep -f takes the file -q", `printf x | grep -f -q`},
		{"grep after --", `printf x | grep -- -q`},
		{"head -n -1", `git log | head -n -1`},
		{"head -c -5", `git log | head -c -5`},
		{"head --lines=-3", `git log | head --lines=-3`},
		{"head --bytes=-3", `git log | head --bytes=-3`},
		{"sed -n 1p", `git log | sed -n 1p`},
		{"sed s///q", `git log | sed 's/x/q/'`},
		{"sed s///g chain", `git log | sed 's/a/b/g;s/c/d/'`},
		{"sed q in a regex", `git log | sed -n '/q/p'`},
		{"sed y///", `git log | sed 'y/a/q/'`},
		{"awk END exit", `git log | awk 'END{exit}'`},
		{"awk END exit 1", `git log | awk '{print} END { exit 1 }'`},
		{"awk exit in a string", `git log | awk '{print "exit"}'`},
		{"first-stage head", `head -1 file | cat`},
		{"first-stage grep -q", `grep -q x file | cat`},
		{"cmp -s", `printf x | cmp -s - file`},
		{"diff -q", `printf x | diff -q - file`},
		{"sort -c", `printf x | sort -c`},
		{"plain consumer", `printf x | cat`},
		{"no pipe at all", `grep -q x file`},
		{"while read break (known gap)", `lsof | while read -r l; do break; done`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustAnalyze(t, bashOneLine(tc.line)); len(got) != 0 {
				t.Fatalf("%q must not be flagged, got %s", tc.line, describeFindings(got))
			}
		})
	}
}

func TestPipefailGuardIgnoresCommentsHeredocsAndStrings(t *testing.T) {
	files := map[string]string{"t.sh": bashHeader + `
# printf x | grep -q x
echo "run printf x | grep -q y to see it"
echo 'git log | head -1'
cat <<'EOF'
printf x | grep -q inside_heredoc
EOF
cat <<EOF
git log | head -1
EOF
`}
	if got := mustAnalyze(t, files); len(got) != 0 {
		t.Fatalf("comment, string and heredoc text must not be flagged, got %s", describeFindings(got))
	}
}

func TestPipefailGuardReportsProducerAndRewrite(t *testing.T) {
	cases := []struct {
		name         string
		files        map[string]string
		producer     string
		rewriteHas   []string
		rewriteLacks []string
	}{
		{
			name:       "bash variable-fed grep: here-string",
			files:      bashOneLine(`printf '%s\n' "$v" | grep -q x`),
			producer:   "printf",
			rewriteHas: []string{"<<<"},
		},
		{
			name:       "bash command-fed grep: drain form",
			files:      bashOneLine(`git diff --name-only | grep -q .`),
			producer:   "git",
			rewriteHas: []string{">/dev/null"},
		},
		{
			name:       "bash head: sed -n Np",
			files:      bashOneLine(`git log | head -1`),
			producer:   "git",
			rewriteHas: []string{"sed -n"},
		},
		{
			name:         "posix grep: no bash-only rewrite",
			files:        map[string]string{"t.sh": posixHeader + "printf x | grep -q x\n"},
			producer:     "printf",
			rewriteHas:   []string{">/dev/null"},
			rewriteLacks: []string{"<<<", "<("},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustAnalyze(t, tc.files)
			if len(got) != 1 {
				t.Fatalf("want 1 finding, got %s", describeFindings(got))
			}
			if got[0].Producer != tc.producer {
				t.Errorf("producer = %q, want %q", got[0].Producer, tc.producer)
			}
			for _, s := range tc.rewriteHas {
				if !strings.Contains(got[0].Rewrite, s) {
					t.Errorf("rewrite %q lacks %q", got[0].Rewrite, s)
				}
			}
			for _, s := range tc.rewriteLacks {
				if strings.Contains(got[0].Rewrite, s) {
					t.Errorf("rewrite %q must not contain %q", got[0].Rewrite, s)
				}
			}
		})
	}
}

func TestFormatPipefailFinding(t *testing.T) {
	got := formatPipefailFinding(pipefailFinding{
		File: "scripts/x.sh", Line: 12, Reader: "grep", Producer: "printf",
		Rewrite: `grep -q P <<<"$v"`,
	})
	for _, s := range []string{
		"scripts/x.sh:12: ",
		"grep can exit before its input ends",
		"printf may die of SIGPIPE (141) under pipefail and turn a match into a miss",
		`Rewrite: grep -q P <<<"$v"`,
		pipefailGuardDoc,
	} {
		if !strings.Contains(got, s) {
			t.Errorf("report line %q lacks %q", got, s)
		}
	}
}

func TestPipefailGuardSortsFindingsByFileThenLine(t *testing.T) {
	files := map[string]string{
		"b.sh": bashHeader + "printf x | grep -q a\nprintf x | head -1\n",
		"a.sh": bashHeader + "git log | head -1\n",
	}
	got := mustAnalyze(t, files)
	if len(got) != 3 {
		t.Fatalf("want 3 findings, got %s", describeFindings(got))
	}
	order := []string{"a.sh", "b.sh", "b.sh"}
	for i, f := range got {
		if f.File != order[i] {
			t.Fatalf("finding %d is in %s, want %s; got %s", i, f.File, order[i], describeFindings(got))
		}
	}
	if got[1].Line >= got[2].Line {
		t.Errorf("b.sh findings not in line order: %s", describeFindings(got))
	}
}

func TestPipefailGuardFailsOnAParseError(t *testing.T) {
	files := map[string]string{"bad.sh": bashHeader + "echo ok\nif [ x\n"}
	if _, err := analyzeFixture(t, files); err == nil || !strings.Contains(err.Error(), "bad.sh:") {
		t.Fatalf("a file that fails to parse must fail the analysis naming file and line, got %v", err)
	}
}

func TestShellSourceDiscoveryByContent(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		content string
		ok      bool
		dialect shellDialect
	}{
		{"env bash", "bin/tool", "#!/usr/bin/env bash\n:\n", true, dialectBash},
		{"bash path with flag", "bin/tool", "#!/bin/bash -e\n:\n", true, dialectBash},
		{"sh", "bin/tool", "#!/bin/sh\n:\n", true, dialectPOSIX},
		{"dash", "bin/tool", "#!/bin/dash\n:\n", true, dialectPOSIX},
		{"env ksh", "bin/tool", "#!/usr/bin/env ksh\n:\n", true, dialectPOSIX},
		{"ash", "bin/tool", "#!/bin/ash\n:\n", true, dialectPOSIX},
		{".sh with no shebang is bash", "lib/x.sh", ":\n", true, dialectBash},
		{".bash with no shebang is bash", "lib/x.bash", ":\n", true, dialectBash},
		{"python", "bin/tool", "#!/usr/bin/env python3\nprint(1)\n", false, 0},
		{"zsh is not checked", "bin/tool", "#!/usr/bin/env zsh\n:\n", false, 0},
		{"plain text", "docs/readme.md", "# title\n", false, 0},
		{"go source", "cmd/main.go", "package main\n", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, ok := newShellSource(tc.path, []byte(tc.content))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && src.Dialect != tc.dialect {
				t.Errorf("dialect = %v, want %v", src.Dialect, tc.dialect)
			}
		})
	}
}

func TestPipefailScopeRules(t *testing.T) {
	t.Run("bash with no set at all is checked", func(t *testing.T) {
		assertFindings(t,
			map[string]string{"t.sh": "#!/usr/bin/env bash\nprintf x | grep -q x\n"},
			[]want{{"t.sh", "grep -q x", "grep"}})
	})
	t.Run("a .sh with no shebang is bash and checked", func(t *testing.T) {
		assertFindings(t,
			map[string]string{"t.sh": "printf x | grep -q x\n"},
			[]want{{"t.sh", "grep -q x", "grep"}})
	})
	t.Run("a .bash library is checked", func(t *testing.T) {
		assertFindings(t,
			map[string]string{"lib.bash": "printf x | grep -q x\n"},
			[]want{{"lib.bash", "grep -q x", "grep"}})
	})
	t.Run("POSIX sh without pipefail is skipped", func(t *testing.T) {
		if got := mustAnalyze(t, map[string]string{"t.sh": "#!/bin/sh\nprintf x | grep -q x\n"}); len(got) != 0 {
			t.Fatalf("got %s", describeFindings(got))
		}
	})
	for _, set := range []string{"set -o pipefail", "set -eo pipefail", "set -euo pipefail"} {
		t.Run("POSIX sh with "+set+" is checked", func(t *testing.T) {
			assertFindings(t,
				map[string]string{"t.sh": "#!/bin/sh\n" + set + "\nprintf x | grep -q x\n"},
				[]want{{"t.sh", "grep -q x", "grep"}})
		})
	}
	t.Run("POSIX sh library sourced from a bash file is checked", func(t *testing.T) {
		assertFindings(t, map[string]string{
			"main.sh":     bashHeader + `. "$DIR/lib/util.sh"` + "\n",
			"lib/util.sh": "#!/bin/sh\nprintf x | grep -q lib_needle\n",
		}, []want{{"lib/util.sh", "lib_needle", "grep"}})
	})
	t.Run("sourced through the source keyword", func(t *testing.T) {
		assertFindings(t, map[string]string{
			"main.sh":     bashHeader + `source ./lib/util.sh` + "\n",
			"lib/util.sh": "#!/bin/sh\nprintf x | grep -q lib_needle\n",
		}, []want{{"lib/util.sh", "lib_needle", "grep"}})
	})
	t.Run("sourcing is transitive", func(t *testing.T) {
		assertFindings(t, map[string]string{
			"main.sh":  bashHeader + `. lib/a.sh` + "\n",
			"lib/a.sh": "#!/bin/sh\n. \"$HERE/b.sh\"\n",
			"lib/b.sh": "#!/bin/sh\nprintf x | grep -q deep_needle\n",
		}, []want{{"lib/b.sh", "deep_needle", "grep"}})
	})
	t.Run("a POSIX library sourced only from a skipped POSIX file stays skipped", func(t *testing.T) {
		got := mustAnalyze(t, map[string]string{
			"main.sh":     "#!/bin/sh\n. lib/util.sh\n",
			"lib/util.sh": "#!/bin/sh\nprintf x | grep -q x\n",
		})
		if len(got) != 0 {
			t.Fatalf("got %s", describeFindings(got))
		}
	})
	t.Run("set +o pipefail in a subshell is honored", func(t *testing.T) {
		files := map[string]string{"t.sh": bashHeader + "( set +o pipefail; printf x | grep -q in_subshell )\nprintf x | grep -q after_subshell\n"}
		assertFindings(t, files, []want{{"t.sh", "after_subshell", "grep"}})
	})
	t.Run("set +o pipefail in a command substitution is honored", func(t *testing.T) {
		files := map[string]string{"t.sh": bashHeader + "v=$(set +o pipefail; printf x | grep -q in_subst)\nprintf x | grep -q after_subst\n"}
		assertFindings(t, files, []want{{"t.sh", "after_subst", "grep"}})
	})
}

func workflowFixture(steps string) map[string]string {
	return map[string]string{".github/workflows/ci.yml": "name: ci\non: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n" + steps}
}

func TestPipefailWorkflowRunBlocks(t *testing.T) {
	const wf = ".github/workflows/ci.yml"
	t.Run("explicit shell bash runs with pipefail", func(t *testing.T) {
		assertFindings(t, workflowFixture(`      - name: gate
        shell: bash
        run: |
          printf x | grep -q explicit_bash_needle
`), []want{{wf, "explicit_bash_needle", "grep"}})
	})
	t.Run("an unspecified shell with set -euo pipefail is checked", func(t *testing.T) {
		assertFindings(t, workflowFixture(`      - name: gate
        run: |
          set -euo pipefail
          printf x | grep -q set_pipefail_needle
`), []want{{wf, "set_pipefail_needle", "grep"}})
	})
	t.Run("an unspecified shell without pipefail is not checked", func(t *testing.T) {
		got := mustAnalyze(t, workflowFixture(`      - name: gate
        run: |
          set -eu
          printf x | grep -q x
`))
		if len(got) != 0 {
			t.Fatalf("unspecified shell is bash -e with no pipefail; got %s", describeFindings(got))
		}
	})
	t.Run("expression placeholders still parse and lines stay YAML lines", func(t *testing.T) {
		files := workflowFixture(`      - name: first
        run: echo ok
      - name: gate
        run: |
          set -euo pipefail
          echo "${{ github.sha }} ${{ matrix.os }}"
          printf x | grep -q yaml_line_needle
`)
		assertFindings(t, files, []want{{wf, "yaml_line_needle", "grep"}})
	})
	t.Run("a single-line run reports the run: line", func(t *testing.T) {
		assertFindings(t, workflowFixture(`      - name: gate
        shell: bash
        run: printf x | grep -q one_line_needle
`), []want{{wf, "one_line_needle", "grep"}})
	})
	t.Run("two blocks keep their own line numbers", func(t *testing.T) {
		assertFindings(t, workflowFixture(`      - name: one
        shell: bash
        run: |
          printf x | grep -q first_block_needle
      - name: two
        shell: bash
        run: |
          echo a
          git log | head -1 # second_block_needle
`), []want{{wf, "first_block_needle", "grep"}, {wf, "second_block_needle", "head"}})
	})
	t.Run("a step with no run is ignored", func(t *testing.T) {
		got := mustAnalyze(t, workflowFixture(`      - uses: actions/checkout@v4
        with:
          note: "printf x | grep -q x"
`))
		if len(got) != 0 {
			t.Fatalf("got %s", describeFindings(got))
		}
	})
}

// Historical regressions, verbatim: each must be flagged, which proves the
// guard would have caught the incident and stops it rotting into matching
// nothing.
func TestPipefailGuardCatchesTheHistoricalIncidents(t *testing.T) {
	t.Run("pre-push #6866: git diff --name-only into grep -q .", func(t *testing.T) {
		files := map[string]string{".githooks/pre-push": `#!/usr/bin/env bash
set -euo pipefail

while read -r local_ref local_sha remote_ref remote_sha; do
  if git diff --name-only "$remote_sha" "$local_sha" -- '*.go' 2>/dev/null | grep -q .; then
    go_changed=1
  fi
done < <(replay_refs)
`}
		assertFindings(t, files, []want{{".githooks/pre-push", "grep -q .", "grep"}})
	})
	t.Run("orphan-sweep #6714: the agent list and the live session id list", func(t *testing.T) {
		files := map[string]string{"orphan-sweep.sh": `#!/usr/bin/env bash
set -uo pipefail

agent_exists() {
  printf '%s\n' "$AGENTS" | grep -Fxq -- "$candidate"
}

live_session_match() {
  printf '%s\n' "$LIVE_SESSION_IDS" | grep -Fxq -- "$c"
}
`}
		assertFindings(t, files, []want{
			{"orphan-sweep.sh", `"$AGENTS" | grep`, "grep"},
			{"orphan-sweep.sh", `"$LIVE_SESSION_IDS" | grep`, "grep"},
		})
	})
	t.Run("the CI gate: ! bd init --help | grep -q in a set -euo pipefail run block", func(t *testing.T) {
		const wf = ".github/workflows/ci.yml"
		assertFindings(t, workflowFixture(`      - name: Verify bd supports proxied-server
        run: |
          set -euo pipefail
          bd version
          if ! bd init --help | grep -q -- --proxied-server; then
            echo "::error::bd has no --proxied-server support" >&2
            exit 1
          fi
`), []want{{wf, "--proxied-server; then", "grep"}})
	})
}

// walkFixtureTree writes the same small tree into root for both discovery
// halves.
func walkFixtureTree(t *testing.T, root string, withPruned bool) {
	t.Helper()
	files := map[string]string{
		"scripts/a.sh":                "#!/usr/bin/env bash\n:\n",
		"bin/tool":                    "#!/bin/sh\n:\n",
		"lib/b.bash":                  ":\n",
		"docs/readme.md":              "# readme\n",
		"data/py":                     "#!/usr/bin/env python3\nprint(1)\n",
		".github/workflows/ci.yml":    "name: ci\non: push\njobs:\n  t:\n    runs-on: x\n    steps:\n      - run: |\n          echo hi\n",
		".githooks/pre-push":          "#!/usr/bin/env bash\n:\n",
		"examples/e/run":              "#!/usr/bin/env bash\n:\n",
		"internal/x/not_shell.go":     "package x\n",
		"internal/y/assets/script.sh": ":\n",
	}
	if withPruned {
		files["node_modules/pkg/x.sh"] = ":\n"
		files["dist/out.sh"] = ":\n"
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func sourcePaths(sources []shellSource) []string {
	seen := map[string]bool{}
	for _, s := range sources {
		seen[s.Path] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func TestWalkShellSourcesFindsShellByContentAndPrunes(t *testing.T) {
	root := t.TempDir()
	walkFixtureTree(t, root, true)
	got, err := walkShellSources(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".githooks/pre-push", ".github/workflows/ci.yml", "bin/tool",
		"examples/e/run", "internal/y/assets/script.sh", "lib/b.bash", "scripts/a.sh",
	}
	if g := sourcePaths(got); strings.Join(g, ",") != strings.Join(want, ",") {
		t.Fatalf("walk found %v, want %v", g, want)
	}
}

func TestGitAndWalkDiscoveryAgree(t *testing.T) {
	root := t.TempDir()
	walkFixtureTree(t, root, false)
	// testutil.RunGit strips the ambient GIT_DIR-style variables, and routes the
	// subprocess through the one helper the resource census already counts, so
	// this test adds no subprocess call site of its own.
	testutil.RunGit(t, root, "init", "-q")
	testutil.RunGit(t, root, "add", "-A")
	viaGit, err := gitShellSources(root)
	if err != nil {
		t.Fatal(err)
	}
	viaWalk, err := walkShellSources(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(viaGit) == 0 {
		t.Fatal("git discovery found nothing in a tracked fixture tree")
	}
	if g, w := sourcePaths(viaGit), sourcePaths(viaWalk); strings.Join(g, ",") != strings.Join(w, ",") {
		t.Fatalf("git discovery %v != walk discovery %v", g, w)
	}
	auto, err := discoverShellSources(root)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := sourcePaths(auto), sourcePaths(viaGit); strings.Join(g, ",") != strings.Join(w, ",") {
		t.Fatalf("discoverShellSources %v != git discovery %v", g, w)
	}
}
