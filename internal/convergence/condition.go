package convergence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// SafePATH is the fallback PATH for gate script execution.
const SafePATH = "/usr/local/bin:/usr/bin:/bin"

const (
	textFileBusyRetryAttempts = 5
	textFileBusyRetryDelay    = 25 * time.Millisecond
)

// ErrConditionNotRegular reports that a gate condition path resolved, inside its
// containment boundary, to something other than a regular file (a directory,
// device, socket or FIFO).
var ErrConditionNotRegular = errors.New("not a regular file")

// ErrConditionNotExecutable reports that a gate condition path resolved to a
// regular file with no execute permission bit set.
var ErrConditionNotExecutable = errors.New("file is not executable")

// conditionGCHome resolves the gc state directory for gate subprocesses. Gate
// HOME is intentionally sandboxed to the city, so it cannot also be used for
// gc's machine-level cache and registry state.
func conditionGCHome() string {
	return gchome.ResolveReadOnly().Path()
}

// conditionToolNames are the tools gate scripts call by name.
var conditionToolNames = []string{"bd", "gc", "dolt", "jq"}

// conditionTools resolves each tool gate scripts call by name to the binary
// the running city uses. gc is the running binary itself when this process is
// a gc binary; every other tool, and gc otherwise, resolves on this process's
// PATH. Tools that do not resolve are omitted.
func conditionTools() map[string]string {
	tools := make(map[string]string, len(conditionToolNames))
	for _, name := range conditionToolNames {
		if path, err := exec.LookPath(name); err == nil {
			if abs, absErr := filepath.Abs(path); absErr == nil {
				path = abs
			}
			tools[name] = path
		}
	}
	if exe, err := os.Executable(); err == nil && filepath.Base(exe) == "gc" {
		tools["gc"] = exe
	}
	return tools
}

