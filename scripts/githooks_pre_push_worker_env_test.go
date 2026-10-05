package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Remote execution from pre-push sends //platforms:rbe_worker's worker-env
// pin, and rbe-west's oss schedulers match it exactly against what live
// workers advertise: the pin on origin's main, nothing else. A branch whose
// committed pin differs (it predates a re-pin, or moves the pin itself)
// would queue every action forever while the pool scaler starts Blacksmith
// VMs that cannot take them, and so would main's own pin while it has an
// open drift issue. push-suite.sh therefore compares the checkout's pin with
// a freshly fetched origin/main's (tools/rbe/worker-env-drift pin), asks
// worker-env-drift preflight about drift issues (best effort, bounded), and
// refuses remote execution on either: auto falls back to the non-remote
// mode, an explicit GC_PREPUSH_SUITE=rbe fails the push with guidance.

const (
	prePushPinMain  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	prePushPinOther = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	// What origin's URL says; insteadOf maps it to the fixture's bare repo,
	// so the hook fetches locally and still derives the GitHub repository
	// whose drift issues it asks gh about.
	prePushOriginURL  = "https://github.com/gastownhall/gascity.git"
	prePushOriginRepo = "gastownhall/gascity"
)

// prePushWorkerEnv is the fixture's worker-env world: origin's main (a bare
// repo fed from a scratch clone) and a gh that answers drift issues.
type prePushWorkerEnv struct {
	upstream string // scratch clone that commits and pushes origin's main
	bare     string // origin itself
	ghLog    string
	issues   string
}

// withWorkerEnv (withFakeBazel's) gives the fixture a checkout pinning prePushPinMain, the
// worker-env-drift tool, an origin whose main pins prePushPinMain and a
// stub gh with no open drift issues.
func (f *prePushFixture) withWorkerEnv(t *testing.T) *prePushWorkerEnv {
	t.Helper()
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, rbeWorkerEnvDrift))
	if err != nil {
		t.Fatalf("read %s: %v", rbeWorkerEnvDrift, err)
	}
	if err := os.MkdirAll(filepath.Join(f.repo, "tools/rbe"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(f.repo, rbeWorkerEnvDrift), string(body))
	f.setBranchPin(t, prePushPinMain)

	dir := t.TempDir()
	w := &prePushWorkerEnv{
		upstream: filepath.Join(dir, "upstream"),
		bare:     filepath.Join(dir, "origin.git"),
		ghLog:    filepath.Join(dir, "gh.log"),
		issues:   filepath.Join(dir, "issues.json"),
	}
	f.gitIn(t, "", "init", "-q", "--bare", w.bare)
	f.gitIn(t, "", "init", "-q", "-b", "main", w.upstream)
	f.gitIn(t, w.upstream, "remote", "add", "origin", w.bare)
	f.setMainPin(t, w, prePushPinMain)

	f.git(t, "remote", "add", "origin", prePushOriginURL)
	f.git(t, "config", "url."+w.bare+".insteadOf", prePushOriginURL)

	writeExecutable(t, filepath.Join(f.binDir, "gh"), ghStub)
	w.setIssues(t)
	f.env = append(f.env, "GH_LOG="+w.ghLog, "GH_ISSUES="+w.issues)
	return w
}

func (f *prePushFixture) gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testCommand("git", args...)
	cmd.Dir = dir
	cmd.Env = f.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setBranchPin writes the checkout's platforms/BUILD.bazel: the suite reads
// the working tree it is about to test.
func (f *prePushFixture) setBranchPin(t *testing.T, pin string) {
	t.Helper()
	writeDriftFile(t, filepath.Join(f.repo, "platforms/BUILD.bazel"), buildWithPin(pin))
}

