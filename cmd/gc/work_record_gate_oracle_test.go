package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Regression coverage for the close gate's publication-oracle helpers (gas-amf).
//
// The gate's scenario tests inject fake oracles (alwaysOnRemote, neverOnRemote,
// remoteContains) so they can exercise the decision logic without a git repo.
// That is the right shape for those tests, but it means the real helper
// implementations are never reached: measured at PR #5103's head, isSelfRemoteURL
// sat at 50% having never once been observed returning true, boundLandingBranches
// at 50% with its truncation arm unexercised, and looksLikeSCPRemote at 60%.
// Six of the eight fixes in the gas-6tc/gas-dr5 review landed in this layer with
// no regression test. These are those tests.
//
// They live in their own file on purpose: work_record_gate_test.go is the
// conflict surface for the in-flight port of this layer onto main (PR #5103,
// then gas-cj7 and gas-avv), and none of the behavior pinned here is touched by
// those commits.

// TestLooksLikeSCPRemote pins the scp-shorthand discriminator. It exists so
// isSelfRemoteURL can reject `git@host:org/repo` without mistaking a local path
// that merely contains a colon for a remote.
func TestLooksLikeSCPRemote(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"scp shorthand with user", "git@github.com:org/repo.git", true},
		{"scp shorthand without user", "github.com:org/repo", true},
		{"colon with no slash anywhere", "host:", true},
		{"absolute path", "/srv/git/repo", false},
		{"relative path", "../sibling/repo", false},
		{"bare name", "origin", false},
		{"empty", "", false},
		// The colon sits after the first slash, so this is a path that happens
		// to contain a colon — not scp shorthand. This is the discrimination
		// the function exists for.
		{"colon after a slash is a path", "/srv/git/weird:name", false},
		// A scheme URL also satisfies colon-before-slash. isSelfRemoteURL never
		// asks this function about one — it routes "://" down the scheme branch
		// first — so this is a documented shape of the helper in isolation, not
		// a live defect. Pinned so a future caller knows to pre-filter schemes.
		{"scheme url reads as scp in isolation", "https://example.com/org/repo", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeSCPRemote(tc.url); got != tc.want {
				t.Errorf("looksLikeSCPRemote(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

// TestIsSelfRemoteURL is probe B from gas-amf: the table the injected-fake suite
// could not reach. isSelfRemoteURL resolves both sides through
// `git rev-parse --git-common-dir`, so only a real repo can drive it to true —
// and true is the answer that matters. It is the arm that filters a
// self-referential path remote (the gascity rig's own `herdr-src`) out of the
// publication set, so a commit pushed only back into the same repo is not
// mistaken for durably published (gas-6tc).
func TestIsSelfRemoteURL(t *testing.T) {
	selfDir := t.TempDir()
	runGit(t, selfDir, "init", "--initial-branch=main")
	selfCommon := gitCommonDir("", selfDir)
	if selfCommon == "" {
		t.Fatalf("gitCommonDir(%q) = %q, want the repo's git dir — precondition failed", selfDir, selfCommon)
	}

	otherDir := t.TempDir()
	runGit(t, otherDir, "init", "--initial-branch=main")

	nonRepo := t.TempDir()

	// A repo whose path contains "@". isSelfRemoteURL rejects any at-sign URL
	// as ssh shorthand before it ever resolves the path, so this genuinely
	// self-referential remote reads as "not self". See the dedicated test below.
	atDir := filepath.Join(t.TempDir(), "worktree@2")
	if err := os.MkdirAll(atDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", atDir, err)
	}
	runGit(t, atDir, "init", "--initial-branch=main")
	atCommon := gitCommonDir("", atDir)
	if atCommon == "" {
		t.Fatalf("gitCommonDir(%q) = %q, want the repo's git dir — precondition failed", atDir, atCommon)
	}

	tests := []struct {
		name       string
		url        string
		selfCommon string
		want       bool
	}{
		// The two arms that were never observed returning true.
		{"plain path to self", selfDir, selfCommon, true},
		{"file:// url to self", "file://" + selfDir, selfCommon, true},

		{"plain path to a different repo", otherDir, selfCommon, false},
		{"file:// url to a different repo", "file://" + otherDir, selfCommon, false},
		{"path that is not a repo", nonRepo, selfCommon, false},

		// Non-file schemes are remote by definition, even when the path half
		// points at this very repo.
		{"ssh://localhost to self path", "ssh://localhost" + selfDir, selfCommon, false},
		{"https url", "https://github.com/org/repo.git", selfCommon, false},
		{"git protocol url", "git://example.com/org/repo.git", selfCommon, false},

		{"scp shorthand", "git@github.com:org/repo.git", selfCommon, false},

		{"empty url", "", selfCommon, false},
		{"empty selfCommon", selfDir, "", false},
		{"both empty", "", "", false},

		// gitCommonDir refuses a leading dash rather than handing git something
		// it would read as a flag.
		{"dash-prefixed path", "-" + selfDir, selfCommon, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSelfRemoteURL(selfDir, tc.url, tc.selfCommon); got != tc.want {
				t.Errorf("isSelfRemoteURL(%q, %q, %q) = %v, want %v", selfDir, tc.url, tc.selfCommon, got, tc.want)
			}
		})
	}
}

// TestIsSelfRemoteURLMissesSelfPathContainingAtSign pins a known limitation
// rather than an intended behavior: the at-sign test that rejects ssh shorthand
// runs before any path resolution, so a repo living under a directory with "@"
// in its name is not recognized as self-referential.
//
// The failure is in the unsafe direction — such a remote stays in the
// publication set, so a commit pushed only into the repo itself would read as
// durably published. It needs a real path to reproduce, so it is pinned here and
// filed rather than silently carried.
func TestIsSelfRemoteURLMissesSelfPathContainingAtSign(t *testing.T) {
	atDir := filepath.Join(t.TempDir(), "user@corp")
	if err := os.MkdirAll(atDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", atDir, err)
	}
	runGit(t, atDir, "init", "--initial-branch=main")
	atCommon := gitCommonDir("", atDir)
	if atCommon == "" {
		t.Fatalf("gitCommonDir(%q) = %q, want the repo's git dir — precondition failed", atDir, atCommon)
	}

	// The path IS this repo: resolution would say so if it were reached.
	if got := isSelfRemoteURL(atDir, atDir, atCommon); got {
		t.Fatalf("isSelfRemoteURL(%q, %q, %q) = true — the at-sign limitation is fixed; "+
			"delete this test and drop the at-sign case from TestIsSelfRemoteURL", atDir, atDir, atCommon)
	}
}

// TestBoundLandingBranches covers the truncation arm the advisory path never
// exercised: the branch list is capped so the gate's suggestion stays one
// readable line, and the suggested correction is always first, so truncation
// costs context and never correctness.
func TestBoundLandingBranches(t *testing.T) {
	if maxLandingBranches != 5 {
		t.Fatalf("maxLandingBranches = %d; this table is written against 5 — update the cases", maxLandingBranches)
	}
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, []string{}},
		{"under the cap", []string{"main", "release"}, []string{"main", "release"}},
		{"exactly the cap", []string{"a", "b", "c", "d", "e"}, []string{"a", "b", "c", "d", "e"}},
		{"one over the cap", []string{"a", "b", "c", "d", "e", "f"}, []string{"a", "b", "c", "d", "e", "(+1 more)"}},
		{"several over the cap", []string{"a", "b", "c", "d", "e", "f", "g", "h"}, []string{"a", "b", "c", "d", "e", "(+3 more)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := boundLandingBranches(slices.Clone(tc.in))
			if !slices.Equal(got, tc.want) {
				t.Errorf("boundLandingBranches(%v) = %v, want %v", tc.in, got, tc.want)
			}
			if len(got) > maxLandingBranches+1 {
				t.Errorf("boundLandingBranches(%v) returned %d entries, want at most %d", tc.in, len(got), maxLandingBranches+1)
			}
		})
	}
}

