package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// This file is cmd/gc's side of the one class-must-not-flip guard
// (coordclass.CheckFlip). On a split city the work stores hold only work beads
// and the class binding only its infrastructure classes. A write that changes
// a bead's class IN PLACE strands it in a store its class is never read from,
// and one stranded row in the city work store makes every later routed gc
// command refuse until the fleet is stopped and the row repaired. Every door
// that lets a caller write a bead's type, labels or metadata states that write
// as a coordclass.Mutation and asks CheckFlip. The doors, each with its own
// table-driven test:
//
//   - gc bd (doBd → bdClassFlipGuard): on a split city an allowlist of
//     vetted commands and flags (bd_split_allowlist.go), whose mutating
//     entries are judged here. TestGcBdSplitAllowlistDecidesEveryCommand
//     decides every command and alias in the pinned bd inventory;
//     TestGcBdRefusesAClassFlipOnASplitCity and
//     class_flip_guard_review_test.go are the behavior tables.
//   - gc bd's by-id door, binding side (serveBdByIDResolved):
//     TestGcBdByIDUpdateRefusesTurningABindingResidentIntoWork.
//   - gc formula cook --meta (cookMetaClassFlip):
//     TestFormulaCookMetaRefusesAClassFlipOnASplitCity.
//   - gc beads metadata-cas (beadsMetadataCASClassFlipRefusal):
//     TestBeadsMetadataCASRefusesAClassFlipOnASplitCity.
//   - the bead API's PATCH/POST update (internal/api refuseClassFlip):
//     TestBeadAPIUpdateRefusesAClassFlipOnASplitCity.
//
// The answer to "which classes does this store hold" is pure configuration,
// the same input relocatedBeadClasses and the create refusal read, so it is the
// same before and after a migration has physically moved the beads. A city
// that relocates nothing answers nil, and nil disarms the guard: single-store
// cities see no behavior change.

// storeClassHolds returns the classes the store behind binding holds on this
// city, or nil when the city relocates nothing (its one store holds every
// class). config.StorageWorkBinding names the work stores — the city's and
// every rig's — which hold work plus any class no binding serves.
func storeClassHolds(cfg *config.City, binding string) []coordclass.Class {
	if cfg == nil || cfg.Storage == nil {
		return nil
	}
	storage := cfg.EffectiveStorage()
	relocated := false
	var holds []coordclass.Class
	if binding == config.StorageWorkBinding {
		holds = append(holds, coordclass.ClassWork)
	}
	for _, class := range infraMigrationClasses {
		served := strings.TrimSpace(storage.Classes.BindingFor(class))
		if served == "" {
			served = config.StorageWorkBinding
		}
		if served != config.StorageWorkBinding {
			relocated = true
		}
		if served == binding {
			holds = append(holds, coordclassFor(string(class)))
		}
	}
	if !relocated {
		return nil
	}
	return holds
}

// workStoreClassHolds is storeClassHolds for a work store.
func workStoreClassHolds(cfg *config.City) []coordclass.Class {
	return storeClassHolds(cfg, config.StorageWorkBinding)
}

// routesClassHolds is storeClassHolds for a door that already holds the
// opened routes and the store it is about to write: a binding holds the
// classes routed to it, any other store (the city or a rig work store) holds
// work plus every class the routes leave on work. Routes that relocate
// nothing — a single-store city — answer nil.
func routesClassHolds(routes *storageRoutes, store beads.Store) []coordclass.Class {
	if routes == nil || len(routes.stores) == 0 {
		return nil
	}
	var holds []coordclass.Class
	for _, class := range coordclass.Classes() {
		if served, ok := routes.stores[class]; ok && served == store {
			holds = append(holds, class)
		}
	}
	if len(holds) > 0 {
		return holds
	}
	holds = []coordclass.Class{coordclass.ClassWork}
	for _, class := range coordclass.Classes() {
		if _, relocated := routes.stores[class]; class.IsInfrastructure() && !relocated {
			holds = append(holds, class)
		}
	}
	return holds
}

// classFlipRefusalText renders a refused flip for a CLI door. who names the
// write ("gc bd update demo-1"), and the text ends with the command that does
// what the caller was after.
func classFlipRefusalText(who string, flip *coordclass.Flip) string {
	return fmt.Sprintf("%s: refused: %s. %s", who, flip.Error(), flip.Guidance())
}

// bdClassGuard is what the gc bd class-must-not-flip guard decided for one
// invocation. A non-empty refusal stops the passthrough before bd runs. args
// is the argv to forward (gc's own flags, such as import --skip-infra, are
// consumed); stdin, when non-nil, replaces os.Stdin because the guard had to
// read it to judge it.
type bdClassGuard struct {
	refusal string
	// kind and path describe a refusal for telemetry (the gc_bd.refused
	// event): which gate refused, and the command path when known.
	kind    string
	path    string
	args    []string
	stdin   io.Reader
	notice  string
	cleanup func()
}

