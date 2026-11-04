package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

// ErrSurgicalRigRemoveUnsupported indicates RemoveRigAndWriteSiteBindingsForEdit
// could not delete the rig's blocks from city.toml in place without changing
// anything else, so it wrote nothing. Callers must not treat this as success:
// fall back to a full rewrite (accepting comment loss) or surface the error.
var ErrSurgicalRigRemoveUnsupported = errors.New("cannot delete the rig's blocks in place")

// RemoveRig deletes the rig named rigName from c, together with every block
// that refers to it and would dangle once it is gone (#3666): the
// [[patches.agent]] (dir or rig), [[patches.named_session]] (dir),
// [[patches.rigs]] and [[github.pr_monitor]] entries hard-fail a later
// LoadWithIncludes, as do the [[patches.github_pr_monitor]] entries naming one
// of the rig's monitors, and an [[orders.overrides]] entry dangles at
// order-scan time. It reports whether c declared the rig; when it did not, c
// is unchanged.
func (c *City) RemoveRig(rigName string) bool {
	sweep := newRigSweep(c, rigName)
	if !slices.Contains(sweep.rigs, true) {
		return false
	}
	c.Rigs = withoutMarked(c.Rigs, sweep.rigs)
	c.Patches.Agents = withoutMarked(c.Patches.Agents, sweep.agentPatches)
	c.Patches.NamedSessions = withoutMarked(c.Patches.NamedSessions, sweep.namedSessionPatches)
	c.Patches.Rigs = withoutMarked(c.Patches.Rigs, sweep.rigPatches)
	c.GitHub.PRMonitors = withoutMarked(c.GitHub.PRMonitors, sweep.prMonitors)
	c.Patches.GitHubPRMonitors = withoutMarked(c.Patches.GitHubPRMonitors, sweep.prMonitorPatches)
	c.Orders.Overrides = withoutMarked(c.Orders.Overrides, sweep.orderOverrides)
	return true
}

// rigSweep marks, list by list and index by index, the entries of a City that
// RemoveRig deletes for one rig.
type rigSweep struct {
	rigs                []bool
	agentPatches        []bool
	namedSessionPatches []bool
	rigPatches          []bool
	prMonitors          []bool
	prMonitorPatches    []bool
	orderOverrides      []bool
}

// newRigSweep marks the entries of c that RemoveRig deletes for rigName.
func newRigSweep(c *City, rigName string) rigSweep {
	sweep := rigSweep{
		rigs:                marked(c.Rigs, func(r Rig) bool { return r.Name == rigName }),
		agentPatches:        marked(c.Patches.Agents, func(p AgentPatch) bool { return p.Dir == rigName || p.Rig == rigName }),
		namedSessionPatches: marked(c.Patches.NamedSessions, func(p NamedSessionPatch) bool { return p.Dir == rigName }),
		rigPatches:          marked(c.Patches.Rigs, func(p RigPatch) bool { return p.Name == rigName }),
		prMonitors:          marked(c.GitHub.PRMonitors, func(m GitHubPRMonitor) bool { return m.Rig == rigName }),
		orderOverrides:      marked(c.Orders.Overrides, func(o OrderOverride) bool { return o.Rig == rigName }),
	}
	// A [[patches.github_pr_monitor]] has no rig of its own: it names the
	// monitor it patches.
	removedMonitors := map[string]bool{}
	for i, m := range c.GitHub.PRMonitors {
		if sweep.prMonitors[i] {
			removedMonitors[m.Name] = true
		}
	}
	sweep.prMonitorPatches = marked(c.Patches.GitHubPRMonitors, func(p GitHubPRMonitorPatch) bool { return removedMonitors[p.Name] })
	return sweep
}

