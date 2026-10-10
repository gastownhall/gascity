package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// rbe-worker shim tests (rbe-worker-repo-design.md §5.2, §9 S3a). The shim
// (.github/actions/rbe-worker/{action.yml,fetch.sh,pin}) fetches
// gastownhall/rbe-worker anonymously, verifies the pinned commit is in that
// repository's main history, and checks it out. ./pin carries RW0, the
// first commit on gastownhall/rbe-worker's main (TESTING.md "Bumping the
// rbe-worker pin"); these tests pin fetch.sh's invariants and prove its
// refusal logic dynamically, against a local file:// upstream.
const (
	rbeWorkerActionDir   = ".github/actions/rbe-worker"
	rbeWorkerFetchScript = rbeWorkerActionDir + "/fetch.sh"
	rbeWorkerPinFile     = rbeWorkerActionDir + "/pin"
	rbeWorkerWorkflow    = ".github/workflows/rbe-worker-pool.yml"
)

var fortyHexLine = regexp.MustCompile(`(?m)^[0-9a-f]{40}$`)

const allZeroSHA = "0000000000000000000000000000000000000000"

// TestRBEWorkerPinFormat: fetch.sh requires exactly one 40-hex line in the
// pin file (comment lines are fine; §5.2 "Pin files, each with exactly one
// 40-hex line"), and it must not be the all-zero placeholder: a real pin is
// required before this step, or H5, can mean anything.
func TestRBEWorkerPinFormat(t *testing.T) {
	root := repoRoot(t)
	content := readFile(t, root, rbeWorkerPinFile)
	got := fortyHexLine.FindAllString(content, -1)
	if len(got) != 1 {
		t.Fatalf("%s: found %d lines matching ^[0-9a-f]{40}$, want exactly 1:\n%s", rbeWorkerPinFile, len(got), content)
	}
	if got[0] == allZeroSHA {
		t.Fatalf("%s: pin is the all-zero placeholder; set it to a real gastownhall/rbe-worker commit", rbeWorkerPinFile)
	}
}

// codeLines strips full-line comments (including the shebang) and blank
// lines, so the invariant checks below scan fetch.sh's logic, not its prose:
// the comments at the top of the real file describe several of the things
// this test refuses, in words.
func codeLines(script string) string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestRBEWorkerFetchScriptInvariants pins the properties S3a's go/no-go and
// §6 (trust boundary) depend on: a fixed URL (no caller, pin file or input
// can redirect the fetch to another repository), refs/heads/main only (no
// fork-network commit is ever fetched), no secret or token reaches this
// step, ancestry is checked before any checkout, the script never prints
// the scaler's isolation-phase marker, and it stays bash-3.2-safe (no
// mapfile, no external timeout command), because gascity's script tests
// also run this on macOS's /bin/bash.
func TestRBEWorkerFetchScriptInvariants(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, rbeWorkerFetchScript)
	code := codeLines(script)

	if !strings.Contains(code, `url=https://github.com/gastownhall/rbe-worker.git`) {
		t.Errorf("%s: the repository URL is not the fixed literal gastownhall/rbe-worker.git", rbeWorkerFetchScript)
	}
	if n := strings.Count(code, "url="); n != 1 {
		t.Errorf("%s: %d assignments to url=, want exactly 1 (a fixed literal nothing can override)", rbeWorkerFetchScript, n)
	}

	if !strings.Contains(code, "+refs/heads/main:refs/remotes/origin/main") {
		t.Errorf("%s: does not fetch refs/heads/main into refs/remotes/origin/main", rbeWorkerFetchScript)
	}
	if strings.Contains(code, "refs/heads/*") || strings.Contains(code, "refs/*") || strings.Contains(code, "--tags") {
		t.Errorf("%s: fetches more than refs/heads/main", rbeWorkerFetchScript)
	}

	if strings.Contains(code, "secrets.") {
		t.Errorf("%s: references secrets.*; this step must hold no secret", rbeWorkerFetchScript)
	}
	if regexp.MustCompile(`(?i)\btoken\b`).MatchString(code) {
		t.Errorf("%s: mentions a token; the fetch must be anonymous", rbeWorkerFetchScript)
	}

	ancestry := strings.Index(code, "merge-base --is-ancestor")
	checkout := strings.Index(code, "checkout -q --detach")
	if ancestry < 0 {
		t.Fatalf("%s: no ancestry check (merge-base --is-ancestor)", rbeWorkerFetchScript)
	}
	if checkout < 0 {
		t.Fatalf("%s: no detached checkout", rbeWorkerFetchScript)
	}
	if ancestry > checkout {
		t.Errorf("%s: ancestry is checked after the checkout, not before", rbeWorkerFetchScript)
	}

	if strings.Contains(code, "isolation:") {
		t.Errorf("%s: must never print the scaler's isolation-phase marker 'isolation:' (R7)", rbeWorkerFetchScript)
	}

	if strings.Contains(code, "mapfile") {
		t.Errorf("%s: uses mapfile, which macOS's bash 3.2 lacks", rbeWorkerFetchScript)
	}
	if regexp.MustCompile(`(?m)(^|[;&|]\s*)timeout\s`).MatchString(code) {
		t.Errorf("%s: uses the external timeout command, which macOS's bash 3.2 environment may lack", rbeWorkerFetchScript)
	}
}