// The gates a split-city gc bd refusal can come from.
const (
	bdRefusalUninventoried = "bd-uninventoried"
	bdRefusalAllowlist     = "allowlist"
	bdRefusalJudge         = "judge"
)

// bdLabelFlagValues parses a label FLAG value — update's --add-label, -l,
// --remove-label and --set-labels, and create's --labels/-l/--label — exactly
// as bd does: they are cobra StringSlice flags, which read the value as ONE
// CSV record (so `"wisp"` is the label wisp, quotes stripped), and bd then
// trims each label and drops empty ones. A value bd's own parse rejects is an
// error; the caller refuses rather than guesses.
func bdLabelFlagValues(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	record, err := csv.NewReader(strings.NewReader(value)).Read()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range record {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// bdLabelVerbList splits the label argument of `bd label add|remove|propagate`
// the way bd's label verbs do (splitLabelArg): on plain commas, trimming each,
// with no CSV quoting — so `"wisp"` there is a label spelled with quotes, not
// wisp.
func bdLabelVerbList(value string) []string {
	var out []string
	for _, l := range strings.Split(value, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// sqlDoltCallRE matches a Dolt procedure called as a function.
var sqlDoltCallRE = regexp.MustCompile(`(?i)\bdolt_[a-z_]+\s*\(`)

// sqlBacktickDoltRE matches a backtick-quoted Dolt name (`dolt_merge`), which a
// plain identifier match does not see and which can name a procedure or a
// system table.
var sqlBacktickDoltRE = regexp.MustCompile("(?i)`\\s*dolt_")

// sqlReadOnlyProblem returns why query is not a single plain read, or "".
func sqlReadOnlyProblem(query string) string {
	q := strings.TrimSpace(query)
	q = strings.TrimSpace(strings.TrimSuffix(q, ";"))
	switch {
	case q == "":
		return "the query is empty"
	case strings.Contains(q, "/*"), strings.Contains(q, "--"), strings.Contains(q, "#"):
		return "the query carries a comment (/*, -- or #)"
	case strings.Contains(q, ";"):
		return "the query holds more than one statement"
	case sqlDoltCallRE.MatchString(q):
		return "the query calls a Dolt procedure"
	case sqlBacktickDoltRE.MatchString(q):
		return "the query names a backtick-quoted Dolt object"
	}
	words := strings.FieldsFunc(q, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '('
	})
	switch strings.ToUpper(words[0]) {
	case "SELECT", "SHOW":
		return ""
	case "DESCRIBE", "DESC", "EXPLAIN":
		if len(words) > 1 {
			switch strings.ToUpper(words[1]) {
			case "UPDATE", "INSERT", "DELETE", "REPLACE", "ANALYZE", "WITH", "CALL", "TABLE":
				return "the EXPLAIN/DESCRIBE wraps a statement that is not a plain SELECT"
			}
		}
		return ""
	default:
		return fmt.Sprintf("%q is not a read statement", words[0])
	}
}

// bdDeleteAllowDanglingFlag is gc's own flag on `gc bd delete`: delete a work
// bead even though a bead in the class binding depends on it. gc consumes it;
// bd never sees it.
const bdDeleteAllowDanglingFlag = "--gc-allow-dangling-edges"

// bdGCOwnFlags are gc's own bool flags on a gc bd verb; gc consumes them and
// bd never sees them.
var bdGCOwnFlags = map[string]string{
	"import": bdImportSkipInfraFlag,
	"delete": bdDeleteAllowDanglingFlag,
}

// stripGCOwnFlags removes the verb's gc-only flag from argv and reports
// whether it was given.
func stripGCOwnFlags(argv []string) ([]string, bool) {
	verb, _, ok := bdRelocatedClassVerb(argv)
	flag, has := bdGCOwnFlags[verb]
	if !ok || !has {
		return argv, false
	}
	out := make([]string, 0, len(argv))
	found := false
	for _, a := range argv {
		if a == flag {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// batchScriptClassProblem returns why a bd batch script would write a bead
// outside the work class, or "".
func batchScriptClassProblem(script []byte, holds []coordclass.Class) string {
	safeUpdateKeys := map[string]bool{"status": true, "priority": true, "title": true, "assignee": true, "force": true}
	scanner := bufio.NewScanner(bytes.NewReader(script))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		tokens := tokenizeBdBatchLine(text)
		if len(tokens) == 0 {
			continue
		}
		switch tokens[0] {
		case "close", "dep":
		case "update":
			for _, kv := range tokens[min(2, len(tokens)):] {
				key, _, _ := strings.Cut(kv, "=")
				if !safeUpdateKeys[key] {
					return fmt.Sprintf("line %d updates %q, which gc does not know to be a field that leaves a bead's storage class alone", line, key)
				}
			}
		case "create":
			if len(tokens) < 2 {
				continue
			}
			shape := beads.Bead{Type: tokens[1]}
			if flip := coordclass.CheckFlip(beads.Bead{}, shape, holds); flip != nil {
				return fmt.Sprintf("line %d creates a %s-class bead (type %q) in the work store, where this city never reads that class; mint it with `%s`", line, flip.After, tokens[1], coordclass.MintCommand(flip.After))
			}
		default:
			return fmt.Sprintf("line %d is a batch command gc does not know (%q)", line, tokens[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Sprintf("reading the script: %v", err)
	}
	return ""
}

// tokenizeBdBatchLine splits a batch line the way bd does: whitespace
// separated, with double-quoted tokens where \" and \\ escape.
func tokenizeBdBatchLine(line string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(line) && (line[i+1] == '"' || line[i+1] == '\\'):
			cur.WriteByte(line[i+1])
			i++
		case c == '"':
			inQuote = !inQuote
			have = true
		case !inQuote && (c == ' ' || c == '\t'):
			if have {
				tokens = append(tokens, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// bdImportSkipInfraFlag is gc's own flag on `gc bd import`: import only the
// work-class rows of the file and report the rest. gc consumes it; bd never
// sees it.
const bdImportSkipInfraFlag = "--skip-infra"

// bdImportSplit is a bd export JSONL split by class.
type bdImportSplit struct {
	keep    [][]byte // rows of a class the store holds, and the lines bd imports as no issue (the header, memories)
	skipped int      // issue rows of a class the store does not hold
	byClass map[coordclass.Class]int
	// keptIDs are the kept issue rows' ids; keptDeps every depends_on_id
	// their dependencies name.
	keptIDs  []string
	keptDeps []string
}

// bdImportNonIssueLine reports whether bd's import reads a JSONL line as
// something other than an issue, mirroring bd's dispatch exactly
// (parseImportRecords in cmd/bd/import.go, bd 1.3.2 and 1.3.0 Enterprise): a
// line carrying a _schema key is the export header and is skipped, a line
// whose _type key — that exact spelling — holds the string "memory" is a
// memory record, and an issue whose status is "tombstone" is skipped. bd
// imports every other line as an issue, whatever else its _type says.
func bdImportNonIssueLine(raw []byte) (bool, error) {
	var peek map[string]json.RawMessage
	if err := json.Unmarshal(raw, &peek); err != nil {
		return false, err
	}
	if _, header := peek["_schema"]; header {
		return true, nil
	}
	if kind, ok := peek["_type"]; ok {
		var s string
		if json.Unmarshal(kind, &s) == nil && s == "memory" {
			return true, nil
		}
	}
	// bd decodes the issue with encoding/json, as here, and a status that is
	// not a string fails its whole import.
	var issue struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &issue); err != nil {
		return false, err
	}
	return issue.Status == "tombstone", nil
}

// splitBdImportRows splits a bd export JSONL into the lines to keep and counts
// the rest by class. A row is kept when the target store HOLDS its class — on
// a split city's work store that is work plus any class the city leaves on
// work — not merely when it is work. Every line bd imports as an issue is
// classified (bdImportNonIssueLine decides which those are).
func splitBdImportRows(data []byte, holds []coordclass.Class) (bdImportSplit, string) {
	split := bdImportSplit{byClass: map[coordclass.Class]int{}}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		nonIssue, err := bdImportNonIssueLine(raw)
		if err != nil {
			return bdImportSplit{}, fmt.Sprintf("line %d is not a JSON object gc can classify (%v)", line, err)
		}
		keepLine := append([]byte(nil), raw...)
		if nonIssue {
			split.keep = append(split.keep, keepLine)
			continue
		}
		var row struct {
			ID           string           `json:"id"`
			Type         string           `json:"issue_type"`
			Labels       []string         `json:"labels"`
			Metadata     *beads.StringMap `json:"metadata"`
			Parent       string           `json:"parent"`
			Dependencies []struct {
				DependsOnID string `json:"depends_on_id"`
			} `json:"dependencies"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return bdImportSplit{}, fmt.Sprintf("line %d is not a JSON object gc can classify (%v)", line, err)
		}
		b := beads.Bead{Type: row.Type, Labels: row.Labels}
		if row.Metadata != nil {
			b.Metadata = map[string]string(*row.Metadata)
		}
		if class := coordclass.Classify(b); !slices.Contains(holds, class) {
			split.skipped++
			split.byClass[class]++
			continue
		}
		split.keep = append(split.keep, keepLine)
		split.keptIDs = append(split.keptIDs, row.ID)
		for _, d := range row.Dependencies {
			if d.DependsOnID != "" {
				split.keptDeps = append(split.keptDeps, d.DependsOnID)
			}
		}
		if row.Parent != "" {
			split.keptDeps = append(split.keptDeps, row.Parent)
		}
	}
	if err := scanner.Err(); err != nil {
		return bdImportSplit{}, fmt.Sprintf("reading the file: %v", err)
	}
	return split, ""
}

// formatClassCounts renders {graph: 3, sessions: 1} as "3 graph, 1 sessions".
func formatClassCounts(byClass map[coordclass.Class]int) string {
	classes := make([]coordclass.Class, 0, len(byClass))
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i] < classes[j] })
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%d %s", byClass[c], c))
	}
	return strings.Join(parts, ", ")
}
