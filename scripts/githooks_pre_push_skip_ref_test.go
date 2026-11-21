package scripts_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// withRecordingGuard replaces the fixture's ownership-guard stub with one that
// records each call, so a test can tell the guard still ran for a push whose
// suite was skipped.
func (f *prePushFixture) withRecordingGuard(t *testing.T) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "guard-runs")
	writeExecutable(t, filepath.Join(f.repo, "scripts", "push-ownership-guard.sh"), `#!/usr/bin/env bash
assert_bead_still_claimed() { echo checked >> "`+record+`"; return 0; }
`)
	return record
}

// TestPrePushRunsSuiteWithoutSkipRefConfig pins the default: with no
// gascity.prePushSkipRef configured, a Go change runs the push-time suite
// whatever the destination ref is called.
func TestPrePushRunsSuiteWithoutSkipRefConfig(t *testing.T) {
	f := newPrePushFixture(t)
	code, out := f.run(t, "refs/heads/wip/x "+f.commitNew+" refs/heads/wip/x "+f.commitOld+"\n")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("push-time suite not reached without skip config (make invocations = %q)\n%s", got, out)
	}
}

// TestPrePushSkipsSuiteForConfiguredDestinationRefs covers each way a push
// reaches a configured ref: an existing branch, a new remote branch (no base
// to diff), and a detached `git push origin HEAD:refs/heads/<ref>`, where the
// local side is HEAD. Patterns match the destination ref, so all three skip,
// and the ownership guard still runs. The skip is decided before any engine
// choice: neither bazel nor the local-runtime probe is consulted.
func TestPrePushSkipsSuiteForConfiguredDestinationRefs(t *testing.T) {
	zero := strings.Repeat("0", 40)
	cases := []struct {
		name  string
		stdin func(f *prePushFixture) string
	}{
		{"existing branch", func(f *prePushFixture) string {
			return "refs/heads/wip/x " + f.commitNew + " refs/heads/wip/x " + f.commitOld + "\n"
		}},
		{"new remote branch", func(f *prePushFixture) string {
			return "refs/heads/wip/x " + f.commitNew + " refs/heads/wip/x " + zero + "\n"
		}},
		{"detached HEAD to a configured ref", func(f *prePushFixture) string {
			return "HEAD " + f.commitNew + " refs/heads/salvage/x " + f.commitOld + "\n"
		}},
		{"local name differs from destination", func(f *prePushFixture) string {
			return "refs/heads/main " + f.commitNew + " refs/heads/hold/convoy-1 " + zero + "\n"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrePushFixture(t)
			guard := f.withRecordingGuard(t)
			runtimeProbes := filepath.Join(t.TempDir(), "runtime-probes")
			installLocalRuntimeProbe(t, repoRoot(t), f.repo, "missing libicuuc.so.74", runtimeProbes)
			bazelRuns, bazelProbes := f.withFakeBazel(t, announceNone)
			f.git(t, "config", "--add", "gascity.prePushSkipRef", "refs/heads/wip/*")
			f.git(t, "config", "--add", "gascity.prePushSkipRef", "refs/heads/salvage/*")
			f.git(t, "config", "--add", "gascity.prePushSkipRef", "refs/heads/hold/convoy-*")

			code, out := f.run(t, tc.stdin(f))
			if code != 0 {
				t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
			}
			if got := f.read(t, f.makeRuns); got != "" {
				t.Fatalf("push-time suite ran for a configured skip ref: %q\n%s", got, out)
			}
			if !strings.Contains(out, "gascity.prePushSkipRef") {
				t.Fatalf("skip is silent; want a line naming gascity.prePushSkipRef\n%s", out)
			}
			for name, record := range map[string]string{"bazel test": bazelRuns, "bazel info": bazelProbes, "local-runtime probe": runtimeProbes} {
				if got := f.read(t, record); got != "" {
					t.Fatalf("%s ran for a configured skip ref: %q\n%s", name, got, out)
				}
			}
			if got := f.read(t, guard); got != "checked\n" {
				t.Fatalf("ownership guard calls = %q, want one: a skipped suite must not skip the guard", got)
			}
		})
	}
}

// TestPrePushRunsSuiteWhenAnyRefIsNotSkipped keeps a mixed push honest: one
// configured ref must not exempt another ref that changes Go sources.
func TestPrePushRunsSuiteWhenAnyRefIsNotSkipped(t *testing.T) {
	f := newPrePushFixture(t)
	f.git(t, "config", "--add", "gascity.prePushSkipRef", "refs/heads/wip/*")
	stdin := "refs/heads/wip/x " + f.commitNew + " refs/heads/wip/x " + f.commitOld + "\n" +
		"refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"
	code, out := f.run(t, stdin)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("mixed push skipped the suite (make invocations = %q)\n%s", got, out)
	}
}

// TestPrePushSkipRefPatternMatchesWholeRefName rules out a prefix or
// short-name match: the glob is matched against the full destination ref.
func TestPrePushSkipRefPatternMatchesWholeRefName(t *testing.T) {
	f := newPrePushFixture(t)
	f.git(t, "config", "--add", "gascity.prePushSkipRef", "wip/*")
	code, out := f.run(t, "refs/heads/wip/x "+f.commitNew+" refs/heads/wip/x "+f.commitOld+"\n")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("short pattern %q matched refs/heads/wip/x; want full-ref matching (make invocations = %q)\n%s", "wip/*", got, out)
	}
}
