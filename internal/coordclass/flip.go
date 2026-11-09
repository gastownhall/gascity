package coordclass

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Mutation is a proposed write to the three fields [Classify] reads: type,
// labels and metadata. Every door that lets a caller write those fields on an
// existing bead states its write as a Mutation, so one function decides
// whether the write would move the bead out of the class its store holds.
type Mutation struct {
	// Type replaces the bead type when non-nil.
	Type *string
	// SetLabels replaces every label when ReplaceLabels is true (bd's
	// --set-labels). An empty SetLabels with ReplaceLabels clears them.
	SetLabels     []string
	ReplaceLabels bool
	// AddLabels and RemoveLabels are applied after any replacement, in that
	// order, so a label both added and removed ends up removed.
	AddLabels    []string
	RemoveLabels []string
	// SetMetadata writes keys; UnsetMetadata removes keys after the writes.
	SetMetadata   map[string]string
	UnsetMetadata []string
}

// TouchesClass reports whether the mutation writes anything [Classify] reads:
// the type, a label one of its arms matches, or a metadata key one of its arms
// reads. A mutation that does not cannot change a bead's class, whatever the
// bead, so a caller may skip the read it would otherwise need for
// [CheckFlip] — an agent stamping gc.outcome on every step costs nothing extra.
// It reads the same vocabulary table Classify reads through (classifierLabels,
// classifierLabelPrefixes, classifierMetadataKeys), so the two cannot drift.
func (m Mutation) TouchesClass() bool {
	if m.Type != nil || m.ReplaceLabels {
		return true
	}
	for _, labels := range [][]string{m.AddLabels, m.RemoveLabels} {
		for _, l := range labels {
			if isClassifierLabel(l) {
				return true
			}
		}
	}
	for k := range m.SetMetadata {
		if classifierMetadataKeys[k] {
			return true
		}
	}
	for _, k := range m.UnsetMetadata {
		if classifierMetadataKeys[k] {
			return true
		}
	}
	return false
}

