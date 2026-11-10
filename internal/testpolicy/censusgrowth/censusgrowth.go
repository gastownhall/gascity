// Package censusgrowth reports how a change moves the source resource census:
// per-row aggregate and Small figures, per-file record changes, moves between
// files, and changes to policy rows. It is a pure function of four files.
package censusgrowth

import (
	"errors"

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

var errNotImplemented = errors.New("censusgrowth: not implemented")

// Compare computes the report for in.
func Compare(Input) (Report, error) { return Report{}, errNotImplemented }

// Growth reports whether any aggregate or Small figure increased or a record
// was added.
func (Report) Growth() bool { return false }

// PolicyRowChanged reports whether any policy row was added, removed or
// changed.
func (Report) PolicyRowChanged() bool { return false }

// Markdown renders the report.
func (Report) Markdown() string { return "" }
