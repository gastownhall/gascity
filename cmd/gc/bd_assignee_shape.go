package main

import (
	"fmt"
	"strings"
)

// bdAssigneeShapeRefusal reports whether a `gc bd update` invocation carries
// a structurally-invalid, non-empty --assignee value: one with an empty rig
// or role segment (e.g. "cairn/", "/pm", "a//b"). Such a value is invisible
// to both tiers of standard discovery -- exact --assignee=<role> matching
// and --unassigned matching -- so a bead written with it becomes silently
// unreachable by every agent. See crn-23jqc (the 2026-08-15 cairn incident
// this guard defends against) for the root-cause writer bug.
//
// Deliberately narrower than a full value validator: "" is the documented
// idiom for clearing an assignee and must keep working, so only a
// non-empty-but-malformed value is refused.
//
// Scoped to the "update" subcommand -- the only write path a caller uses to
// set --assignee on an existing bead through this seam. Runs after the
// bdMutationWriteIDs guard has already validated the flag shape is
// unambiguous for "update" (or doBd has already returned 1), so every flag
// encountered here is a known value-consuming or boolean flag.
func bdAssigneeShapeRefusal(args []string) (string, bool) {
	if len(args) == 0 || args[0] != "update" {
		return "", false
	}
	valueFlags := bdSubcmdValueFlags("update")
	boolFlags := bdSubcmdBoolFlags("update")

	positional := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if positional {
			continue
		}
		if arg == "--" {
			positional = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		// --flag=value form: value is embedded, no next-arg consumed.
		if eq := strings.Index(arg, "="); eq >= 0 {
			if flagName := arg[:eq]; flagName == "--assignee" || flagName == "-a" {
				if msg, bad := bdAssigneeShapeInvalid(arg[eq+1:]); bad {
					return msg, true
				}
			}
			continue
		}
		flagName := strings.TrimLeft(arg, "-")
		longForm := "--" + flagName
		shortForm := "-" + flagName // only meaningful when flagName is 1 char
		isAssignee := longForm == "--assignee" || (len(flagName) == 1 && shortForm == "-a")

		if valueFlags[longForm] || (len(flagName) == 1 && valueFlags[shortForm]) {
			// Known value-consuming flag: its value is the next argument.
			if isAssignee && i+1 < len(args) {
				if msg, bad := bdAssigneeShapeInvalid(args[i+1]); bad {
					return msg, true
				}
			}
			i++
			continue
		}
		if boolFlags[longForm] || (len(flagName) == 1 && boolFlags[shortForm]) {
			// Known boolean flag (e.g. --claim): no value to inspect.
			continue
		}
		// Unknown flag shape: bdMutationWriteIDs already fails closed on this
		// before doBd ever reaches this guard, so this arm is unreachable in
		// practice. Nothing further to check.
	}
	return "", false
}

// bdAssigneeShapeInvalid reports whether value is non-empty and contains an
// empty rig or role segment (leading slash, trailing slash, or a doubled
// slash). Empty is always valid -- it is the documented clear idiom.
func bdAssigneeShapeInvalid(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	if !strings.HasPrefix(value, "/") && !strings.HasSuffix(value, "/") && !strings.Contains(value, "//") {
		return "", false
	}
	return fmt.Sprintf("gc bd: refusing --assignee=%q: %q has an empty rig or role segment\n(a \"<rig>/\" value is invisible to both --assignee and --unassigned\ndiscovery; see crn-23jqc)\n", value, value), true
}
