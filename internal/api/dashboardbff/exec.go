package dashboardbff

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Output caps and concurrency, mirroring the BFF's exec-core.ts contract.
const (
	maxBytes        = 100 << 10 // default per-call stdout cap (100 KB)
	maxRunDiffBytes = 512 << 10 // larger cap for run diffs (512 KB)
	maxConcurrent   = 4         // simultaneous subprocesses

	gitLogTimeout   = 10 * time.Second
	runGitTimeout   = 5 * time.Second
	bdDoctorTimeout = 15 * time.Second
	gitLogRecentN   = "200"
)

// execErrKind classifies why a sandboxed subprocess failed.
type execErrKind int

const (
	execErrValidation execErrKind = iota
	execErrTimeout
	execErrSpawn
)

type execError struct {
	msg  string
	kind execErrKind
}

func (e *execError) Error() string { return e.msg }

func validationErr(msg string) error { return &execError{msg: msg, kind: execErrValidation} }

// execResult is the outcome of a sandboxed subprocess.
type execResult struct {
	exitCode  int
	stdout    string
	stderr    string
	truncated bool
	duration  time.Duration
}

// execRunner bounds subprocess concurrency with a semaphore (MAX_CONCURRENT in
// the BFF) and runs every command shell-free under a clean environment.
type execRunner struct {
	sem chan struct{}
}

func newExecRunner() *execRunner {
	return &execRunner{sem: make(chan struct{}, maxConcurrent)}
}

// run executes cmd with positional args (never a shell string), under a clean
// environment, capping stdout at capBytes (killing the process on overflow)
// and bounding wall-clock time. It returns an *execError on validation,
// timeout, or spawn failure; a non-zero exit code is reported in the result,
// not as an error.
func (r *execRunner) run(ctx context.Context, cmd string, args []string, timeout time.Duration, capBytes int) (*execResult, error) {
	if capBytes <= 0 {
		capBytes = maxBytes
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, &execError{msg: "exec canceled before start", kind: execErrSpawn}
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	c := exec.CommandContext(cctx, cmd, args...)
	c.Env = cleanEnv()
	stdout := &cappedBuffer{limit: capBytes, onOverflow: cancel}
	stderr := &cappedBuffer{limit: maxBytes}
	c.Stdout = stdout
	c.Stderr = stderr

	runErr := c.Run()
	dur := time.Since(start)

	if cctx.Err() == context.DeadlineExceeded && !stdout.truncated {
		return nil, &execError{msg: "exec timed out", kind: execErrTimeout}
	}

	exitCode := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else if !stdout.truncated {
			// A kill triggered by our own overflow cancel surfaces as a
			// non-ExitError; treat that as a (truncated) success, not a spawn
			// failure. Any other non-exit error is a genuine spawn problem.
			return nil, &execError{msg: "spawn failed: " + runErr.Error(), kind: execErrSpawn}
		}
	}
	return &execResult{
		exitCode:  exitCode,
		stdout:    stdout.String(),
		stderr:    stderr.String(),
		truncated: stdout.truncated,
		duration:  dur,
	}, nil
}

// cappedBuffer accumulates output up to limit bytes, then marks itself
// truncated and (once) invokes onOverflow to stop the producer. It always
// reports the full write length so the child's pipe never blocks on a short
// write.
type cappedBuffer struct {
	limit      int
	buf        bytes.Buffer
	truncated  bool
	onOverflow func()
	fired      bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.markTruncated()
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.markTruncated()
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) markTruncated() {
	b.truncated = true
	if b.onOverflow != nil && !b.fired {
		b.fired = true
		b.onOverflow()
	}
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// cleanEnv builds the minimal environment passed to every subprocess. No host
// environment is inherited; PATH/HOME/LANG are assigned intentionally and
// GITHUB_TOKEN is forwarded only when present (mirrors exec-core.ts::cleanEnv).
func cleanEnv() []string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/tmp"
	}
	path := os.Getenv("ADMIN_PATH")
	if path == "" {
		path = home + "/.local/bin:/usr/local/bin:/usr/bin:/bin"
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + home,
		"LANG=C.UTF-8",
		"NO_COLOR=1",
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		env = append(env, "GITHUB_TOKEN="+tok)
	}
	return env
}

