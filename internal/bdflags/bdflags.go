// Package bdflags is the single source of truth for bd CLI flag names per
// subcommand. It backs both the write-mutation ID guard in cmd/gc/cmd_bd.go
// and the gc lint check that validates bd invocations embedded in prompt
// templates, so the two call sites cannot drift apart from each other.
//
// Sourced from bd <sub> --help output (2026-07-13, bd v1.1.0).
package bdflags

import "sort"

// globalValueFlags are accepted by every bd subcommand and consume the next
// argument as their value.
var globalValueFlags = map[string]bool{
	"--actor": true, "--db": true, "-C": true, "--directory": true,
	"--dolt-auto-commit": true,
}

// globalBoolFlags are accepted by every bd subcommand and take no value.
var globalBoolFlags = map[string]bool{
	"--global": true, "--ignore-schema-skew": true, "--json": true,
	"--profile": true, "-q": true, "--quiet": true, "--readonly": true,
	"--sandbox": true, "-v": true, "--verbose": true, "-h": true, "--help": true,
}

// valueFlagsBySub holds each subcommand's value-consuming flags (beyond the
// global set), keyed by subcommand: a single word ("update") or, for
// compound bd subcommands, "parent child" ("mol pour"). The key set here
// defines every subcommand this package knows about — see Known/Subcommands.
var valueFlagsBySub = map[string]map[string]bool{
	"create": {
		"--acceptance": true, "--append-notes": true, "-a": true, "--assignee": true,
		"--body-file": true, "--context": true, "--defer": true, "--deps": true,
		"-d": true, "--description": true, "--design": true, "--design-file": true,
		"--due": true, "-e": true, "--estimate": true, "--event-actor": true,
		"--event-category": true, "--event-payload": true, "--event-target": true,
		"--external-ref": true, "-f": true, "--file": true, "--graph": true,
		"--id": true, "-l": true, "--labels": true, "--metadata": true,
		"--mol-type": true, "--notes": true, "--parent": true, "-p": true,
		"--priority": true, "--repo": true, "--skills": true, "--spec-id": true,
		"-s": true, "--status": true, "--title": true, "-t": true, "--type": true, "--waits-for": true,
		"--waits-for-gate": true, "--wisp-type": true,
	},
	"update": {
		"--acceptance": true, "--add-label": true, "--append-notes": true,
		"-a": true, "--assignee": true, "--await-id": true, "--body-file": true,
		"--defer": true, "-d": true, "--description": true, "--design": true,
		"--design-file": true, "--due": true, "-e": true, "--estimate": true,
		"--external-ref": true, "--metadata": true, "--notes": true,
		"--parent": true, "-p": true, "--priority": true, "--remove-label": true,
		"--session": true, "--set-labels": true, "--set-metadata": true,
		"-s": true, "--status": true, "-t": true, "--type": true,
		"--title": true, "--spec-id": true, "--unset-metadata": true,
	},
	"close": {
		"-r": true, "--reason": true, "--reason-file": true, "--session": true,
	},
	"reopen": {
		"-r": true, "--reason": true,
	},
	"delete": {
		"--from-file": true,
	},
	"ready": {
		"-a": true, "--assignee": true, "--exclude-label": true, "--exclude-type": true,
		"--has-metadata-key": true, "-l": true, "--label": true, "--label-any": true,
		"-n": true, "--limit": true, "--metadata-field": true, "--mol": true,
		"--mol-type": true, "--offset": true, "--parent": true, "-p": true,
		"--priority": true, "-s": true, "--sort": true, "-t": true, "--type": true,
	},
	"list": {
		"-a": true, "--assignee": true, "--closed-after": true, "--closed-before": true,
		"--created-after": true, "--created-before": true, "--defer-after": true,
		"--defer-before": true, "--desc-contains": true, "--due-after": true,
		"--due-before": true, "--exclude-label": true, "--exclude-type": true,
		"--format": true, "--has-metadata-key": true, "--id": true, "-l": true,
		"--label": true, "--label-any": true, "--label-pattern": true,
		"--label-regex": true, "-n": true, "--limit": true, "--metadata-field": true,
		"--mol-type": true, "--notes-contains": true, "--offset": true,
		"--parent": true, "-p": true, "--priority": true, "--priority-max": true,
		"--priority-min": true, "--sort": true, "--spec": true, "-s": true,
		"--status": true, "--title": true, "--title-contains": true, "-t": true,
		"--type": true, "--updated-after": true, "--updated-before": true,
		"--wisp-type": true,
	},
	"show": {
		"--as-of": true, "--id": true,
	},
	"mol current": {
		"--for": true, "--limit": true, "--range": true,
	},
	"mol pour": {
		"--assignee": true, "--attach": true, "--attach-type": true, "--var": true,
	},
	"mol wisp": {
		"--var": true,
	},
	"mol burn": {},
	"gate check": {
		"-l": true, "--limit": true, "-t": true, "--type": true,
	},
	"gate list": {
		"-n": true, "--limit": true,
	},
	"dep add": {
		"--blocked-by": true, "--depends-on": true, "--file": true, "-t": true, "--type": true,
	},
	"dep list": {
		"--direction": true, "-t": true, "--type": true,
	},
	"dep remove": {},
}

