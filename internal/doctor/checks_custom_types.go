package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
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
	// missing is populated by Run for use by Fix. It lists required types
	// absent from the store's types.custom CSV config.
	missing []string
	// tableMissing is populated by Run for use by Fix. It lists required
	// types absent from the store's normalized custom_types table. bd's
	// create validation reads this table, not the CSV, so the two can
	// drift: a store can have a complete CSV yet still reject
	// `bd create --type <t>` with "invalid issue type: <t>" because the
	// table row was never (re)created.
	tableMissing []string
}

// NewCustomTypesCheck creates a check for a specific store directory.
func NewCustomTypesCheck(dir, label string) *CustomTypesCheck {
	return &CustomTypesCheck{Dir: dir, Label: label}
}

// Name returns the check identifier.
func (c *CustomTypesCheck) Name() string {
	return "custom-types:" + c.Label
}

// Run checks that all required types are registered — both in the
// types.custom CSV config and in the store's normalized custom_types
// table. bd's create validation reads the table, so both are checked
// independently: a store can pass the CSV check yet still reject
// `bd create --type <t>` if the table row is missing (see
// TestCustomTypesCheck_TableDrift).
func (c *CustomTypesCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	// Check if .beads directory exists — if not, skip (no store here).
	beadsDir := filepath.Join(c.Dir, ".beads")
	if !dirExists(beadsDir) {
		r.Status = StatusOK
		r.Message = "no .beads directory, skipping"
		return r
	}

	// Get current custom types from the CSV config.
	current, err := getCustomTypes(c.Dir)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("could not read types.custom: %v", err)
		r.FixHint = "run gc doctor --fix to set required custom types"
		// Treat as all missing — fix will set the full list.
		c.missing = RequiredCustomTypes
		c.tableMissing = nil
		return r
	}
	c.missing = typesNotIn(RequiredCustomTypes, current)

	// Get registered types from the normalized custom_types table — the
	// source of truth bd's create validation checks.
	registered, err := getRegisteredTypes(c.Dir)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("could not read custom_types table: %v", err)
		r.FixHint = "run gc doctor --fix to register required custom types"
		c.tableMissing = RequiredCustomTypes
		return r
	}
	c.tableMissing = typesNotIn(RequiredCustomTypes, registered)

	if len(c.missing) == 0 && len(c.tableMissing) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("all %d required types registered", len(RequiredCustomTypes))
		return r
	}

	var parts []string
	if len(c.missing) != 0 {
		parts = append(parts, fmt.Sprintf("missing %d custom type(s): %s", len(c.missing), strings.Join(c.missing, ", ")))
	}
	if len(c.tableMissing) != 0 {
		parts = append(parts, fmt.Sprintf("%d type(s) not registered in custom_types table (validator will reject): %s", len(c.tableMissing), strings.Join(c.tableMissing, ", ")))
	}
	r.Status = StatusError
	r.Message = strings.Join(parts, "; ")
	r.FixHint = "run gc doctor --fix to register missing types"
	return r
}

// typesNotIn returns the entries of want that are absent from have, in
// want's order. Entries are compared after trimming whitespace. Shared by
// the CSV completeness check and the custom_types table drift check.
func typesNotIn(want, have []string) []string {
	haveSet := make(map[string]bool, len(have))
	for _, t := range have {
		haveSet[strings.TrimSpace(t)] = true
	}
	var missing []string
	for _, w := range want {
		if !haveSet[strings.TrimSpace(w)] {
			missing = append(missing, w)
		}
	}
	return missing
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
//
// Fix also runs when only c.tableMissing is non-empty (CSV already
// complete): re-issuing `bd config set types.custom` with the same CSV
// value is what reconciles a drifted custom_types table, since bd's set
// path is what keeps the table in sync with the CSV.
func (c *CustomTypesCheck) Fix(_ *CheckContext) error {
	if len(c.missing) == 0 && len(c.tableMissing) == 0 {
		return nil
	}
	// Read the current list so we can preserve user-added types.
	// If we cannot read it, return the error rather than overwriting —
	// silently dropping user types is worse than failing loud.
	current, err := getCustomTypes(c.Dir)
	if err != nil {
		return fmt.Errorf("reading current custom types: %w", err)
	}
	merged := contract.MergeCustomTypes(current, RequiredCustomTypes)
	return setCustomTypes(c.Dir, strings.Join(merged, ","))
}

// getCustomTypes reads the current types.custom config from a bd store.
// Uses --json so an unset key returns an empty string value rather than
// the human-readable "types.custom (not set)" sentinel (which would
// otherwise be persisted as a fake custom type when Fix() merges).
func getCustomTypes(dir string) ([]string, error) {
	start := time.Now()
	args := []string{"config", "get", "--json", "types.custom"}
	cmd := exec.Command("bd", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	beads.TraceBDCall("go:doctor.getCustomTypes", dir, args, start, exitCode, err)
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

// getRegisteredTypes reads the bd store's normalized custom_types table —
// the source of truth bd's create validation checks — as opposed to
// getCustomTypes, which reads the types.custom CSV config value.
func getRegisteredTypes(dir string) ([]string, error) {
	start := time.Now()
	args := []string{"types", "--json"}
	cmd := exec.Command("bd", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	beads.TraceBDCall("go:doctor.getRegisteredTypes", dir, args, start, exitCode, err)
	if err != nil {
		return nil, err
	}
	return parseRegisteredTypesJSON(out)
}

// parseRegisteredTypesJSON decodes the output of `bd types --json` and
// returns its custom_types field — the table-backed list, distinct from
// parseCustomTypesJSON's CSV-config value.
func parseRegisteredTypesJSON(out []byte) ([]string, error) {
	var parsed struct {
		CustomTypes []string `json:"custom_types"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parsing bd types output: %w", err)
	}
	return parsed.CustomTypes, nil
}

// setCustomTypes writes the types.custom config to a bd store.
func setCustomTypes(dir, types string) error {
	start := time.Now()
	args := []string{"config", "set", "types.custom", types}
	cmd := exec.Command("bd", args...)
	cmd.Dir = dir
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	beads.TraceBDCall("go:doctor.setCustomTypes", dir, args, start, exitCode, err)
	return err
}

// dirExists checks if a directory exists.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