// conditionPATH builds the PATH for gate scripts: narrow, and resolving each
// tool in conditionToolNames to exactly the binary conditionTools chose, never
// to whatever older copy happens to live elsewhere.
//
// Listing the resolved tools' directories is not enough. One directory can
// hold several of these tools, for example bd beside an older gc in
// ~/.local/bin, and the first directory listed would then shadow a tool
// resolved from a later one. So PATH starts with a directory of links, one per
// tool, to the exact resolved binary. The resolved directories and SafePATH
// follow for anything else a script runs. When the link directory cannot be
// prepared, PATH falls back to the resolved directories alone.
func conditionPATH() string {
	tools := conditionTools()
	dirs := make([]string, 0, 8)
	seen := make(map[string]struct{})
	addDir := func(dir string) {
		if dir == "" {
			return
		}
		if _, ok := seen[dir]; ok {
			return
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	if linkDir, err := conditionToolLinkDir(conditionGCHome(), tools); err == nil {
		addDir(linkDir)
	}
	for _, name := range conditionToolNames {
		if path, ok := tools[name]; ok {
			addDir(filepath.Dir(path))
		}
	}
	for _, dir := range strings.Split(SafePATH, ":") {
		addDir(dir)
	}
	return strings.Join(dirs, ":")
}

// conditionToolLinkDir returns a directory under gcHome holding one symlink
// per tool, named for the tool and pointing at its resolved binary. The
// directory is named for its contents, so a given set of tools reuses one
// directory and a changed set gets a new one; it is built under a temporary
// name and renamed into place, so a concurrent gate never sees a partial set.
func conditionToolLinkDir(gcHome string, tools map[string]string) (string, error) {
	if gcHome == "" || len(tools) == 0 {
		return "", errors.New("no gc home or no tools")
	}
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	sum := sha256.New()
	for _, name := range names {
		_, _ = fmt.Fprintf(sum, "%s=%s\n", name, tools[name])
	}
	parent := filepath.Join(gcHome, "run", "gate-tools")
	dir := filepath.Join(parent, hex.EncodeToString(sum.Sum(nil))[:16])
	if conditionToolLinksMatch(dir, tools) {
		return dir, nil
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup of an unused staging dir
	for _, name := range names {
		if err := os.Symlink(tools[name], filepath.Join(tmp, name)); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, dir); err != nil && !conditionToolLinksMatch(dir, tools) {
		return "", err
	}
	if !conditionToolLinksMatch(dir, tools) {
		return "", fmt.Errorf("gate tool links in %s do not match", dir)
	}
	return dir, nil
}

// conditionToolLinksMatch reports whether dir holds exactly a link per tool to
// its resolved binary.
func conditionToolLinksMatch(dir string, tools map[string]string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != len(tools) {
		return false
	}
	for name, target := range tools {
		got, err := os.Readlink(filepath.Join(dir, name))
		if err != nil || got != target {
			return false
		}
	}
	return true
}

// ConditionEnv builds the environment variables for a gate condition script.
// All bead-derived values are passed as env vars — never interpolated into commands.
type ConditionEnv struct {
	BeadID               string
	Iteration            int
	CityPath             string
	StorePath            string
	WorkDir              string
	WispID               string
	DocPath              string // from var.doc_path, may be empty
	MoleculeDir          string // molecule.Dir(CityPath, rootID); may be empty for non-molecule beads
	ArtifactDir          string // per-step artifact dir; GC_ARTIFACT_DIR is omitted when empty (matches the sling-time contract)
	IterationDurationMs  int64
	CumulativeDurationMs int64
	MaxIterations        int
	AgentVerdict         string // normalized verdict, may be empty
	AgentProvider        string // may be empty
	AgentModel           string // may be empty
}

// conditionBeadsCredentialsFile resolves the beads credentials file gate
// commands must read.
//
// Gate commands run with HOME=<CityPath> so they cannot see the controller's
// home directory. bd resolves its credentials file from HOME, so under the
// sandbox its own default lands on <CityPath>/.config/beads/credentials — a
// path that never exists — and every bd read inside a gate fails to
// authenticate (gastownhall/gascity ga-pqlgh).
//
// The fix threads one explicit file path across the sandbox boundary instead of
// widening HOME: the whitelist stays enumerable, .ssh/.gnupg stay invisible, and
// no secret material is copied into the city directory (which is listable and
// backed up). An ambient BEADS_CREDENTIALS_FILE wins, because operators already
// use it to select scoped stores. Otherwise the OS default resolved against the
// controller's real home is used, and only when it exists — exporting a path
// that does not resolve would mask bd's own fallback for no gain.
func conditionBeadsCredentialsFile() string {
	if path := strings.TrimSpace(os.Getenv("BEADS_CREDENTIALS_FILE")); path != "" {
		return path
	}
	path := doltauth.DefaultCredentialsPath()
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return path
}

// Environ returns the environment variable slice for exec.Cmd.
// Only whitelisted variables: PATH (safe default), HOME, TMPDIR, convergence
// vars, Dolt/Beads connection env, the resolved beads credentials file, and
// GC_INTEGRATION_REAL_BD when present for integration-test bd shims.
func (ce ConditionEnv) Environ() []string {
	// Use CityPath as HOME to sandbox gate scripts from the
	// controller's home directory (which may contain .ssh, .gnupg, etc).
	home := ce.CityPath
	if home == "" {
		home = os.TempDir()
	}
	storePath := ce.StorePath
	if storePath == "" {
		storePath = ce.CityPath
	}
	env := []string{
		"PATH=" + conditionPATH(),
		"HOME=" + home,
		"GC_HOME=" + conditionGCHome(),
		"TMPDIR=" + os.TempDir(),
		"BEADS_DIR=" + filepath.Join(storePath, ".beads"),
		"GC_BEAD_ID=" + ce.BeadID,
		"GC_ITERATION=" + strconv.Itoa(ce.Iteration),
		"GC_WISP_ID=" + ce.WispID,
		"GC_ITERATION_DURATION_MS=" + strconv.FormatInt(ce.IterationDurationMs, 10),
		"GC_CUMULATIVE_DURATION_MS=" + strconv.FormatInt(ce.CumulativeDurationMs, 10),
		"GC_MAX_ITERATIONS=" + strconv.Itoa(ce.MaxIterations),
	}
	env = append(env, citylayout.CityRuntimeEnvForRuntimeDir(ce.CityPath, citylayout.TrustedAmbientCityRuntimeDir(ce.CityPath))...)

	// Optional fields: only include if non-empty.
	if ce.DocPath != "" {
		env = append(env, "GC_DOC_PATH="+ce.DocPath)
	}
	if ce.AgentVerdict != "" {
		env = append(env, "GC_AGENT_VERDICT="+ce.AgentVerdict)
	}
	if ce.AgentProvider != "" {
		env = append(env, "GC_AGENT_PROVIDER="+ce.AgentProvider)
	}
	if ce.AgentModel != "" {
		env = append(env, "GC_AGENT_MODEL="+ce.AgentModel)
	}
	if ce.WorkDir != "" {
		env = append(env, "GC_WORK_DIR="+ce.WorkDir)
	}
	if ce.StorePath != "" {
		env = append(env, "GC_STORE_PATH="+ce.StorePath)
	}
	if ce.ArtifactDir != "" {
		env = append(env, "GC_ARTIFACT_DIR="+ce.ArtifactDir)
	}
	if ce.MoleculeDir != "" {
		env = append(env, "GC_MOLECULE_DIR="+ce.MoleculeDir)
	}
	if realBD := os.Getenv("GC_INTEGRATION_REAL_BD"); realBD != "" {
		env = append(env, "GC_INTEGRATION_REAL_BD="+realBD)
	}
	if credentials := conditionBeadsCredentialsFile(); credentials != "" {
		env = append(env, "BEADS_CREDENTIALS_FILE="+credentials)
	}
	for _, key := range []string{
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
		"GC_DOLT",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
	} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}

	return env
}

// containedIn reports whether absPath is the same as or nested under root.
// Both arguments must already be cleaned/absolute; the comparison is lexical
// (no further symlink resolution) and is intended to be combined with
// pre-resolved (EvalSymlinks'd) inputs at the call site.
func containedIn(absPath, root string) bool {
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return false
	}
	return !pathutil.IsOutsideDir(rel)
}

// ResolveConditionPath resolves and validates a gate condition path.
//
//   - envelope: a security boundary; relative-path traversal validation
//     accepts the resolved path if it stays under this root. For city-scoped
//     gates pass the city path; for rig-scoped ralph checks
//     (gastownhall/gascity#2320) pass the city path here even though `base`
//     may point at a rig subtree. Must be non-empty — an empty envelope
//     would silently disable the traversal check, so it is rejected.
//   - base: the directory that relative conditionPath values are joined
//     against, AND a second permitted security boundary used in addition
//     to envelope: paths that stay under base are accepted even when base
//     is not a subtree of envelope (gastownhall/gascity#2354 — sibling
//     rig/city layouts). Callers MUST ensure base is an operator-controlled
//     path; this function performs no validation of base itself. When
//     empty, falls back to envelope to preserve historical single-arg
//     behavior.
//   - conditionPath: the path declared by the gate. May be absolute or
//     relative to `base`.
//
// For relative paths, both the lexically-joined and the symlink-resolved
// targets must land inside envelope OR base — defending against both
// `../`-style traversal and symlinks that escape containment after
// resolution. Absolute paths skip containment in this function because
// imported and registry-installed packs can live outside the city/store roots.
// Callers must only pass absolute paths from surfaces they trust. Returns the
// canonical absolute path after symlink resolution and an exec-eligible file
// check. Unlaunchable targets are classified by IsConditionUnlaunchable.
func ResolveConditionPath(envelope, base, conditionPath string) (string, error) {
	if conditionPath == "" {
		return "", fmt.Errorf("resolving gate condition path: empty path")
	}
	if envelope == "" {
		return "", fmt.Errorf("resolving gate condition path: empty envelope")
	}
	if base == "" {
		base = envelope
	}

	// Canonicalize envelope and base first via pathutil.NormalizePathForCompare,
	// which absolutizes before resolving symlinks (falling back to a
	// best-effort ancestor walk when the path doesn't exist yet). This keeps
	// symlinked workspace roots (e.g., /tmp → /private/tmp on macOS) from
	// causing false rejections, keeps a relative envelope/base (e.g. ".")
	// from staying relative while a resolved target becomes absolute via a
	// symlink — which broke filepath.Rel in the containment checks below —
	// and ensures the post-resolution containment check compares like with
	// like.
	//
	// NormalizePathForCompare does more than absolutize-and-resolve: on
	// darwin it also collapses the /private/tmp and /private/var host
	// aliases back to /tmp and /var, which is the REVERSE direction from
	// bare filepath.EvalSymlinks. Any value compared against canonEnvelope
	// or canonBase must therefore go through pathutil too — a bare
	// EvalSymlinks result is in a different convention and will mismatch on
	// darwin even when the paths name the same location.
	canonEnvelope := pathutil.NormalizePathForCompare(envelope)
	canonBase := pathutil.NormalizePathForCompare(base)

	var absPath string
	if filepath.IsAbs(conditionPath) {
		absPath = filepath.Clean(conditionPath)
	} else {
		absPath = filepath.Clean(filepath.Join(canonBase, conditionPath))
	}

	// Pre-resolution containment: for relative paths the lexical join
	// must stay under envelope OR base (gastownhall/gascity#2354). This
	// rejects `../../foo` style traversal before any filesystem access.
	// Absolute paths skip the check here; callers vouch for them.
	if !filepath.IsAbs(conditionPath) {
		if !containedIn(absPath, canonEnvelope) && !containedIn(absPath, canonBase) {
			return "", fmt.Errorf("resolving gate condition path: path traversal not allowed: %s", conditionPath)
		}
	}

	// Resolve symlinks to the real path. Scripts may be symlinked from
	// a shared tooling directory (e.g., ~/tooling/scripts/).
	// canonical-path-exception: existence/resolvability only, not comparison
	// preparation. This call's error path is the behavior — a dangling or
	// unresolvable conditionPath must fail gate resolution here, so it
	// cannot be replaced with pathutil.NormalizePathForCompare, which never
	// errors.
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("resolving gate condition path: %w", err)
	}

	// Post-resolution containment: a symlink under envelope or base can
	// point outside both trees (e.g. `base/scripts/check.sh -> /etc/passwd`).
	// Re-validate the symlink-resolved path against the same envelope-OR-base
	// rule to close the symlink-escape gap (gastownhall/gascity#2354 review).
	// Absolute paths still skip — same rationale as the pre-resolution check.
	//
	// Use pathutil.PathWithin rather than the lexical containedIn: resolved
	// comes from bare filepath.EvalSymlinks, so on darwin it carries the
	// /private prefix that canonEnvelope/canonBase have had collapsed away.
	// PathWithin normalizes both operands, so the alias collapse applies
	// symmetrically. (The pre-resolution check above keeps containedIn:
	// absPath is derived from canonBase, so both sides already share a
	// convention there.)
	if !filepath.IsAbs(conditionPath) {
		if !pathutil.PathWithin(canonEnvelope, resolved) && !pathutil.PathWithin(canonBase, resolved) {
			return "", fmt.Errorf("resolving gate condition path: symlink target outside containment: %s", conditionPath)
		}
	}

	// Check the resolved file exists and is a regular executable.
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("resolving gate condition path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("resolving gate condition path: %w: %s", ErrConditionNotRegular, resolved)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("resolving gate condition path: %w: %s", ErrConditionNotExecutable, resolved)
	}

	return resolved, nil
}