// TestBoundLandingBranchesAliasesCallerSlice pins that the truncation appends
// into the caller's backing array, overwriting the first dropped element.
//
// Both current callers hand over a freshly built slice they discard, so this is
// latent rather than live — which is exactly why it needs a test. If a future
// caller reuses its slice after the call, this documents the trap; if someone
// makes the function copy instead, this test fails and is simply deleted.
func TestBoundLandingBranchesAliasesCallerSlice(t *testing.T) {
	in := []string{"a", "b", "c", "d", "e", "f", "g"}
	_ = boundLandingBranches(in)
	if in[maxLandingBranches] != "(+2 more)" {
		t.Fatalf("in[%d] = %q, want %q — boundLandingBranches no longer aliases its "+
			"argument; that is an improvement, so delete this test",
			maxLandingBranches, in[maxLandingBranches], "(+2 more)")
	}
}

// TestRunWorkRecordCloseGateIgnoresNonCloseArgs pins that only a real close
// reaches the gate, asserted at the runWorkRecordCloseGate seam rather than
// against workRecordCloseTargets in isolation.
//
// The panicOnGetStore double is what gives it teeth: a bd invocation wrongly
// classified as a close walks on to the bead lookup, and with no preFetched
// entry that panics instead of quietly returning false. Verified by mutation —
// making bdUpdateClosesStatus return true unconditionally fails this test.
//
// The two `update` cases carry that weight. The `show` and `list` cases are
// cheap breadth: a downstream write-ID guard independently rejects them, so
// they would survive a regression in the verb switch alone.
func TestRunWorkRecordCloseGateIgnoresNonCloseArgs(t *testing.T) {
	t.Setenv(workRecordEnforceEnvVar, "1")
	for _, args := range [][]string{
		{"show", "wr-1"},
		{"list"},
		{"update", "wr-1", "--notes", "x"},
		{"update", "wr-1", "--status=open"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stderr strings.Builder
			if block := runWorkRecordCloseGate(args, t.TempDir(), "/nonexistent/does-not-exist", nil, panicOnGetStore{}, nil, &stderr); block {
				t.Errorf("runWorkRecordCloseGate(%v) = true, want false — non-close args must never be gated", args)
			}
			if stderr.String() != "" {
				t.Errorf("runWorkRecordCloseGate(%v) wrote %q to stderr, want nothing", args, stderr.String())
			}
		})
	}
}

// runWorkRecordCloseGate's remaining two arms are deliberately left uncovered
// here, and the reasons are worth recording so the next reader does not spend
// the same afternoon on them:
//
//   - The store-opening arm (preOpened == nil) is not unit-testable. Handing the
//     gate a real scope path makes openStoreAtForCityWithConfig start a managed
//     `dolt sql-server`, which the package's own leak guard then fails the run
//     over. That is why every gate test in this package injects preOpened.
//
//   - The fail-open arm ("cannot verify — never block a close on our own read
//     failure") is unreachable by any input. openStoreAtForCityWithConfig
//     returned a usable store for every hostile path tried — nonexistent, a
//     regular file, a chmod-000 directory, a malformed .beads config — because
//     it defers failure to query time.
//
// Both need an injectable store-opener seam to cover, which is a production
// change and not this bead's scope. Left uncovered and filed rather than faked:
// a test that cannot fail is worse than an honest gap.
