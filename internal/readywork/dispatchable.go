package readywork

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Dispatchable: the count a ready set is honestly measured by.
//
// # The defect this closes
//
// "Ready" and "dispatchable" are not the same population, and every consumer
// that measures queue health has been treating them as one. `gc ready` (and
// `bd ready` behind it) answers "which beads could a claim take right now", and
// that answer legitimately includes rows that no worker can be given:
//
//   - Synthetic input convoys (gc.synthetic=true, issue_type=convoy), which gc
//     MINTS one per dispatch. They are open by construction, sort at P1/P2
//     ahead of real work, and leave the pool only when the convoy lands, so the
//     population grows monotonically with throughput.
//   - Molecule step beads (gc.step_id / gc.root_bead_id), which are the
//     execution DAG of a workflow already in flight, not new work.
//   - Deliberately parked beads (deploy-handoff, hold:*, owner-action,
//     human-decision, human), which by their own text are waiting on something
//     that is not a worker.
//   - Already-routed beads (gc.routed_to / gc.execution_routed_to), which have
//     been handed to a seat and are merely waiting to be claimed.
//   - Assigned beads. Every automatic dispatch tier asks the store for
//     --unassigned rows, so an open row with an assignee is owned work the pool
//     cannot be handed. This class is not in the original four; it was added
//     because the live check found 12 of the 54 rows that survived the first
//     four classes already assigned, which would have reproduced the same
//     over-report one layer down.
//
// Measured on this city's gk store at 2026-11-06T15:1xZ: 40 raw ready rows, 0
// dispatchable. The charter's "keep >=5 ready beads in the queue" floor is
// measured against the raw number, so it read 40 and concluded healthy while
// the pool had nothing to hand out.
//
// # Why this is a READ-ONLY surface
//
// Nothing here changes what `gc ready` returns or what dispatch picks up. The
// raw list stays complete because callers legitimately want convoy rows (the
// convoy reconciler lists them; the triage sweep dedupes against them). This
// classifies the same rows instead, so a measurement can be honest without
// changing behavior — the alternative, filtering inside `gc ready`, would hide
// rows from agents that need them.
//
// # Where the rules live
//
// This is the one place the count is defined, and `gc ready --census` is the
// surface that reports it — that pair is what the Automation Charter's "keep
// >=5 dispatchable beads in the queue" floor is measured against, and the
// charter names the command so the floor cannot drift back onto the raw count.
//
// Every exclusion below is a fact about gc's own vocabulary (beadmeta keys, the
// labels gc's parking conventions use, and the assignee field the dispatch
// tiers filter on), not a policy this file invents. The charter's triage-sweep
// formula applies the same four classes; when one of them changes, it changes in
// beadmeta and this file follows, rather than being re-derived in a second
// place. The assigned class is this file's own addition and has no triage-sweep
// counterpart: the sweep's jq filter runs over `bd ready --json`, whose payload
// carries no assignee field at all, so the sweep never had to decide it.

// Dispatchability is the classification of one ready row: why it is or is not
// dispatchable work.
type Dispatchability struct {
	// Dispatchable is true when the row is work a worker could be handed right
	// now: not synthetic, not a step of some other workflow, not parked, not
	// already routed, and not already assigned.
	Dispatchable bool
	// Class is why, in a stable machine-readable spelling. It is
	// ClassDispatchable ("" is never used — see Classify) for a dispatchable
	// row, and the name of the FIRST exclusion that applies otherwise. The
	// classes are ordered by how structurally certain they are, so a row that
	// is both a step bead and labeled hold:mayor reports "step": it is not work
	// regardless of the label, and a reader wanting to know "how much of this is
	// parked" can still count the label itself.
	Class DispatchClass
}

// DispatchClass names why a ready row is or is not dispatchable work.
type DispatchClass string