// IsConditionUnlaunchable reports whether err, as returned by
// ResolveConditionPath, means the declared condition names nothing that can be
// executed: the target is missing (including a dangling symlink), is not a
// regular file, or is not executable. These are repaired out of band (ship the
// script, chmod it) and never by re-running. Containment refusals (traversal,
// symlink escape) and argument errors are deliberately not unlaunchable, with
// one exception: a dangling symlink whose absent target lies outside
// containment still reports unlaunchable, because the post-resolution
// containment check needs a resolved target and a dangling link has none. Once
// that target exists, the same path is a containment refusal.
func IsConditionUnlaunchable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, ErrConditionNotRegular) ||
		errors.Is(err, ErrConditionNotExecutable)
}

// RunCondition executes a gate condition script with the given environment.
// Handles timeout, output capture (truncated to MaxOutputBytes), and retry logic.
// The retryBudget parameter controls max retries on timeout (0 = no retries).
// Returns the final GateResult after all retries are exhausted.
func RunCondition(ctx context.Context, scriptPath string, env ConditionEnv, timeout time.Duration, retryBudget int) GateResult {
	var lastResult GateResult
	retries := 0

	for attempt := 0; attempt <= retryBudget; attempt++ {
		lastResult = runOnce(ctx, scriptPath, env, timeout)

		// Only retry on timeout outcomes.
		if lastResult.Outcome != GateTimeout || attempt == retryBudget {
			lastResult.RetryCount = retries
			return lastResult
		}
		retries++
	}

	// Should not reach here, but be safe.
	lastResult.RetryCount = retries
	return lastResult
}

