package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The push-time suite (.githooks/lib/push-suite.sh) runs `bazel test //...`
// so a push reuses the action results CI already computed (the committed
// .bazelrc pins every key-affecting flag; scripts/bazel_key_parity_test.go).
// Mode selection, by GC_PREPUSH_SUITE (default auto):
//
//	auto   bazel on PATH and .bazelrc.local configures a remote executor
//	       (a maintainer's credential): --config=remote-exec; bazel on PATH
//	       otherwise: --config=fork-cache (read-only cache, local misses,
//	       nothing uploaded); no bazel: make test-fast-parallel.
//	rbe    --config=remote-exec.     cache  --config=fork-cache.
//	go     make test-fast-parallel (the escape hatch).
//
// An explicit bazel mode without bazel, or an unknown mode, fails the push
// rather than silently running something else.

const (
	prePushBazelCacheArgs = "test //... --config=fork-cache --keep_going"
	prePushBazelRBEArgs   = "test //... --config=remote-exec --keep_going"
	prePushMakeArgs       = "test-fast-parallel"
)

// withFakeBazel puts a `bazel` on the fixture's PATH that records its
// arguments and exits with BAZEL_EXIT (default 0); it returns the record path.
func (f *prePushFixture) withFakeBazel(t *testing.T) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "bazel-runs")
	writeExecutable(t, filepath.Join(f.binDir, "bazel"), `#!/usr/bin/env sh
printf '%s\n' "$*" >> "$BAZEL_RECORD"
exit "${BAZEL_EXIT:-0}"
`)
	f.env = append(f.env, "BAZEL_RECORD="+record)
	return record
}

func (f *prePushFixture) writeBazelRCLocal(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repo, ".bazelrc.local"), []byte(body), 0o644); err != nil {
		t.Fatalf("write .bazelrc.local: %v", err)
	}
}

func (f *prePushFixture) pushRefLine() string {
	return "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"
}

func TestPrePushSuiteModeSelection(t *testing.T) {
	executor := "build:remote-exec --remote_executor=grpcs://rbe.example:443\n" +
		"build:remote-exec --tls_client_certificate=/home/me/rbe.crt\n" +
		"build:remote-exec --tls_client_key=/home/me/rbe.key\n"
	for _, tc := range []struct {
		name      string
		bazel     bool
		mode      string
		rcLocal   string
		wantBazel string
		wantMake  string
	}{
		{name: "no bazel falls back to go test", wantMake: prePushMakeArgs},
		{name: "bazel without credential reads the cache", bazel: true, wantBazel: prePushBazelCacheArgs},
		{name: "maintainer credential executes remotely", bazel: true, rcLocal: executor, wantBazel: prePushBazelRBEArgs},
		{name: "executor flag with a space", bazel: true, rcLocal: "build:remote-exec --remote_executor grpcs://rbe.example:443\n", wantBazel: prePushBazelRBEArgs},
		{name: "commented-out executor", bazel: true, rcLocal: "# build:remote-exec --remote_executor=grpcs://rbe.example:443\n", wantBazel: prePushBazelCacheArgs},
		{name: "certificate without executor", bazel: true, rcLocal: "build:remote-exec --tls_client_certificate=/x.crt\n", wantBazel: prePushBazelCacheArgs},
		{name: "executor outside remote-exec", bazel: true, rcLocal: "build:other --remote_executor=grpcs://rbe.example:443\n", wantBazel: prePushBazelCacheArgs},
		{name: "empty executor", bazel: true, rcLocal: "build:remote-exec --remote_executor=\n", wantBazel: prePushBazelCacheArgs},
		{name: "go escape hatch", bazel: true, mode: "go", rcLocal: executor, wantMake: prePushMakeArgs},
		{name: "forced cache", bazel: true, mode: "cache", rcLocal: executor, wantBazel: prePushBazelCacheArgs},
		{name: "forced rbe", bazel: true, mode: "rbe", wantBazel: prePushBazelRBEArgs},
		{name: "explicit auto", bazel: true, mode: "auto", wantBazel: prePushBazelCacheArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrePushFixture(t)
			var bazelRecord string
			if tc.bazel {
				bazelRecord = f.withFakeBazel(t)
			}
			f.env = append(f.env, "GC_PREPUSH_SUITE="+tc.mode)
			if tc.rcLocal != "" {
				f.writeBazelRCLocal(t, tc.rcLocal)
			}

			code, out := f.run(t, f.pushRefLine())
			if code != 0 {
				t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
			}
			if got := strings.TrimSpace(f.read(t, f.makeRuns)); got != tc.wantMake {
				t.Errorf("make ran %q, want %q\n%s", got, tc.wantMake, out)
			}
			if bazelRecord != "" {
				if got := strings.TrimSpace(f.read(t, bazelRecord)); got != tc.wantBazel {
					t.Errorf("bazel ran %q, want %q\n%s", got, tc.wantBazel, out)
				}
			}
		})
	}
}