const (
	// ClassDispatchable is a row a worker could be handed now.
	ClassDispatchable DispatchClass = "dispatchable"
	// ClassSynthetic is a convoy carrier gc minted for a dispatch: tracking,
	// not work. It covers both the issue_type and the gc.synthetic stamp,
	// because either alone identifies one (convoy.IsSynthetic reads the stamp;
	// the type is what `gc ready` prints).
	ClassSynthetic DispatchClass = "synthetic"
	// ClassStep is a step bead of a molecule already in flight: its execution
	// is owned by the workflow, so handing it to a pool worker would run the
	// same step twice.
	ClassStep DispatchClass = "step"
	// ClassParked is a bead deliberately parked off the queue by a label:
	// deploy-handoff, hold:*, owner-action, human-decision, human.
	ClassParked DispatchClass = "parked"
	// ClassRouted is a bead already handed to a seat, waiting only to be
	// claimed. It is not new supply.
	ClassRouted DispatchClass = "routed"
	// ClassAssigned is an open ready bead that already carries an assignee: it
	// is owned work, and the pool cannot be handed it. Every automatic
	// dispatch tier asks the store for --unassigned rows, so counting an
	// assigned row as supply here would over-report exactly the way the raw
	// ready count does, one layer down.
	ClassAssigned DispatchClass = "assigned"
)

// DispatchClasses is every class Classify can return, in report order: the
// dispatchable rows first, then the exclusions in the order they are tested.
func DispatchClasses() []DispatchClass {
	return []DispatchClass{
		ClassDispatchable,
		ClassSynthetic,
		ClassStep,
		ClassParked,
		ClassRouted,
		ClassAssigned,
	}
}

// parkedLabels are the labels that park a bead off the automatic queue.
//
// hold:* is matched by prefix because the hold taxonomy is open at the value
// (engdocs/contributors/hold-label-conventions.md sanctions exactly two today,
// but the label SHAPE is what marks a pause). The rest are the named parks the
// triage sweep and the Automation Charter already exclude: deploy-handoff (work
// owned off-city), owner-action and human-decision (a decision for a person),
// and bd's native human label (waiting on a person's answer).
var parkedLabels = []string{
	"deploy-handoff",
	"owner-action",
	"human-decision",
	beadmeta.HumanLabel,
}

// holdLabelPrefix parks a bead whose required next actor or condition is named
// by the value; see engdocs/contributors/hold-label-conventions.md.
const holdLabelPrefix = "hold:"

// syntheticTypes are the issue types that are containers or control records
// rather than work a single agent closes. The store already refuses the long
// tail of these at Ready() (beads.readyExcludeTypes: step, gate, message,
// session, agent, role, rig, merge-request, op, ...), so a row of one of those
// types should not reach here at all. The types below are the ones a ready read
// can still hand back, and the ones the charter's triage sweep refuses by name:
//
//   - convoy dominates a busy city's ready set. It is identified twice over:
//     by type, and by the gc.synthetic=true stamp a carrier is minted with.
//     Either signal alone is enough, which is why both are checked.
//   - molecule is the workflow root, whose steps are the actionable unit.
//   - epic and milestone are batch/rollup containers with no single owner.
//
// The set is deliberately LOCAL rather than a call into beads: this classifier
// reads the wire row, and a ready answer can come from a store whose version
// disagrees with this binary's exclusion list. Naming the types here pins what
// the measurement means regardless of which store answered.
//
// This is a NON-ALLOW-LIST's complement: an unknown or empty type is admitted,
// matching the triage sweep's own rule that "unknown type is not evidence of
// not-work". Only types that are structurally not-pickable are excluded.
var syntheticTypes = map[string]bool{
	"convoy":    true,
	"epic":      true,
	"molecule":  true,
	"milestone": true,
}

// Classify reports whether row is dispatchable work and why.
//
// The exclusions are tested in a fixed order (synthetic, step, parked, routed,
// assigned) and the first that applies names the row, so a multi-cause row has
// a stable answer. The order is by structural certainty: a synthetic carrier and
// a step bead are not work by construction, while parked, routed and assigned
// rows are work that something has already decided not to hand out yet.
func Classify(row ReadyBead) Dispatchability {
	if isSyntheticRow(row) {
		return Dispatchability{Class: ClassSynthetic}
	}
	if isStepRow(row) {
		return Dispatchability{Class: ClassStep}
	}
	if isParkedRow(row) {
		return Dispatchability{Class: ClassParked}
	}
	if isRoutedRow(row) {
		return Dispatchability{Class: ClassRouted}
	}
	if isAssignedRow(row) {
		return Dispatchability{Class: ClassAssigned}
	}
	return Dispatchability{Dispatchable: true, Class: ClassDispatchable}
}

