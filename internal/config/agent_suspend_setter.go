package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Rig agent-setter tables: the TOML keys of a [[rigs]] entry whose
// AgentOverride entries target pack-stamped agents by bare name.
const (
	// RigSetterOverrides is the [[rigs.overrides]] table (Rig.Overrides).
	RigSetterOverrides = "overrides"
	// RigSetterPatches is the [[rigs.patches]] table (Rig.RigPatches).
	RigSetterPatches = "patches"
)

var tomlAgentLineRe = regexp.MustCompile(`^\s*agent\s*=\s*"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$`)

// SetRigAgentSuspendedForEdit sets suspended to an explicit true or false on
// the single [[rigs.<table>]] entry of rig rigName whose agent key is agent
// and which already carries a suspended key, directly in the on-disk
// city.toml bytes. Every other byte -- comments, key order, formatting and
// unrelated entries -- is preserved; the replaced line keeps its spacing and
// trailing comment. table is RigSetterOverrides or RigSetterPatches. cfg is
// the raw config, used only to keep rig site bindings in sync.
//
// It returns an error wrapping ErrSurgicalAgentEditUnsupported when the rig
// entry or the setter cannot be unambiguously located in the raw text (for
// example a duplicate rig, an inline-table or dotted-key form, or a setter
// declared in a fragment rather than city.toml).
func SetRigAgentSuspendedForEdit(fs fsys.FS, tomlPath string, cfg *City, rigName, table, agent string, suspended bool) error {
	if table != RigSetterOverrides && table != RigSetterPatches {
		return fmt.Errorf("rig %q agent %q: unknown rig setter table %q", rigName, agent, table)
	}
	return writeCityBytesForEdit(fs, tomlPath, cfg, func(existing []byte) ([]byte, error) {
		lines := strings.Split(string(existing), "\n")
		idx, err := findRigSetterSuspendedLine(lines, rigName, table, agent)
		if err != nil {
			return nil, fmt.Errorf("%w: rig %q [[rigs.%s]] agent %q in %s: %w", ErrSurgicalAgentEditUnsupported, rigName, table, agent, tomlPath, err)
		}
		lines[idx] = setSuspendedLine(lines[idx], suspended)
		return []byte(strings.Join(lines, "\n")), nil
	})
}

// SetAgentPatchSuspendedForEdit sets suspended to an explicit true or false
// on the single existing [[patches.agent]] block targeting (dir, name),
// directly in the on-disk city.toml bytes, preserving every other byte. An
// existing suspended line keeps its spacing and trailing comment; otherwise a
// "suspended = <bool>" line is added after the block's last non-blank line.
// Unlike the inline [[agent]] toggle it never deletes the key: a patch's
// suspended is a pointer, so absence means "inherit" and only an explicit
// false overrides a suspended lower layer. cfg is the raw config, used only
// to keep rig site bindings in sync.
//
// It returns an error wrapping ErrSurgicalAgentEditUnsupported when the
// block cannot be unambiguously located (missing, duplicated, keyed by rig
// instead of dir, or declared outside city.toml).
func SetAgentPatchSuspendedForEdit(fs fsys.FS, tomlPath string, cfg *City, dir, name string, suspended bool) error {
	return writeCityBytesForEdit(fs, tomlPath, cfg, func(existing []byte) ([]byte, error) {
		lines := strings.Split(string(existing), "\n")
		start, end, ok := findTOMLArrayBlock(lines, "[[patches.agent]]", name, dir)
		if !ok {
			return nil, fmt.Errorf("%w: [[patches.agent]] block for dir %q name %q could not be unambiguously located in %s; set suspended in it by hand", ErrSurgicalAgentEditUnsupported, dir, name, tomlPath)
		}
		for i := start; i < end; i++ {
			if tomlSuspendedLineRe.MatchString(lines[i]) {
				lines[i] = setSuspendedLine(lines[i], suspended)
				return []byte(strings.Join(lines, "\n")), nil
			}
		}
		insertAt := lastNonBlankLineIndex(lines, start, end)
		out := make([]string, 0, len(lines)+1)
		out = append(out, lines[:insertAt+1]...)
		out = append(out, fmt.Sprintf("suspended = %t", suspended))
		out = append(out, lines[insertAt+1:]...)
		return []byte(strings.Join(out, "\n")), nil
	})
}

// findRigSetterSuspendedLine returns the index of the suspended line inside
// the single [[rigs.<table>]] block with agent = agent that belongs to the
// single [[rigs]] entry named rigName.
func findRigSetterSuspendedLine(lines []string, rigName, table, agent string) (int, error) {
	start, end, err := findRigRegion(lines, rigName)
	if err != nil {
		return -1, err
	}
	header := "[[rigs." + table + "]]"
	found := -1
	matches := 0
	for i := start; i < end; i++ {
		if strings.TrimSpace(lines[i]) != header {
			continue
		}
		bodyEnd := end
		for j := i + 1; j < end; j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "[") {
				bodyEnd = j
				break
			}
		}
		target, suspendedIdx := "", -1
		for j := i + 1; j < bodyEnd; j++ {
			if m := tomlAgentLineRe.FindStringSubmatch(lines[j]); m != nil {
				target = m[1]
				continue
			}
			if tomlSuspendedLineRe.MatchString(lines[j]) {
				suspendedIdx = j
			}
		}
		if target != agent || suspendedIdx < 0 {
			continue
		}
		matches++
		found = suspendedIdx
	}
	switch matches {
	case 0:
		return -1, fmt.Errorf("no %s entry with agent = %q and a suspended = <bool> line", header, agent)
	case 1:
		return found, nil
	default:
		return -1, fmt.Errorf("%d %s entries with agent = %q set suspended", matches, header, agent)
	}
}

// findRigRegion returns the [start, end) line range of the single [[rigs]]
// entry whose own name key is rigName. The range runs from the entry's
// header through its rigs.* sub-tables, stopping at the next [[rigs]] entry
// or any other top-level table.
func findRigRegion(lines []string, rigName string) (int, int, error) {
	start, end, matches := -1, -1, 0
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "[[rigs]]" {
			continue
		}
		regionEnd := len(lines)
		ownEnd := -1
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if !strings.HasPrefix(trimmed, "[") {
				continue
			}
			if ownEnd < 0 {
				ownEnd = j
			}
			if trimmed == "[[rigs]]" || (!strings.HasPrefix(trimmed, "[rigs.") && !strings.HasPrefix(trimmed, "[[rigs.")) {
				regionEnd = j
				break
			}
		}
		if ownEnd < 0 || ownEnd > regionEnd {
			ownEnd = regionEnd
		}
		for j := i + 1; j < ownEnd; j++ {
			if m := tomlNameLineRe.FindStringSubmatch(lines[j]); m != nil && m[1] == rigName {
				matches++
				start, end = i, regionEnd
				break
			}
		}
	}
	switch matches {
	case 0:
		return -1, -1, fmt.Errorf("no [[rigs]] entry with name = %q", rigName)
	case 1:
		return start, end, nil
	default:
		return -1, -1, fmt.Errorf("%d [[rigs]] entries with name = %q", matches, rigName)
	}
}

// setSuspendedLine rewrites a "suspended = <bool>" line to the requested
// explicit value, keeping its key spacing and any trailing comment.
func setSuspendedLine(line string, suspended bool) string {
	m := tomlSuspendedLineRe.FindStringSubmatch(line)
	return m[1] + fmt.Sprintf("%t", suspended) + m[2]
}