// localUpstreamFixture is the bash script that builds the dynamic refusal
// test's local file:// upstream: a "main" branch with one worker commit, and
// an "off-main" branch with a second commit unreachable from main. It writes
// each branch's sha to a file beside the upstream directory, so the Go test
// never shells out to git itself (runRBEScript is the one subprocess call
// site this file uses, same as every other call below: §8, the census).
const localUpstreamFixture = `#!/usr/bin/env bash
set -euo pipefail
dir=${1:?usage: fixture.sh UPSTREAM_DIR}
git -C "$dir" init -q -b main
git -C "$dir" config user.email t@example.invalid
git -C "$dir" config user.name "rbe-worker shim test"
mkdir -p "$dir/worker"
printf '#!/bin/sh\necho worker\n' >"$dir/worker/blacksmith-worker.sh"
chmod +x "$dir/worker/blacksmith-worker.sh"
git -C "$dir" add -A
git -C "$dir" commit -q -m "main: worker/blacksmith-worker.sh"
git -C "$dir" rev-parse HEAD >"$dir/../main.sha"
git -C "$dir" checkout -q -b off-main
echo "# off-main only" >>"$dir/worker/blacksmith-worker.sh"
git -C "$dir" commit -q -am "off-main: a commit main never merges"
git -C "$dir" rev-parse HEAD >"$dir/../off-main.sha"
git -C "$dir" checkout -q main
`

// rbeWorkerFixture builds the local upstream once per subtest and returns
// the sha of its main commit and of a commit reachable only from a
// different branch.
func rbeWorkerFixture(t *testing.T) (dir, mainSHA, offMainSHA string) {
	t.Helper()
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture.sh")
	if err := os.WriteFile(fixture, []byte(localUpstreamFixture), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}
	upstream := filepath.Join(root, "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatalf("mkdir upstream: %v", err)
	}
	if _, stderr, err := runRBEScript(root, os.Environ(), fixture, upstream); err != nil {
		t.Fatalf("build local upstream fixture: %v\n%s", err, stderr)
	}
	mainSHA = strings.TrimSpace(readFile(t, root, "main.sha"))
	offMainSHA = strings.TrimSpace(readFile(t, root, "off-main.sha"))
	return upstream, mainSHA, offMainSHA
}

// rbeWorkerFetchCopy copies fetch.sh into its own directory with the fixed
// URL replaced by a local file:// upstream (the one invariant this dynamic
// test must not exercise as written, since §6's whole point is that nothing
// can redirect the real fetch). It writes pinfile beside the copy, exactly
// as action.yml's $GITHUB_ACTION_PATH does in production.
func rbeWorkerFetchCopy(t *testing.T, upstream, pinfile, pinContent string) string {
	t.Helper()
	root := repoRoot(t)
	script := readFile(t, root, rbeWorkerFetchScript)
	want := `url=https://github.com/gastownhall/rbe-worker.git`
	if !strings.Contains(script, want) {
		t.Fatalf("%s: missing %q; cannot localize for the dynamic test", rbeWorkerFetchScript, want)
	}
	script = strings.Replace(script, want, "url=file://"+upstream, 1)
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "fetch.sh")
	if err := os.WriteFile(copyPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fetch.sh copy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, pinfile), []byte(pinContent+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", pinfile, err)
	}
	return copyPath
}

func rbeWorkerRunFetch(t *testing.T, script string, args ...string) (stdout, stderr string, outputs map[string]string, err error) {
	t.Helper()
	runnerTemp := t.TempDir()
	outFile := filepath.Join(t.TempDir(), "output")
	summaryFile := filepath.Join(t.TempDir(), "summary")
	if err := os.WriteFile(outFile, nil, 0o644); err != nil {
		t.Fatalf("create GITHUB_OUTPUT: %v", err)
	}
	if err := os.WriteFile(summaryFile, nil, 0o644); err != nil {
		t.Fatalf("create GITHUB_STEP_SUMMARY: %v", err)
	}
	env := append(os.Environ(),
		"RUNNER_TEMP="+runnerTemp,
		"GITHUB_OUTPUT="+outFile,
		"GITHUB_STEP_SUMMARY="+summaryFile,
		"GITHUB_WORKSPACE="+runnerTemp,
		// Every caller of rbeWorkerRunFetch exercises rbeWorkerFetchCopy's
		// local file:// upstream (§6: nothing in a real run can redirect the
		// fetch off https); this is fetch.sh's one test-only escape hatch
		// for that, gated so a real CI invocation never sets it.
		"RBE_WORKER_TEST_ALLOW_FILE=1",
	)
	stdout, stderr, err = runRBEScript(filepath.Dir(script), env, script, args...)
	outputs = map[string]string{}
	if raw, rerr := os.ReadFile(outFile); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				outputs[k] = v
			}
		}
	}
	return stdout, stderr, outputs, err
}

