package doctor

import "strings"

// SelectChecks narrows checks to the entries whose Name matches one of names,
// keeping the order the checks were registered in rather than the order they
// were requested. Registration order is load-bearing: doctor deliberately
// registers the beads-role check ahead of every store-dependent check so one
// root cause reports before the symptoms it causes.
//
// An empty names slice selects every check, which is how a plain `gc doctor`
// run reaches this function. Every name that matches nothing is returned in
// unmatched, in request order, so the caller can fail the whole invocation
// rather than report the subset that did match: a caller filtering doctor
// output by name reads a short result set as a clean one, so a typo would
// otherwise report health nobody measured.
//
// Names match exactly, after surrounding whitespace is trimmed. A repeated
// name selects its check once. A name shared by more than one registered
// check selects all of them — the in-tree checks scope their names
// (`custom-types:<label>`, `rig:<name>:path`), but nothing in the Check
// interface enforces uniqueness and pack-declared names come from config.
func SelectChecks(checks []Check, names []string) (selected []Check, unmatched []string) {
	if len(names) == 0 {
		return nonNilChecks(checks), nil
	}

	requested := make(map[string]struct{}, len(names))
	requestOrder := make([]string, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if _, dup := requested[trimmed]; dup {
			continue
		}
		requested[trimmed] = struct{}{}
		requestOrder = append(requestOrder, trimmed)
	}

	matched := make(map[string]struct{}, len(requested))
	for _, check := range checks {
		if check == nil {
			continue
		}
		name := check.Name()
		if _, want := requested[name]; !want {
			continue
		}
		matched[name] = struct{}{}
		selected = append(selected, check)
	}

	for _, name := range requestOrder {
		if _, ok := matched[name]; !ok {
			unmatched = append(unmatched, name)
		}
	}
	return selected, unmatched
}

// CheckNames returns the names of every registered check in registration
// order, which is the order a full run reports them in. Callers use it to tell
// an operator what is selectable in this workspace; the answer is per-workspace
// because doctor registers checks conditionally (a file-backed store registers
// no Dolt checks, a suspended rig registers none of its own).
func CheckNames(checks []Check) []string {
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		if check == nil {
			continue
		}
		names = append(names, check.Name())
	}
	return names
}

func nonNilChecks(checks []Check) []Check {
	for _, check := range checks {
		if check == nil {
			return compactChecks(checks)
		}
	}
	return checks
}

func compactChecks(checks []Check) []Check {
	out := make([]Check, 0, len(checks))
	for _, check := range checks {
		if check != nil {
			out = append(out, check)
		}
	}
	return out
}
