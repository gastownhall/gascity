package orders

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// RigWildcard is the Override.Rig value that matches every order with the
// override's name regardless of rig scope (city-level + every rig-scoped
// instance). It is reserved as a config-time literal: real rig names
// equal to "*" are rejected by config validation.
const RigWildcard = "*"

// Override modifies a scanned order's scheduling fields.
// Uses pointer fields to distinguish "not set" from "set to zero value."
// Mirrors config.OrderOverride but lives in the orders package
// to avoid a circular dependency.
type Override struct {
	Name     string
	Rig      string
	Enabled  *bool
	Trigger  *string
	Interval *string
	Schedule *string
	Check    *string
	On       *string
	Pool     *string
	Timeout  *string
}

// ApplyOverrides applies each override to the matching order in aa.
//
// Matching rules:
//   - ov.Rig == "":  matches only city-level orders (those with no rig).
//     If no city-level order with the name exists but rig-scoped instances
//     do, returns an error suggesting the explicit rig = "<name>" syntax.
//   - ov.Rig == "*": wildcard — matches every order with the name,
//     regardless of rig.
//   - otherwise:     matches only the order with that exact rig.
//
// Returns an error if an override targets a nonexistent order (following
// the agent override pattern where unmatched targets are errors, not
// silent no-ops).
func ApplyOverrides(aa []Order, overrides []Override) error {
	for i, ov := range overrides {
		if ov.Name == "" {
			return fmt.Errorf("orders.overrides[%d]: name is required", i)
		}
		found := false
		for j := range aa {
			if aa[j].Name != ov.Name {
				continue
			}
			if !rigMatches(ov.Rig, aa[j].Rig) {
				continue
			}
			applyOverride(&aa[j], &ov)
			found = true
		}
		if !found {
			return notFoundError(i, ov, aa)
		}
	}
	return nil
}

func rigMatches(ovRig, orderRig string) bool {
	if ovRig == RigWildcard {
		return true
	}
	return ovRig == orderRig
}

// notFoundError builds the unmatched-override error. When the override is
// rigless ("") but the slice contains rig-scoped orders with the same
// name, the error names every such rig so the user knows exactly what to
// type — this is the gotcha that the previous error message hid.
func notFoundError(idx int, ov Override, aa []Order) error {
	switch ov.Rig {
	case "":
		rigs := rigsForName(aa, ov.Name)
		if len(rigs) > 0 {
			return fmt.Errorf(
				"orders.overrides[%d]: order %q not found at city scope (%s rig-scoped); "+
					"set %s to target a per-rig instance, "+
					"or use rig = %q to target all instances",
				idx, ov.Name, pluralizeRigCount(len(rigs)), formatRigSuggestions(rigs), RigWildcard,
			)
		}
		return fmt.Errorf("orders.overrides[%d]: order %q not found", idx, ov.Name)
	case RigWildcard:
		return fmt.Errorf("orders.overrides[%d]: order %q not found (rig %q matches no instances)", idx, ov.Name, RigWildcard)
	default:
		return fmt.Errorf("orders.overrides[%d]: order %q (rig %q) not found", idx, ov.Name, ov.Rig)
	}
}

func rigsForName(aa []Order, name string) []string {
	seen := map[string]struct{}{}
	for _, a := range aa {
		if a.Name != name || a.Rig == "" {
			continue
		}
		seen[a.Rig] = struct{}{}
	}
	return slices.Sorted(maps.Keys(seen))
}

func formatRigSuggestions(rigs []string) string {
	parts := make([]string, len(rigs))
	for i, r := range rigs {
		parts[i] = fmt.Sprintf("rig = %q", r)
	}
	return strings.Join(parts, ", ")
}

func pluralizeRigCount(n int) string {
	if n == 1 {
		return "1 instance"
	}
	return fmt.Sprintf("%d instances", n)
}

func applyOverride(a *Order, ov *Override) {
	if ov.Enabled != nil {
		a.Enabled = ov.Enabled
	}
	if ov.Trigger != nil {
		a.Trigger = *ov.Trigger
	}
	if ov.Interval != nil {
		a.Interval = *ov.Interval
	}
	if ov.Schedule != nil {
		a.Schedule = *ov.Schedule
	}
	if ov.Check != nil {
		a.Check = *ov.Check
	}
	if ov.On != nil {
		a.On = *ov.On
	}
	if ov.Pool != nil {
		a.Pool = *ov.Pool
	}
	if ov.Timeout != nil {
		a.Timeout = *ov.Timeout
	}
}
