package config

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

// ErrSurgicalAgentEditUnsupported indicates the on-disk [[agent]] (or
// [[patches.agent]]) block identified by identity could not be
// unambiguously located in city.toml, so a byte-preserving surgical edit
// cannot proceed. Callers must not treat this as success: fall back to a
// full rewrite (accepting comment loss) or surface the error, but never
// silently skip the mutation.
var ErrSurgicalAgentEditUnsupported = errors.New("surgical agent suspend/resume edit not supported for this city.toml")

var (
	tomlNameLineRe      = regexp.MustCompile(`^\s*name\s*=\s*"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$`)
	tomlDirLineRe       = regexp.MustCompile(`^\s*dir\s*=\s*"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$`)
	tomlSuspendedLineRe = regexp.MustCompile(`^(\s*suspended\s*=\s*)(?:true|false)\b(.*)$`)
)

// WriteCityAgentSuspendedForEdit toggles the suspended value for the inline
// [[agent]] block matching identity (see ParseQualifiedName/AgentMatchesIdentity)
// directly in the on-disk city.toml bytes, preserving every other byte --
// comments, key order, and formatting all survive untouched. cfg is the
// already-mutated config, used only to keep rig site bindings in sync with
// the whole-file rewrite path; the agent mutation itself is applied to the
// raw on-disk bytes, not re-serialized from cfg.
//
// It returns an error wrapping ErrSurgicalAgentEditUnsupported when the
// target [[agent]] block cannot be unambiguously located in the raw text.
func WriteCityAgentSuspendedForEdit(fs fsys.FS, tomlPath string, cfg *City, identity string, suspended bool) error {
	target, ok := findAgentByIdentity(cfg, identity)
	if !ok {
		return fmt.Errorf("%w: agent %q not found in %s", ErrSurgicalAgentEditUnsupported, identity, tomlPath)
	}

	return writeCityBytesForEdit(fs, tomlPath, cfg, func(existing []byte) ([]byte, error) {
		content, ok := toggleAgentSuspendedBytes(existing, "[[agent]]", target.Name, target.Dir, suspended)
		if !ok {
			return nil, fmt.Errorf("%w: agent %q's [[agent]] block could not be unambiguously located in %s", ErrSurgicalAgentEditUnsupported, identity, tomlPath)
		}
		return content, nil
	})
}

// AppendAgentPatchSuspendedForEdit appends a new [[patches.agent]] block
// setting suspended for the agent identified by (dir, name), directly in the
// on-disk city.toml bytes, preserving every other byte. It refuses (wrapping
// ErrSurgicalAgentEditUnsupported) rather than guess when a [[patches.agent]]
// block for (dir, name) already exists -- callers should fall back to a full
// rewrite in that case.
func AppendAgentPatchSuspendedForEdit(fs fsys.FS, tomlPath string, cfg *City, dir, name string, suspended bool) error {
	return writeCityBytesForEdit(fs, tomlPath, cfg, func(existing []byte) ([]byte, error) {
		if _, ok := toggleAgentSuspendedBytes(existing, "[[patches.agent]]", name, dir, suspended); ok {
			return nil, fmt.Errorf("%w: a [[patches.agent]] block for %q already exists in %s; edit it directly", ErrSurgicalAgentEditUnsupported, name, tomlPath)
		}

		block, err := marshalAgentPatchBlock(dir, name, suspended)
		if err != nil {
			return nil, err
		}
		return appendBlock(existing, block), nil
	})
}

// findAgentByIdentity returns the first agent in cfg.Agents matching
// identity (see AgentMatchesIdentity), by value so callers get a stable
// Name/Dir pair independent of any later mutation of cfg.Agents.
func findAgentByIdentity(cfg *City, identity string) (Agent, bool) {
	for i := range cfg.Agents {
		if AgentMatchesIdentity(&cfg.Agents[i], identity) {
			return cfg.Agents[i], true
		}
	}
	return Agent{}, false
}

// writeCityBytesForEdit reads tomlPath's raw bytes, runs mutate over them,
// and -- if mutate succeeds -- writes the result back atomically and
// re-persists rig site bindings from cfg, restoring the prior city.toml and
// site binding if that persist step fails. It mirrors
// AppendRigAndWriteSiteBindingsForEdit's I/O shape so byte-preserving agent
// edits get the same crash-safety guarantees as the rig-append path.
func writeCityBytesForEdit(fs fsys.FS, tomlPath string, cfg *City, mutate func(existing []byte) ([]byte, error)) error {
	cityRoot := filepath.Dir(tomlPath)
	writePath, err := ResolveCityAppendPath(fs, tomlPath)
	if err != nil {
		return err
	}

	existing, err := fs.ReadFile(writePath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", writePath, err)
	}

	content, err := mutate(existing)
	if err != nil {
		return err
	}

	snapshot, err := snapshotCityAndSiteFiles(fs, writePath, SiteBindingPath(cityRoot))
	if err != nil {
		return err
	}
	if err := fsys.WriteFileIfChangedAtomic(fs, writePath, content, 0o644); err != nil {
		return err
	}
	if err := persistRigSiteBindings(fs, cityRoot, cfg.Rigs, nil); err != nil {
		if restoreErr := snapshot.restore(fs); restoreErr != nil {
			return fmt.Errorf("writing .gc/site.toml failed and restoring city.toml/site binding failed: %w", errors.Join(err, restoreErr))
		}
		return fmt.Errorf("writing .gc/site.toml failed; restored city.toml and previous site binding, fix the site binding write error and retry: %w", err)
	}
	return nil
}

