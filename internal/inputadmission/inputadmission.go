// Package inputadmission decides whether a native workflow step may start new
// execution given the current readiness facts of its workflow's input.
//
// A graph.v2 root records its input as a convoy (gc.input_convoy_id). The
// step's own graph edges say nothing about that input, so a reader that judges
// readiness from the row and its edges alone serves a step whose input is
// parked, blocked or deferred. This package reads the root -> input convoy ->
// members relation live and returns one typed verdict per row, which every
// admission surface (the ready reader and its demand gate, hook claims, the
// controller's wake and nudge predicates) consults so they cannot disagree.
//
// Nothing here is copied into the workflow: each Judge reads current facts and
// memoizes them only for its own life, one read or one controller pass.
package inputadmission

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
)

// Reason names the input fact that withholds a step.
type Reason string

const (
	// ReasonHeld is an input carrying a canonical dispatch hold
	// (beadmeta.DispatchHoldLabels).
	ReasonHeld Reason = "held"
	// ReasonBlocked is an input whose store records it blocked.
	ReasonBlocked Reason = "blocked"
	// ReasonDeferred is an input that is deferred (beads.IsDeferred).
	ReasonDeferred Reason = "deferred"
	// ReasonPrerequisite is an input with a ready-blocking dependency
	// (beads.IsReadyBlockingDependencyType) that no store holds satisfied.
	ReasonPrerequisite Reason = "prerequisite"
)

// Withhold is the verdict that a step must not start: which input fact holds
// it, and the relation it was read through.
type Withhold struct {
	// Reason is the input fact that withholds the step.
	Reason Reason
	// RootID is the step's workflow root.
	RootID string
	// ConvoyID is the root's input convoy (gc.input_convoy_id).
	ConvoyID string
	// InputID is the convoy member whose fact withholds the step.
	InputID string
	// BlockerID is the unsatisfied prerequisite of InputID, for a
	// prerequisite verdict.
	BlockerID string
}

// String describes the verdict for a log line.
func (w Withhold) String() string {
	s := fmt.Sprintf("input %s of convoy %s (workflow root %s) is %s", w.InputID, w.ConvoyID, w.RootID, w.Reason)
	if w.BlockerID != "" {
		s += " by " + w.BlockerID
	}
	return s
}

// Source is one store a Judge reads, named for the errors that cite it.
type Source struct {
	Label  string
	Reader beads.StoreReader
}

// Subject reports whether row is subject to input admission at all: an open
// row (new execution; an in_progress row is existing ownership) that names a
// workflow root, through gc.root_bead_id or by carrying gc.input_convoy_id
// itself.
func Subject(row beads.Bead) bool {
	return row.Status == "open" && rootIDOf(row) != ""
}