// runOnce executes a single attempt of the gate condition script.
func runOnce(ctx context.Context, scriptPath string, env ConditionEnv, timeout time.Duration) GateResult {
	var result GateResult
	for attempt := 0; attempt <= textFileBusyRetryAttempts; attempt++ {
		result = runOnceNoPreExecRetry(ctx, scriptPath, env, timeout)
		if !isTextFileBusyPreExecError(result) || attempt == textFileBusyRetryAttempts {
			return result
		}

		timer := time.NewTimer(textFileBusyRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return result
		case <-timer.C:
		}
	}
	return result
}

func isTextFileBusyPreExecError(result GateResult) bool {
	return result.Outcome == GateError && strings.Contains(strings.ToLower(result.Stderr), "text file busy")
}

func runOnceNoPreExecRetry(ctx context.Context, scriptPath string, env ConditionEnv, timeout time.Duration) GateResult {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, scriptPath)
	cmd.Dir = env.CityPath
	if env.StorePath != "" {
		cmd.Dir = env.StorePath
	}
	if env.WorkDir != "" {
		cmd.Dir = env.WorkDir
	}
	cmd.Env = env.Environ()
	// WaitDelay ensures cmd.Wait returns promptly after the context
	// cancels and SIGKILL is sent, even if child I/O pipes are still open.
	cmd.WaitDelay = time.Second

	// Capture slightly more than MaxOutputBytes so that TruncateOutput
	// can detect overflow and properly trim to a UTF-8 rune boundary.
	stdout := newBoundedBuffer(MaxOutputBytes + utf8.UTFMax)
	stderr := newBoundedBuffer(MaxOutputBytes + utf8.UTFMax)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	outStr, outTrunc := TruncateOutput(stdout.Bytes(), MaxOutputBytes)
	errStr, errTrunc := TruncateOutput(stderr.Bytes(), MaxOutputBytes)
	truncated := outTrunc || errTrunc || stdout.Overflowed() || stderr.Overflowed()

	// Check parent context first — if the parent is done, don't
	// misclassify as a gate-level timeout (which would trigger retries
	// against an already-canceled parent).
	if ctx.Err() != nil {
		return GateResult{
			Outcome:   GateError,
			Stdout:    outStr,
			Stderr:    errStr,
			Duration:  duration,
			Truncated: truncated,
		}
	}

	// Check for gate-level timeout (per-script deadline).
	if execCtx.Err() != nil && errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return GateResult{
			Outcome:   GateTimeout,
			Stdout:    outStr,
			Stderr:    errStr,
			Duration:  duration,
			Truncated: truncated,
		}
	}

	if err != nil {
		// Try to extract exit code.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			return GateResult{
				Outcome:   GateFail,
				ExitCode:  &code,
				Stdout:    outStr,
				Stderr:    errStr,
				Duration:  duration,
				Truncated: truncated,
			}
		}
		// Non-exit error (e.g., script not found, permission denied).
		return GateResult{
			Outcome:   GateError,
			Stderr:    err.Error(),
			Duration:  duration,
			Truncated: truncated,
		}
	}

	// Successful exit (code 0).
	code := 0
	return GateResult{
		Outcome:   GatePass,
		ExitCode:  &code,
		Stdout:    outStr,
		Stderr:    errStr,
		Duration:  duration,
		Truncated: truncated,
	}
}