// TestRBEWorkerFetchAcceptsThePinnedMainCommit: the happy path against a
// local file:// upstream.
func TestRBEWorkerFetchAcceptsThePinnedMainCommit(t *testing.T) {
	upstream, mainSHA, _ := rbeWorkerFixture(t)
	script := rbeWorkerFetchCopy(t, upstream, "pin", mainSHA)
	stdout, stderr, outputs, err := rbeWorkerRunFetch(t, script, "pin")
	if err != nil {
		t.Fatalf("fetch of the pinned main commit failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if outputs["sha"] != mainSHA {
		t.Errorf("sha output = %q, want %q", outputs["sha"], mainSHA)
	}
	if outputs["dir"] == "" || !strings.HasSuffix(outputs["dir"], "/worker") {
		t.Errorf("dir output = %q, want a path ending in /worker", outputs["dir"])
	}
}

// TestRBEWorkerFetchRefusesOffMainCommit: a commit that exists but is not an
// ancestor of main is refused (R2's impostor-commit guard), even though it
// is a real commit in the same repository fetch.sh pulled from.
func TestRBEWorkerFetchRefusesOffMainCommit(t *testing.T) {
	upstream, _, offMainSHA := rbeWorkerFixture(t)
	script := rbeWorkerFetchCopy(t, upstream, "pin", offMainSHA)
	stdout, _, outputs, err := rbeWorkerRunFetch(t, script, "pin")
	if err == nil {
		t.Fatalf("fetch of an off-main commit succeeded; it must be refused")
	}
	// fetch.sh's "::error ..." workflow-command lines go to stdout (echo,
	// no redirect), as GitHub's own annotations do; only its usage message
	// goes to stderr.
	if !strings.Contains(stdout, "is not in gastownhall/rbe-worker main's history; refused") {
		t.Errorf("stdout = %q, want the refusal message", stdout)
	}
	if outputs["sha"] != "" || outputs["dir"] != "" {
		t.Errorf("outputs = %v, want no dir/sha written for a refused commit", outputs)
	}
}

// TestRBEWorkerFetchMalformedPin: a pin file with no 40-hex line, or more
// than one, fails closed with exit 2 (a config error, not a fetch error).
func TestRBEWorkerFetchMalformedPin(t *testing.T) {
	upstream, mainSHA, _ := rbeWorkerFixture(t)
	for name, pin := range map[string]string{
		"empty":  "",
		"short":  "deadbeef",
		"double": mainSHA + "\n" + mainSHA,
	} {
		t.Run(name, func(t *testing.T) {
			script := rbeWorkerFetchCopy(t, upstream, "pin", pin)
			stdout, _, _, err := rbeWorkerRunFetch(t, script, "pin")
			if err == nil {
				t.Fatalf("malformed pin %q: fetch.sh exited 0, want 2", pin)
			}
			if !strings.Contains(stdout, "needs exactly one 40-hex line") {
				t.Errorf("stdout = %q, want the pin-format error", stdout)
			}
		})
	}
}

// TestRBEWorkerFetchVerifySkipsCheckout: `verify` confirms ancestry (fetching
// is still required, to check it) but never checks out and never writes
// dir/sha, so a client-side pin-client verify never produces a tree to run.
func TestRBEWorkerFetchVerifySkipsCheckout(t *testing.T) {
	upstream, mainSHA, _ := rbeWorkerFixture(t)
	script := rbeWorkerFetchCopy(t, upstream, "pin-client", mainSHA)
	stdout, stderr, outputs, err := rbeWorkerRunFetch(t, script, "verify", "pin-client")
	if err != nil {
		t.Fatalf("verify of the pinned main commit failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if len(outputs) != 0 {
		t.Errorf("verify wrote outputs %v, want none (no checkout)", outputs)
	}
	if !strings.Contains(stdout, "is on main") {
		t.Errorf("stdout = %q, want the verify confirmation", stdout)
	}
}

// TestRBEWorkerFetchFileSchemeRequiresTestFlag: every dynamic test in this
// file localizes fetch.sh to a file:// upstream via
// RBE_WORKER_TEST_ALLOW_FILE=1, fetch.sh's one test-only escape hatch. This
// test is the one that proves the hatch is load-bearing: with it unset (as
// every real invocation leaves it), the same file:// upstream that every
// other test above fetches successfully must instead be refused outright
// by git's own protocol.file.allow=never default, before fetch.sh's own
// ancestry check ever runs.
func TestRBEWorkerFetchFileSchemeRequiresTestFlag(t *testing.T) {
	upstream, mainSHA, _ := rbeWorkerFixture(t)
	script := rbeWorkerFetchCopy(t, upstream, "pin", mainSHA)
	runnerTemp := t.TempDir()
	outFile := filepath.Join(t.TempDir(), "output")
	summaryFile := filepath.Join(t.TempDir(), "summary")
	for _, p := range []string{outFile, summaryFile} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}
	// A no-op sleep stub: fetch.sh retries a failed fetch up to 3 times
	// with a growing backoff, and a protocol refusal fails every attempt
	// the same way; this keeps the test from waiting out that backoff.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write sleep stub: %v", err)
	}
	env := append(os.Environ(),
		"RUNNER_TEMP="+runnerTemp,
		"GITHUB_OUTPUT="+outFile,
		"GITHUB_STEP_SUMMARY="+summaryFile,
		"GITHUB_WORKSPACE="+runnerTemp,
		"PATH="+bin+":"+os.Getenv("PATH"),
		// Deliberately no RBE_WORKER_TEST_ALLOW_FILE=1 here.
	)
	stdout, stderr, outputs, err := rbeWorkerRunFetchEnv(t, script, env, "pin")
	if err == nil {
		t.Fatalf("fetch over file:// without RBE_WORKER_TEST_ALLOW_FILE succeeded; it must fail")
	}
	if !strings.Contains(stdout, "cannot fetch") {
		t.Errorf("stdout = %q, want fetch.sh's own fetch-failed error", stdout)
	}
	if !strings.Contains(stderr, "not allowed") {
		t.Errorf("stderr = %q, want git's protocol-refusal message", stderr)
	}
	if outputs["sha"] != "" || outputs["dir"] != "" {
		t.Errorf("outputs = %v, want none written for a fetch that never ran", outputs)
	}
}

