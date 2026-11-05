package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
)

// CityEndpointKeyEdit is one scalar-key edit in city.toml, scoped to the keys
// the beads city endpoint commands own: [dolt] host/port at the city level
// (RigName == "") and dolt_host/dolt_port on a [[rigs]] block identified by
// its name key. Value is the raw TOML scalar to write (already quoted for
// strings); an empty Value removes the key.
type CityEndpointKeyEdit struct {
	RigName string
	Key     string
	Value   string
}

// ApplyCityEndpointKeyEditsInPlace applies the given scalar edits to the
// city.toml at tomlPath by editing lines in place, preserving every other
// byte — comments, key order, formatting, and unknown keys. This is the
// comment-preserving counterpart to the struct-rewrite writer, in the same
// spirit as AppendRigAndWriteSiteBindingsForEdit: the marshal round-trip
// strips every comment, so edits the endpoint commands make must not
// re-serialize the whole file.
//
// It returns ok=false (without writing) when the file's layout is not one the
// line editor understands — a missing file, an inline or dotted [dolt] table,
// a quoted key, a rig block it cannot find — or when the edited content fails
// decode-verification against the intended change. Callers fall back to the
// struct rewrite in that case, so exotic layouts keep the old behavior
// instead of risking a corrupt write.
func ApplyCityEndpointKeyEditsInPlace(fs fsys.FS, tomlPath string, edits []CityEndpointKeyEdit) (bool, error) {
	writePath, err := fsys.ResolveSymlinks(fs, tomlPath)
	if err != nil {
		return false, fmt.Errorf("resolving %s for in-place edit: %w", tomlPath, err)
	}
	original, err := fs.ReadFile(writePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s for in-place edit: %w", writePath, err)
	}
	before, err := decodeCityTOMLForEndpointEdit(original)
	if err != nil {
		return false, nil
	}
	if !endpointEditDoltShapeSupported(string(original), before, edits) {
		return false, nil
	}

	edited, ok := applyEndpointKeyEditsToText(string(original), edits)
	if !ok {
		return false, nil
	}
	after, err := decodeCityTOMLForEndpointEdit([]byte(edited))
	if err != nil {
		return false, nil
	}
	if !applyEndpointKeyEditsToConfig(before, edits) {
		return false, nil
	}
	if !reflect.DeepEqual(before, after) {
		return false, nil
	}
	if err := fsys.WriteFileIfChangedAtomic(fs, writePath, []byte(edited), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// decodeCityTOMLForEndpointEdit decodes city.toml bytes into the raw City
// struct with no expansion, for the in-place editor's decode-verification.
func decodeCityTOMLForEndpointEdit(data []byte) (*City, error) {
	cfg := &City{}
	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEndpointKeyEditsToConfig applies the edits' intended semantics to a
// decoded City so the edited text can be verified against it. Returns false
// when an edit does not map onto the struct (unknown key, unknown rig).
func applyEndpointKeyEditsToConfig(cfg *City, edits []CityEndpointKeyEdit) bool {
	for _, edit := range edits {
		if edit.RigName == "" {
			switch edit.Key {
			case "host":
				host := ""
				if edit.Value != "" {
					unquoted, err := strconv.Unquote(edit.Value)
					if err != nil {
						return false
					}
					host = unquoted
				}
				cfg.Dolt.Host = host
			case "port":
				port := 0
				if edit.Value != "" {
					parsed, err := strconv.Atoi(edit.Value)
					if err != nil {
						return false
					}
					port = parsed
				}
				cfg.Dolt.Port = port
			default:
				return false
			}
			continue
		}
		rig := (*Rig)(nil)
		for i := range cfg.Rigs {
			if cfg.Rigs[i].Name == edit.RigName {
				rig = &cfg.Rigs[i]
				break
			}
		}
		if rig == nil {
			return false
		}
		value := ""
		if edit.Value != "" {
			unquoted, err := strconv.Unquote(edit.Value)
			if err != nil {
				return false
			}
			value = unquoted
		}
		switch edit.Key {
		case "dolt_host":
			rig.DoltHost = value
		case "dolt_port":
			rig.DoltPort = value
		default:
			return false
		}
	}
	return true
}

var (
	endpointEditHeaderRe = regexp.MustCompile(`^\s*(\[\[?)\s*([A-Za-z0-9_.-]+)\s*\]?\]\s*(#.*)?$`)
	endpointEditNameRe   = regexp.MustCompile(`^\s*name\s*=\s*"([^"]*)"\s*(#.*)?$`)
)

// endpointEditDoltShapeSupported refuses city-level set edits when the file
// carries dolt config the line scanner cannot see — an inline `dolt = {...}`
// table or dotted `dolt.host = ...` keys. Appending a [dolt] section next to
// one of those would leave two definitions in the file (BurntSushi resolves
// the collision last-wins instead of erroring when other tables intervene, so
// decode-verification alone does not catch it).
func endpointEditDoltShapeSupported(content string, before *City, edits []CityEndpointKeyEdit) bool {
	needsDoltSection := false
	for _, edit := range edits {
		if edit.RigName == "" && edit.Value != "" {
			needsDoltSection = true
			break
		}
	}
	if !needsDoltSection {
		return true
	}
	for _, section := range scanEndpointEditSections(strings.Split(content, "\n")) {
		if section.header == "dolt" && !section.array {
			return true
		}
	}
	return reflect.DeepEqual(before.Dolt, DoltConfig{})
}

// endpointEditSection is a contiguous run of lines under one table header.
type endpointEditSection struct {
	header  string // "dolt" for [dolt], "rigs" for [[rigs]]
	array   bool
	rigName string // decoded name key for [[rigs]] sections
	start   int    // header line index
	end     int    // exclusive: index of next header line or len(lines)
}

// applyEndpointKeyEditsToText performs the linewise edits. Returns ok=false
// when the layout is not editable (the caller then decode-verifies nothing
// and falls back to the struct rewrite).
func applyEndpointKeyEditsToText(content string, edits []CityEndpointKeyEdit) (string, bool) {
	lines := strings.Split(content, "\n")
	sections := scanEndpointEditSections(lines)

	type lineOp struct {
		index   int
		remove  bool
		replace string
		inserts []string
	}
	ops := make([]lineOp, 0, len(edits))
	var appendDolt []string
	removedCityDoltKey := false

	for _, edit := range edits {
		section, found := findEndpointEditSection(sections, edit)
		if !found {
			if edit.RigName != "" {
				return "", false
			}
			if edit.Value == "" {
				// Removing a key from a missing [dolt] table: decode-verify
				// will refuse if the value actually lives elsewhere.
				continue
			}
			appendDolt = append(appendDolt, edit.Key+" = "+edit.Value)
			continue
		}
		keyLine, keyCount := findEndpointEditKeyLine(lines, section, edit.Key)
		if keyCount > 1 {
			return "", false
		}
		if keyCount == 0 {
			if edit.Value == "" {
				continue
			}
			ops = append(ops, lineOp{index: lastEndpointEditKeyLine(lines, section), inserts: []string{edit.Key + " = " + edit.Value}})
			continue
		}
		if edit.Value == "" {
			ops = append(ops, lineOp{index: keyLine, remove: true})
			if edit.RigName == "" {
				removedCityDoltKey = true
			}
			continue
		}
		replaced, ok := replaceEndpointEditValue(lines[keyLine], edit.Key, edit.Value)
		if !ok {
			return "", false
		}
		ops = append(ops, lineOp{index: keyLine, replace: replaced})
	}

	sort.SliceStable(ops, func(i, j int) bool { return ops[i].index > ops[j].index })
	for _, op := range ops {
		switch {
		case op.remove:
			lines = append(lines[:op.index], lines[op.index+1:]...)
		case op.replace != "":
			lines[op.index] = op.replace
		default:
			tail := append([]string(nil), lines[op.index+1:]...)
			lines = append(append(lines[:op.index+1], op.inserts...), tail...)
		}
	}

	if removedCityDoltKey {
		lines = dropEmptiedDoltSection(lines)
	}

	edited := strings.Join(lines, "\n")
	if len(appendDolt) > 0 {
		if edited != "" && !strings.HasSuffix(edited, "\n") {
			edited += "\n"
		}
		edited += "\n[dolt]\n" + strings.Join(appendDolt, "\n") + "\n"
	}
	return edited, true
}

// dropEmptiedDoltSection removes a `[dolt]` header that the edits just emptied,
// so removing the last dolt key is the exact inverse of the append above rather
// than leaving a stray header behind. Without this, use-external followed by
// use-managed does not restore city.toml byte-identically.
//
// Deliberately narrow: only the non-array [dolt] table, and only when nothing
// but blank lines remains under it. A leftover comment is user content — the
// whole point of this file is preserving those — so a section still holding one
// keeps its header rather than orphaning the comment under the previous table.
// [[rigs]] sections are never dropped; they carry identity beyond endpoint keys.
// Narrow in one more way: the caller runs this only when this edit set removed a
// city-level [dolt] key, so a table that was already empty before the edit — say
// during a rig-only edit — is left exactly as the user wrote it.
func dropEmptiedDoltSection(lines []string) []string {
	for _, section := range scanEndpointEditSections(lines) {
		if section.header != "dolt" || section.array {
			continue
		}
		for i := section.start + 1; i < section.end; i++ {
			if strings.TrimSpace(lines[i]) != "" {
				return lines
			}
		}
		// The section's range already covers the header and the blank lines
		// under it — including, when [dolt] is last, the one the append emitted
		// as its leading separator, which sits inside the *previous* section's
		// range and is what carries the file's trailing newline afterwards.
		return append(lines[:section.start], lines[section.end:]...)
	}
	return lines
}

func scanEndpointEditSections(lines []string) []endpointEditSection {
	var sections []endpointEditSection
	for i, line := range lines {
		m := endpointEditHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if len(sections) > 0 {
			sections[len(sections)-1].end = i
		}
		sections = append(sections, endpointEditSection{header: m[2], array: m[1] == "[[", start: i, end: len(lines)})
	}
	for i := range sections {
		if sections[i].header != "rigs" || !sections[i].array {
			continue
		}
		for j := sections[i].start + 1; j < sections[i].end; j++ {
			if m := endpointEditNameRe.FindStringSubmatch(lines[j]); m != nil {
				sections[i].rigName = m[1]
				break
			}
		}
	}
	return sections
}

func findEndpointEditSection(sections []endpointEditSection, edit CityEndpointKeyEdit) (endpointEditSection, bool) {
	for _, section := range sections {
		if edit.RigName == "" {
			if section.header == "dolt" && !section.array {
				return section, true
			}
			continue
		}
		if section.header == "rigs" && section.array && section.rigName == edit.RigName {
			return section, true
		}
	}
	return endpointEditSection{}, false
}

// findEndpointEditKeyLine returns the index of the `key = ...` line inside the
// section body and how many such lines exist (more than one means the file is
// malformed and not safe to edit).
func findEndpointEditKeyLine(lines []string, section endpointEditSection, key string) (int, int) {
	index, count := -1, 0
	re := endpointEditKeyRe(key)
	for i := section.start + 1; i < section.end; i++ {
		if re.MatchString(lines[i]) {
			index, count = i, count+1
		}
	}
	return index, count
}

// lastEndpointEditKeyLine returns the line index a new key should be inserted
// after: the section's last key-value line, or its header when the section
// has no keys yet.
func lastEndpointEditKeyLine(lines []string, section endpointEditSection) int {
	last := section.start
	for i := section.start + 1; i < section.end; i++ {
		if strings.Contains(lines[i], "=") && !strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			last = i
		}
	}
	return last
}

func endpointEditKeyRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(key) + `\s*=\s*)(.*)$`)
}

