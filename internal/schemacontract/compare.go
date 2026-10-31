package schemacontract

import (
	"bytes"
	"fmt"
	"regexp"
)

// referencePattern matches an entry's issue or pr reference: #N or a GitHub
// pull request URL.
var referencePattern = regexp.MustCompile(`^(#[0-9]+|https://github\.com/[^/\s]+/[^/\s]+/pull/[0-9]+)$`)

// regenerate is the instruction appended to problems the generator fixes.
const regenerate = "run the generator: bazel run //cmd/gen-schema-baseline"

// Check compares the live schemas against the baseline and returns one
// message per violation, ordered by schema path. A change that narrows or
// tightens a contract is clean; an additive change asks for a regenerated
// baseline; a breaking change needs a schema_version bump with an entry.
func Check(b Baseline, live map[string]Entry) []string {
	paths := map[string]bool{}
	for path := range b.Schemas {
		paths[path] = true
	}
	for path := range live {
		paths[path] = true
	}
	var problems []string
	for _, path := range sortedKeys(paths) {
		entries, recorded := b.Schemas[path]
		liveEntry, isLive := live[path]
		switch {
		case !recorded:
			problems = append(problems, fmt.Sprintf("%s: no baseline record; %s", path, regenerate))
		case !isLive:
			problems = append(problems, fmt.Sprintf("%s: schema removed from schemas/; removing a CLI JSON contract is breaking: add a `BREAKING CHANGE:` footer to the commit and delete its baseline record deliberately", path))
		default:
			problems = append(problems, checkSchema(path, entries, liveEntry)...)
		}
	}
	return problems
}

func checkSchema(path string, entries []Entry, live Entry) []string {
	problems := validateEntries(path, entries)
	idx := -1
	for i, e := range entries {
		if e.SchemaVersion == live.SchemaVersion {
			idx = i
		}
	}
	newest := entries[len(entries)-1]
	switch {
	case idx < 0 && live.SchemaVersion == "":
		return append(problems, fmt.Sprintf("%s: the schema_version const was removed (newest recorded version %q); restore it or bump to a new version", path, newest.SchemaVersion))
	case idx < 0:
		return append(problems, fmt.Sprintf("%s: schema_version %q has no baseline entry (bump without entry): %s, then fill the entry's note and its issue or pr reference", path, live.SchemaVersion, regenerate))
	case idx < len(entries)-1:
		return append(problems, fmt.Sprintf("%s: schema_version %q went backwards: the baseline already records newer version %q", path, live.SchemaVersion, newest.SchemaVersion))
	}
	return append(problems, compareShape(path, newest, live)...)
}

// validateEntries checks the history rules: unique versions, and a note plus
// a well-formed reference on every entry after a schema's first.
func validateEntries(path string, entries []Entry) []string {
	var problems []string
	seen := map[string]bool{}
	for i, e := range entries {
		if seen[e.SchemaVersion] {
			problems = append(problems, fmt.Sprintf("%s: duplicate entry for schema_version %q", path, e.SchemaVersion))
		}
		seen[e.SchemaVersion] = true
		if i == 0 {
			continue
		}
		if e.Note == "" {
			problems = append(problems, fmt.Sprintf("%s: entry for schema_version %q needs a note describing the change", path, e.SchemaVersion))
		}
		switch refs := []string{e.Issue, e.PR}; {
		case e.Issue == "" && e.PR == "":
			problems = append(problems, fmt.Sprintf("%s: entry for schema_version %q needs an issue or pr reference", path, e.SchemaVersion))
		default:
			for _, ref := range refs {
				if ref != "" && !referencePattern.MatchString(ref) {
					problems = append(problems, fmt.Sprintf("%s: entry for schema_version %q has a malformed reference %q (want #N or a pull request URL)", path, e.SchemaVersion, ref))
				}
			}
		}
	}
	return problems
}

// compareShape classifies the difference between the newest recorded entry
// and the live schema at the same version.
func compareShape(path string, recorded, live Entry) []string {
	var problems []string
	breaking := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: BREAKING: %s; bump schema_version and add an entry, or revert", path, fmt.Sprintf(format, args...)))
	}
	if recorded.JSONL != live.JSONL {
		breaking("jsonl framing changed (%t -> %t)", recorded.JSONL, live.JSONL)
	}
	if recorded.RawJSON != live.RawJSON {
		breaking("raw_json framing changed (%t -> %t)", recorded.RawJSON, live.RawJSON)
	}
	for _, key := range sortedKeys(recorded.Variants) {
		liveFields, ok := live.Variants[key]
		if !ok {
			breaking("variant %q removed", key)
			continue
		}
		byPath := map[string]Field{}
		for _, f := range liveFields {
			byPath[f.Path] = f
		}
		for _, old := range recorded.Variants[key] {
			cur, ok := byPath[old.Path]
			if !ok {
				breaking("%s removed or renamed%s", old.Path, variantSuffix(key))
				continue
			}
			if !subset(cur.Types, old.Types) {
				breaking("%s types widened from %v to %v%s", old.Path, old.Types, cur.Types, variantSuffix(key))
			}
			if len(old.Const) != 0 && !bytes.Equal(old.Const, cur.Const) {
				breaking("%s const changed or dropped (%s -> %s)%s", old.Path, old.Const, constText(cur.Const), variantSuffix(key))
			}
			if old.Required && !cur.Required {
				breaking("%s is no longer required%s", old.Path, variantSuffix(key))
			}
		}
	}
	return append(problems, additions(path, recorded, live)...)
}

// additions reports live variants and fields the baseline does not record.
func additions(path string, recorded, live Entry) []string {
	var problems []string
	for _, key := range sortedKeys(live.Variants) {
		old, ok := recorded.Variants[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: variant %q added; %s", path, key, regenerate))
			continue
		}
		have := map[string]bool{}
		for _, f := range old {
			have[f.Path] = true
		}
		for _, f := range live.Variants[key] {
			if !have[f.Path] {
				problems = append(problems, fmt.Sprintf("%s: field %s added%s; %s", path, f.Path, variantSuffix(key), regenerate))
			}
		}
	}
	return problems
}

// subset reports whether every type in sub is allowed by super; a recorded
// "any" allows everything.
func subset(sub, super []string) bool {
	allowed := map[string]bool{}
	for _, t := range super {
		allowed[t] = true
	}
	if allowed["any"] {
		return true
	}
	for _, t := range sub {
		if !allowed[t] {
			return false
		}
	}
	return true
}

func variantSuffix(key string) string {
	if key == "" {
		return ""
	}
	return fmt.Sprintf(" in variant %q", key)
}

func constText(raw []byte) string {
	if len(raw) == 0 {
		return "none"
	}
	return string(raw)
}
