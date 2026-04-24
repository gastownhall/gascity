package orders

import "fmt"

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
// Matching is by name, optionally scoped by rig. Returns an error if an
// override targets a nonexistent order (following the agent override
// pattern where unmatched targets are errors, not silent no-ops).
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
			// Scope matching: when ov.Rig is set, only match that rig.
			// When ov.Rig is empty, only match city-level orders
			// (those with no rig), not rig-scoped ones.
			if aa[j].Rig != ov.Rig {
				continue
			}
			applyOverride(&aa[j], &ov)
			found = true
		}
		if !found {
			if ov.Rig != "" {
				return fmt.Errorf("orders.overrides[%d]: order %q (rig %q) not found", i, ov.Name, ov.Rig)
			}
			return fmt.Errorf("orders.overrides[%d]: order %q not found", i, ov.Name)
		}
	}
	return nil
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