// filterDriverUpstreamFixture builds a one-commit upstream whose
// .gitattributes marks every "*.sh" file for a smudge filter named "evil".
const filterDriverUpstreamFixture = `#!/usr/bin/env bash
set -euo pipefail
dir=${1:?usage: fixture.sh UPSTREAM_DIR}
git -C "$dir" init -q -b main
git -C "$dir" config user.email t@example.invalid
git -C "$dir" config user.name "rbe-worker shim test"
mkdir -p "$dir/worker"
printf '#!/bin/sh\necho worker\n' >"$dir/worker/blacksmith-worker.sh"
chmod +x "$dir/worker/blacksmith-worker.sh"
printf '*.sh filter=evil\n' >"$dir/.gitattributes"
git -C "$dir" add -A
git -C "$dir" commit -q -m "worker, with a *.sh smudge filter in .gitattributes"
git -C "$dir" rev-parse HEAD >"$dir/../main.sha"
`

// TestRBEWorkerFetchIgnoresAFilterDriverFromConfig: a filter driver named in
// gitconfig, matched by a .gitattributes rule the fetched tree itself
// carries, must never run. fetch.sh sets GIT_CONFIG_GLOBAL=/dev/null and
// GIT_CONFIG_NOSYSTEM=1 before its first git call, so a filter.* entry a
// compromised worker VM's own global config already carries (nothing to do
// with the fetched repository) is never consulted, however convincingly
// its .gitattributes names it; GIT_LFS_SKIP_SMUDGE=1 covers the LFS case
// the same way. This proves it dynamically: the filter would otherwise run
// on checkout and leave its mark.
func TestRBEWorkerFetchIgnoresAFilterDriverFromConfig(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture.sh")
	if err := os.WriteFile(fixture, []byte(filterDriverUpstreamFixture), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}
	upstream := filepath.Join(root, "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatalf("mkdir upstream: %v", err)
	}
	if _, stderr, err := runRBEScript(root, os.Environ(), fixture, upstream); err != nil {
		t.Fatalf("build filter-driver upstream fixture: %v\n%s", err, stderr)
	}
	mainSHA := strings.TrimSpace(readFile(t, root, "main.sha"))

	// This worker VM's own pre-existing global gitconfig (nothing to do
	// with the fetched repository), naming a filter driver that records
	// whether it ran and otherwise behaves like cat, so a run that does
	// honor it still checks out successfully (and so the test can tell
	// "ignored" apart from "fetch broke some other way").
	marker := filepath.Join(root, "evil-ran")
	filterScript := filepath.Join(root, "evil-filter.sh")
	filterBody := "#!/bin/sh\necho ran >>" + marker + "\ncat\n"
	if err := os.WriteFile(filterScript, []byte(filterBody), 0o755); err != nil {
		t.Fatalf("write filter script: %v", err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	gitconfig := "[filter \"evil\"]\n\tsmudge = " + filterScript + "\n\trequired = true\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatalf("write .gitconfig: %v", err)
	}

	script := rbeWorkerFetchCopy(t, upstream, "pin", mainSHA)
	runnerTemp := t.TempDir()
	outFile := filepath.Join(t.TempDir(), "output")
	summaryFile := filepath.Join(t.TempDir(), "summary")
	for _, p := range []string{outFile, summaryFile} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}
	env := append(os.Environ(),
		"RUNNER_TEMP="+runnerTemp,
		"GITHUB_OUTPUT="+outFile,
		"GITHUB_STEP_SUMMARY="+summaryFile,
		"GITHUB_WORKSPACE="+runnerTemp,
		"RBE_WORKER_TEST_ALLOW_FILE=1",
		// Go's os/exec: a duplicate key's last value wins, so this
		// overrides the real $HOME this test process inherited.
		"HOME="+home,
	)
	stdout, stderr, outputs, err := rbeWorkerRunFetchEnv(t, script, env, "pin")
	if err != nil {
		t.Fatalf("fetch of the pinned main commit failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if outputs["sha"] != mainSHA {
		t.Errorf("outputs[sha] = %q, want %q", outputs["sha"], mainSHA)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the filter driver ran (marker file exists); fetch.sh's GIT_CONFIG_GLOBAL=/dev/null must keep it from ever being consulted")
	}
}

// TestRBEWorkerFetchMalformedPinExitCode: a malformed pin is a config error
// (exit 2), distinct from a refused-but-well-formed sha (exit 1): the two
// must not collapse into one exit status a caller can't tell apart.
func TestRBEWorkerFetchMalformedPinExitCode(t *testing.T) {
	upstream, _, _ := rbeWorkerFixture(t)
	script := rbeWorkerFetchCopy(t, upstream, "pin", "deadbeef")
	_, _, _, err := rbeWorkerRunFetch(t, script, "pin")
	if got := exitCode(err); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
}

// refusalUpstreamFixture builds a local upstream exercising every shape the
// pin must refuse, besides the plain off-main branch rbeWorkerFixture
// already covers: a commit that was main's tip before a force-push
// (forced.sha, no longer reachable from main's current tip, but still
// physically present locally afterwards, since fetch.sh's local file://
// transport copies the whole object store, not just what main's tip
// reaches: cat-file sees it, only merge-base refuses it), a commit that
// only ever lived under refs/pull/ (pull.sha), an annotated tag object
// (tag.sha, type tag, not commit), and the root tree and one blob from
// main's own commit (tree.sha, blob.sha): real objects, never commits.
const refusalUpstreamFixture = `#!/usr/bin/env bash
set -euo pipefail
dir=${1:?usage: fixture.sh UPSTREAM_DIR}
git -C "$dir" init -q -b main
git -C "$dir" config user.email t@example.invalid
git -C "$dir" config user.name "rbe-worker shim test"
mkdir -p "$dir/worker"
printf '#!/bin/sh\necho worker\n' >"$dir/worker/blacksmith-worker.sh"
chmod +x "$dir/worker/blacksmith-worker.sh"
git -C "$dir" add -A
git -C "$dir" commit -q -m "root"
git -C "$dir" rev-parse HEAD >"$dir/../forced.sha"
git -C "$dir" rev-parse HEAD:worker >"$dir/../tree.sha"
git -C "$dir" rev-parse HEAD:worker/blacksmith-worker.sh >"$dir/../blob.sha"
git -C "$dir" tag -a -m v1 v1 HEAD
git -C "$dir" rev-parse v1 >"$dir/../tag.sha"
git -C "$dir" checkout -q -b pull-branch
echo "# pull only" >>"$dir/worker/blacksmith-worker.sh"
git -C "$dir" commit -q -am "pull: never under refs/heads"
git -C "$dir" rev-parse HEAD >"$dir/../pull.sha"
git -C "$dir" update-ref refs/pull/7/head HEAD
git -C "$dir" checkout -q main
git -C "$dir" checkout -q --orphan main-new
git -C "$dir" commit -q -m "force-pushed root"
git -C "$dir" rev-parse HEAD >"$dir/../main.sha"
git -C "$dir" branch -M main-new main
`

type refusalFixture struct {
	upstream                                              string
	mainSHA, forcedSHA, pullSHA, tagSHA, treeSHA, blobSHA string
}

func rbeWorkerRefusalFixture(t *testing.T) refusalFixture {
	t.Helper()
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture.sh")
	if err := os.WriteFile(fixture, []byte(refusalUpstreamFixture), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}
	upstream := filepath.Join(root, "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatalf("mkdir upstream: %v", err)
	}
	if _, stderr, err := runRBEScript(root, os.Environ(), fixture, upstream); err != nil {
		t.Fatalf("build refusal upstream fixture: %v\n%s", err, stderr)
	}
	return refusalFixture{
		upstream:  upstream,
		mainSHA:   strings.TrimSpace(readFile(t, root, "main.sha")),
		forcedSHA: strings.TrimSpace(readFile(t, root, "forced.sha")),
		pullSHA:   strings.TrimSpace(readFile(t, root, "pull.sha")),
		tagSHA:    strings.TrimSpace(readFile(t, root, "tag.sha")),
		treeSHA:   strings.TrimSpace(readFile(t, root, "tree.sha")),
		blobSHA:   strings.TrimSpace(readFile(t, root, "blob.sha")),
	}
}

// TestRBEWorkerFetchRefusalMatrix: every shape of "not a commit on
// rbe-worker main" the pin can name is refused, not just the plain
// off-main branch (TestRBEWorkerFetchRefusesOffMainCommit) covers: an
// unknown sha nothing produced, a force-pushed-away former main tip, a
// commit that only ever lived under refs/pull/, a tag object, a tree and a
// blob.
func TestRBEWorkerFetchRefusalMatrix(t *testing.T) {
	f := rbeWorkerRefusalFixture(t)
	cases := map[string]string{
		"unknown sha":           "abcdabcdabcdabcdabcdabcdabcdabcdabcdabcd",
		"force-pushed main tip": f.forcedSHA,
		"refs/pull/* commit":    f.pullSHA,
		"tag object":            f.tagSHA,
		"tree":                  f.treeSHA,
		"blob":                  f.blobSHA,
	}
	for name, sha := range cases {
		t.Run(name, func(t *testing.T) {
			script := rbeWorkerFetchCopy(t, f.upstream, "pin", sha)
			stdout, _, outputs, err := rbeWorkerRunFetch(t, script, "pin")
			if err == nil {
				t.Fatalf("fetch of %s (%s) succeeded; it must be refused", name, sha)
			}
			if !strings.Contains(stdout, "refused") {
				t.Errorf("stdout = %q, want a refusal message", stdout)
			}
			if outputs["sha"] != "" || outputs["dir"] != "" {
				t.Errorf("outputs = %v, want no dir/sha written for a refused commit", outputs)
			}
		})
	}
}

// TestRBEWorkerFetchAncestryCheckedAfterFetch: the force-pushed-away former
// main tip is a real commit, and fetch.sh's local file:// transport (same
// as any other local clone) copies the whole object store, so it ends up
// present, so only the merge-base ancestry check can be refusing it (R2's
// impostor-commit guard must not get to lean on "the object isn't even
// there": a worker whose RUNNER_TEMP is reused across steps, or an
// upstream pack that happens to carry extra objects, must still be
// refused on ancestry, not accidentally waved through because the
// existence check alone would have passed).
//
// fetch.sh always starts from an empty clone (rm -rf "$dir" first), so a
// real single fetch of refs/heads/main cannot by itself produce this
// state: this test uses a copy of fetch.sh with that one line removed, and
// pre-seeds "$dir" with both branches fetched, to exercise the check on
// its own terms.
func TestRBEWorkerFetchAncestryCheckedAfterFetch(t *testing.T) {
	upstream, _, offMainSHA := rbeWorkerFixture(t)
	runnerTemp := t.TempDir()
	clone := filepath.Join(runnerTemp, "rbe-worker-pin")

	seed := filepath.Join(t.TempDir(), "seed.sh")
	const seedScript = `#!/usr/bin/env bash
set -euo pipefail
dir=${1:?} upstream=${2:?}
git init -q "$dir"
git -C "$dir" fetch -q --no-tags "$upstream" "+refs/heads/*:refs/remotes/origin/*"
`
	if err := os.WriteFile(seed, []byte(seedScript), 0o755); err != nil {
		t.Fatalf("write seed.sh: %v", err)
	}
	if _, stderr, err := runRBEScript(runnerTemp, os.Environ(), seed, clone, upstream); err != nil {
		t.Fatalf("seed the clone with both branches: %v\n%s", err, stderr)
	}

	// Confirm the object is present before fetch.sh ever runs: decouples
	// "it's fetched" from fetch.sh's own behavior.
	catFile := filepath.Join(t.TempDir(), "cat-file.sh")
	if err := os.WriteFile(catFile, []byte("#!/usr/bin/env bash\nset -euo pipefail\ngit -C \"$1\" cat-file -e \"$2^{commit}\"\n"), 0o755); err != nil {
		t.Fatalf("write cat-file.sh: %v", err)
	}
	if _, stderr, err := runRBEScript(clone, os.Environ(), catFile, clone, offMainSHA); err != nil {
		t.Fatalf("off-main commit %s is not present in the pre-seeded clone: %v\n%s", offMainSHA, err, stderr)
	}

	script := rbeWorkerFetchCopy(t, upstream, "pin", offMainSHA)
	content := readFile(t, filepath.Dir(script), "fetch.sh")
	noWipe := strings.Replace(content, `rm -rf "$dir"`+"\n", "", 1)
	if noWipe == content {
		t.Fatalf("fetch.sh: no rm -rf \"$dir\" line to remove for this test")
	}
	if err := os.WriteFile(script, []byte(noWipe), 0o755); err != nil {
		t.Fatalf("rewrite fetch.sh copy: %v", err)
	}

	outFile := filepath.Join(t.TempDir(), "output")
	summaryFile := filepath.Join(t.TempDir(), "summary")
	for _, p := range []string{outFile, summaryFile} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}
	env := append(os.Environ(),
		"RUNNER_TEMP="+runnerTemp,
		"GITHUB_OUTPUT="+outFile,
		"GITHUB_STEP_SUMMARY="+summaryFile,
		"GITHUB_WORKSPACE="+runnerTemp,
		"RBE_WORKER_TEST_ALLOW_FILE=1",
	)
	stdout, _, _, err := rbeWorkerRunFetchEnv(t, script, env, "pin")
	if err == nil {
		t.Fatalf("fetch of the pre-seeded off-main commit succeeded; it must be refused")
	}
	if !strings.Contains(stdout, "refused") {
		t.Errorf("stdout = %q, want a refusal message", stdout)
	}
}