// setMainPin commits pin to origin's main (a re-pin landing upstream). The
// fixture's own origin/main is not updated: the hook has to fetch it.
func (f *prePushFixture) setMainPin(t *testing.T, w *prePushWorkerEnv, pin string) {
	t.Helper()
	writeDriftFile(t, filepath.Join(w.upstream, "platforms/BUILD.bazel"), buildWithPin(pin))
	f.gitIn(t, w.upstream, "add", "-A")
	f.gitIn(t, w.upstream, "-c", "user.email=up@example.com", "-c", "user.name=up", "commit", "-q", "--no-verify", "-m", "pin "+pin)
	f.gitIn(t, w.upstream, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/main")
}

// unreachableOrigin points origin's GitHub URL at a repository that does not
// exist, so the hook's fetch fails without touching the network.
func (f *prePushFixture) unreachableOrigin(t *testing.T, w *prePushWorkerEnv) {
	t.Helper()
	f.git(t, "config", "--unset", "url."+w.bare+".insteadOf")
	f.git(t, "config", "url."+filepath.Join(t.TempDir(), "gone.git")+".insteadOf", prePushOriginURL)
}

// setIssues sets the open drift issues: title, url pairs.
func (w *prePushWorkerEnv) setIssues(t *testing.T, titleURL ...string) {
	t.Helper()
	var items []string
	for i := 0; i+1 < len(titleURL); i += 2 {
		items = append(items, `{"title":"`+titleURL[i]+`","url":"`+titleURL[i+1]+`"}`)
	}
	writeDriftFile(t, w.issues, "["+strings.Join(items, ",")+"]\n")
}

func TestPrePushSuiteWorkerEnvGuard(t *testing.T) {
	const issueURL = "https://github.com/gastownhall/gascity/issues/9001"
	for _, tc := range []struct {
		name      string
		mode      string
		setup     func(t *testing.T, f *prePushFixture, w *prePushWorkerEnv)
		wantCode  int
		wantBazel string
		wantMake  string
		wantOut   []string
		wantGH    bool
	}{
		{
			name:      "current pin executes remotely",
			wantBazel: prePushBazelRBEArgs, wantGH: true,
		},
		{
			name: "branch predating a re-pin reads the cache",
			setup: func(t *testing.T, f *prePushFixture, w *prePushWorkerEnv) {
				f.setMainPin(t, w, prePushPinOther)
			},
			wantBazel: prePushBazelCacheArgs,
			wantOut:   []string{prePushPinMain, prePushPinOther, "Rebase onto main"},
		},
		{
			name: "branch moving the pin reads the cache",
			setup: func(t *testing.T, f *prePushFixture, _ *prePushWorkerEnv) {
				f.setBranchPin(t, prePushPinOther)
			},
			wantBazel: prePushBazelCacheArgs,
			wantOut:   []string{prePushPinMain, prePushPinOther, "after merge"},
		},
		{
			name: "stale pin without go on the pinned PATH runs go test",
			setup: func(t *testing.T, f *prePushFixture, w *prePushWorkerEnv) {
				f.setMainPin(t, w, prePushPinOther)
				rc := "test --test_env=PATH=" + filepath.Join(t.TempDir(), "empty") + "\n"
				writeDriftFile(t, filepath.Join(f.repo, ".bazelrc"), rc)
			},
			wantMake: prePushMakeArgs,
			wantOut:  []string{prePushPinOther},
		},
		{
			name: "explicit rbe on a stale pin fails with guidance",
			mode: "rbe",
			setup: func(t *testing.T, f *prePushFixture, w *prePushWorkerEnv) {
				f.setMainPin(t, w, prePushPinOther)
			},
			wantCode: 2,
			wantOut:  []string{prePushPinMain, prePushPinOther, "Rebase onto main", "GC_PREPUSH_SUITE=cache"},
		},
		{
			name: "open drift issue for the current pin reads the cache",
			setup: func(t *testing.T, _ *prePushFixture, w *prePushWorkerEnv) {
				w.setIssues(t, driftTitle(prePushPinMain), issueURL)
			},
			wantBazel: prePushBazelCacheArgs, wantGH: true,
			wantOut: []string{issueURL},
		},
		{
			name: "explicit rbe with an open drift issue fails",
			mode: "rbe",
			setup: func(t *testing.T, _ *prePushFixture, w *prePushWorkerEnv) {
				w.setIssues(t, driftTitle(prePushPinMain), issueURL)
			},
			wantCode: 2, wantGH: true,
			wantOut: []string{issueURL, "GC_PREPUSH_SUITE=cache"},
		},
		{
			name: "another pin's drift issue does not block",
			setup: func(t *testing.T, _ *prePushFixture, w *prePushWorkerEnv) {
				w.setIssues(t, driftTitle(prePushPinOther), issueURL)
			},
			wantBazel: prePushBazelRBEArgs, wantGH: true,
		},
		{
			name: "gh failure is best effort",
			setup: func(_ *testing.T, f *prePushFixture, _ *prePushWorkerEnv) {
				f.env = append(f.env, "GH_FAIL=1")
			},
			wantBazel: prePushBazelRBEArgs, wantGH: true,
			wantOut: []string{"could not check"},
		},
		{
			name: "origin outside GitHub skips the drift issue check",
			setup: func(t *testing.T, f *prePushFixture, w *prePushWorkerEnv) {
				f.git(t, "remote", "set-url", "origin", w.bare)
			},
			wantBazel: prePushBazelRBEArgs,
		},
		{
			name: "unreachable origin uses the last fetched main",
			setup: func(t *testing.T, f *prePushFixture, w *prePushWorkerEnv) {
				f.git(t, "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/origin/main")
				f.unreachableOrigin(t, w)
			},
			wantBazel: prePushBazelRBEArgs, wantGH: true,
			wantOut: []string{"could not fetch"},
		},
		{
			name: "unreachable origin never fetched reads the cache",
			setup: func(t *testing.T, f *prePushFixture, w *prePushWorkerEnv) {
				f.unreachableOrigin(t, w)
			},
			wantBazel: prePushBazelCacheArgs,
			wantOut:   []string{"origin/main"},
		},
		{
			name: "GC_PREPUSH_MAIN_REMOTE names the remote carrying main",
			setup: func(t *testing.T, f *prePushFixture, w *prePushWorkerEnv) {
				f.setMainPin(t, w, prePushPinOther)
				f.git(t, "remote", "rename", "origin", "upstream")
				f.env = append(f.env, "GC_PREPUSH_MAIN_REMOTE=upstream")
			},
			wantBazel: prePushBazelCacheArgs,
			wantOut:   []string{"upstream/main", prePushPinOther},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrePushFixture(t)
			runs, _ := f.withFakeBazel(t, announceHome)
			w := f.workerEnv
			if tc.setup != nil {
				tc.setup(t, f, w)
			}
			f.env = append(f.env, "GC_PREPUSH_SUITE="+tc.mode)

			code, out := f.run(t, f.pushRefLine())
			if code != tc.wantCode {
				t.Fatalf("pre-push exit = %d, want %d\n%s", code, tc.wantCode, out)
			}
			if got := strings.TrimSpace(f.read(t, runs)); got != tc.wantBazel {
				t.Errorf("bazel ran %q, want %q\n%s", got, tc.wantBazel, out)
			}
			if got := strings.TrimSpace(f.read(t, f.makeRuns)); got != tc.wantMake {
				t.Errorf("make ran %q, want %q\n%s", got, tc.wantMake, out)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			ghLog := f.read(t, w.ghLog)
			if tc.wantGH != (ghLog != "") {
				t.Errorf("gh calls = %q, want calls: %v\n%s", ghLog, tc.wantGH, out)
			}
			if tc.wantGH && !strings.Contains(ghLog, "issue list -R "+prePushOriginRepo+" --label "+rbeWorkerEnvLabel) {
				t.Errorf("gh did not list %s's drift issues: %q", prePushOriginRepo, ghLog)
			}
		})
	}
}
