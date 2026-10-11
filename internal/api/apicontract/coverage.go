package apicontract

import (
	"fmt"
	"sort"
	"strings"
)

// Coverage is the result of comparing a spec's operations with the set a
// contract suite exercised plus the set it explicitly waived.
type Coverage struct {
	Total     int
	Exercised int
	Waived    int
	// Missing lists operations neither exercised nor waived. A non-empty
	// list means a new operation landed without contract coverage.
	Missing []string
	// StaleWaivers lists waivers naming an operation the spec no longer has.
	StaleWaivers []string
	// WaivedButExercised lists waivers the suite now covers; the waiver
	// should be deleted so it cannot hide a future regression.
	WaivedButExercised []string
	// Unknown lists exercised ids that are not in the spec.
	Unknown []string
}

// Percent is the share of spec operations the suite exercised.
func (c Coverage) Percent() float64 {
	if c.Total == 0 {
		return 0
	}
	return 100 * float64(c.Exercised) / float64(c.Total)
}

// Err returns a descriptive error when the coverage guard fails: an operation
// is neither exercised nor waived, a waiver is stale, or a waiver is redundant.
func (c Coverage) Err() error {
	var b strings.Builder
	if len(c.Missing) > 0 {
		fmt.Fprintf(&b, "operations with no contract coverage and no waiver (exercise them in the suite or add a waiver with a reason):\n  %s\n", strings.Join(c.Missing, "\n  "))
	}
	if len(c.StaleWaivers) > 0 {
		fmt.Fprintf(&b, "waivers for operations no longer in the spec (delete them):\n  %s\n", strings.Join(c.StaleWaivers, "\n  "))
	}
	if len(c.WaivedButExercised) > 0 {
		fmt.Fprintf(&b, "waived operations the suite now exercises (delete the waiver):\n  %s\n", strings.Join(c.WaivedButExercised, "\n  "))
	}
	if len(c.Unknown) > 0 {
		fmt.Fprintf(&b, "exercised operationIds missing from the spec:\n  %s\n", strings.Join(c.Unknown, "\n  "))
	}
	if b.Len() == 0 {
		return nil
	}
	return fmt.Errorf("%s", b.String())
}

// CheckCoverage compares ops with the exercised ids and the waivers
// (operationId -> reason).
func CheckCoverage(ops []Operation, exercised []string, waivers map[string]string) Coverage {
	inSpec := make(map[string]bool, len(ops))
	for _, op := range ops {
		inSpec[op.ID] = true
	}
	ex := make(map[string]bool, len(exercised))
	for _, id := range exercised {
		ex[id] = true
	}
	c := Coverage{Total: len(ops)}
	for _, op := range ops {
		_, waived := waivers[op.ID]
		switch {
		case ex[op.ID]:
			c.Exercised++
			if waived {
				c.WaivedButExercised = append(c.WaivedButExercised, op.ID)
			}
		case waived:
			c.Waived++
		default:
			c.Missing = append(c.Missing, op.ID)
		}
	}
	for id := range waivers {
		if !inSpec[id] {
			c.StaleWaivers = append(c.StaleWaivers, id)
		}
	}
	for id := range ex {
		if !inSpec[id] {
			c.Unknown = append(c.Unknown, id)
		}
	}
	sort.Strings(c.Missing)
	sort.Strings(c.StaleWaivers)
	sort.Strings(c.WaivedButExercised)
	sort.Strings(c.Unknown)
	return c
}