// rbeWorkerRunFetchEnv is rbeWorkerRunFetch with the caller supplying the
// full environment (so it can pre-set RUNNER_TEMP, GITHUB_OUTPUT etc. to
// paths it already seeded).
func rbeWorkerRunFetchEnv(t *testing.T, script string, env []string, args ...string) (stdout, stderr string, outputs map[string]string, err error) {
	t.Helper()
	stdout, stderr, err = runRBEScript(filepath.Dir(script), env, script, args...)
	outputs = map[string]string{}
	var outFile string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "GITHUB_OUTPUT" {
			outFile = v
		}
	}
	if raw, rerr := os.ReadFile(outFile); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				outputs[k] = v
			}
		}
	}
	return stdout, stderr, outputs, err
}

// symlinkUpstreamFixture builds a one-commit local upstream whose worker
// tree is a symlink (variant "worker"), or whose
// worker/blacksmith-worker.sh is a symlink to somewhere outside it
// (variant "script"): a checkout would follow either, so fetch.sh must
// refuse them by their git ls-tree mode/type, before ever checking out.
const symlinkUpstreamFixture = `#!/usr/bin/env bash
set -euo pipefail
dir=${1:?usage: fixture.sh UPSTREAM_DIR VARIANT}
variant=${2:?usage: fixture.sh UPSTREAM_DIR VARIANT}
git -C "$dir" init -q -b main
git -C "$dir" config user.email t@example.invalid
git -C "$dir" config user.name "rbe-worker shim test"
case "$variant" in
worker)
	ln -s /tmp "$dir/worker"
	;;
script)
	mkdir -p "$dir/worker"
	ln -s /etc/passwd "$dir/worker/blacksmith-worker.sh"
	;;
nested)
	mkdir -p "$dir/worker"
	printf '#!/bin/sh\necho worker\n' >"$dir/worker/blacksmith-worker.sh"
	chmod +x "$dir/worker/blacksmith-worker.sh"
	ln -s /outside "$dir/worker/rbe-action-launch"
	;;
esac
git -C "$dir" add -A
git -C "$dir" commit -q -m "$variant: a symlink in place of a real worker/"
git -C "$dir" rev-parse HEAD >"$dir/../main.sha"
`