// TestPrePushSuiteRejectsUnusableModes: an explicit bazel mode on a machine
// without bazel, or a mistyped mode, must fail the push instead of quietly
// running a different suite.
func TestPrePushSuiteRejectsUnusableModes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bazel bool
		mode  string
	}{
		{name: "rbe without bazel", mode: "rbe"},
		{name: "cache without bazel", mode: "cache"},
		{name: "unknown mode", bazel: true, mode: "bazel-please"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrePushFixture(t)
			var bazelRecord string
			if tc.bazel {
				bazelRecord = f.withFakeBazel(t)
			}
			f.env = append(f.env, "GC_PREPUSH_SUITE="+tc.mode)

			code, out := f.run(t, f.pushRefLine())
			if code == 0 {
				t.Fatalf("pre-push exit = 0, want a failure for GC_PREPUSH_SUITE=%s\n%s", tc.mode, out)
			}
			if !strings.Contains(out, "GC_PREPUSH_SUITE") {
				t.Errorf("failure does not name GC_PREPUSH_SUITE:\n%s", out)
			}
			if got := f.read(t, f.makeRuns); got != "" {
				t.Errorf("make ran %q", got)
			}
			if bazelRecord != "" {
				if got := f.read(t, bazelRecord); got != "" {
					t.Errorf("bazel ran %q", got)
				}
			}
		})
	}
}

// TestPrePushSuitePropagatesBazelFailure: a failing bazel suite blocks the
// push with bazel's exit code, and never retries under go test.
func TestPrePushSuitePropagatesBazelFailure(t *testing.T) {
	f := newPrePushFixture(t)
	f.withFakeBazel(t)
	f.env = append(f.env, "GC_PREPUSH_SUITE=", "BAZEL_EXIT=3")

	code, out := f.run(t, f.pushRefLine())
	if code != 3 {
		t.Fatalf("pre-push exit = %d, want bazel's 3\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Errorf("make ran after a bazel failure: %q", got)
	}
}

// TestPrePushSuiteAutoNeedsGoOnPinnedPath: fork-cache misses run tests on
// this machine with .bazelrc's pinned test PATH, and tests that exec `go`
// fail there when Go lives elsewhere (Homebrew, asdf, ~/sdk). auto then runs
// the go suite instead; remote execution and an explicit cache mode are
// unaffected.
func TestPrePushSuiteAutoNeedsGoOnPinnedPath(t *testing.T) {
	executor := "build:remote-exec --remote_executor=grpcs://rbe.example:443\n"
	for _, tc := range []struct {
		name      string
		goOnPath  bool
		mode      string
		rcLocal   string
		wantBazel string
		wantMake  string
	}{
		{name: "go on the pinned PATH reads the cache", goOnPath: true, wantBazel: prePushBazelCacheArgs},
		{name: "no go on the pinned PATH falls back to go test", wantMake: prePushMakeArgs},
		{name: "remote execution needs no local go", rcLocal: executor, wantBazel: prePushBazelRBEArgs},
		{name: "explicit cache still runs bazel", mode: "cache", wantBazel: prePushBazelCacheArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrePushFixture(t)
			bazelRecord := f.withFakeBazel(t)
			pinned := t.TempDir()
			if tc.goOnPath {
				writeExecutable(t, filepath.Join(pinned, "go"), "#!/usr/bin/env sh\nexit 0\n")
			}
			rc := "test --test_env=PATH=/nonexistent/forwarded\n" +
				"test --test_env=PATH=" + filepath.Join(t.TempDir(), "empty") + ":" + pinned + " # pinned\n"
			if err := os.WriteFile(filepath.Join(f.repo, ".bazelrc"), []byte(rc), 0o644); err != nil {
				t.Fatalf("write .bazelrc: %v", err)
			}
			f.env = append(f.env, "GC_PREPUSH_SUITE="+tc.mode)
			if tc.rcLocal != "" {
				f.writeBazelRCLocal(t, tc.rcLocal)
			}

			code, out := f.run(t, f.pushRefLine())
			if code != 0 {
				t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
			}
			if got := strings.TrimSpace(f.read(t, f.makeRuns)); got != tc.wantMake {
				t.Errorf("make ran %q, want %q\n%s", got, tc.wantMake, out)
			}
			if got := strings.TrimSpace(f.read(t, bazelRecord)); got != tc.wantBazel {
				t.Errorf("bazel ran %q, want %q\n%s", got, tc.wantBazel, out)
			}
			if tc.wantMake != "" && !strings.Contains(out, pinned) {
				t.Errorf("fallback does not name the pinned PATH %s:\n%s", pinned, out)
			}
		})
	}
}