// Apply returns the bead the mutation would leave, without touching b.
func (m Mutation) Apply(b beads.Bead) beads.Bead {
	after := b
	if m.Type != nil {
		after.Type = *m.Type
	}
	labels := slices.Clone(b.Labels)
	if m.ReplaceLabels {
		labels = slices.Clone(m.SetLabels)
	}
	for _, l := range m.AddLabels {
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	labels = slices.DeleteFunc(labels, func(l string) bool { return slices.Contains(m.RemoveLabels, l) })
	after.Labels = labels
	if len(m.SetMetadata) > 0 || len(m.UnsetMetadata) > 0 {
		meta := maps.Clone(b.Metadata)
		if meta == nil {
			meta = make(map[string]string, len(m.SetMetadata))
		}
		maps.Copy(meta, m.SetMetadata)
		for _, k := range m.UnsetMetadata {
			delete(meta, k)
		}
		after.Metadata = meta
	}
	return after
}

// Flip is a write [CheckFlip] refused: it would move a bead from class Before
// to class After in a store that holds only the classes in Holds.
type Flip struct {
	Before, After Class
	Holds         []Class
}

// Error describes the flip in the store's terms.
func (f *Flip) Error() string {
	held := make([]string, 0, len(f.Holds))
	for _, c := range f.Holds {
		held = append(held, c.String())
	}
	return fmt.Sprintf("this write would turn a %s-class bead into a %s-class bead in a store that holds only %s beads; "+
		"this city keeps %s beads in a different store, so the bead would be stranded where no %s reader looks "+
		"— where it stays invisible to its class readers, and the city reports itself degraded, until it is repaired",
		f.Before, f.After, strings.Join(held, "/"), f.After, f.After)
}

// Guidance names the supported way to get what the write was after: the gc
// command that makes a bead of the target class in the store that holds it.
func (f *Flip) Guidance() string {
	return fmt.Sprintf("Nothing was written. Make a %s bead with `%s`, which writes the store that holds that class, "+
		"or drop the type, label or metadata that changes the class.", f.After, MintCommand(f.After))
}

// CheckFlip is the one class-must-not-flip guard. It returns the refused flip
// when the write from before to after changes the bead's class AND the store
// the write lands in does not hold the new class; otherwise nil.
//
// holds is the set of classes the target store holds. A nil or empty holds —
// or one that lists every class — means the store holds everything, which is
// a single-store city, and the guard never refuses there.
//
// Only a CHANGE of class is refused. A bead already in the wrong store (a
// stranded row) can still be written as long as the write does not move it to
// another class the store does not hold, and it can be repaired back into a
// class the store does hold.
func CheckFlip(before, after beads.Bead, holds []Class) *Flip {
	if len(holds) == 0 {
		return nil
	}
	from, to := Classify(before), Classify(after)
	if from == to || slices.Contains(holds, to) {
		return nil
	}
	return &Flip{Before: from, After: to, Holds: slices.Clone(holds)}
}

// MintCommand names the gc command that makes a bead of class c in the store
// that holds that class. A refusal quotes it so the operator is left holding a
// command that works rather than only the one that did not.
func MintCommand(c Class) string {
	switch c {
	case ClassWork:
		return "gc bd create"
	case ClassGraph:
		return "gc sling"
	case ClassMessaging:
		return "gc mail send"
	case ClassSessions:
		return "gc session new"
	case ClassOrders:
		return "gc order run"
	case ClassNudges:
		return "gc nudge"
	default:
		return ""
	}
}

// ClassMetadataOnly reports the classifier metadata keys whose removal returns
// b to the work class, when metadata ALONE takes it out of work: no classifier
// label and no class-deciding type is involved. ok is false for a work bead,
// and for any bead whose type or labels decide its class (a gc:session bead,
// a convergence root, a message, a wisp label), because unsetting a key there
// would not make it work, or would make it the wrong thing.
//
// It exists for the stranded-row repair. An agent that copies its workflow
// root onto ordinary work as provenance (`gc.root_bead_id` on a task, the
// substrate 2026-11-09 incident) makes that work graph class, and a split
// city then counts it as stranded. Such a row is repaired in place, on the
// running city, by unsetting exactly these keys: CheckFlip allows a write that
// returns a bead to a class its store holds. Copying it into the binding
// instead would hand ordinary work to a workflow's ownership.
//
// The keys are minimal and sorted: a classifier key present on the bead that
// does not take part in its class (gc.synthetic on a non-convoy) is not named.
func ClassMetadataOnly(b beads.Bead) (keys []string, ok bool) {
	if Classify(b) == ClassWork {
		return nil, false
	}
	for _, l := range b.Labels {
		if isClassifierLabel(l) {
			return nil, false
		}
	}
	present := make([]string, 0, len(classifierMetadataKeys))
	for _, k := range ClassifierMetadataKeys() {
		if strings.TrimSpace(b.Metadata[k]) != "" {
			present = append(present, k)
		}
	}
	if len(present) == 0 || Classify(Mutation{UnsetMetadata: present}.Apply(b)) != ClassWork {
		return nil, false
	}
	// Drop every key the work verdict does not need.
	keys = slices.Clone(present)
	for _, k := range present {
		trial := slices.DeleteFunc(slices.Clone(keys), func(x string) bool { return x == k })
		if Classify(Mutation{UnsetMetadata: trial}.Apply(b)) == ClassWork {
			keys = trial
		}
	}
	return keys, true
}

// DeclassifyCommand is the one spelling of the in-place repair for a bead
// ClassMetadataOnly answered for: the gc bd update that unsets those keys.
func DeclassifyCommand(id string, keys []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gc bd update %s", id)
	for _, k := range keys {
		fmt.Fprintf(&b, " --unset-metadata %s", k)
	}
	return b.String()
}
