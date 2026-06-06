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

	// Canonicalize cityPath first so that symlinked workspace roots
	// (e.g., /tmp -> /private/tmp on macOS) don't cause false rejections.
	canonCity, err := filepath.EvalSymlinks(cityPath)
	if err != nil {
		canonCity = filepath.Clean(cityPath) // best-effort if city doesn't exist yet
	}

	resolved := filepath.Clean(filepath.Join(canonCity, promptPath))

	// Prevent path traversal: the resolved path must stay under cityPath.
	rel, err := filepath.Rel(canonCity, resolved)
	if err != nil || pathutil.IsOutsideDir(rel) {
		return EvaluateStep{}, fmt.Errorf("evaluate prompt path escapes city directory: %s", promptPath)
	}

	// Reject symlinks in the resolved path (matching ResolveConditionPath).
	realResolved, err := filepath.EvalSymlinks(resolved)
	if err == nil && realResolved != resolved {
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