// rootIDOf is the workflow root row belongs to: its gc.root_bead_id when that
// names another bead, else itself when it carries gc.input_convoy_id, else "".
func rootIDOf(row beads.Bead) string {
	if rootID := strings.TrimSpace(row.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != "" && rootID != row.ID {
		return rootID
	}
	if strings.TrimSpace(row.Metadata[beadmeta.InputConvoyIDMetadataKey]) != "" {
		return row.ID
	}
	return ""
}

// memberReason is the rule for one non-terminal input member: the first fact
// it carries that says "do not start yet", or "" when it carries none. Routing
// and ownership facts (status open or in_progress, an assignee, any other
// label) never withhold.
func memberReason(m beads.Bead, now time.Time) Reason {
	switch {
	case beadmeta.HasDispatchHold(m.Labels):
		return ReasonHeld
	case m.Status == "blocked" || m.SourceStatus == "blocked":
		return ReasonBlocked
	case beads.IsDeferred(m, now):
		return ReasonDeferred
	}
	return ""
}

// Judge judges rows against their workflows' current input facts. Each Judge
// memoizes the roots and convoy verdicts it reads, so build one per read or
// controller pass and never keep it across them.
type Judge struct {
	sources []Source
	now     time.Time

	mu      sync.Mutex
	roots   map[string]rootFact
	convoys map[string]convoyFact
}

type rootFact struct {
	bead  beads.Bead
	found bool
}

type convoyFact struct {
	withhold Withhold
	withheld bool
}

// New returns a Judge over sources, judging deferral at now.
func New(sources []Source, now time.Time) *Judge {
	return &Judge{
		sources: sources,
		now:     now,
		roots:   make(map[string]rootFact),
		convoys: make(map[string]convoyFact),
	}
}

// Judge returns the rows that must not start, keyed by id, with each one's
// verdict. own is the index of the source that served rows (-1 when unknown);
// a root is read from it first. A row whose root no source holds, whose root
// names no input convoy, or whose convoy has no member carrying a withholding
// fact is admitted. A failed read fails the whole call, naming the bead and
// the source: a row that could not be judged is never admitted silently.
func (j *Judge) Judge(ctx context.Context, own int, rows []beads.Bead) (map[string]Withhold, error) {
	out := make(map[string]Withhold)
	if j == nil {
		return out, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	var need []string
	for _, row := range rows {
		if !Subject(row) {
			continue
		}
		if rootID := rootIDOf(row); rootID != row.ID {
			if _, done := j.roots[rootID]; !done {
				need = append(need, rootID)
			}
		}
	}
	if err := j.loadRoots(ctx, own, need); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if !Subject(row) {
			continue
		}
		rootID := rootIDOf(row)
		root := rootFact{bead: row, found: true}
		if rootID != row.ID {
			root = j.roots[rootID]
		}
		if !root.found {
			continue
		}
		convoyID := strings.TrimSpace(root.bead.Metadata[beadmeta.InputConvoyIDMetadataKey])
		if convoyID == "" {
			continue
		}
		verdict, err := j.convoyVerdict(ctx, convoyID)
		if err != nil {
			return nil, fmt.Errorf("judging input admission of %s (workflow root %s): %w", row.ID, rootID, err)
		}
		if !verdict.withheld {
			continue
		}
		w := verdict.withhold
		w.RootID = rootID
		out[row.ID] = w
	}
	return out, nil
}

// order is the sources in the order a root is looked up: own first.
func (j *Judge) order(own int) []int {
	idx := make([]int, 0, len(j.sources))
	if own >= 0 && own < len(j.sources) {
		idx = append(idx, own)
	}
	for i := range j.sources {
		if i != own {
			idx = append(idx, i)
		}
	}
	return idx
}

// loadRoots reads ids, the workflow roots not yet memoized, from the sources
// in lookup order until each is found, and memoizes every one, found or not.
func (j *Judge) loadRoots(ctx context.Context, own int, ids []string) error {
	pending := ids
	for _, i := range j.order(own) {
		if len(pending) == 0 {
			break
		}
		src := j.sources[i]
		if src.Reader == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		found, err := beads.GetMany(src.Reader, pending)
		if err != nil {
			return fmt.Errorf("%s store: reading workflow roots %s: %w", src.Label, strings.Join(pending, ", "), err)
		}
		var still []string
		for _, id := range pending {
			if b, ok := found[id]; ok {
				j.roots[id] = rootFact{bead: b, found: true}
				continue
			}
			still = append(still, id)
		}
		pending = still
	}
	for _, id := range pending {
		j.roots[id] = rootFact{}
	}
	return nil
}

// convoyVerdict judges convoyID's current members, once per Judge.
func (j *Judge) convoyVerdict(ctx context.Context, convoyID string) (convoyFact, error) {
	if v, done := j.convoys[convoyID]; done {
		return v, nil
	}
	if err := ctx.Err(); err != nil {
		return convoyFact{}, err
	}
	readers := make([]beads.StoreReader, len(j.sources))
	labels := make([]string, len(j.sources))
	for i, src := range j.sources {
		readers[i] = src.Reader
		labels[i] = src.Label
	}
	members, err := convoy.MembersAcross(readers, labels, convoyID, false)
	if err != nil {
		return convoyFact{}, fmt.Errorf("reading input convoy %s: %w", convoyID, err)
	}
	var live []convoy.HeldMember
	for _, m := range members {
		if convoy.IsUnresolvedTrackedItem(m.Bead) || convoy.IsTerminalStatus(m.Status) {
			continue
		}
		if reason := memberReason(m.Bead, j.now); reason != "" {
			v := convoyFact{withheld: true, withhold: Withhold{Reason: reason, ConvoyID: convoyID, InputID: m.ID}}
			j.convoys[convoyID] = v
			return v, nil
		}
		live = append(live, m)
	}
	inputID, blockerID, err := j.unsatisfiedPrerequisite(ctx, live)
	if err != nil {
		return convoyFact{}, fmt.Errorf("reading prerequisites of input convoy %s: %w", convoyID, err)
	}
	v := convoyFact{}
	if inputID != "" {
		v = convoyFact{withheld: true, withhold: Withhold{Reason: ReasonPrerequisite, ConvoyID: convoyID, InputID: inputID, BlockerID: blockerID}}
	}
	j.convoys[convoyID] = v
	return v, nil
}

// unsatisfiedPrerequisite returns the first of members, in their order, with a
// ready-blocking dependency that is not satisfied, and that dependency's
// target. Each member's edges are read from the store that holds it, live,
// when the convoy is judged, so a prerequisite added after the workflow was
// materialized counts. A target is judged from every copy the sources hold
// (beads.DependencySatisfiedByCopies): "external:<id>" names the bead <id>, a
// capability ref never withholds, and a target no source holds withholds.
func (j *Judge) unsatisfiedPrerequisite(ctx context.Context, members []convoy.HeldMember) (inputID, blockerID string, err error) {
	byReader := make(map[int][]string)
	for _, m := range members {
		byReader[m.Reader] = append(byReader[m.Reader], m.ID)
	}
	edges := make(map[string][]beads.Dep, len(members))
	for i, ids := range byReader {
		deps, err := j.depsFrom(ctx, i, ids)
		if err != nil {
			return "", "", err
		}
		for id, d := range deps {
			edges[id] = d
		}
	}
	targetsOf := make(map[string][]string, len(members))
	var targets []string
	for _, m := range members {
		for _, dep := range edges[m.ID] {
			if !beads.IsReadyBlockingDependencyType(dep.Type) {
				continue
			}
			target, capability := beads.ExternalBlockerTarget(dep.DependsOnID)
			if capability || target == "" {
				continue
			}
			targetsOf[m.ID] = append(targetsOf[m.ID], target)
			targets = append(targets, target)
		}
	}
	if len(targets) == 0 {
		return "", "", nil
	}
	copies := make(map[string][]beads.Bead, len(targets))
	for _, src := range j.sources {
		if src.Reader == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		found, err := beads.GetMany(src.Reader, targets)
		if err != nil {
			return "", "", fmt.Errorf("%s store: reading prerequisites %s: %w", src.Label, strings.Join(targets, ", "), err)
		}
		for id, b := range found {
			copies[id] = append(copies[id], b)
		}
	}
	for _, m := range members {
		for _, target := range targetsOf[m.ID] {
			if !beads.DependencySatisfiedByCopies(copies[target]) {
				return m.ID, target, nil
			}
		}
	}
	return "", "", nil
}

// depsFrom reads the down edges of ids from sources[i]: one batched read where
// the store has one, otherwise one live DepList per id.
func (j *Judge) depsFrom(ctx context.Context, i int, ids []string) (map[string][]beads.Dep, error) {
	src := j.sources[i]
	if src.Reader == nil {
		return nil, nil
	}
	if batch, ok := beads.DepListBatchFor(src.Reader); ok {
		deps, err := batch.DepListBatch(ids)
		if err == nil {
			return deps, nil
		}
		if !errors.Is(err, beads.ErrDepListBatchUnsupported) {
			return nil, fmt.Errorf("%s store: listing dependencies of %s: %w", src.Label, strings.Join(ids, ", "), err)
		}
	}
	live := beads.LiveFor(src.Reader)
	out := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		deps, err := live.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("%s store: listing dependencies of %s: %w", src.Label, id, err)
		}
		out[id] = deps
	}
	return out, nil
}