// TestRBEWorkerFetchRefusesSymlinks: a worker that is itself a symlink, a
// worker/blacksmith-worker.sh that is a symlink instead of a regular
// executable, or any other symlink anywhere under worker/ (nested: `sudo
// install`, the worker's own setup, follows a symlink wherever it sits,
// not just at these two well-known paths), is refused on its git ls-tree
// mode/type, never checked out (fix 9, tightened by the S3 re-review).
func TestRBEWorkerFetchRefusesSymlinks(t *testing.T) {
	for _, variant := range []string{"worker", "script", "nested"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			fixture := filepath.Join(root, "fixture.sh")
			if err := os.WriteFile(fixture, []byte(symlinkUpstreamFixture), 0o755); err != nil {
				t.Fatalf("write fixture script: %v", err)
			}
			upstream := filepath.Join(root, "upstream")
			if err := os.MkdirAll(upstream, 0o755); err != nil {
				t.Fatalf("mkdir upstream: %v", err)
			}
			if _, stderr, err := runRBEScript(root, os.Environ(), fixture, upstream, variant); err != nil {
				t.Fatalf("build symlink upstream fixture: %v\n%s", err, stderr)
			}
			mainSHA := strings.TrimSpace(readFile(t, root, "main.sha"))
			script := rbeWorkerFetchCopy(t, upstream, "pin", mainSHA)
			stdout, _, outputs, err := rbeWorkerRunFetch(t, script, "pin")
			if err == nil {
				t.Fatalf("fetch of a %s symlink succeeded; it must be refused", variant)
			}
			if !strings.Contains(stdout, "symlink") {
				t.Errorf("stdout = %q, want fetch.sh's symlink refusal message", stdout)
			}
			if outputs["sha"] != "" || outputs["dir"] != "" {
				t.Errorf("outputs = %v, want no dir/sha written for a refused commit", outputs)
			}
		})
	}
}

