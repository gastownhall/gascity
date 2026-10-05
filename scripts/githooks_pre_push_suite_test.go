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
//	auto   bazel on PATH and some rc file Bazel reads names a remote
//	       executor (.bazelrc.local's build:remote-exec, or a machine rc
//	       such as an agent host's ~/.bazelrc): --config=remote-exec; bazel
//	       on PATH otherwise: --config=fork-cache (read-only cache, local
//	       misses, nothing uploaded); no bazel: make test-fast-parallel.
//	rbe    --config=remote-exec.     cache  --config=fork-cache.
//	go     make test-fast-parallel (the escape hatch).
//
// The executor comes from Bazel's own reading of its rc files (`bazel info
// --announce_rc`), which the stub below answers. An explicit bazel mode
// without bazel, rbe without an executor, an unreadable option set or an
// unknown mode fails the push rather than silently running something else.

const (
	prePushBazelCacheArgs = "test //... --config=fork-cache --keep_going"
	prePushBazelRBEArgs   = "test //... --config=remote-exec --keep_going"
	prePushMakeArgs       = "test-fast-parallel"
	// The one probe push-suite.sh runs before choosing a bazel mode.
	prePushBazelProbeArgs = "info --announce_rc --config=remote-exec release"
)

// Bazel 9's --announce_rc listing (stderr) for the probe, by rc source. The
// sections' shape is copied from a real `bazel info --announce_rc
// --config=remote-exec` on an agent host.
const (
	announceClient    = "INFO: Options provided by the client:\n  Inherited 'common' options: --isatty=0 --terminal_columns=80\n"
	announceWorkspace = "INFO: Reading rc options for 'info' from /repo/.bazelrc:\n  Inherited 'common' options: --enable_bzlmod --color=no\n"
	announceRemoteDef = "INFO: Found applicable config definition build:remote-exec in file /repo/.bazelrc: --remote_timeout=3600 --noremote_upload_local_results --remote_download_minimal --jobs=64\n"
	announceNone      = announceClient + announceWorkspace + announceRemoteDef
	// An agent host: ~/.bazelrc executes everything on rbe-west.
	announceHome = announceClient + announceWorkspace +
		"INFO: Reading rc options for 'info' from /home/me/.bazelrc:\n" +
		"  Inherited 'build' options: --remote_executor=grpcs://rbe.example:443 --tls_client_certificate=/home/me/rbe.crt --tls_client_key=/home/me/rbe.key --remote_download_toplevel --jobs=64 --noremote_upload_local_results\n" +
		"WARNING: option '--remote_download_outputs' was expanded from both option '--remote_download_toplevel' (source /home/me/.bazelrc) and option '--remote_download_minimal' (source expanded from --config=remote-exec)\n" +
		announceRemoteDef
	// A system rc (/etc/bazel.bazelrc) with the executor.
	announceSystem = announceClient + announceWorkspace +
		"INFO: Reading rc options for 'info' from /etc/bazel.bazelrc:\n  Inherited 'build' options: --disk_cache=/cache --remote_executor grpcs://rbe.example:443\n" +
		announceRemoteDef
	// A maintainer's .bazelrc.local build:remote-exec credential.
	announceLocalDef = announceNone +
		"INFO: Found applicable config definition build:remote-exec in file /repo/.bazelrc.local: --remote_executor=grpcs://rbe.example:443 --tls_client_certificate=/home/me/rbe.crt\n"
)

// withFakeBazel puts a `bazel` on the fixture's PATH. `bazel info` (the mode
// probe) prints BAZEL_ANNOUNCE to stderr, exits BAZEL_INFO_EXIT (default 0)
// and is recorded in the returned probe record; anything else is recorded
// in the returned run record and exits BAZEL_EXIT (default 0).
func (f *prePushFixture) withFakeBazel(t *testing.T, announce string) (runs, probes string) {
	t.Helper()
	dir := t.TempDir()
	runs, probes = filepath.Join(dir, "bazel-runs"), filepath.Join(dir, "bazel-probes")
	writeExecutable(t, filepath.Join(f.binDir, "bazel"), `#!/usr/bin/env sh
if [ "$1" = info ]; then
  printf '%s\n' "$*" >> "$BAZEL_PROBE_RECORD"
  printf '%s' "$BAZEL_ANNOUNCE" >&2
  echo 'release 9.2.0'
  exit "${BAZEL_INFO_EXIT:-0}"
fi
printf '%s\n' "$*" >> "$BAZEL_RECORD"
exit "${BAZEL_EXIT:-0}"
`)
	f.env = append(f.env, "BAZEL_RECORD="+runs, "BAZEL_PROBE_RECORD="+probes, "BAZEL_ANNOUNCE="+announce)
	// A remote run first checks the checkout's worker-env pin against
	// origin's main (githooks_pre_push_worker_env_test.go): current here.
	f.workerEnv = f.withWorkerEnv(t)
	return runs, probes
}

func (f *prePushFixture) pushRefLine() string {
	return "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"
}

func TestPrePushSuiteModeSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bazel     bool
		mode      string
		announce  string
		wantProbe bool
		wantBazel string
		wantMake  string
	}{
		{name: "no bazel falls back to go test", wantMake: prePushMakeArgs},
		{name: "bazel without an executor reads the cache", bazel: true, announce: announceNone, wantProbe: true, wantBazel: prePushBazelCacheArgs},
		{name: "maintainer credential in .bazelrc.local executes remotely", bazel: true, announce: announceLocalDef, wantProbe: true, wantBazel: prePushBazelRBEArgs},
		{name: "agent host executor in ~/.bazelrc executes remotely", bazel: true, announce: announceHome, wantProbe: true, wantBazel: prePushBazelRBEArgs},
		{name: "system rc executor with a space executes remotely", bazel: true, announce: announceSystem, wantProbe: true, wantBazel: prePushBazelRBEArgs},
		{
			name: "empty executor reads the cache", bazel: true, wantProbe: true, wantBazel: prePushBazelCacheArgs,
			announce: announceNone + "INFO: Found applicable config definition build:remote-exec in file /repo/.bazelrc.local: --remote_executor= --tls_client_certificate=/x.crt\n",
		},
		{
			name: "executor in another config reads the cache", bazel: true, wantProbe: true, wantBazel: prePushBazelCacheArgs,
			announce: announceNone + "INFO: Found applicable config definition build:other in file /repo/.bazelrc.local: --remote_executor=grpcs://rbe.example:443\n",
		},
		{
			name: "executor only in a warning reads the cache", bazel: true, wantProbe: true, wantBazel: prePushBazelCacheArgs,
			announce: announceNone + "WARNING: option '--remote_executor' was expanded from both option --remote_executor=grpcs://rbe.example:443\n",
		},
		{
			// CI's .bazelrc.local selects fork-cache, whose definition resets
			// the executor; the home rc's executor still makes this remote.
			name: "fork-cache reset does not hide a home executor", bazel: true, wantProbe: true, wantBazel: prePushBazelRBEArgs,
			announce: announceHome + "INFO: Found applicable config definition build:fork-cache in file /repo/.bazelrc: --remote_cache=grpcs://cache.example:8443 --remote_executor= --remote_download_minimal\n",
		},
		{name: "go escape hatch", bazel: true, mode: "go", announce: announceHome, wantMake: prePushMakeArgs},
		{name: "forced cache", bazel: true, mode: "cache", announce: announceHome, wantBazel: prePushBazelCacheArgs},
		{name: "forced rbe", bazel: true, mode: "rbe", announce: announceHome, wantProbe: true, wantBazel: prePushBazelRBEArgs},
		{name: "explicit auto", bazel: true, mode: "auto", announce: announceNone, wantProbe: true, wantBazel: prePushBazelCacheArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrePushFixture(t)
			var runs, probes string
			if tc.bazel {
				runs, probes = f.withFakeBazel(t, tc.announce)
			}
			f.env = append(f.env, "GC_PREPUSH_SUITE="+tc.mode)

			code, out := f.run(t, f.pushRefLine())
			if code != 0 {
				t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
			}
			if got := strings.TrimSpace(f.read(t, f.makeRuns)); got != tc.wantMake {
				t.Errorf("make ran %q, want %q\n%s", got, tc.wantMake, out)
			}
			if !tc.bazel {
				return
			}
			if got := strings.TrimSpace(f.read(t, runs)); got != tc.wantBazel {
				t.Errorf("bazel ran %q, want %q\n%s", got, tc.wantBazel, out)
			}
			wantProbe := ""
			if tc.wantProbe {
				wantProbe = prePushBazelProbeArgs
			}
			if got := strings.TrimSpace(f.read(t, probes)); got != wantProbe {
				t.Errorf("bazel probed %q, want %q\n%s", got, wantProbe, out)
			}
			if tc.wantBazel == prePushBazelRBEArgs && !strings.Contains(out, "grpcs://rbe.example:443") {
				t.Errorf("remote run does not name its executor:\n%s", out)
			}
		})
	}
}

// TestPrePushSuiteRejectsUnusableModes: an explicit bazel mode on a machine
// without bazel, or a mistyped mode, must fail the push instead of quietly
// running a different suite.
func TestPrePushSuiteRejectsUnusableModes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bazel    bool
		mode     string
		announce string
		infoExit string
	}{
		{name: "rbe without bazel", mode: "rbe"},
		{name: "cache without bazel", mode: "cache"},
		{name: "unknown mode", bazel: true, mode: "bazel-please"},
		// --config=remote-exec without an executor would compile and run the
		// whole suite on this machine at --jobs=64.
		{name: "rbe without an executor", bazel: true, mode: "rbe", announce: announceNone},
		{name: "unreadable options in auto", bazel: true, announce: "ERROR: Config value 'remote-exec' is not defined in any .rc file\n", infoExit: "2"},
		{name: "unreadable options in rbe", bazel: true, mode: "rbe", announce: "ERROR: bad rc\n", infoExit: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrePushFixture(t)
			var bazelRecord string
			if tc.bazel {
				bazelRecord, _ = f.withFakeBazel(t, tc.announce)
			}
			f.env = append(f.env, "GC_PREPUSH_SUITE="+tc.mode, "BAZEL_INFO_EXIT="+tc.infoExit)

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
	f.withFakeBazel(t, announceHome)
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
	for _, tc := range []struct {
		name      string
		goOnPath  bool
		mode      string
		announce  string
		wantBazel string
		wantMake  string
	}{
		{name: "go on the pinned PATH reads the cache", goOnPath: true, wantBazel: prePushBazelCacheArgs},
		{name: "no go on the pinned PATH falls back to go test", wantMake: prePushMakeArgs},
		{name: "remote execution needs no local go", announce: announceHome, wantBazel: prePushBazelRBEArgs},
		{name: "explicit cache still runs bazel", mode: "cache", wantBazel: prePushBazelCacheArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrePushFixture(t)
			announce := tc.announce
			if announce == "" {
				announce = announceNone
			}
			bazelRecord, _ := f.withFakeBazel(t, announce)
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