// isSyntheticRow reports whether row is a gc-minted container or control
// record rather than work.
func isSyntheticRow(row ReadyBead) bool {
	if syntheticTypes[strings.TrimSpace(row.Type)] {
		return true
	}
	// convoy.IsSynthetic's rule, restated rather than imported: internal/convoy
	// is a workflow package, and this classifier must stay a leaf over the wire
	// shape (ReadyBead) so both `gc ready` and the API can call it.
	return strings.TrimSpace(row.Metadata[beadmeta.SyntheticMetadataKey]) == "true"
}

// isStepRow reports whether row is a step of a molecule: it carries the step
// id, the root it belongs to, or both.
func isStepRow(row ReadyBead) bool {
	return strings.TrimSpace(row.Metadata[beadmeta.StepIDMetadataKey]) != "" ||
		strings.TrimSpace(row.Metadata[beadmeta.RootBeadIDMetadataKey]) != ""
}

// isParkedRow reports whether row carries a label that parks it off the queue.
func isParkedRow(row ReadyBead) bool {
	for _, label := range row.Labels {
		label = strings.TrimSpace(label)
		if strings.HasPrefix(label, holdLabelPrefix) {
			return true
		}
		for _, parked := range parkedLabels {
			if label == parked {
				return true
			}
		}
	}
	return false
}

// isRoutedRow reports whether row has already been handed to a seat.
//
// Both keys are checked: gc.routed_to is stamped by `gc sling` on the source
// bead, and gc.execution_routed_to is the v2 dispatch marker. An empty string
// is not a route — gk-jbg30 carries `"gc.routed_to": ""` while it is mid-merge,
// and treating that as routed would hide a bead that a worker can still act on.
func isRoutedRow(row ReadyBead) bool {
	return strings.TrimSpace(row.Metadata[beadmeta.RoutedToMetadataKey]) != "" ||
		strings.TrimSpace(row.Metadata[beadmeta.ExecutionRoutedToMetadataKey]) != ""
}

// isAssignedRow reports whether row already carries an assignee. It is tested
// LAST, after every structural class: an assigned step bead is still a step
// bead, and a reader asking "how much work is claimed" wants the assigned count
// among rows that are otherwise work.
func isAssignedRow(row ReadyBead) bool {
	return strings.TrimSpace(row.Assignee) != ""
}

// DispatchableSummary is the census a queue-health read reports: how many rows
// a worker could be handed now, and how the rest divide up.
type DispatchableSummary struct {
	// Total is every row classified.
	Total int `json:"total"`
	// Dispatchable is the count of rows a worker could be handed now — the
	// number the charter's ">=5" floor is measured against.
	Dispatchable int `json:"dispatchable"`
	// ByClass is the per-class count, every class present (zero included) so a
	// consumer reading a JSON object never has to tell "absent" from "zero".
	ByClass map[DispatchClass]int `json:"by_class"`
	// Rows is the dispatchable subset, in the input order.
	Rows []ReadyBead `json:"rows"`
}

// Summarize classifies every row and returns the census.
//
// The dispatchable rows are returned as well as counted, because "0 free but N
// in-flight" and "0 free and nothing in flight" are different states (the
// distinction the charter's scheduling order needs), and neither is answerable
// from a count alone — a caller compares Dispatchable against Total.
func Summarize(rows []ReadyBead) DispatchableSummary {
	summary := DispatchableSummary{
		Total:   len(rows),
		ByClass: make(map[DispatchClass]int, len(DispatchClasses())),
		Rows:    make([]ReadyBead, 0, len(rows)),
	}
	for _, class := range DispatchClasses() {
		summary.ByClass[class] = 0
	}
	for _, row := range rows {
		verdict := Classify(row)
		summary.ByClass[verdict.Class]++
		if verdict.Dispatchable {
			summary.Dispatchable++
			summary.Rows = append(summary.Rows, row)
		}
	}
	return summary
}

// InFlight is the count of rows that are work but not dispatchable because
// something has already taken or parked them: step beads of a live workflow,
// rows already routed to a seat, and open beads already assigned.
//
// It is the number that makes "0 free" readable: a pipeline with eight beads
// in flight is busy, while a pipeline with none is starved, and the charter's
// scheduling order escalates only the second.
func (s DispatchableSummary) InFlight() int {
	return s.ByClass[ClassStep] + s.ByClass[ClassRouted] + s.ByClass[ClassAssigned]
}
