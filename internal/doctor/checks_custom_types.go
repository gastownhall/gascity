package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RequiredCustomTypes lists the bead types that Gas City requires
// to be registered with every bd store (city + rigs).
//
// "convergence" is included because gc's convergence handler
// (internal/convergence/create.go) creates beads with type="convergence"
// as the root of every convergence loop. Without it registered, every
// `gc converge create` call fails with "invalid issue type: convergence".
//
// "step" is included because formula instantiation creates non-root
// step beads with type="step" (internal/molecule/molecule.go Instantiate)
// so Ready() and `bd ready` can exclude formula scaffolding from actionable
// work queues. Without it registered, formula dispatch fails with
// "invalid issue type: step" (#1039).
var RequiredCustomTypes = []string{
	"molecule", "convoy", "message", "event", "gate",
	"merge-request", "agent", "role", "rig", "session", "spec",
	"convergence", "step",
}

// CustomTypesCheck verifies that all required Gas City custom bead
// types are registered in a bd store's types.custom config.
type CustomTypesCheck struct {
	// Dir is the directory to check (city root or rig path).
	Dir string
	// Label identifies this check instance (e.g., "city" or rig name).
	Label string
	// missing is populated by Run for use by Fix.
	missing []string
}

// NewCustomTypesCheck creates a check for a specific store directory.
func NewCustomTypesCheck(dir, label string) *CustomTypesCheck {
	return &CustomTypesCheck{Dir: dir, Label: label}
}

// Name returns the check identifier.
func (c *CustomTypesCheck) Name() string {
	return "custom-types:" + c.Label
}

// Run checks that all required types are registered.
func (c *CustomTypesCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	// Check if .beads directory exists — if not, skip (no store here).
	beadsDir := filepath.Join(c.Dir, ".beads")
	if !dirExists(beadsDir) {
		r.Status = StatusOK
		r.Message = "no .beads directory, skipping"
		return r
	}

	// Get current custom types.
	current, err := getCustomTypes(c.Dir)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("could not read types.custom: %v", err)
		r.FixHint = "run gc doctor --fix to set required custom types"
		// Treat as all missing — fix will set the full list.
		c.missing = RequiredCustomTypes
		return r
	}

	// Check for missing types.
	currentSet := make(map[string]bool, len(current))
	for _, t := range current {
		currentSet[strings.TrimSpace(t)] = true
	}
	c.missing = nil
	for _, req := range RequiredCustomTypes {
		if !currentSet[req] {
			c.missing = append(c.missing, req)
		}
	}

	if len(c.missing) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("all %d required types registered", len(RequiredCustomTypes))
		return r
	}

	r.Status = StatusError
	r.Message = fmt.Sprintf("missing %d custom type(s): %s", len(c.missing), strings.Join(c.missing, ", "))
	r.FixHint = "run gc doctor --fix to register missing types"
	return r
}

// CanFix returns true — missing types can be registered.
func (c *CustomTypesCheck) CanFix() bool { return true }

// Fix registers any missing required custom types with the bd store,
// preserving any additional custom types the user has already added.
//
// This function MUST merge — not overwrite — because a city may have
// additional custom types registered beyond the RequiredCustomTypes
// baseline (e.g., pack-specific types, user-defined types). Overwriting
// would silently delete those, causing failures the next time code tries
// to create beads of the deleted types.
func (c *CustomTypesCheck) Fix(_ *CheckContext) error {
	if len(c.missing) == 0 {
		return nil
	}
	// Read the current list so we can preserve user-added types.
	// If we cannot read it, return the error rather than overwriting —
	// silently dropping user types is worse than failing loud.
	current, err := getCustomTypes(c.Dir)
	if err != nil {
		return fmt.Errorf("reading current custom types: %w", err)
	}
	merged := mergeCustomTypes(current, RequiredCustomTypes)
	return setCustomTypes(c.Dir, strings.Join(merged, ","))
}

// mergeCustomTypes returns the union of current and required, in order:
// current entries first (preserving user order), then any required entries
// not already present. Empty/whitespace-only entries are dropped and
// duplicates are removed.
func mergeCustomTypes(current, required []string) []string {
	seen := make(map[string]bool, len(current)+len(required))
	merged := make([]string, 0, len(current)+len(required))
	for _, t := range current {
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			continue
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		merged = append(merged, trimmed)
	}
	for _, req := range required {
		if seen[req] {
			continue
		}
		seen[req] = true
		merged = append(merged, req)
	}
	return merged
}

// getCustomTypes reads the current types.custom config from a bd store.
// Uses --json so an unset key returns an empty string value rather than
// the human-readable "types.custom (not set)" sentinel (which would
// otherwise be persisted as a fake custom type when Fix() merges).
func getCustomTypes(dir string) ([]string, error) {
	cmd := exec.Command("bd", "config", "get", "--json", "types.custom")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseCustomTypesJSON(out)
}

// parseCustomTypesJSON decodes the output of `bd config get --json types.custom`
// into a list of types. Empty values yield nil (not []string{""}).
func parseCustomTypesJSON(out []byte) ([]string, error) {
	var parsed struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parsing bd config get output: %w", err)
	}
	raw := strings.TrimSpace(parsed.Value)
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, ","), nil
}

// setCustomTypes writes the types.custom config to a bd store.
func setCustomTypes(dir, types string) error {
	cmd := exec.Command("bd", "config", "set", "types.custom", types)
	cmd.Dir = dir
	return cmd.Run()
}

// dirExists checks if a directory exists.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
