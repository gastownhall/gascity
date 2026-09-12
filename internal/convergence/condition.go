package convergence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// SafePATH is the fallback PATH for gate script execution.
const SafePATH = "/usr/local/bin:/usr/bin:/bin"

const (
	textFileBusyRetryAttempts = 5
	textFileBusyRetryDelay    = 25 * time.Millisecond
)

// conditionGCHome resolves the gc state directory for gate subprocesses. Gate
// HOME is intentionally sandboxed to the city, so it cannot also be used for
// gc's machine-level cache and registry state.
func conditionGCHome() (string, error) {
	path := gchome.ResolveReadOnly().Path()
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve GC_HOME %q: %w", path, err)
	}
	return absPath, nil
}

// conditionPATH resolves the tool directories gate scripts actually need.
// This keeps the env narrow while ensuring gate scripts use the same bd/gc
// binaries as the running city instead of whatever older copy happens to live
// in /usr/local/bin.
func conditionPATH() string {
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
	for _, name := range []string{"bd", "gc", "dolt", "jq"} {
		if path, err := exec.LookPath(name); err == nil {
			addDir(filepath.Dir(path))
		}
	}
	for _, dir := range strings.Split(SafePATH, ":") {
		addDir(dir)
	}
	return strings.Join(dirs, ":")
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

// Environ returns the environment variable slice for exec.Cmd.
// Only whitelisted variables: PATH (safe default), HOME, TMPDIR, convergence
// vars, Dolt/Beads connection env, and GC_INTEGRATION_REAL_BD when present for
// integration-test bd shims.
func (ce ConditionEnv) Environ() ([]string, error) {
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
	gcHome, err := conditionGCHome()
	if err != nil {
		return nil, err
	}
	env := []string{
		"PATH=" + conditionPATH(),
		"HOME=" + home,
		"GC_HOME=" + gcHome,
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

	return env, nil
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
// check.
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
		return "", fmt.Errorf("resolving gate condition path: not a regular file: %s", resolved)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("resolving gate condition path: file is not executable: %s", resolved)
	}

	return resolved, nil
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
	cmdEnv, envErr := env.Environ()
	if envErr != nil {
		return GateResult{
			Outcome: GateError,
			Stderr:  fmt.Sprintf("build gate environment: %v", envErr),
		}
	}
	cmd.Env = cmdEnv
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