// boolFlagsBySub holds each subcommand's boolean (no-value) flags beyond the
// global set. Same keying convention as valueFlagsBySub.
var boolFlagsBySub = map[string]map[string]bool{
	"create": {
		"--dry-run": true, "--ephemeral": true, "--force": true, "--no-history": true,
		"--no-inherit-labels": true, "--silent": true, "--stdin": true, "--validate": true,
	},
	"update": {
		"--allow-empty-description": true, "--claim": true, "--ephemeral": true,
		"--history": true, "--no-history": true, "--persistent": true, "--stdin": true,
	},
	"close": {
		"--claim-next": true, "--continue": true, "-f": true, "--force": true,
		"--no-auto": true, "--suggest-next": true,
	},
	"reopen": {},
	"delete": {
		"--cascade": true, "--dry-run": true, "-f": true, "--force": true,
	},
	"ready": {
		"--claim": true, "--explain": true, "--gated": true, "--include-deferred": true,
		"--include-ephemeral": true, "--plain": true, "--pretty": true, "-u": true, "--unassigned": true,
	},
	"list": {
		"--all": true, "--deferred": true, "--empty-description": true, "--flat": true,
		"--include-gates": true, "--include-infra": true, "--include-templates": true,
		"--long": true, "--no-assignee": true, "--no-labels": true, "--no-pager": true,
		"--no-parent": true, "--no-pinned": true, "--overdue": true, "--pinned": true,
		"--pretty": true, "--ready": true, "-r": true, "--reverse": true,
		"--skip-labels": true, "--tree": true, "-w": true, "--watch": true,
	},
	"show": {
		"--children": true, "--current": true, "--include-comments": true,
		"--include-dependents": true, "--local-time": true, "--long": true,
		"--refs": true, "--short": true, "--thread": true, "-w": true, "--watch": true,
	},
	"mol current": {},
	"mol pour": {
		"--dry-run": true,
	},
	"mol wisp": {
		"--dry-run": true, "--root-only": true,
	},
	"mol burn": {
		"--dry-run": true, "--force": true,
	},
	"gate check": {
		"--dry-run": true, "-e": true, "--escalate": true,
	},
	"gate list": {
		"-a": true, "--all": true,
	},
	"dep add": {
		"--no-cycle-check": true,
	},
	"dep list":   {},
	"dep remove": {},
}

// Subcommands returns the bd subcommand keys this package has flag
// manifests for (e.g. "close", "mol pour"), in no particular order.
func Subcommands() []string {
	keys := make([]string, 0, len(valueFlagsBySub))
	for k := range valueFlagsBySub {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Known reports whether sub is a subcommand key this package has a flag
// manifest for.
func Known(sub string) bool {
	_, ok := valueFlagsBySub[sub]
	return ok
}

// ValueFlags returns the set of value-consuming flag names (long and short
// form) for sub, merged with the global flags shared by every bd
// subcommand. Returns nil if sub is not a known subcommand key.
func ValueFlags(sub string) map[string]bool {
	subFlags, ok := valueFlagsBySub[sub]
	if !ok {
		return nil
	}
	return mergeFlagSets(globalValueFlags, subFlags)
}

// BoolFlags returns the set of boolean flag names for sub, merged with the
// global boolean flags shared by every bd subcommand. Returns nil if sub is
// not a known subcommand key.
func BoolFlags(sub string) map[string]bool {
	subFlags, ok := boolFlagsBySub[sub]
	if !ok {
		return nil
	}
	return mergeFlagSets(globalBoolFlags, subFlags)
}

func mergeFlagSets(sets ...map[string]bool) map[string]bool {
	merged := make(map[string]bool)
	for _, set := range sets {
		for k := range set {
			merged[k] = true
		}
	}
	return merged
}
