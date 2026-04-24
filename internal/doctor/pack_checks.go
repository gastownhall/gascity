package doctor

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// PackScriptCheck implements Check by running a script shipped with
// a pack. The script follows the pack doctor protocol:
//
//   - Exit 0 = OK, Exit 1 = Warning, Exit 2 = Error
//   - First line of stdout = message (shown after check name)
//   - Remaining stdout lines = details (shown in verbose mode)
//
// The script receives environment variables:
//
//	GC_CITY_PATH    — absolute path to the city root
//	GC_PACK_DIR — absolute path to the pack directory
//
// When FixScript is non-empty, the check also supports `gc doctor --fix`:
// the fix script is dispatched with the same environment contract as
// Script. Exit 0 = remediation succeeded (Fix returns nil); non-zero
// exit surfaces as a fix error (Fix returns an error carrying the
// exit code and captured output). Packs opt into auto-remediation by
// declaring `fix = "..."` in their pack.toml [[doctor]] entry (or in a
// convention-discovered doctor/<name>/doctor.toml manifest).
type PackScriptCheck struct {
	// CheckName is the fully-qualified name, e.g. "maintenance:check-binaries".
	CheckName string
	// Script is the absolute path to the check script.
	Script string
	// FixScript is the absolute path to the remediation script, or
	// empty when the check is diagnostic-only. When set, CanFix returns
	// true and Fix dispatches to this script.
	FixScript string
	// PackDir is the absolute pack directory path.
	PackDir string
	// PackName is the logical pack name used for runtime env injection.
	PackName string
}

// Name returns the check's fully-qualified name.
func (c *PackScriptCheck) Name() string { return c.CheckName }

// CanFix reports whether the pack declared a fix script for this check.
// When true, `gc doctor --fix` will dispatch to FixScript after the
// check returns a non-OK status.
func (c *PackScriptCheck) CanFix() bool { return c.FixScript != "" }

// Fix runs the pack's fix script with the same environment contract as
// Run. Returns nil on exit 0 (remediation succeeded); returns an error
// carrying the exit code and any captured output on non-zero exit or
// if the script cannot be executed. When FixScript is empty this is a
// no-op and returns nil — callers should gate on CanFix first.
func (c *PackScriptCheck) Fix(ctx *CheckContext) error {
	if c.FixScript == "" {
		return nil
	}

	cmd := exec.Command(c.FixScript) //nolint:gosec // path from pack config
	cmd.Dir = c.PackDir
	cmd.Env = append(cmd.Environ(), citylayout.PackRuntimeEnv(ctx.CityPath, c.PackName)...)
	cmd.Env = append(cmd.Env,
		"GC_PACK_DIR="+c.PackDir,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			output := strings.TrimSpace(string(out))
			if output == "" {
				return fmt.Errorf("fix script exited with status %d", exitErr.ExitCode())
			}
			return fmt.Errorf("fix script exited with status %d: %s", exitErr.ExitCode(), output)
		}
		return fmt.Errorf("fix script error: %w", err)
	}
	return nil
}

// Run executes the pack script and interprets its output.
func (c *PackScriptCheck) Run(ctx *CheckContext) *CheckResult {
	cmd := exec.Command(c.Script) //nolint:gosec // script path from pack config
	cmd.Dir = c.PackDir
	cmd.Env = append(cmd.Environ(), citylayout.PackRuntimeEnv(ctx.CityPath, c.PackName)...)
	cmd.Env = append(cmd.Env,
		"GC_PACK_DIR="+c.PackDir,
	)

	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			// Script not found or not executable.
			return &CheckResult{
				Name:    c.CheckName,
				Status:  StatusError,
				Message: "script error: " + err.Error(),
			}
		}
	}

	message, details := parseScriptOutput(string(out))
	if message == "" {
		message = "check completed"
	}

	var status CheckStatus
	switch exitCode {
	case 0:
		status = StatusOK
	case 1:
		status = StatusWarning
	default:
		status = StatusError
	}

	return &CheckResult{
		Name:    c.CheckName,
		Status:  status,
		Message: message,
		Details: details,
	}
}

// parseScriptOutput splits script output into a message (first line)
// and details (remaining non-empty lines).
func parseScriptOutput(output string) (string, []string) {
	output = strings.TrimSpace(output)
	if output == "" {
		return "", nil
	}

	lines := strings.Split(output, "\n")
	message := strings.TrimSpace(lines[0])

	var details []string
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			details = append(details, trimmed)
		}
	}
	return message, details
}
