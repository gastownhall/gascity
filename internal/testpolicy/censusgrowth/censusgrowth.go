// Package censusgrowth reports how a change moves the source resource census:
// per-row aggregate and Small figures, per-file record changes, moves between
// files, and changes to policy rows. It is a pure function of four files.
package censusgrowth

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/testpolicy/resourcecensus"
)

// Marker is the hidden first line of every report; a sticky PR comment is
// keyed on it.
const Marker = "<!-- census-growth-report -->"

// ChangeKind says how a record or policy row differs between base and head.
type ChangeKind string

// The kinds of change.
const (
	Added   ChangeKind = "added"
	Removed ChangeKind = "removed"
	Changed ChangeKind = "changed"
)

// Input is the four files a report is computed from. Empty BaseCounts means
// the base predates the counts file.
type Input struct {
	BaseCounts, HeadCounts []byte
	BasePolicy, HeadPolicy []byte
}

// RowDelta is one counted policy row's figures before and after.
type RowDelta struct {
	Scope                resourcecensus.Scope
	Resource             resourcecensus.Resource
	Base, Head           resourcecensus.Count
	BaseSmall, HeadSmall resourcecensus.Count
}

// FileChange is one changed counts record.
type FileChange struct {
	Path       string
	Resource   resourcecensus.Resource
	Kind       ChangeKind
	Base, Head *resourcecensus.CountRecord
}

// Move is a decrease in some files matched by an equal increase of the same
// resource in others.
type Move struct {
	Resource resourcecensus.Resource
	Calls    int
	From, To []string
}

// PolicyChange is one changed policy row.
type PolicyChange struct {
	Section string
	Key     string
	Kind    ChangeKind
	Fields  []string
}

// Report is the result of Compare.
type Report struct {
	BasePredatesCounts bool
	Rows               []RowDelta
	Files              []FileChange
	Moves              []Move
	PolicyChanges      []PolicyChange
}

type recordKey struct {
	path     string
	resource resourcecensus.Resource
}

func indexRecords(records []resourcecensus.CountRecord) map[recordKey]*resourcecensus.CountRecord {
	byKey := make(map[recordKey]*resourcecensus.CountRecord, len(records))
	for i := range records {
		byKey[recordKey{records[i].Path, records[i].Resource}] = &records[i]
	}
	return byKey
}

// Compare computes the report for in. Figures come from resourcecensus's own
// derivations, so the Small rule has one implementation.
func Compare(in Input) (Report, error) {
	if len(in.HeadCounts) == 0 {
		return Report{}, fmt.Errorf("head %s is missing or empty", resourcecensus.CountsPath)
	}
	headRecords, err := resourcecensus.ParseCounts(in.HeadCounts)
	if err != nil {
		return Report{}, fmt.Errorf("head counts: %w", err)
	}
	headLedger, err := resourcecensus.ParseLedger(in.HeadPolicy)
	if err != nil {
		return Report{}, fmt.Errorf("head policy: %w", err)
	}
	if len(in.BaseCounts) == 0 {
		return Report{BasePredatesCounts: true}, nil
	}
	baseRecords, err := resourcecensus.ParseCounts(in.BaseCounts)
	if err != nil {
		return Report{}, fmt.Errorf("base counts: %w", err)
	}
	baseLedger, err := resourcecensus.ParseLedger(in.BasePolicy)
	if err != nil {
		return Report{}, fmt.Errorf("base policy: %w", err)
	}
	report := Report{
		Rows:          compareRows(baseRecords, headRecords, baseLedger, headLedger),
		PolicyChanges: comparePolicy(baseLedger, headLedger),
	}
	report.Files = compareFiles(baseRecords, headRecords)
	report.Moves = findMoves(report.Files)
	return report, nil
}