// byTable keys the sweep's marks by the dotted name of the TOML
// array-of-tables each list decodes from.
func (s rigSweep) byTable() map[string][]bool {
	return map[string][]bool{
		"rigs":                      s.rigs,
		"patches.agent":             s.agentPatches,
		"patches.named_session":     s.namedSessionPatches,
		"patches.rigs":              s.rigPatches,
		"github.pr_monitor":         s.prMonitors,
		"patches.github_pr_monitor": s.prMonitorPatches,
		"orders.overrides":          s.orderOverrides,
	}
}

// marked reports, for each entry of list, whether match holds for it.
func marked[E any](list []E, match func(E) bool) []bool {
	marks := make([]bool, len(list))
	for i, entry := range list {
		marks[i] = match(entry)
	}
	return marks
}

// withoutMarked returns the entries of list whose mark is false.
func withoutMarked[E any](list []E, marks []bool) []E {
	var kept []E
	for i, entry := range list {
		if !marks[i] {
			kept = append(kept, entry)
		}
	}
	return kept
}

// RemoveRigAndWriteSiteBindingsForEdit removes rigName from city.toml by
// deleting its [[rigs]] entry, and every block RemoveRig sweeps with it, from
// the existing text instead of re-serializing the whole file, so every other
// comment and the existing key order survive (#7899). It is the removal
// counterpart of AppendRigAndWriteSiteBindingsForEdit (#3414).
//
// The edit is proven before anything is written: decoded, the new city.toml
// must equal the current one with exactly those entries deleted. When it
// cannot be, nothing is written and the error wraps
// ErrSurgicalRigRemoveUnsupported. A city.toml that does not declare rigName
// is a plain error.
//
// cfg is the city with RemoveRig(rigName) already applied and rig paths
// bound; it supplies the .gc/site.toml rig bindings, from which rigName's is
// dropped. If that write fails after city.toml is changed, both files are
// restored before the error is returned.
func RemoveRigAndWriteSiteBindingsForEdit(fs fsys.FS, tomlPath string, cfg *City, rigName string) error {
	// cityRoot stays the symlink's directory even when city.toml links
	// elsewhere: that is where the city's .gc/ state lives.
	cityRoot := filepath.Dir(tomlPath)
	writePath, err := ResolveCityAppendPath(fs, tomlPath)
	if err != nil {
		return err
	}
	existing, err := fs.ReadFile(writePath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", writePath, err)
	}
	content, err := removeRigFromCityTOML(existing, rigName)
	if err != nil {
		return fmt.Errorf("removing rig %q from %s: %w", rigName, writePath, err)
	}

	snapshot, err := snapshotCityEdit(fs, writePath, cityRoot)
	if err != nil {
		return err
	}
	if err := fsys.WriteFileIfChangedAtomic(fs, writePath, content, 0o644); err != nil {
		return err
	}
	if err := persistRigSiteBindings(fs, cityRoot, cfg.Rigs, map[string]struct{}{rigName: {}}); err != nil {
		if restoreErr := snapshot.restore(fs); restoreErr != nil {
			return fmt.Errorf("writing .gc/site.toml failed and restoring city.toml/site binding failed: %w", errors.Join(err, restoreErr))
		}
		return fmt.Errorf("writing .gc/site.toml failed; restored city.toml and previous site binding, fix the site binding write error and retry: %w", err)
	}
	return nil
}

// removeRigFromCityTOML returns city.toml content with the entries RemoveRig
// deletes for rigName removed from the text, and proves the edit: decoded,
// the result must equal content's own decode with exactly those entries
// deleted. That covers keys gc does not model as well as those it does, so a
// line the scanner misreads can never cost, keep or move another setting;
// the edit wraps ErrSurgicalRigRemoveUnsupported instead.
func removeRigFromCityTOML(content []byte, rigName string) ([]byte, error) {
	var cfg City
	if _, err := toml.Decode(string(content), &cfg); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	sweep := newRigSweep(&cfg, rigName)
	if !slices.Contains(sweep.rigs, true) {
		return nil, errors.New("no [[rigs]] entry declares it")
	}
	marks := sweep.byTable()
	result := removeMarkedTOMLEntries(content, marks)
	if err := checkMarkedEntriesRemoved(content, result, marks); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSurgicalRigRemoveUnsupported, err)
	}
	return result, nil
}

