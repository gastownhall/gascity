package testenv_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// These tests pin the contract of the hermetic git template that init()
// installs in a go-test binary (ga-zoe1wr, ga-buns9h): every repo a Go test
// creates with `git init` or `git clone` carries maintenance.auto=false, so
// git's detached auto-maintenance can never outlive a test and race its
// t.TempDir cleanup.

// gitEnv is the environment these tests give git: the test process's own, so
// GIT_TEMPLATE_DIR is whatever init() left there, minus every other GIT_*
// variable (a pre-push hook exports GIT_DIR and friends, which would relocate
// the repo under test), plus a config that cannot decide the outcome.
func gitEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+8+len(extra))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "GIT_TEMPLATE_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=gc-test", "GIT_AUTHOR_EMAIL=gc-test@example.invalid",
		"GIT_COMMITTER_NAME=gc-test", "GIT_COMMITTER_EMAIL=gc-test@example.invalid",
	)
	return append(env, extra...)
}

// runGit runs git in dir under gitEnv(extra...) and fails the test on error.
func runGit(t *testing.T, dir string, extra []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(extra...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestGitReposCreatedByTestsCarryAutoMaintenanceOff covers the four ways a
// test makes a repo: each leaves maintenance.auto=false in the repo's own
// config, which is what keeps git from spawning detached maintenance after a
// commit, merge, fetch or pull in it.
func TestGitReposCreatedByTestsCarryAutoMaintenanceOff(t *testing.T) {
	work := t.TempDir()
	// An empty bare repo is enough to clone from, and making it runs no
	// porcelain command that could spawn maintenance in an uncovered repo.
	source := filepath.Join(work, "source.git")
	runGit(t, work, nil, "init", "-q", "--bare", source)

	cases := []struct {
		name string
		bare bool
		args []string // the new repo's path is appended
	}{
		{"init", false, []string{"init", "-q"}},
		{"init --bare", true, []string{"init", "-q", "--bare"}},
		{"clone", false, []string{"clone", "-q", source}},
		{"clone --bare", true, []string{"clone", "-q", "--bare", source}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(work, "repo"+strconv.Itoa(i))
			runGit(t, work, nil, append(tc.args, repo)...)

			cmd := exec.Command("git", "-C", repo, "config", "--local", "--get", "maintenance.auto")
			cmd.Env = gitEnv()
			out, err := cmd.Output()
			if got := strings.TrimSpace(string(out)); err != nil || got != "false" {
				t.Errorf("`git config --local --get maintenance.auto` in a repo made by `git %s` = %q (err %v), want %q; GIT_TEMPLATE_DIR=%q",
					strings.Join(tc.args, " "), got, err, "false", os.Getenv("GIT_TEMPLATE_DIR"))
			}

			// A template without these would leave them absent, and anything
			// that writes a hook or an ignore rule into a test repo would fail.
			gitDir := filepath.Join(repo, ".git")
			if tc.bare {
				gitDir = repo
			}
			if fi, err := os.Stat(filepath.Join(gitDir, "hooks")); err != nil || !fi.IsDir() {
				t.Errorf("no hooks directory in %s (stat err %v)", gitDir, err)
			}
			if _, err := os.Stat(filepath.Join(gitDir, "info", "exclude")); err != nil {
				t.Errorf("no info/exclude in %s: %v", gitDir, err)
			}
		})
	}
}

// TestCommitInAFreshTestRepoSpawnsNoAutoMaintenance is the end-to-end check:
// git must not even start `git maintenance run` when a test commits. The spawn
// is already in the trace when `git commit` returns, so there is nothing to
// wait for. The amplifier makes every auto-maintenance check due, so an
// uncovered repo spawns on its first commit rather than after the many
// commits the default thresholds need.
func TestCommitInAFreshTestRepoSpawnsNoAutoMaintenance(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	runGit(t, filepath.Dir(repo), nil, "init", "-q", repo)

	trace := filepath.Join(t.TempDir(), "git.trace")
	runGit(t, repo, []string{
		"GIT_TRACE=" + trace,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=maintenance.geometric-repack.auto",
		"GIT_CONFIG_VALUE_0=-1",
	}, "commit", "-q", "--allow-empty", "-m", "seed")

	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("reading the git trace: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "built-in: git commit") {
		t.Fatalf("the trace never saw the commit, so an empty result would prove nothing; trace:\n%s", got)
	}
	if strings.Contains(got, "maintenance run") {
		t.Errorf("`git commit` in a fresh test repo spawned auto-maintenance; GIT_TEMPLATE_DIR=%q; trace:\n%s",
			os.Getenv("GIT_TEMPLATE_DIR"), got)
	}
}

// childReportsGitTemplateDir is the whole job of the re-exec child in the
// tests below: print GIT_TEMPLATE_DIR as init() left it, then exit.
func childReportsGitTemplateDir() {
	os.Stdout.WriteString("GIT_TEMPLATE_DIR=" + os.Getenv("GIT_TEMPLATE_DIR") + "\n") //nolint:errcheck
	os.Exit(0)
}

// runTemplateChild runs binary as the child of the named test, under exactly
// env, and returns what it wrote. The error is the run's own, so a caller can
// expect a non-zero exit.
func runTemplateChild(binary, test string, env ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(binary, "-test.run=^"+test+"$", "-test.v")
	cmd.Env = append([]string{"GC_TESTENV_CHILD=1"}, env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// reexecReportingGitTemplateDir runs binary as the child of the named test,
// under exactly env, and returns the GIT_TEMPLATE_DIR the child reports.
func reexecReportingGitTemplateDir(t *testing.T, binary, test string, env ...string) string {
	t.Helper()
	stdout, stderr, err := runTemplateChild(binary, test, env...)
	if err != nil {
		t.Fatalf("re-exec: %v\nstderr: %s", err, stderr)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if v, ok := strings.CutPrefix(line, "GIT_TEMPLATE_DIR="); ok {
			return v
		}
	}
	t.Fatalf("child reported no GIT_TEMPLATE_DIR line; output:\n%s", stdout)
	return ""
}

// assertIsGitTemplateIn fails unless dir is a template directory named the way
// ensureGitTemplate names it, directly under root.
func assertIsGitTemplateIn(t *testing.T, dir, root string) {
	t.Helper()
	if got := filepath.Dir(dir); got != root {
		t.Errorf("GIT_TEMPLATE_DIR=%q is under %q, want it directly under %q", dir, got, root)
	}
	name := regexp.MustCompile(`^gc-test-gittemplate-` + strconv.Itoa(os.Getuid()) + `-[0-9a-f]{12}$`)
	if !name.MatchString(filepath.Base(dir)) {
		t.Errorf("GIT_TEMPLATE_DIR=%q is not named gc-test-gittemplate-<uid>-<12 hex>", dir)
	}
}

// TestInitInstallsItsGitTemplateOverTheAmbientOne verifies init() points
// GIT_TEMPLATE_DIR at its own template even when the caller exported another:
// an ambient one would silently drop the coverage. A test that needs its own
// passes --template or an explicit env, which still wins. The template lives
// under $TEST_TMPDIR when Bazel sets it, else under the temp dir.
func TestInitInstallsItsGitTemplateOverTheAmbientOne(t *testing.T) {
	if os.Getenv("GC_TESTENV_CHILD") == "1" {
		childReportsGitTemplateDir()
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}

	t.Run("under the temp dir", func(t *testing.T) {
		ambient, root := t.TempDir(), t.TempDir()
		got := reexecReportingGitTemplateDir(t, exe, "TestInitInstallsItsGitTemplateOverTheAmbientOne",
			"TEST_SRCDIR=bazel", "TMPDIR="+root, "GIT_TEMPLATE_DIR="+ambient)
		if got == ambient {
			t.Fatalf("init() left the ambient GIT_TEMPLATE_DIR=%q in place", ambient)
		}
		assertIsGitTemplateIn(t, got, root)
	})

	t.Run("TEST_TMPDIR beats the temp dir", func(t *testing.T) {
		ambient, testTmp, tmp := t.TempDir(), t.TempDir(), t.TempDir()
		got := reexecReportingGitTemplateDir(t, exe, "TestInitInstallsItsGitTemplateOverTheAmbientOne",
			"TEST_SRCDIR=bazel", "TEST_TMPDIR="+testTmp, "TMPDIR="+tmp, "GIT_TEMPLATE_DIR="+ambient)
		if got == ambient {
			t.Fatalf("init() left the ambient GIT_TEMPLATE_DIR=%q in place", ambient)
		}
		assertIsGitTemplateIn(t, got, testTmp)
	})
}

// TestInitInstallsNoGitTemplateInTestscriptSubcommandMode verifies init()
// leaves GIT_TEMPLATE_DIR alone, and creates no template, when the binary is
// re-invoked under a non-`.test` name as a testscript subcommand: that process
// is the program under test, not the test, and must see the environment it
// was given. The same binary under a `.test` name is the control, so an empty
// result cannot come from the child failing to report.
func TestInitInstallsNoGitTemplateInTestscriptSubcommandMode(t *testing.T) {
	if os.Getenv("GC_TESTENV_CHILD") == "1" {
		childReportsGitTemplateDir()
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	bin := t.TempDir()
	subcommand, control := filepath.Join(bin, "gc"), filepath.Join(bin, "gc.test")
	for _, dst := range []string{subcommand, control} {
		if err := copyFile(exe, dst); err != nil {
			t.Fatalf("copyFile: %v", err)
		}
	}

	templates := func(root string) []string {
		found, err := filepath.Glob(filepath.Join(root, "gc-test-gittemplate-*"))
		if err != nil {
			t.Fatalf("Glob: %v", err)
		}
		return found
	}

	controlRoot := t.TempDir()
	got := reexecReportingGitTemplateDir(t, control, "TestInitInstallsNoGitTemplateInTestscriptSubcommandMode",
		"TMPDIR="+controlRoot)
	assertIsGitTemplateIn(t, got, controlRoot)
	if found := templates(controlRoot); len(found) != 1 {
		t.Fatalf("control run (go-test mode) left %v under %s, want exactly its one template", found, controlRoot)
	}

	root := t.TempDir()
	if got := reexecReportingGitTemplateDir(t, subcommand, "TestInitInstallsNoGitTemplateInTestscriptSubcommandMode",
		"TMPDIR="+root); got != "" {
		t.Errorf("subcommand mode set GIT_TEMPLATE_DIR=%q, want it unset", got)
	}
	if found := templates(root); len(found) != 0 {
		t.Errorf("subcommand mode created %v under %s, want nothing", found, root)
	}
}

// TestInitRefusesAPlantedGitTemplate verifies init() stops the test binary,
// naming the offending path and the remedy, when the template it finds is not
// exactly what it writes. Git copies a template's hooks/ into every repo made
// from it, so a hook planted there would run on every test's commit. The first
// run makes the template; the second finds a hook added to it; the third finds
// it removed again, which shows the second refused the hook and not something
// else about the run.
func TestInitRefusesAPlantedGitTemplate(t *testing.T) {
	if os.Getenv("GC_TESTENV_CHILD") == "1" {
		childReportsGitTemplateDir()
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	root := t.TempDir()
	env := []string{"TEST_SRCDIR=bazel", "TMPDIR=" + root}

	template := reexecReportingGitTemplateDir(t, exe, "TestInitRefusesAPlantedGitTemplate", env...)
	assertIsGitTemplateIn(t, template, root)
	if t.Failed() {
		t.FailNow()
	}

	planted := filepath.Join(template, "hooks", "pre-commit")
	if err := os.WriteFile(planted, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("planting a hook: %v", err)
	}
	stdout, stderr, err := runTemplateChild(exe, "TestInitRefusesAPlantedGitTemplate", env...)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("a binary whose template had a planted hook ran on (err %v)\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	for _, want := range []string{planted, "remove"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not say %q; stderr:\n%s", want, stderr)
		}
	}

	if err := os.Remove(planted); err != nil {
		t.Fatalf("removing the planted hook: %v", err)
	}
	if got := reexecReportingGitTemplateDir(t, exe, "TestInitRefusesAPlantedGitTemplate", env...); got != template {
		t.Errorf("after the hook was removed, GIT_TEMPLATE_DIR=%q, want the same template %q", got, template)
	}
}
