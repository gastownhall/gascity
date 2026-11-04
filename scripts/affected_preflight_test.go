package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The affected pre-flight (make check-affected, scripts/affected-preflight)
// bounds a branch pre-flight without launching a whole-repository suite. These
// tests drive the real Makefile target and selector over a temporary module
// with real git and go list; make and bazel are recorders, and the fake bazel
// answers the reader queries from files the test writes.

var affectedGuards = []string{
	"check-release-dist-ignore",
	"check-routed-test-rows",
	"check-split-topology-rows",
	"check-residency-boundary",
	"fmt-check-changed",
	"LINT_CHANGED_SCOPE=worktree",
}

func TestAffectedPreflightSelectsTheDiff(t *testing.T) {
	t.Run("changed Go file selects its package, its importer and its readers", func(t *testing.T) {
		f := newAffectedFixture(t)
		writeTestFile(t, filepath.Join(f.repo, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")
		f.answerQueries(t, "go_test rule //alpha:alpha_test\n", "sh_test rule //:format_test\n")

		f.mustRun(t)
		f.requireMakeCalls(t,
			f.guards(),
			[]string{"lint-targets", "LINT_TARGETS=//alpha:all //beta:all"},
			[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//:format_test //alpha:alpha_test //beta:beta_test"},
		)
		queries := f.bazelCalls(t)
		if len(queries) != 3 || !strings.Contains(strings.Join(queries[0], " "), `set("//alpha:alpha.go")`) {
			t.Errorf("bazel calls = %v, want a direct and an aggregate query over //alpha:alpha.go", queries)
		}
	})

	t.Run("the diff starts at the merge base, not the moved base tip", func(t *testing.T) {
		f := newAffectedFixture(t)
		f.git(t,
			"git checkout -q -b work",
			"printf 'package gamma\\n\\nfunc Value() int { return 2 }\\n' > gamma/gamma.go",
			"git commit -qam work",
			"git checkout -q base",
			"printf 'package beta\\n\\n// moved\\n' >> beta/beta.go",
			"git commit -qam moved",
			"git checkout -q work",
		)

		f.mustRun(t)
		f.requireMakeCalls(t,
			f.guards(),
			[]string{"lint-targets", "LINT_TARGETS=//gamma:all"},
			[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//gamma:gamma_test"},
		)
	})

	t.Run("untracked Go file selects its package", func(t *testing.T) {
		f := newAffectedFixture(t)
		writeTestFile(t, filepath.Join(f.repo, "gamma", "extra.go"), "package gamma\n\nfunc Extra() {}\n")

		f.mustRun(t)
		f.requireMakeCalls(t,
			f.guards(),
			[]string{"lint-targets", "LINT_TARGETS=//gamma:all"},
			[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//gamma:gamma_test"},
		)
	})

	t.Run("untracked file without a Bazel target does not widen the check", func(t *testing.T) {
		f := newAffectedFixture(t)
		writeTestFile(t, filepath.Join(f.repo, ".cursor", "hooks.json"), "{}\n")
		writeTestFile(t, filepath.Join(f.queryDir, "undeclared"), "//:.cursor/hooks.json\n")

		out := f.mustRun(t)
		f.requireMakeCalls(t, f.guards())
		if strings.Contains(out, "full check:") {
			t.Errorf("untracked undeclared file widened the check:\n%s", out)
		}
		calls := f.bazelCalls(t)
		if len(calls) != 1 || !strings.Contains(strings.Join(calls[0], " "), "//:.cursor/hooks.json") {
			t.Errorf("Bazel calls = %v, want only an existence query for the untracked file", calls)
		}
	})

	t.Run("tracked file without a Bazel target still widens the check", func(t *testing.T) {
		f := newAffectedFixture(t)
		writeTestFile(t, filepath.Join(f.repo, ".cursor", "hooks.json"), "{}\n")
		f.git(t, "git add .cursor/hooks.json")
		writeTestFile(t, filepath.Join(f.queryDir, "undeclared"), "//:.cursor/hooks.json\n")

		out := f.mustRefuse(t)
		f.requireMakeCalls(t)
		if !strings.Contains(out, "affected-preflight: full check: selection failed:") ||
			!strings.Contains(out, "no such target '//:.cursor/hooks.json'") {
			t.Errorf("tracked undeclared file did not select the full check:\n%s", out)
		}
	})

	t.Run("untracked undeclared file does not hide an unrelated Bazel error", func(t *testing.T) {
		f := newAffectedFixture(t)
		writeTestFile(t, filepath.Join(f.repo, ".cursor", "hooks.json"), "{}\n")
		writeTestFile(t, filepath.Join(f.queryDir, "undeclared"), "//:.cursor/hooks.json\n")
		writeTestFile(t, filepath.Join(f.queryDir, "fail"), "")

		out := f.mustRefuse(t)
		f.requireMakeCalls(t)
		if !strings.Contains(out, "affected-preflight: full check: selection failed:") ||
			!strings.Contains(out, "fake query failure") {
			t.Errorf("unrelated Bazel error did not select the full check:\n%s", out)
		}
	})

	t.Run("embedded file selects the embedding package", func(t *testing.T) {
		f := newAffectedFixture(t)
		writeTestFile(t, filepath.Join(f.repo, "alpha", "assets", "msg.txt"), "changed\n")

		f.mustRun(t)
		f.requireMakeCalls(t,
			f.guards(),
			[]string{"lint-targets", "LINT_TARGETS=//alpha:all //beta:all"},
			[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//alpha:alpha_test //beta:beta_test"},
		)
	})

	t.Run("data file runs its direct readers only", func(t *testing.T) {
		f := newAffectedFixture(t)
		writeTestFile(t, filepath.Join(f.repo, "docs", "guide.md"), "changed\n")
		f.answerQueries(t, "go_test rule //gamma:gamma_test\n", "")

		f.mustRun(t)
		f.requireMakeCalls(t,
			f.guards(),
			[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//gamma:gamma_test"},
		)
		queries := f.bazelCalls(t)
		if len(queries) == 0 || !strings.Contains(strings.Join(queries[0], " "), `set("//:docs/guide.md")`) {
			t.Errorf("bazel calls = %v, want a query over //:docs/guide.md", queries)
		}
	})

	t.Run("deleted data file tests the package that held it", func(t *testing.T) {
		f := newAffectedFixture(t)
		if err := os.Remove(filepath.Join(f.repo, "gamma", "testdata", "golden.txt")); err != nil {
			t.Fatalf("delete testdata: %v", err)
		}

		f.mustRun(t)
		f.requireMakeCalls(t,
			f.guards(),
			[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//gamma:gamma_test"},
		)
		if calls := f.bazelCalls(t); len(calls) != 1 || !strings.Contains(strings.Join(calls[0], " "), "//gamma:all") {
			t.Errorf("deleted data queries = %v, want the owning package target query", calls)
		}
	})

	t.Run("no change runs the guards only", func(t *testing.T) {
		f := newAffectedFixture(t)

		f.mustRun(t)
		f.requireMakeCalls(t, f.guards())
		f.requireNoBazelCalls(t)
	})
}

func TestAffectedPreflightCapsIndirectTests(t *testing.T) {
	t.Run("importer tests past the cap are linted, not tested", func(t *testing.T) {
		f := newAffectedFixture(t)
		writeTestFile(t, filepath.Join(f.repo, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")

		out := f.mustRun(t, "AFFECTED_MAX_RDEP_TESTS=0")
		f.requireMakeCalls(t,
			f.guards(),
			[]string{"lint-targets", "LINT_TARGETS=//alpha:all //beta:all"},
			[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//alpha:alpha_test"},
		)
		if !strings.Contains(out, "importer tests not run (1 > AFFECTED_MAX_RDEP_TESTS=0") || !strings.Contains(out, "./beta") {
			t.Errorf("output does not name the untested importer:\n%s", out)
		}
	})

	t.Run("aggregate readers past the cap are not run", func(t *testing.T) {
		f := newAffectedFixture(t)
		writeTestFile(t, filepath.Join(f.repo, "gamma", "gamma.go"), "package gamma\n\nfunc Value() int { return 2 }\n")
		f.answerQueries(t, "", "go_test rule //alpha:alpha_test\nsh_test rule //:format_test\n")

		out := f.mustRun(t, "AFFECTED_MAX_READER_TESTS=1")
		f.requireMakeCalls(t,
			f.guards(),
			[]string{"lint-targets", "LINT_TARGETS=//gamma:all"},
			[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//gamma:gamma_test"},
		)
		if !strings.Contains(out, "aggregate-reader tests not run (2 > AFFECTED_MAX_READER_TESTS=1): ./alpha //:format_test") {
			t.Errorf("output does not name the untested readers:\n%s", out)
		}
	})
}

func TestAffectedPreflightRequiresFullGate(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, f affectedFixture){
		"go.mod":               func(t *testing.T, f affectedFixture) { appendTestFile(t, f, "go.mod", "\n") },
		"MODULE.bazel":         func(t *testing.T, f affectedFixture) { appendTestFile(t, f, "MODULE.bazel", "# x\n") },
		"Makefile":             func(t *testing.T, f affectedFixture) { appendTestFile(t, f, "Makefile", "# x\n") },
		"bzl file":             func(t *testing.T, f affectedFixture) { appendTestFile(t, f, "alpha/defs.bzl", "# x\n") },
		"tools tree":           func(t *testing.T, f affectedFixture) { appendTestFile(t, f, "tools/nogo/config.json", "{}\n") },
		"root BUILD":           func(t *testing.T, f affectedFixture) { appendTestFile(t, f, "BUILD.bazel", "# x\n") },
		"BUILD outside a pkg":  func(t *testing.T, f affectedFixture) { appendTestFile(t, f, "docs/BUILD.bazel", "# x\n") },
		"linter configuration": func(t *testing.T, f affectedFixture) { appendTestFile(t, f, ".golangci.yml", "# x\n") },
		"deleted package": func(t *testing.T, f affectedFixture) {
			if err := os.RemoveAll(filepath.Join(f.repo, "gamma")); err != nil {
				t.Fatalf("delete package: %v", err)
			}
		},
		"failed reader query": func(t *testing.T, f affectedFixture) {
			appendTestFile(t, f, "gamma/gamma.go", "// x\n")
			writeTestFile(t, filepath.Join(f.queryDir, "fail"), "")
		},
		"unknown base": func(t *testing.T, f affectedFixture) {
			appendTestFile(t, f, "gamma/gamma.go", "// x\n")
			f.git(t, "git branch -D -q base")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAffectedFixture(t)
			change(t, f)

			out := f.mustRefuse(t)
			f.requireMakeCalls(t)
			if !strings.Contains(out, "affected-preflight: full check: ") {
				t.Errorf("output does not say why it ran the full check:\n%s", out)
			}
		})
	}
}

func TestAffectedPreflightRefusesAPackageWithoutBuildFile(t *testing.T) {
	f := newAffectedFixture(t)
	writeTestFile(t, filepath.Join(f.repo, "delta", "delta.go"), "package delta\n")

	out, err := f.run()
	if err == nil {
		t.Fatalf("check-affected passed a package with no BUILD file:\n%s", out)
	}
	if !strings.Contains(out, "./delta; run 'make bazel-sync'") {
		t.Errorf("refusal does not point at make bazel-sync:\n%s", out)
	}
	f.requireMakeCalls(t)
}

func TestAffectedPreflightStopsAtTheFirstFailingStep(t *testing.T) {
	f := newAffectedFixture(t)
	appendTestFile(t, f, "gamma/gamma.go", "// x\n")

	out, err := f.run("AFFECTED_FAKE_MAKE_FAIL=lint-targets")
	if err == nil {
		t.Fatalf("check-affected passed with a failing lint step:\n%s", out)
	}
	f.requireMakeCalls(t,
		f.guards(),
		[]string{"lint-targets", "LINT_TARGETS=//gamma:all"},
	)
}

func TestAffectedPreflightPlanRunsNothing(t *testing.T) {
	f := newAffectedFixture(t)
	appendTestFile(t, f, "gamma/gamma.go", "// x\n")

	out := f.mustRun(t, "AFFECTED_ACTION=plan")
	f.requireMakeCalls(t)
	if !strings.Contains(out, "affected-preflight: bazel test: //gamma:gamma_test") {
		t.Errorf("plan does not list the selected tests:\n%s", out)
	}
}

func TestAffectedPreflightCoversEveryRegisteredUnitTarget(t *testing.T) {
	f := newAffectedFixture(t)
	appendTestFile(t, f, "gamma/gamma.go", "// changed\n")
	f.answerPackage(t, "gamma", "go_test rule //gamma:gamma_test\ngo_test rule //gamma:auxiliary_test\ngo_variant_test rule //gamma:variant_test\ngo_variant_test rule //gamma:integration_test manual\n")

	f.mustRun(t)
	f.requireMakeCalls(t,
		f.guards(),
		[]string{"lint-targets", "LINT_TARGETS=//gamma:all"},
		[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//gamma:auxiliary_test //gamma:gamma_test //gamma:variant_test"},
	)
}

func TestAffectedPreflightDirectReadersKeepAllPackageTargets(t *testing.T) {
	f := newAffectedFixture(t)
	appendTestFile(t, f, "docs/guide.md", "changed\n")
	f.answerQueries(t, "go_test rule //gamma:gamma_test\nsh_test rule //checks:reader_test\ngo_variant_test rule //gamma:slow_test manual\n", "")
	f.answerPackage(t, "gamma", "go_test rule //gamma:gamma_test\ngo_test rule //gamma:other_test\n")

	f.mustRun(t)
	f.requireMakeCalls(t,
		f.guards(),
		[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//checks:reader_test //gamma:gamma_test //gamma:other_test"},
	)
}

func TestAffectedPreflightManualReadersStayInTheirTier(t *testing.T) {
	f := newAffectedFixture(t)
	appendTestFile(t, f, "docs/guide.md", "changed\n")
	f.answerQueries(t, "go_test rule //gamma:integration_test manual\n", "go_variant_test rule //gamma:acceptance_test manual\n")

	out := f.mustRun(t)
	f.requireMakeCalls(t, f.guards())
	if !strings.Contains(out, "manual") {
		t.Errorf("plan must explain excluded manual targets:\n%s", out)
	}
}

func TestAffectedPreflightRefusesMissingRegisteredTests(t *testing.T) {
	f := newAffectedFixture(t)
	appendTestFile(t, f, "alpha/alpha.go", "// changed\n")
	f.answerPackage(t, "alpha", "")

	out := f.mustRefuse(t)
	f.requireMakeCalls(t)
	if !strings.Contains(out, "./alpha") || !strings.Contains(out, "make bazel-sync") {
		t.Errorf("missing-target refusal lacks package and recovery command:\n%s", out)
	}
}

func TestAffectedPreflightRejectsMalformedTargetMapping(t *testing.T) {
	for name, query := range map[string]string{
		"foreign label":    `<query><rule class="go_test" name="@foreign//gamma:gamma_test"/></query>`,
		"unsupported rule": `<query><rule class="go_test_run" name="//gamma:run"/></query>`,
		"ambiguous label":  `<query><rule class="go_test" name="//gamma:gamma_test"/><rule class="sh_test" name="//gamma:gamma_test"/></query>`,
		"truncated XML":    `<query><rule`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newAffectedFixture(t)
			appendTestFile(t, f, "docs/guide.md", "changed\n")
			writeTestFile(t, filepath.Join(f.queryDir, "direct"), query)

			f.mustRefuse(t)
			f.requireMakeCalls(t)
		})
	}
}

func TestAffectedPreflightImporterKeepsAuxiliaryTargets(t *testing.T) {
	f := newAffectedFixture(t)
	appendTestFile(t, f, "alpha/alpha.go", "// changed\n")
	f.answerPackage(t, "beta", "go_test rule //beta:beta_test\ngo_test rule //beta:aux_test\n")

	f.mustRun(t)
	f.requireMakeCalls(t,
		f.guards(),
		[]string{"lint-targets", "LINT_TARGETS=//alpha:all //beta:all"},
		[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//alpha:alpha_test //beta:aux_test //beta:beta_test"},
	)
}

func TestAffectedPreflightGuardFailureStopsBeforeBazel(t *testing.T) {
	f := newAffectedFixture(t)
	appendTestFile(t, f, "gamma/gamma.go", "// changed\n")
	out, err := f.run("AFFECTED_FAKE_MAKE_FAIL=check-release-dist-ignore")
	if err == nil || !strings.Contains(out, "Error 3") {
		t.Fatalf("guard failure was not propagated:\n%s\n%v", out, err)
	}
	f.requireMakeCalls(t, f.guards())
}

func TestAffectedPreflightPropagatesTestFailure(t *testing.T) {
	f := newAffectedFixture(t)
	appendTestFile(t, f, "gamma/gamma.go", "// changed\n")
	out, err := f.run("AFFECTED_FAKE_MAKE_FAIL=test-bazel-targets")
	if err == nil || !strings.Contains(out, "Error 3") {
		t.Fatalf("test failure was not propagated:\n%s\n%v", out, err)
	}
	f.requireMakeCalls(t,
		f.guards(),
		[]string{"lint-targets", "LINT_TARGETS=//gamma:all"},
		[]string{"test-bazel-targets", "BAZEL_TEST_TARGETS=//gamma:gamma_test"},
	)
}

type affectedFixture struct {
	repo      string
	makefile  string
	fakeMake  string
	fakeBazel string
	makeLog   string
	bazelLog  string
	queryDir  string
	home      string
	mergeBase string
}

func newAffectedFixture(t *testing.T) affectedFixture {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	for name, content := range map[string]string{
		"go.mod":                    "module example.com/affected\n\ngo 1.23\n",
		"MODULE.bazel":              "",
		"Makefile":                  "",
		".golangci.yml":             "",
		"BUILD.bazel":               "",
		"tools/nogo/config.json":    "{}\n",
		"docs/guide.md":             "baseline\n",
		"alpha/BUILD.bazel":         "",
		"alpha/alpha.go":            "package alpha\n\nimport _ \"embed\"\n\n//go:embed assets/msg.txt\nvar msg string\n\nfunc Value() int { return 1 }\n",
		"alpha/alpha_test.go":       "package alpha\n",
		"alpha/assets/msg.txt":      "baseline\n",
		"beta/BUILD.bazel":          "",
		"beta/beta.go":              "package beta\n\nimport \"example.com/affected/alpha\"\n\nfunc Value() int { return alpha.Value() }\n",
		"gamma/BUILD.bazel":         "",
		"gamma/gamma.go":            "package gamma\n\nfunc Value() int { return 1 }\n",
		"gamma/testdata/golden.txt": "baseline\n",
	} {
		writeTestFile(t, filepath.Join(repo, name), content)
	}

	tools := t.TempDir()
	f := affectedFixture{
		repo:      repo,
		makefile:  filepath.Join(repoRoot(t), "Makefile"),
		fakeMake:  filepath.Join(tools, "make"),
		fakeBazel: filepath.Join(tools, "bazel"),
		makeLog:   filepath.Join(tools, "make.calls"),
		bazelLog:  filepath.Join(tools, "bazel.calls"),
		queryDir:  filepath.Join(tools, "query"),
		home:      t.TempDir(),
	}
	writeExecutable(t, f.fakeMake, framedCallRecorder("AFFECTED_FAKE_MAKE_LOG")+
		`if [ -n "${AFFECTED_FAKE_MAKE_FAIL:-}" ] && [ "$1" = "$AFFECTED_FAKE_MAKE_FAIL" ]; then exit 3; fi
`)
	writeExecutable(t, f.fakeBazel, framedCallRecorder("AFFECTED_FAKE_BAZEL_LOG")+
		`[ "$1" = query ] || exit 0
[ ! -e "$AFFECTED_FAKE_QUERY_DIR/fail" ] || { echo "fake query failure" >&2; exit 7; }
if [ "$2" = --output=label ]; then
  label="$3"
  if [ -e "$AFFECTED_FAKE_QUERY_DIR/undeclared" ] && grep -Fqx "$label" "$AFFECTED_FAKE_QUERY_DIR/undeclared"; then
    echo "ERROR: no such target '$label': not declared in package" >&2
    exit 7
  fi
  echo "$label"
  exit 0
fi
if [ -e "$AFFECTED_FAKE_QUERY_DIR/undeclared" ]; then
  while IFS= read -r label; do
    case "$*" in
      *"$label"*) echo "ERROR: no such target '$label': not declared in package" >&2; exit 7 ;;
    esac
  done < "$AFFECTED_FAKE_QUERY_DIR/undeclared"
fi
case "$*" in
  *'tests(set('*)
    echo '<query>'
    for pkg in alpha beta gamma; do
      case "$*" in
        *"//$pkg:all"*) cat "$AFFECTED_FAKE_QUERY_DIR/package-$pkg" ;;
      esac
    done
    echo '</query>'
    ;;
  *filegroup*) cat "$AFFECTED_FAKE_QUERY_DIR/aggregate" ;;
  *) cat "$AFFECTED_FAKE_QUERY_DIR/direct" ;;
esac
`)
	f.answerQueries(t, "", "")
	for _, pkg := range []string{"alpha", "beta", "gamma"} {
		f.answerPackage(t, pkg, "go_test rule //"+pkg+":"+pkg+"_test\n")
	}
	f.git(t,
		"git init -q -b main",
		"git config user.email affected@example.invalid",
		"git config user.name affected-test",
		"git add .",
		"git commit -qm baseline",
		"git branch base",
	)
	f.mergeBase = strings.TrimSpace(f.git(t, "git rev-parse HEAD"))
	return f
}

func appendTestFile(t *testing.T, f affectedFixture, name, content string) {
	t.Helper()
	path := filepath.Join(f.repo, name)
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", name, err)
	}
	writeTestFile(t, path, string(existing)+content)
}

func (f affectedFixture) answerQueries(t *testing.T, direct, aggregate string) {
	t.Helper()
	writeTestFile(t, filepath.Join(f.queryDir, "direct"), "<query>"+affectedQueryRules(direct)+"</query>")
	writeTestFile(t, filepath.Join(f.queryDir, "aggregate"), "<query>"+affectedQueryRules(aggregate)+"</query>")
}

// Query fixtures include tags so an explicit Bazel label cannot accidentally
// opt a manual integration/acceptance target into the unit pre-flight.
func affectedQueryRules(lines string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}
		fmt.Fprintf(&out, `<rule class=%q name=%q><list name="tags">`, words[0], words[2])
		for _, tag := range words[3:] {
			fmt.Fprintf(&out, `<string value=%q/>`, tag)
		}
		out.WriteString(`</list></rule>`)
	}
	return out.String()
}

func (f affectedFixture) answerPackage(t *testing.T, pkg, targets string) {
	t.Helper()
	writeTestFile(t, filepath.Join(f.queryDir, "package-"+pkg), affectedQueryRules(targets))
}

func (f affectedFixture) mustRefuse(t *testing.T) string {
	t.Helper()
	out, err := f.run()
	if err == nil {
		t.Fatalf("check-affected passed an unbounded or unsupported selection:\n%s", out)
	}
	if !strings.Contains(out, "full check:") || !strings.Contains(out, "remote executor or CI") {
		t.Errorf("refusal lacks an actionable full-gate requirement:\n%s", out)
	}
	return out
}

func (f affectedFixture) git(t *testing.T, commands ...string) string {
	t.Helper()
	return runGitFixtureCommands(t, f.repo, f.env(), commands...)
}

func (f affectedFixture) guards() []string {
	return append(append([]string(nil), affectedGuards...), "LINT_CHANGED_REF="+f.mergeBase)
}

func (f affectedFixture) run(vars ...string) (string, error) {
	args := append([]string{
		"--no-print-directory",
		"-f", f.makefile,
		"AFFECTED_BASE=base",
		"AFFECTED_MAKE=" + f.fakeMake,
		"BAZEL=" + f.fakeBazel,
		"SYS_USR_CGO_FALLBACK=0",
	}, vars...)
	var env []string
	for _, v := range vars {
		if strings.HasPrefix(v, "AFFECTED_FAKE_") {
			env = append(env, v)
		}
	}
	cmd := makeCommand(append(args, "check-affected")...)
	cmd.Dir = f.repo
	cmd.Env = append(f.env(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f affectedFixture) mustRun(t *testing.T, vars ...string) string {
	t.Helper()
	out, err := f.run(vars...)
	if err != nil {
		t.Fatalf("make check-affected: %v\n%s", err, out)
	}
	return out
}

func (f affectedFixture) env() []string {
	env := make([]string, 0, len(os.Environ())+12)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch {
		case name == "HOME", name == "GOROOT", name == "GOFLAGS", name == "GOENV", name == "GOWORK",
			name == "MAKEFLAGS", name == "MFLAGS", name == "GIT_CONFIG",
			strings.HasPrefix(name, "GIT_CONFIG_"), strings.HasPrefix(name, "AFFECTED_"),
			strings.HasPrefix(name, "LINT_CHANGED_"), strings.HasPrefix(name, "BAZEL"):
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HOME="+f.home,
		"AFFECTED_FAKE_MAKE_LOG="+f.makeLog,
		"AFFECTED_FAKE_BAZEL_LOG="+f.bazelLog,
		"AFFECTED_FAKE_QUERY_DIR="+f.queryDir,
		"GOFLAGS=-mod=readonly",
		"GOENV=off",
		"GOWORK=off",
		"GOTOOLCHAIN=local",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
}

func (f affectedFixture) requireMakeCalls(t *testing.T, want ...[]string) {
	t.Helper()
	requireFramedCalls(t, "make", readFramedCalls(t, f.makeLog, "make"), want)
}

func (f affectedFixture) bazelCalls(t *testing.T) [][]string {
	t.Helper()
	return readFramedCalls(t, f.bazelLog, "bazel")
}

func (f affectedFixture) requireNoBazelCalls(t *testing.T) {
	t.Helper()
	requireFramedCalls(t, "bazel", f.bazelCalls(t), nil)
}
