package convergence

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// EvaluateStepName is the reserved step name for the controller-injected
// evaluate step.
const EvaluateStepName = "evaluate"

// DefaultEvaluatePromptPath is the default evaluate prompt relative to
// city root.
const DefaultEvaluatePromptPath = "prompts/convergence/evaluate.md"

// evaluateRequiredSubstrings are the literal substrings that must appear
// in a custom evaluate prompt file.
var evaluateRequiredSubstrings = []string{
	"bd meta set",
	"convergence.agent_verdict",
}

// EvaluateStep represents the injected evaluate step configuration.
type EvaluateStep struct {
	Name       string // always "evaluate"
	PromptPath string // resolved prompt path (custom or default)
}

// ResolveEvaluateStep determines the evaluate step prompt path.
// If the formula declares a custom evaluate_prompt, use that (resolved
// relative to cityPath). Otherwise use DefaultEvaluatePromptPath
// (resolved relative to cityPath).
// Returns an error if the resolved path escapes cityPath.
func ResolveEvaluateStep(cityPath string, formula Formula) (EvaluateStep, error) {
	promptPath := DefaultEvaluatePromptPath
	if formula.EvaluatePrompt != "" {
		promptPath = formula.EvaluatePrompt
	}

	// Canonicalize cityPath first via pathutil.NormalizePathForCompare, which
	// absolutizes before resolving symlinks (falling back to a best-effort
	// ancestor walk when the path doesn't exist yet). This keeps symlinked
	// workspace roots (e.g., /tmp -> /private/tmp on macOS) from causing
	// false rejections, and keeps a relative cityPath (e.g. ".") from
	// producing a relative PromptPath below.
	//
	// NormalizePathForCompare does more than absolutize-and-resolve: on
	// darwin it also collapses the /private/tmp and /private/var host
	// aliases back to /tmp and /var, which is the REVERSE direction from
	// bare filepath.EvalSymlinks. resolved is built on canonCity and so
	// inherits that convention; any value compared against it must pass
	// through pathutil too.
	canonCity := pathutil.NormalizePathForCompare(cityPath)

	resolved := filepath.Clean(filepath.Join(canonCity, promptPath))

	// Prevent path traversal: the resolved path must stay under cityPath.
	rel, err := filepath.Rel(canonCity, resolved)
	if err != nil || pathutil.IsOutsideDir(rel) {
		return EvaluateStep{}, fmt.Errorf("evaluate prompt path escapes city directory: %s", promptPath)
	}

	// Reject symlinks in the resolved path (matching ResolveConditionPath).
	// canonical-path-exception: existence/resolvability only, not comparison
	// preparation. This deliberately checks whether the resolved path IS a
	// symlink — a blanket "reject any symlink component" policy that is
	// stricter than, and different in kind from, plain containment — and
	// silently tolerates an unresolvable path (err != nil) rather than
	// failing, so pathutil.NormalizePathForCompare's fallback-and-never-error
	// contract would change this function's behavior, not just its
	// canonicalization.
	//
	// Only realResolved is normalized before the comparison. It is already
	// fully symlink-resolved, so NormalizePathForCompare on it amounts to
	// the darwin alias collapse alone — which puts it in the same convention
	// as resolved (built on the collapsed canonCity). Do NOT switch this to
	// pathutil.SamePath: that would normalize resolved too, re-resolving it
	// through its own symlink, so a genuinely symlinked prompt would compare
	// equal and this rejection would stop firing.
	realResolved, err := filepath.EvalSymlinks(resolved)
	if err == nil && pathutil.NormalizePathForCompare(realResolved) != resolved {
		return EvaluateStep{}, fmt.Errorf("evaluate prompt path contains symlinks: %s resolves to %s", resolved, realResolved)
	}

	return EvaluateStep{
		Name:       EvaluateStepName,
		PromptPath: resolved,
	}, nil
}

// ValidateEvaluatePrompt checks that a custom evaluate prompt file contains
// the required substrings "bd meta set" and "convergence.agent_verdict".
// Returns nil if valid, error describing what's missing otherwise.
func ValidateEvaluatePrompt(content []byte) error {
	var missing []string
	for _, sub := range evaluateRequiredSubstrings {
		if !bytes.Contains(content, []byte(sub)) {
			missing = append(missing, fmt.Sprintf("%q", sub))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("evaluate prompt missing required substrings: %s", strings.Join(missing, ", "))
}
