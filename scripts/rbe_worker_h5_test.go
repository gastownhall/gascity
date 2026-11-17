package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// h5.sh (.github/actions/rbe-worker/h5.sh, fix 2 and fix 4) is bazel.yml's
// worker-host job's H5 range check, extracted so it can be driven directly
// here, with a fake gh on PATH: every commit between the default branch's
// rbe-worker pin and this PR's must be GitHub-verified, signed, committed
// by web-flow (GitHub's merge-button identity, never a direct push however
// it's signed), and have a pull request merged into rbe-worker's main.
const h5RepoScript = ".github/actions/rbe-worker/h5.sh"

// h5GhStub answers `gh api repos/.../commits/$c` from $H5_COMMITS (a JSON
// object keyed by sha) and `gh api repos/.../commits/$c/pulls` from
// $H5_PULLS (same shape). Every call is logged to $H5_LOG.
const h5GhStub = `#!/bin/sh
printf '%s\n' "$*" >>"$H5_LOG"
jq=
prev=
for a; do
	[ "$prev" = --jq ] && jq=$a
	prev=$a
done
for a; do
	case "$a" in
	*/commits/*/pulls)
		sha=${a#repos/*/commits/}
		sha=${sha%/pulls}
		jq -r "$jq" <"$H5_PULLS/$sha"
		exit 0
		;;
	*/commits/*)
		sha=${a##*/commits/}
		cat "$H5_COMMITS/$sha"
		exit 0
		;;
	esac
done
echo "h5GhStub: unrecognized args: $*" >&2
exit 1
`

type h5Env struct {
	t          *testing.T
	workerRepo string
	bin        string
	log        string
	commits    string
	pulls      string
}

