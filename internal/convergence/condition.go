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
	"github.com/gastownhall/gascity/internal/pathutil"
)

// SafePATH is the fallback PATH for gate script execution.
const SafePATH = "/usr/local/bin:/usr/bin:/bin"

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
	ArtifactDir          string
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
		"TMPDIR=" + os.TempDir(),
		"BEADS_DIR=" + filepath.Join(storePath, ".beads"),
		"GC_BEAD_ID=" + ce.BeadID,
		"GC_ITERATION=" + strconv.Itoa(ce.Iteration),
		"GC_WISP_ID=" + ce.WispID,
		"GC_ARTIFACT_DIR=" + ce.ArtifactDir,
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

	return env
}

// ResolveConditionPath resolves and validates a gate condition path.
// - Resolves relative paths against cityPath
// - Rejects symlinks (EvalSymlinks must equal cleaned path)
// - Returns the canonical absolute path
func ResolveConditionPath(cityPath, conditionPath string) (string, error) {
	if conditionPath == "" {
		return "", fmt.Errorf("resolving gate condition path: empty path")
	}

	// Canonicalize cityPath first so that symlinked workspace roots
	// (e.g., /tmp → /private/tmp on macOS) don't cause false rejections.
	canonCity, err := filepath.EvalSymlinks(cityPath)
	if err != nil {
		canonCity = filepath.Clean(cityPath) // best-effort if city doesn't exist yet
	}

	var absPath string
	if filepath.IsAbs(conditionPath) {
		absPath = filepath.Clean(conditionPath)
	} else {
		absPath = filepath.Clean(filepath.Join(canonCity, conditionPath))
	}

	// Reject path traversal: the resolved path must be under cityPath
	// for relative paths.
	if !filepath.IsAbs(conditionPath) {
		rel, err := filepath.Rel(canonCity, absPath)
		if err != nil || pathutil.IsOutsideDir(rel) {
			return "", fmt.Errorf("resolving gate condition path: path traversal not allowed: %s", conditionPath)
		}
	}

	// Resolve symlinks to the real path. Scripts may be symlinked from
	// a shared tooling directory (e.g., ~/tooling/scripts/).
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("resolving gate condition path: %w", err)
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
