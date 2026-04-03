package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RequiredCustomTypes lists the bead types that Gas City requires
// to be registered with every bd store (city + rigs).
var RequiredCustomTypes = []string{
	"molecule", "convoy", "message", "event", "gate",
	"merge-request", "agent", "role", "rig", "session", "spec",
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

// Fix registers all required custom types with the bd store.
func (c *CustomTypesCheck) Fix(_ *CheckContext) error {
	if len(c.missing) == 0 {
		return nil
	}
	fullList := strings.Join(RequiredCustomTypes, ",")
	return setCustomTypes(c.Dir, fullList)
}

// getCustomTypes reads the current types.custom config from a bd store.
func getCustomTypes(dir string) ([]string, error) {
	cmd := exec.Command("bd", "config", "get", "types.custom")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
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