// countedRows returns the (scope, resource) pairs the counted policy rows of
// either ledger govern, in sorted order.
func countedRows(ledgers ...resourcecensus.Ledger) []RowDelta {
	seen := map[RowDelta]bool{}
	var rows []RowDelta
	for _, ledger := range ledgers {
		for _, section := range [][]resourcecensus.Baseline{ledger.AuditBaseline, ledger.Debt, ledger.SmallDebt} {
			for _, row := range section {
				key := RowDelta{Scope: row.Scope, Resource: row.Resource}
				if !seen[key] {
					seen[key] = true
					rows = append(rows, key)
				}
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Scope != rows[j].Scope {
			return rows[i].Scope < rows[j].Scope
		}
		return rows[i].Resource < rows[j].Resource
	})
	return rows
}

func compareRows(base, head []resourcecensus.CountRecord, baseLedger, headLedger resourcecensus.Ledger) []RowDelta {
	rows := countedRows(baseLedger, headLedger)
	for i := range rows {
		scope, resource := rows[i].Scope, rows[i].Resource
		rows[i].Base = resourcecensus.DerivedCount(base, scope, resource)
		rows[i].Head = resourcecensus.DerivedCount(head, scope, resource)
		rows[i].BaseSmall = resourcecensus.DerivedSmallCount(base, scope, resource)
		rows[i].HeadSmall = resourcecensus.DerivedSmallCount(head, scope, resource)
	}
	return rows
}

func compareFiles(base, head []resourcecensus.CountRecord) []FileChange {
	baseByKey, headByKey := indexRecords(base), indexRecords(head)
	var changes []FileChange
	for key, was := range baseByKey {
		switch now, ok := headByKey[key]; {
		case !ok:
			changes = append(changes, FileChange{Path: key.path, Resource: key.resource, Kind: Removed, Base: was})
		case *now != *was:
			changes = append(changes, FileChange{Path: key.path, Resource: key.resource, Kind: Changed, Base: was, Head: now})
		}
	}
	for key, now := range headByKey {
		if _, ok := baseByKey[key]; !ok {
			changes = append(changes, FileChange{Path: key.path, Resource: key.resource, Kind: Added, Head: now})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Resource < changes[j].Resource
	})
	return changes
}

// callsDelta is a record change's effect on the resource's total calls.
func (c FileChange) callsDelta() int {
	delta := 0
	if c.Head != nil {
		delta += c.Head.Calls
	}
	if c.Base != nil {
		delta -= c.Base.Calls
	}
	return delta
}

// findMoves reports each resource whose per-file decreases exactly equal its
// per-file increases.
func findMoves(files []FileChange) []Move {
	byResource := map[resourcecensus.Resource]*Move{}
	net := map[resourcecensus.Resource]int{}
	var order []resourcecensus.Resource
	for _, change := range files {
		delta := change.callsDelta()
		if delta == 0 {
			continue
		}
		move, ok := byResource[change.Resource]
		if !ok {
			move = &Move{Resource: change.Resource}
			byResource[change.Resource] = move
			order = append(order, change.Resource)
		}
		net[change.Resource] += delta
		if delta > 0 {
			move.Calls += delta
			move.To = append(move.To, change.Path)
		} else {
			move.From = append(move.From, change.Path)
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	var moves []Move
	for _, resource := range order {
		move := byResource[resource]
		if net[resource] == 0 && len(move.From) > 0 && len(move.To) > 0 {
			moves = append(moves, *move)
		}
	}
	return moves
}

// diffSection compares one policy section row by row, keyed by key. A changed
// row names every field whose toml value differs.
func diffSection[T any](section string, base, head []T, key func(T) string) []PolicyChange {
	index := func(rows []T) map[string]T {
		byKey := make(map[string]T, len(rows))
		for _, row := range rows {
			byKey[key(row)] = row
		}
		return byKey
	}
	baseByKey, headByKey := index(base), index(head)
	var changes []PolicyChange
	for k, was := range baseByKey {
		now, ok := headByKey[k]
		switch {
		case !ok:
			changes = append(changes, PolicyChange{Section: section, Key: k, Kind: Removed})
		case !reflect.DeepEqual(was, now):
			changes = append(changes, PolicyChange{Section: section, Key: k, Kind: Changed, Fields: changedFields(was, now)})
		}
	}
	for k := range headByKey {
		if _, ok := baseByKey[k]; !ok {
			changes = append(changes, PolicyChange{Section: section, Key: k, Kind: Added})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Key < changes[j].Key })
	return changes
}

func changedFields[T any](was, now T) []string {
	var fields []string
	wasValue, nowValue := reflect.ValueOf(was), reflect.ValueOf(now)
	for i := 0; i < wasValue.NumField(); i++ {
		if !reflect.DeepEqual(wasValue.Field(i).Interface(), nowValue.Field(i).Interface()) {
			fields = append(fields, wasValue.Type().Field(i).Tag.Get("toml"))
		}
	}
	return fields
}

func comparePolicy(base, head resourcecensus.Ledger) []PolicyChange {
	baselineKey := func(row resourcecensus.Baseline) string { return string(row.Scope) + "/" + string(row.Resource) }
	var changes []PolicyChange
	changes = append(changes, diffSection("audit_baseline", base.AuditBaseline, head.AuditBaseline, baselineKey)...)
	changes = append(changes, diffSection("debt", base.Debt, head.Debt, baselineKey)...)
	changes = append(changes, diffSection("medium", base.Medium, head.Medium, func(row resourcecensus.MediumOwner) string {
		return row.PackageDir + "/" + row.PackageName + "/" + row.Owner
	})...)
	changes = append(changes, diffSection("reviewed_hermetic_body", base.ReviewedHermeticBody, head.ReviewedHermeticBody, func(row resourcecensus.ReviewedHermeticBody) string {
		return row.PackageDir + "/" + row.PackageName + "/" + row.Owner
	})...)
	return append(changes, diffSection("small_debt", base.SmallDebt, head.SmallDebt, baselineKey)...)
}

// Growth reports whether any aggregate or Small figure increased or a record
// was added.
func (r Report) Growth() bool {
	for _, row := range r.Rows {
		if row.Head.Calls > row.Base.Calls || row.Head.Files > row.Base.Files ||
			row.HeadSmall.Calls > row.BaseSmall.Calls || row.HeadSmall.Files > row.BaseSmall.Files {
			return true
		}
	}
	for _, change := range r.Files {
		if change.Kind == Added {
			return true
		}
	}
	return false
}

// PolicyRowChanged reports whether any policy row was added, removed or
// changed.
func (r Report) PolicyRowChanged() bool { return len(r.PolicyChanges) > 0 }

// maxFileLines caps the per-file detail of a report.
const maxFileLines = 50

func arrow(base, head int) string { return fmt.Sprintf("%d → %d", base, head) }

// Markdown renders the report. It opens with Marker so a sticky PR comment can
// find and replace its previous version.
func (r Report) Markdown() string {
	var b strings.Builder
	b.WriteString(Marker + "\n## Resource census report\n\n")
	if r.BasePredatesCounts {
		b.WriteString("base predates the counts file: no delta available\n")
		return b.String()
	}
	if r.Growth() {
		b.WriteString("**GROWTH**: an aggregate or Small figure increased, or a counts record was added.\n\n")
	}
	if r.PolicyRowChanged() {
		b.WriteString("**POLICY-ROW**: a policy row was added, removed or changed. The counts do not cover policy rows, so review each one below.\n\n")
	}
	b.WriteString("| scope | resource | calls | files | Small calls | Small files |\n|---|---|---|---|---|---|\n")
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", row.Scope, row.Resource,
			arrow(row.Base.Calls, row.Head.Calls), arrow(row.Base.Files, row.Head.Files),
			arrow(row.BaseSmall.Calls, row.HeadSmall.Calls), arrow(row.BaseSmall.Files, row.HeadSmall.Files))
	}
	if len(r.Moves) > 0 {
		b.WriteString("\n### Moves\n\n")
		for _, move := range r.Moves {
			fmt.Fprintf(&b, "- %s: %d call(s) moved from %s to %s (nets to zero)\n", move.Resource, move.Calls,
				strings.Join(move.From, ", "), strings.Join(move.To, ", "))
		}
	}
	if len(r.Files) > 0 {
		b.WriteString("\n### Changed records\n\n")
		for i, change := range r.Files {
			if i == maxFileLines {
				fmt.Fprintf(&b, "- and %d more\n", len(r.Files)-maxFileLines)
				break
			}
			fmt.Fprintf(&b, "- %s `%s` (%s): %s\n", change.Kind, change.Path, change.Resource, change.describe())
		}
	}
	if len(r.PolicyChanges) > 0 {
		b.WriteString("\n### Policy rows\n\n")
		for _, change := range r.PolicyChanges {
			fmt.Fprintf(&b, "- %s `%s` in %s", change.Kind, change.Key, change.Section)
			if len(change.Fields) > 0 {
				fmt.Fprintf(&b, ": %s", strings.Join(change.Fields, ", "))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// describe shows a record's figures; a changed record shows before → after.
func (c FileChange) describe() string {
	switch {
	case c.Base == nil:
		return fmt.Sprintf("calls %d, medium %d, tagged %t", c.Head.Calls, c.Head.Medium, c.Head.Tagged)
	case c.Head == nil:
		return fmt.Sprintf("calls %d, medium %d, tagged %t", c.Base.Calls, c.Base.Medium, c.Base.Tagged)
	}
	return fmt.Sprintf("calls %s, medium %s, tagged %t → %t", arrow(c.Base.Calls, c.Head.Calls), arrow(c.Base.Medium, c.Head.Medium), c.Base.Tagged, c.Head.Tagged)
}