// replaceEndpointEditValue swaps the scalar value on a `key = value` line,
// keeping the prefix spacing and anything after the value — typically a
// trailing comment — byte for byte.
func replaceEndpointEditValue(line, key, value string) (string, bool) {
	m := endpointEditKeyRe(key).FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	rest := m[2]
	valueEnd, ok := endpointEditScalarEnd(rest)
	if !ok {
		return "", false
	}
	return m[1] + value + rest[valueEnd:], true
}

// endpointEditScalarEnd returns the byte offset just past the scalar value at
// the start of rest, honoring basic and literal string quoting so a '#'
// inside a string is not mistaken for a comment. Only single-line scalars are
// supported; anything else refuses the edit.
func endpointEditScalarEnd(rest string) (int, bool) {
	if rest == "" {
		return 0, false
	}
	switch rest[0] {
	case '"':
		for i := 1; i < len(rest); i++ {
			switch rest[i] {
			case '\\':
				i++
			case '"':
				return i + 1, true
			}
		}
		return 0, false
	case '\'':
		end := strings.IndexByte(rest[1:], '\'')
		if end < 0 {
			return 0, false
		}
		return end + 2, true
	case '{', '[':
		return 0, false
	default:
		end := len(rest)
		if hash := strings.IndexByte(rest, '#'); hash >= 0 {
			end = hash
		}
		return len(strings.TrimRight(rest[:end], " \t")), true
	}
}