// checkMarkedEntriesRemoved checks that result decodes to content's decode
// with exactly the array-of-tables entries in marks deleted.
func checkMarkedEntriesRemoved(content, result []byte, marks map[string][]bool) error {
	var want, got map[string]any
	if _, err := toml.Decode(string(content), &want); err != nil {
		return fmt.Errorf("parsing the current file: %w", err)
	}
	if _, err := toml.Decode(string(result), &got); err != nil {
		return fmt.Errorf("the edited file would not parse: %w", err)
	}
	if err := deleteMarkedEntries(want, marks); err != nil {
		return err
	}
	pruneEmptyTables(want)
	pruneEmptyTables(got)
	if !reflect.DeepEqual(want, got) {
		return errors.New("its blocks could not be told apart from the rest of the file")
	}
	return nil
}

// deleteMarkedEntries deletes the entries in marks from doc, a TOML document
// decoded into plain maps, where each array of tables keeps its entries in
// file order.
func deleteMarkedEntries(doc map[string]any, marks map[string][]bool) error {
	for table, tableMarks := range marks {
		if !slices.Contains(tableMarks, true) {
			continue
		}
		parent, key := parentTable(doc, table)
		entries, ok := parent[key].([]map[string]any)
		if !ok || len(entries) != len(tableMarks) {
			return fmt.Errorf("its [[%s]] entries are not array-of-tables blocks", table)
		}
		if kept := withoutMarked(entries, tableMarks); len(kept) > 0 {
			parent[key] = kept
		} else {
			delete(parent, key)
		}
	}
	return nil
}

// parentTable returns the table of doc that holds the last part of the dotted
// name table, and that part. The table is nil when doc does not declare it.
func parentTable(doc map[string]any, table string) (map[string]any, string) {
	parts := strings.Split(table, ".")
	for _, part := range parts[:len(parts)-1] {
		doc, _ = doc[part].(map[string]any)
	}
	return doc, parts[len(parts)-1]
}

// pruneEmptyTables deletes the tables of doc that hold nothing, at any depth:
// a table left empty by deleted entries reads the same as one never declared.
func pruneEmptyTables(doc map[string]any) {
	for key, value := range doc {
		if table, ok := value.(map[string]any); ok {
			pruneEmptyTables(table)
			if len(table) == 0 {
				delete(doc, key)
			}
		}
	}
}

// tomlKeyPart matches one part of a dotted TOML key: a bare key, a basic
// string or a literal string.
const tomlKeyPart = `[A-Za-z0-9_-]+|"(?:[^"\\]|\\.)*"|'[^']*'`

var (
	// tomlHeaderPattern matches a [table] or [[array-of-tables]] header line,
	// capturing its opening bracket(s) and its dotted key. Whitespace may
	// surround the key and its dots, and a comment may follow the header.
	tomlHeaderPattern  = regexp.MustCompile(`^[ \t]*(\[\[?)[ \t]*((?:` + tomlKeyPart + `)(?:[ \t]*\.[ \t]*(?:` + tomlKeyPart + `))*)[ \t]*\]\]?[ \t]*(?:#.*)?$`)
	tomlKeyPartPattern = regexp.MustCompile(tomlKeyPart)
)

// tomlHeader parses a [table] or [[array-of-tables]] header line. It returns
// the header's dotted name with quoted parts unquoted, and whether the header
// opens an array-of-tables entry. A trailing \r (a CRLF file) is ignored.
func tomlHeader(line string) (name string, arrayTable, ok bool) {
	m := tomlHeaderPattern.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
	if m == nil {
		return "", false, false
	}
	parts := tomlKeyPartPattern.FindAllString(m[2], -1)
	for i, part := range parts {
		parts[i] = unquoteTOMLKeyPart(part)
	}
	return strings.Join(parts, "."), m[1] == "[[", true
}