// Terminal-output sanitizer, ported from exec.ts. Strips OSC sequences,
// non-SGR CSI sequences, C0/DEL/C1 control bytes, and all 12 Unicode
// bidi/RTL controls from CVE-2021-42574, BEFORE any subprocess output reaches
// the browser.
var (
	oscRE  = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	csiRE  = regexp.MustCompile(`\x1b\[[?0-9;]*[a-ln-zA-LN-Z]`)
	ctrlRE = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f-\x9f]`)
	bidiRE = regexp.MustCompile(`[\x{061c}\x{200e}\x{200f}\x{202a}-\x{202e}\x{2066}-\x{2069}]`)
)

func sanitizeTerminalOutput(s string) string {
	s = oscRE.ReplaceAllString(s, "")
	s = csiRE.ReplaceAllString(s, "")
	s = ctrlRE.ReplaceAllString(s, "")
	s = bidiRE.ReplaceAllString(s, "")
	return s
}

// isValidHostPath reports whether p is a safe absolute host path: absolute,
// NUL-free, with no ".." traversal segment. Ported from lib/hostPath.ts; this
// is the single gate for any supervisor-reported path consumed by a
// subprocess or os.Stat.
func isValidHostPath(p string) bool {
	if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// isPathUnderRoot reports whether cwd equals root or is nested under it,
// matching on path-segment boundaries so "/a/gascity" admits "/a/gascity/x"
// but not the sibling "/a/gascity-evil".
func isPathUnderRoot(cwd, root string) bool {
	root = strings.TrimSuffix(root, "/")
	return cwd == root || strings.HasPrefix(cwd, root+"/")
}

// isValidRunCwd validates a run cwd before it is handed to `git -C <cwd>`.
// Shape is always checked; when allowedRoots is non-empty the cwd must also
// sit at or under one sanctioned root (RUN_CWD_ALLOWED_ROOTS).
func isValidRunCwd(cwd string, allowedRoots []string) bool {
	if !isValidHostPath(cwd) {
		return false
	}
	if len(allowedRoots) == 0 {
		return true
	}
	for _, root := range allowedRoots {
		if isPathUnderRoot(cwd, root) {
			return true
		}
	}
	return false
}

// runReviewablePaths restricts run-diff git reads to reviewable files,
// excluding control-plane dirs (.beads/.gc). Ported from run-diff-policy.ts.
var runReviewablePaths = []string{
	"--", ":/",
	":(exclude,top).beads", ":(exclude,top).beads/**",
	":(exclude,top).gc", ":(exclude,top).gc/**",
}

const gitPretty = "--pretty=format:%H%x09%h%x09%an%x09%aI%x09%D%x09%s"

// gitLogViews is the hardcoded enum of `git log` invocations. The operator can
// only pick a view name; arbitrary git arguments can never reach the server.
var gitLogViews = map[string][]string{
	"recent-main": {"log", gitPretty, "-n", gitLogRecentN, "origin/main"},
	"recent-all":  {"log", gitPretty, "-n", gitLogRecentN, "--branches", "--remotes"},
	"today":       {"log", gitPretty, "--since=24.hours.ago", "--branches", "--remotes"},
	"this-week":   {"log", gitPretty, "--since=7.days.ago", "--branches", "--remotes"},
}

func gitRepoPath() string {
	if p := os.Getenv("ADMIN_GIT_REPO"); p != "" {
		return p
	}
	return os.Getenv("HOME")
}

// execGitLog runs a whitelisted `git log` view against the dashboard host repo.
func (r *execRunner) execGitLog(ctx context.Context, view string) (*execResult, error) {
	args, ok := gitLogViews[view]
	if !ok {
		return nil, validationErr("unknown git view")
	}
	full := append([]string{"-C", gitRepoPath()}, args...)
	return r.run(ctx, "git", full, gitLogTimeout, maxBytes)
}

// runGitViews is the hardcoded enum of per-run git reads for formula run-detail
// diffs (RUN_GIT_VIEWS in exec.ts).
var runGitViews = map[string][]string{
	"root":                {"rev-parse", "--show-toplevel"},
	"upstream":            {"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"},
	"merge-base-upstream": {"merge-base", "HEAD", "@{upstream}"},
}

func runGitArgsWithPaths(base ...string) []string {
	return append(base, runReviewablePaths...)
}

var baseRevisionRE = regexp.MustCompile(`(?i)^[0-9a-f]{40,64}$`)

// execRunGit runs a whitelisted per-run git read in cwd (validated against
// allowedRoots). The "diff-head"/"name-status-head" views carry the reviewable
// path filter; the larger diff cap applies to the unified diff.
func (r *execRunner) execRunGit(ctx context.Context, cwd, view string, allowedRoots []string) (*execResult, error) {
	if !isValidRunCwd(cwd, allowedRoots) {
		return nil, validationErr("invalid run cwd")
	}
	var args []string
	outCap := maxBytes
	switch view {
	case "status":
		args = runGitArgsWithPaths("status", "--porcelain=v1", "--untracked-files=all")
	case "diff-head":
		args = runGitArgsWithPaths("diff", "--no-ext-diff", "--no-color", "HEAD")
		outCap = maxRunDiffBytes
	case "name-status-head":
		args = runGitArgsWithPaths("diff", "--name-status", "--no-ext-diff", "--no-color", "HEAD")
	default:
		v, ok := runGitViews[view]
		if !ok {
			return nil, validationErr("unknown run git view")
		}
		args = v
	}
	full := append([]string{"-C", cwd}, args...)
	return r.run(ctx, "git", full, runGitTimeout, outCap)
}

// execRunGitDiffFrom runs `git diff <baseRevision>` over reviewable paths.
func (r *execRunner) execRunGitDiffFrom(ctx context.Context, cwd, baseRevision string, allowedRoots []string) (*execResult, error) {
	if !isValidRunCwd(cwd, allowedRoots) || !baseRevisionRE.MatchString(baseRevision) {
		return nil, validationErr("invalid run git diff args")
	}
	args := append([]string{"-C", cwd, "diff", "--no-ext-diff", "--no-color", baseRevision}, runReviewablePaths...)
	return r.run(ctx, "git", args, runGitTimeout, maxRunDiffBytes)
}

// execRunGitNameStatusFrom runs `git diff --name-status <baseRevision>`.
func (r *execRunner) execRunGitNameStatusFrom(ctx context.Context, cwd, baseRevision string, allowedRoots []string) (*execResult, error) {
	if !isValidRunCwd(cwd, allowedRoots) || !baseRevisionRE.MatchString(baseRevision) {
		return nil, validationErr("invalid run git name-status args")
	}
	args := append([]string{"-C", cwd, "diff", "--name-status", "--no-ext-diff", "--no-color", baseRevision}, runReviewablePaths...)
	return r.run(ctx, "git", args, runGitTimeout, maxBytes)
}

// execBdDoctor runs a read-only `bd doctor` health probe of a rig's embedded
// dolt .beads store. The path is supervisor-reported and validated here; --fix
// is never passed, so the probe only inspects.
func (r *execRunner) execBdDoctor(ctx context.Context, beadsPath string) (*execResult, error) {
	if !isValidHostPath(beadsPath) || !strings.HasSuffix(beadsPath, "/.beads") {
		return nil, validationErr("invalid beads store path")
	}
	return r.run(ctx, "bd", []string{"doctor", "--readonly", "--db", beadsPath, "--json"}, bdDoctorTimeout, maxBytes)
}