func newH5Env(t *testing.T) *h5Env {
	t.Helper()
	root := t.TempDir()
	e := &h5Env{
		t:          t,
		workerRepo: filepath.Join(root, "worker-repo"),
		bin:        filepath.Join(root, "bin"),
		log:        filepath.Join(root, "gh.log"),
		commits:    filepath.Join(root, "commits"),
		pulls:      filepath.Join(root, "pulls"),
	}
	if err := os.MkdirAll(e.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.commits, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.pulls, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.bin, "gh"), []byte(h5GhStub), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

// setCommit records gh's answer for `commits/$sha`: verified and committer
// login, as GitHub's commit API shapes them.
func (e *h5Env) setCommit(sha string, verified bool, committer string) {
	e.t.Helper()
	body := `{"commit":{"verification":{"verified":` + boolStr(verified) + `}},"committer":{"login":"` + committer + `"}}`
	if err := os.WriteFile(filepath.Join(e.commits, sha), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// setMergedPR records that sha has (or does not have) a pull request
// merged into main.
func (e *h5Env) setMergedPR(sha string, mergedIntoMain bool) {
	e.t.Helper()
	body := "[]"
	if mergedIntoMain {
		body = `[{"merged_at":"2026-01-01T00:00:00Z","base":{"ref":"main"}}]`
	}
	if err := os.WriteFile(filepath.Join(e.pulls, sha), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// run invokes h5.sh against the given worker repo dir and range.
func (e *h5Env) run(old, newSHA string) (stdout, stderr string, err error) {
	e.t.Helper()
	script := filepath.Join(repoRoot(e.t), h5RepoScript)
	env := append(os.Environ(),
		"PATH="+e.bin+":"+os.Getenv("PATH"),
		"GH_TOKEN=test",
		"H5_LOG="+e.log,
		"H5_COMMITS="+e.commits,
		"H5_PULLS="+e.pulls,
	)
	return runRBEScript(e.workerRepo, env, script, e.workerRepo, old, newSHA)
}

// h5Fixture builds a tiny worker-repo git history: root -> a -> b, and
// returns the a and b commits' shas (root's sha is not needed by any test
// here; the chain's existence is what matters).
func h5Fixture(t *testing.T) (dir string, a, b string) {
	t.Helper()
	const fixtureScript = `#!/usr/bin/env bash
set -euo pipefail
dir=${1:?}
git -C "$dir" init -q -b main
git -C "$dir" config user.email t@example.invalid
git -C "$dir" config user.name "h5 test"
git -C "$dir" commit -q --allow-empty -m root
git -C "$dir" rev-parse HEAD >"$dir/../root.sha"
git -C "$dir" commit -q --allow-empty -m a
git -C "$dir" rev-parse HEAD >"$dir/../a.sha"
git -C "$dir" commit -q --allow-empty -m b
git -C "$dir" rev-parse HEAD >"$dir/../b.sha"
`
	root2 := t.TempDir()
	fixture := filepath.Join(root2, "fixture.sh")
	if err := os.WriteFile(fixture, []byte(fixtureScript), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	dir = filepath.Join(root2, "worker-repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runRBEScript(root2, os.Environ(), fixture, dir); err != nil {
		t.Fatalf("build h5 fixture: %v\n%s", err, stderr)
	}
	return dir, strings.TrimSpace(readFile(t, root2, "a.sha")),
		strings.TrimSpace(readFile(t, root2, "b.sha"))
}

// TestRBEWorkerH5FirstPin: old empty or all-zero means no range to check
// (the first pin): h5.sh exits 0 without calling gh at all.
func TestRBEWorkerH5FirstPin(t *testing.T) {
	dir, _, b := h5Fixture(t)
	for _, old := range []string{"", "0000000000000000000000000000000000000000"} {
		e := newH5Env(t)
		e.workerRepo = dir
		stdout, stderr, err := e.run(old, b)
		if err != nil {
			t.Fatalf("old=%q: h5.sh failed: %v\n%s", old, err, stderr)
		}
		if !strings.Contains(stdout, "first pin") {
			t.Errorf("old=%q: stdout = %q, want \"first pin\"", old, stdout)
		}
		if got, err := os.ReadFile(e.log); err == nil && len(got) != 0 {
			t.Errorf("old=%q: gh was called (%q), want no call for the first pin", old, got)
		}
	}
}

// TestRBEWorkerH5MissingBase: the fail-open bug (fix 2) this guards
// against: a base pin not in the fetched history must fail closed with an
// explicit error, not silently iterate zero commits because `git rev-list`
// failed inside a `for c in $(...)` that swallows the failure.
func TestRBEWorkerH5MissingBase(t *testing.T) {
	dir, _, b := h5Fixture(t)
	e := newH5Env(t)
	e.workerRepo = dir
	missing := "abcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"
	stdout, _, err := e.run(missing, b)
	if err == nil {
		t.Fatalf("h5.sh with a missing base pin succeeded; it must fail closed")
	}
	if !strings.Contains(stdout, "is not in rbe-worker main's fetched history") {
		t.Errorf("stdout = %q, want the missing-base error", stdout)
	}
}

// TestRBEWorkerH5RejectsMalformedShas: OLD (once it is not empty or
// all-zero) and NEW must each be exactly 40 lowercase hex characters,
// checked before anything else runs (no gh call, no rev-list): a bad
// caller argument is a config error (exit 2), not a refused commit (exit
// 1) or a silently-empty range.
func TestRBEWorkerH5RejectsMalformedShas(t *testing.T) {
	dir, a, b := h5Fixture(t)
	cases := map[string]struct{ old, new string }{
		"new too short": {old: a, new: "deadbeef"},
		"new uppercase": {old: a, new: strings.ToUpper(b)},
		"new empty":     {old: a, new: ""},
		"old malformed": {old: "not-a-sha", new: b},
		"old too long":  {old: a + "0", new: b},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newH5Env(t)
			e.workerRepo = dir
			stdout, _, err := e.run(c.old, c.new)
			if err == nil {
				t.Fatalf("old=%q new=%q: h5.sh succeeded; it must reject a malformed sha", c.old, c.new)
			}
			if got := exitCode(err); got != 2 {
				t.Errorf("old=%q new=%q: exit %d, want 2", c.old, c.new, got)
			}
			if !strings.Contains(stdout, "40-hex sha") {
				t.Errorf("stdout = %q, want the 40-hex-sha error", stdout)
			}
			if got, err := os.ReadFile(e.log); err == nil && len(got) != 0 {
				t.Errorf("old=%q new=%q: gh was called (%q), want no call before validation", c.old, c.new, got)
			}
		})
	}
}

// TestRBEWorkerH5Succeeds: every commit in the range is verified, signed,
// web-flow, and has a merged PR: h5.sh passes.
func TestRBEWorkerH5Succeeds(t *testing.T) {
	dir, a, b := h5Fixture(t)
	e := newH5Env(t)
	e.workerRepo = dir
	e.setCommit(a, true, "web-flow")
	e.setMergedPR(a, true)
	e.setCommit(b, true, "web-flow")
	e.setMergedPR(b, true)
	stdout, stderr, err := e.run(a, b)
	// range is a..b exclusive..inclusive: a itself is not in the range, so
	// only b's gh answers are actually consulted.
	if err != nil {
		t.Fatalf("h5.sh failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
}

// TestRBEWorkerH5RefusesUnverified: a commit that is not GitHub-verified
// and signed fails the range, whoever committed it.
func TestRBEWorkerH5RefusesUnverified(t *testing.T) {
	dir, a, b := h5Fixture(t)
	e := newH5Env(t)
	e.workerRepo = dir
	e.setCommit(b, false, "web-flow")
	e.setMergedPR(b, true)
	stdout, _, err := e.run(a, b)
	if err == nil {
		t.Fatalf("h5.sh accepted an unverified commit")
	}
	if !strings.Contains(stdout, "not a verified, signed commit") {
		t.Errorf("stdout = %q, want the unverified-commit error", stdout)
	}
}

// TestRBEWorkerH5RefusesNonWebFlowCommitter: a verified, signed commit
// whose committer is not web-flow (a direct push to main signed by the
// maintainer's own key, not a squash-merged PR) still fails: only GitHub's
// merge-button identity passes.
func TestRBEWorkerH5RefusesNonWebFlowCommitter(t *testing.T) {
	dir, a, b := h5Fixture(t)
	e := newH5Env(t)
	e.workerRepo = dir
	e.setCommit(b, true, "the-maintainer")
	e.setMergedPR(b, true)
	stdout, _, err := e.run(a, b)
	if err == nil {
		t.Fatalf("h5.sh accepted a commit not committed by web-flow")
	}
	if !strings.Contains(stdout, "not web-flow") {
		t.Errorf("stdout = %q, want the non-web-flow-committer error", stdout)
	}
}

// TestRBEWorkerH5RefusesNoMergedPR: a verified, signed, web-flow commit
// with no pull request merged into main still fails.
func TestRBEWorkerH5RefusesNoMergedPR(t *testing.T) {
	dir, a, b := h5Fixture(t)
	e := newH5Env(t)
	e.workerRepo = dir
	e.setCommit(b, true, "web-flow")
	e.setMergedPR(b, false)
	stdout, _, err := e.run(a, b)
	if err == nil {
		t.Fatalf("h5.sh accepted a commit with no pull request merged into main")
	}
	if !strings.Contains(stdout, "no pull request merged into main") {
		t.Errorf("stdout = %q, want the no-merged-PR error", stdout)
	}
}