// unquoteTOMLKeyPart returns the name a key part stands for. A basic string
// with an escape Go does not know keeps its quotes: it then names no table
// the scanner sweeps, and its line still counts as a header.
func unquoteTOMLKeyPart(part string) string {
	switch part[0] {
	case '\'':
		return part[1 : len(part)-1]
	case '"':
		if name, err := strconv.Unquote(part); err == nil {
			return name
		}
	}
	return part
}

// removeMarkedTOMLEntries deletes from city.toml content the text of the
// array-of-tables entries marks flags, leaving everything else (comments,
// key order, unrelated tables) untouched.
//
// TOML appends one entry to an array of tables per [[name]] header, so the
// n-th [[name]] header of the file opens the entry at index n of that table's
// marks. An entry's text runs from its header to the next header that is not
// one of its own sub-tables ([rigs.formula_vars], [[rigs.overrides]] and the
// like, while a [[rigs]] entry is open). A comment block directly above a
// removed header (no blank line between) goes with it, since an operator's
// rationale comment conventionally sits directly above the table it explains;
// by the same convention a comment block directly above the next header stays
// with that header. That is a convention, not a TOML rule (TOML attaches
// comments to nothing). The scan reads lines, not TOML syntax: a header-shaped
// line inside a multi-line string or array is taken for a header, which is why
// removeRigFromCityTOML checks the result.
func removeMarkedTOMLEntries(content []byte, marks map[string][]bool) []byte {
	lines := strings.Split(string(content), "\n")
	scanner := tomlEntryScanner{lines: lines, marks: marks, seen: map[string]int{}, remove: make([]bool, len(lines))}
	for i, line := range lines {
		if name, arrayTable, ok := tomlHeader(line); ok {
			scanner.header(i, name, arrayTable)
		}
	}
	scanner.closeEntry(len(lines))

	result := strings.Join(withoutMarked(lines, scanner.remove), "\n")
	if strings.HasSuffix(string(content), "\n") && result != "" && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return []byte(result)
}

// tomlEntryScanner tracks the array-of-tables entry removeMarkedTOMLEntries
// is inside and the lines it has marked for removal.
type tomlEntryScanner struct {
	lines  []string
	marks  map[string][]bool
	seen   map[string]int // [[name]] headers seen so far, per marked table
	remove []bool

	open      bool
	openTable string
	openStart int
	openGoes  bool // the open entry is marked for removal
}

// header handles the header on line i: one of the open entry's sub-tables
// stays inside it, and any other header ends it and may open the next entry.
func (s *tomlEntryScanner) header(i int, name string, arrayTable bool) {
	if s.open && strings.HasPrefix(name, s.openTable+".") {
		return
	}
	s.closeEntry(i)
	tableMarks, swept := s.marks[name]
	if !swept || !arrayTable {
		return
	}
	index := s.seen[name]
	s.seen[name]++
	s.open, s.openTable, s.openStart = true, name, i
	s.openGoes = index < len(tableMarks) && tableMarks[index]
}

// closeEntry ends the open entry before line end and, when the entry is
// marked, marks its lines for removal.
func (s *tomlEntryScanner) closeEntry(end int) {
	if !s.open {
		return
	}
	s.open = false
	if !s.openGoes {
		return
	}
	start := leadingCommentStart(s.lines, s.openStart)
	end = trailingCommentStart(s.lines, s.openStart+1, end)
	for i := start; i < end; i++ {
		s.remove[i] = true
	}
}

// leadingCommentStart returns the first line of the comment block directly
// above line i, or i when there is none.
func leadingCommentStart(lines []string, i int) int {
	for i > 0 && isTOMLComment(lines[i-1]) {
		i--
	}
	return i
}

// trailingCommentStart returns the first line of the comment block directly
// above line end, but no earlier than floor, or end when there is none.
func trailingCommentStart(lines []string, floor, end int) int {
	for end > floor && isTOMLComment(lines[end-1]) {
		end--
	}
	return end
}

func isTOMLComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}