// rbeWorkerFetchJobs: each workflow that runs rbe-worker code, and the jobs
// that fetch it (one fetch step each).
var rbeWorkerFetchJobs = map[string][]string{
	rbeWorkerWorkflow:      {"await-drift", "report-drift", "worker"},
	rbeForkPoolWorkflow:    {"await-drift", "report-drift", "worker"},
	rbeWorkerEnvCanary:     {"measure", "report"},
	bazelMultiLaneWorkflow: {"worker-host"},
}

// TestRBEWorkerAlwaysPinned: the worker code is gastownhall/rbe-worker at
// ./pin and nothing else (S5 deleted gascity's in-tree copy, and with it the
// S3-S5 cutover switch): every fetch step takes no input and no env (a
// SOURCE env would reach fetch.sh as directly as an input), is id
// rbe-worker (what every run step's RBE_WORKER_DIR reads), no workflow
// reads an RBE_WORKER_SOURCE_* variable, the action declares no source
// input and fetch.sh has no branch that returns this checkout's tools/rbe.
func TestRBEWorkerAlwaysPinned(t *testing.T) {
	root := repoRoot(t)
	for path, wantJobs := range rbeWorkerFetchJobs {
		t.Run(filepath.Base(path), func(t *testing.T) {
			text := readFile(t, root, path)
			if strings.Contains(text, "RBE_WORKER_SOURCE") {
				t.Errorf("%s reads an RBE_WORKER_SOURCE_* variable; the worker code is always the pin", path)
			}
			var wf struct {
				Jobs map[string]struct {
					Steps []struct {
						ID   string            `yaml:"id"`
						Uses string            `yaml:"uses"`
						With map[string]any    `yaml:"with"`
						Env  map[string]string `yaml:"env"`
					} `yaml:"steps"`
				} `yaml:"jobs"`
			}
			if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			var found []string
			for id, job := range wf.Jobs {
				for _, step := range job.Steps {
					if step.Uses != "./"+rbeWorkerActionDir {
						continue
					}
					found = append(found, id)
					if len(step.With) != 0 {
						t.Errorf("%s job %s: fetch step with %v, want no inputs", path, id, step.With)
					}
					if len(step.Env) != 0 {
						t.Errorf("%s job %s: fetch step env %v, want none", path, id, step.Env)
					}
					if step.ID != "rbe-worker" {
						t.Errorf("%s job %s: fetch step id %q, want rbe-worker", path, id, step.ID)
					}
				}
			}
			sort.Strings(found)
			if strings.Join(found, ",") != strings.Join(wantJobs, ",") {
				t.Errorf("%s: fetch steps in jobs %v, want one each in %v", path, found, wantJobs)
			}
		})
	}
	action := readFile(t, root, rbeWorkerActionDir+"/action.yml")
	if strings.Contains(action, "inputs:") || strings.Contains(action, "SOURCE") {
		t.Errorf("%s/action.yml declares an input or passes SOURCE; it fetches ./pin and nothing else:\n%s", rbeWorkerActionDir, action)
	}
	code := codeLines(readFile(t, root, rbeWorkerFetchScript))
	for _, banned := range []string{`\bSOURCE\b`, `in-tree`, `GITHUB_WORKSPACE`, `tools/rbe`} {
		if regexp.MustCompile(banned).MatchString(code) {
			t.Errorf("%s matches %q outside a comment; the in-tree cutover branch is gone", rbeWorkerFetchScript, banned)
		}
	}
}