// toggleAgentSuspendedBytes locates the single array-of-tables block
// starting with header (e.g. "[[agent]]" or "[[patches.agent]]") whose body
// has name/dir matching targetName/targetDir, and returns existing with that
// block's suspended key toggled to the desired state. ok is false when zero
// or more than one block matches, since a byte-preserving edit cannot guess
// which one the caller meant.
func toggleAgentSuspendedBytes(existing []byte, header, targetName, targetDir string, suspended bool) ([]byte, bool) {
	lines := strings.Split(string(existing), "\n")
	start, end, ok := findTOMLArrayBlock(lines, header, targetName, targetDir)
	if !ok {
		return nil, false
	}
	return []byte(strings.Join(toggleSuspendedInRange(lines, start, end, suspended), "\n")), true
}

// findTOMLArrayBlock scans lines for array-of-tables blocks introduced by a
// line exactly equal to header (after trimming whitespace), and returns the
// [start, end) body range of the one whose name/dir key values match
// targetName/targetDir. A block's body runs from its header to the next line
// that opens a table/array-of-tables ("[...]"), or to EOF. ok is true only
// when exactly one block matches -- ambiguity is refused, not guessed at.
func findTOMLArrayBlock(lines []string, header, targetName, targetDir string) (start, end int, ok bool) {
	matches := 0
	for i, line := range lines {
		if strings.TrimSpace(line) != header {
			continue
		}
		bodyStart := i + 1
		bodyEnd := len(lines)
		for j := bodyStart; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "[") {
				bodyEnd = j
				break
			}
		}

		name, dir, hasName := "", "", false
		for j := bodyStart; j < bodyEnd; j++ {
			if m := tomlNameLineRe.FindStringSubmatch(lines[j]); m != nil {
				name, hasName = m[1], true
				continue
			}
			if m := tomlDirLineRe.FindStringSubmatch(lines[j]); m != nil {
				dir = m[1]
			}
		}
		if hasName && name == targetName && dir == targetDir {
			matches++
			start, end = bodyStart, bodyEnd
		}
	}
	return start, end, matches == 1
}

// toggleSuspendedInRange returns lines with the suspended key within
// [start, end) toggled to reflect suspended: an existing suspended = <bool>
// line is replaced in place (preserving its exact spacing and any trailing
// comment); when suspended is false and no such line exists, lines is
// returned unchanged; when suspended is true and no such line exists, a
// fresh "suspended = true" line is inserted after the body's last non-blank
// line. Suspended is never written out as an explicit false -- Agent.Suspended
// is toml:"suspended,omitempty", so the canonical "not suspended" state is
// the key's absence.
func toggleSuspendedInRange(lines []string, start, end int, suspended bool) []string {
	suspendedIdx := -1
	for i := start; i < end; i++ {
		if tomlSuspendedLineRe.MatchString(lines[i]) {
			suspendedIdx = i
			break
		}
	}

	out := make([]string, 0, len(lines)+1)
	switch {
	case suspendedIdx >= 0 && suspended:
		m := tomlSuspendedLineRe.FindStringSubmatch(lines[suspendedIdx])
		out = append(out, lines[:suspendedIdx]...)
		out = append(out, m[1]+"true"+m[2])
		out = append(out, lines[suspendedIdx+1:]...)
	case suspendedIdx >= 0:
		out = append(out, lines[:suspendedIdx]...)
		out = append(out, lines[suspendedIdx+1:]...)
	case suspended:
		insertAt := lastNonBlankLineIndex(lines, start, end)
		out = append(out, lines[:insertAt+1]...)
		out = append(out, "suspended = true")
		out = append(out, lines[insertAt+1:]...)
	default:
		out = append(out, lines...)
	}
	return out
}

// lastNonBlankLineIndex returns the index of the last non-blank line in
// [start, end), or start-1 if the range has no non-blank line.
func lastNonBlankLineIndex(lines []string, start, end int) int {
	last := start - 1
	for i := start; i < end; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			last = i
		}
	}
	return last
}

// marshalAgentPatchBlock encodes a single [[patches.agent]] block. It
// encodes under a top-level "agent"-tagged field (matching the rigBlock
// trick in AppendRigAndWriteSiteBindingsForEdit) and rewrites the resulting
// header, rather than encoding a nested "patches.agent" field directly, to
// avoid depending on how the TOML encoder emits headers for a struct field
// that is only reachable through an intermediate table.
func marshalAgentPatchBlock(dir, name string, suspended bool) ([]byte, error) {
	type patchBlock struct {
		Agents []AgentPatch `toml:"agent"`
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(patchBlock{Agents: []AgentPatch{{Dir: dir, Name: name, Suspended: &suspended}}}); err != nil {
		return nil, fmt.Errorf("serializing new agent patch block: %w", err)
	}
	return bytes.Replace(buf.Bytes(), []byte("[[agent]]"), []byte("[[patches.agent]]"), 1), nil
}

// appendBlock appends block to existing, ensuring existing ends with a
// newline and separating the two with exactly one blank line -- the same
// convention AppendRigAndWriteSiteBindingsForEdit uses for appended rig
// blocks.
func appendBlock(existing, block []byte) []byte {
	content := make([]byte, len(existing))
	copy(content, existing)
	if len(content) > 0 && content[len(content)-1] != '\n' {
		content = append(content, '\n')
	}
	content = append(content, '\n')
	content = append(content, block...)
	return content
}