// TestRBEWorkerPoolWorkflowIsolatesActions: the OSS pool's worker step runs
// the pinned rbe-worker with action isolation on by default (the repository
// variable RBE_ACTION_ISOLATION=0 is the rollback), the zstd switches off by
// default, the OSS tier's defaults and its own certificate, from a checkout
// that leaves no GITHUB_TOKEN in .git/config. (What isolation does is
// rbe-worker's to test.)
func TestRBEWorkerPoolWorkflowIsolatesActions(t *testing.T) {
	root := repoRoot(t)
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				With map[string]any    `yaml:"with"`
				Env  map[string]string `yaml:"env"`
				Run  string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, rbeWorkerWorkflow)), &wf); err != nil {
		t.Fatalf("parse %s: %v", rbeWorkerWorkflow, err)
	}
	job, ok := wf.Jobs["worker"]
	if !ok {
		t.Fatalf("%s: no worker job", rbeWorkerWorkflow)
	}
	var checkout, worker bool
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkout = true
			// Remote actions run on this runner: no GITHUB_TOKEN in .git/config.
			if v, _ := step.With["persist-credentials"].(bool); v || step.With["persist-credentials"] == nil {
				t.Errorf("checkout must set persist-credentials: false, got %v", step.With["persist-credentials"])
			}
		}
		// The worker-env measure step runs the script too, without a worker.
		if strings.Contains(step.Run, "blacksmith-worker.sh") && step.Env["WORKER_MODE"] != "measure" {
			worker = true
			// The fetched tree's script, never a path in this checkout.
			if got, want := strings.TrimSpace(step.Run), `"$RBE_WORKER_DIR/blacksmith-worker.sh"`; got != want {
				t.Errorf("worker step run = %q, want %q", got, want)
			}
			if got, want := step.Env["WORKER_MODE"], "pool"; got != want {
				t.Errorf("worker step WORKER_MODE = %q, want %q", got, want)
			}
			if got, want := step.Env["RBE_WORKER_DIR"], "${{ steps.rbe-worker.outputs.dir }}"; got != want {
				t.Errorf("worker step RBE_WORKER_DIR = %q, want %q", got, want)
			}
			if got, want := step.Env["RBE_WORKER_REVISION"], "${{ steps.rbe-worker.outputs.sha }}"; got != want {
				t.Errorf("worker step RBE_WORKER_REVISION = %q, want %q", got, want)
			}
			// Default on; the repository variable RBE_ACTION_ISOLATION=0 is the
			// rollback without a code change.
			if got, want := step.Env["RBE_ACTION_ISOLATION"], "${{ vars.RBE_ACTION_ISOLATION || '1' }}"; got != want {
				t.Errorf("worker step RBE_ACTION_ISOLATION = %q, want %q", got, want)
			}
			// canary: one run in RBE_ACTION_CANARY_EVERY tries isolation.
			if got, want := step.Env["RBE_ACTION_CANARY_EVERY"], "${{ vars.RBE_ACTION_CANARY_EVERY || '4' }}"; got != want {
				t.Errorf("worker step RBE_ACTION_CANARY_EVERY = %q, want %q", got, want)
			}
			// zstd fetches, off unless the repository variable says 1: merging
			// changes nothing, and rollback is the variable.
			if got, want := step.Env["RBE_WIRE_ZSTD"], "${{ vars.RBE_WIRE_ZSTD || '0' }}"; got != want {
				t.Errorf("worker step RBE_WIRE_ZSTD = %q, want %q", got, want)
			}
			// The dedicated zread host; the worker refuses zstd without it.
			if got, want := step.Env["RBE_WIRE_ZSTD_READ_URL"], "${{ vars.RBE_WIRE_ZSTD_READ_URL || '' }}"; got != want {
				t.Errorf("worker step RBE_WIRE_ZSTD_READ_URL = %q, want %q", got, want)
			}
			// The OSS pool keeps the script's defaults (tier oss, :443) and its
			// own certificate; the fork tier is rbe-fork-pool.yml's alone.
			for _, k := range []string{"WORKER_TIER", "RBE_WEST_PORT"} {
				if v, ok := step.Env[k]; ok {
					t.Errorf("worker step sets %s=%q; the OSS pool runs the defaults", k, v)
				}
			}
			if got, want := step.Env["RBE_WORKER_TLS_KEY"], "${{ secrets.RBE_WORKER_TLS_KEY }}"; got != want {
				t.Errorf("worker step RBE_WORKER_TLS_KEY = %q, want %q", got, want)
			}
		}
	}
	if !checkout || !worker {
		t.Fatalf("%s: checkout step found %v, worker step found %v", rbeWorkerWorkflow, checkout, worker)
	}
}
