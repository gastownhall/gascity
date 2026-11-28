// Package inputadmission decides whether a native workflow step may start new
// execution given the current readiness facts of its workflow's input.
//
// A graph.v2 root records its input as a convoy (gc.input_convoy_id). The
// step's own graph edges say nothing about that input, so a reader that judges
// readiness from the row and its edges alone serves a step whose input is
// parked, blocked or deferred. This package reads the root -> input convoy ->
// members relation live and returns one typed verdict per row, which every
// admission surface (the ready reader and its demand gate, hook claims, the
// controller's wake and nudge predicates) consults.
//
// A Judge sees only its sources. The ready reader, demand and the controller's
// probes judge over every store the city's ready reader federates, so they
// agree. The hook judges over the store it claims from (and the binding on a
// split city), so a prerequisite only another store holds is beyond it:
// Verdicts reports such a row Unresolved, and the hook takes the federated
// reader's verdict on it rather than deciding it on fewer stores. A custom
// work_query reads through no federated reader, so it has no such verdict to
// take, and the hook says so when it serves such a row.
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
	"github.com/gastownhall/gascity/internal/workflowstop"
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
	// (beads.IsReadyBlockingDependencyType) whose target a judged store
	// holds unsatisfied. A target no judged store holds does not withhold:
	// the row is Unresolved (prerequisiteFact).
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

// Unresolved is the outcome a Judge cannot decide from its sources: nothing
// it read withholds the step, but its input has a ready-blocking prerequisite
// whose target none of its sources holds. Over every store of the city (the
// ready reader's sources) that is a dangling edge, and the step is admitted,
// as bd admits it. A judge over fewer stores (the hook's) cannot tell a
// dangling edge from a prerequisite another store holds open, so its caller
// leaves the step to the federated reader's verdict.
type Unresolved struct {
	// RootID is the step's workflow root.
	RootID string
	// ConvoyID is the root's input convoy (gc.input_convoy_id).
	ConvoyID string
	// InputID is the convoy member whose prerequisite is unresolved.
	InputID string
	// TargetID is the prerequisite no source holds.
	TargetID string
}

// String describes the outcome for a log line.
func (u Unresolved) String() string {
	return fmt.Sprintf("input %s of convoy %s (workflow root %s) waits on prerequisite %s, which no store this judge reads holds", u.InputID, u.ConvoyID, u.RootID, u.TargetID)
}

// Verdicts is one judgment of rows: those that must not start, and those
// whose admission rests on a prerequisite no source holds. A row in neither
// map is admitted.
type Verdicts struct {
	// Withheld is the rows that must not start, keyed by id, with each
	// one's verdict.
	Withheld map[string]Withhold
	// Unresolved is the rows nothing read withholds but a prerequisite no
	// source holds leaves undecided, keyed by id.
	Unresolved map[string]Unresolved
}

// Source is one store a Judge reads, named for the errors that cite it.
type Source struct {
	Label  string
	Reader beads.StoreReader
}

// Subject reports whether row is subject to input admission at all: an open
// row (new execution; an in_progress row is existing ownership) that is not a
// control kind (dispatcher bookkeeping, drain expansion and finalization keep
// running) and names a workflow root, through gc.root_bead_id or by carrying
// gc.input_convoy_id itself.
func Subject(row beads.Bead) bool {
	return row.Status == "open" &&
		!beadmeta.IsControlKind(strings.TrimSpace(row.Metadata[beadmeta.KindMetadataKey])) &&
		rootIDOf(row) != ""
}

// ReadsInput reports whether judging row needs its workflow input's facts:
// row is subject to input admission (Subject), and its workflow root — row
// itself when it carries gc.input_convoy_id, else the bead root finds by id —
// names an input convoy, or is a bead root cannot find. A row it reports false
// for is admitted without reading any input, so a reader that judges rows from
// one store alone may serve it on that store's own verdict; a row it reports
// true for needs a judgment that reads the input (Judge), wherever the input
// lives. A nil root finds nothing.
func ReadsInput(row beads.Bead, root func(id string) (beads.Bead, bool)) bool {
	if !Subject(row) {
		return false
	}
	rootID := rootIDOf(row)
	if rootID == row.ID {
		return true
	}
	if root == nil {
		return true
	}
	b, found := root(rootID)
	return !found || strings.TrimSpace(b.Metadata[beadmeta.InputConvoyIDMetadataKey]) != ""
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
	stopped map[string]bool
}

type rootFact struct {
	bead  beads.Bead
	found bool
}

// convoyFact is one input convoy's verdict: withheld (withhold), else
// unresolved (unknown) when a prerequisite target is in no source, else
// admitted.
type convoyFact struct {
	withhold   Withhold
	withheld   bool
	unknown    Unresolved
	unresolved bool
}

// New returns a Judge over sources, judging deferral at now.
func New(sources []Source, now time.Time) *Judge {
	return &Judge{
		sources: sources,
		now:     now,
		roots:   make(map[string]rootFact),
		convoys: make(map[string]convoyFact),
		stopped: make(map[string]bool),
	}
}

// Judge returns the rows that must not start, keyed by id, with each one's
// verdict: the Withheld half of Verdicts. A row Verdicts reports Unresolved is
// admitted here, which is the answer for a judge over every store of the city
// (a dangling edge), the ready reader's.
func (j *Judge) Judge(ctx context.Context, own int, rows []beads.Bead) (map[string]Withhold, error) {
	v, err := j.judge(ctx, own, rows, false)
	if err != nil {
		return nil, err
	}
	return v.Withheld, nil
}

// Verdicts judges rows as Judge does and also reports the rows it admitted
// although a ready-blocking prerequisite of their input names a target none
// of its sources holds (Unresolved): a judge over fewer stores than the city's
// ready reader federates asks for them, to leave those rows to that reader.
func (j *Judge) Verdicts(ctx context.Context, own int, rows []beads.Bead) (Verdicts, error) {
	return j.judge(ctx, own, rows, true)
}

// judge is Judge and Verdicts; unresolved asks for the Unresolved rows. own is
// the index of the source that served rows (-1 when unknown); a root is read
// from it first. A row whose root no source holds, whose root names no input
// convoy, whose convoy has no member carrying a withholding fact, or whose
// workflow has stopped is admitted. A failed read fails the whole call, naming
// the bead and the source: a row that could not be judged is never admitted
// silently. The one exception is a failed input read for a workflow its
// ancestry proves stopped, whose rows are admitted whatever the input says.
func (j *Judge) judge(ctx context.Context, own int, rows []beads.Bead, unresolved bool) (Verdicts, error) {
	out := Verdicts{Withheld: make(map[string]Withhold)}
	if unresolved {
		out.Unresolved = make(map[string]Unresolved)
	}
	if j == nil {
		return out, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.loadRoots(ctx, own, j.unreadRoots(rows)); err != nil {
		return Verdicts{}, err
	}
	for _, row := range rows {
		if err := j.judgeRow(ctx, own, row, out); err != nil {
			return Verdicts{}, err
		}
	}
	return out, nil
}

// unreadRoots is the workflow roots of rows' subjects this Judge has not read
// yet; a row that is its own root needs none.
func (j *Judge) unreadRoots(rows []beads.Bead) []string {
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
	return need
}

// judgeRow records row's verdict in out: withheld, unresolved when out
// collects those (a non-nil Unresolved), or nothing for an admitted row. A
// stopped workflow's rows are admitted however its input stands, so an input
// read that fails withholds nothing from them: the stop is checked then, and
// only a live workflow's row, or one whose ancestry cannot be read, fails.
// Once ctx has ended no ancestry read starts, and the input read's error
// fails the row.
func (j *Judge) judgeRow(ctx context.Context, own int, row beads.Bead, out Verdicts) error {
	if !Subject(row) {
		return nil
	}
	rootID := rootIDOf(row)
	root := rootFact{bead: row, found: true}
	if rootID != row.ID {
		root = j.roots[rootID]
	}
	convoyID := strings.TrimSpace(root.bead.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if !root.found || convoyID == "" {
		return nil
	}
	fact, err := j.convoyVerdict(ctx, convoyID)
	if err != nil {
		if ctx.Err() == nil {
			stopped, stopErr := j.workflowStopped(own, rootID, row)
			if stopErr == nil && stopped {
				return nil
			}
			err = errors.Join(err, stopErr)
		}
		return fmt.Errorf("judging input admission of %s (workflow root %s): %w", row.ID, rootID, err)
	}
	if !fact.withheld && (!fact.unresolved || out.Unresolved == nil) {
		return nil
	}
	stopped, err := j.workflowStopped(own, rootID, row)
	if err != nil {
		return fmt.Errorf("judging input admission of %s (workflow root %s): %w", row.ID, rootID, err)
	}
	switch {
	case stopped:
	case fact.withheld:
		w := fact.withhold
		w.RootID = rootID
		out.Withheld[row.ID] = w
	default:
		u := fact.unknown
		u.RootID = rootID
		out.Unresolved[row.ID] = u
	}
	return nil
}

// workflowStopped reports, once per root, whether row's workflow has stopped
// (workflowstop.StoppedWorkflowAncestor): a stopped workflow's rows, its
// teardown tail among them, are served as they would be without input
// admission, so cancellation, teardown and retirement run unchanged. The
// ancestry is read across every source, own first.
func (j *Judge) workflowStopped(own int, rootID string, row beads.Bead) (bool, error) {
	if v, done := j.stopped[rootID]; done {
		return v, nil
	}
	stop, err := workflowstop.StoppedWorkflowAncestor(sourcesGetter{j: j, own: own}, row)
	if err != nil {
		return false, err
	}
	j.stopped[rootID] = stop.Stopped()
	return stop.Stopped(), nil
}

// sourcesGetter reads a bead live from the first source, own first, that
// holds it, and answers beads.ErrNotFound when none does.
type sourcesGetter struct {
	j   *Judge
	own int
}

// Get reads id from the Judge's sources.
func (g sourcesGetter) Get(id string) (beads.Bead, error) {
	for _, i := range g.j.order(g.own) {
		src := g.j.sources[i]
		if src.Reader == nil {
			continue
		}
		b, err := beads.LiveFor(src.Reader).Get(id)
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, fmt.Errorf("%s store: %w", src.Label, err)
		}
	}
	return beads.Bead{}, fmt.Errorf("bead %s: %w", id, beads.ErrNotFound)
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

// idRead reads the beads ids name from one store: beads.GetMany, one batched
// read where the store has one, or beads.GetEach, one read per id.
type idRead func(store beads.StoreReader, ids []string) (map[string]beads.Bead, error)

// loadRoots reads ids, the workflow roots not yet memoized, and memoizes every
// one, found or not. The sources are read in lookup order until each id is
// found: first with one batched read each (beads.GetMany), then, for the ids no
// batch answered, by id (beads.GetEach). A batch read can miss the wisp tier
// (bd's is `bd show`), and a root read as absent admits its rows unjudged, so
// a root is absent only once no source holds it in either tier.
func (j *Judge) loadRoots(ctx context.Context, own int, ids []string) error {
	pending, err := j.findRoots(ctx, own, ids, beads.GetMany)
	if err == nil {
		pending, err = j.findRoots(ctx, own, pending, beads.GetEach)
	}
	if err != nil {
		return err
	}
	for _, id := range pending {
		j.roots[id] = rootFact{}
	}
	return nil
}

// findRoots reads ids from the sources in lookup order with read until each is
// found, memoizes each one found, and returns the ids no source answered.
func (j *Judge) findRoots(ctx context.Context, own int, ids []string, read idRead) ([]string, error) {
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
			return nil, err
		}
		found, err := read(src.Reader, pending)
		if err != nil {
			return nil, fmt.Errorf("%s store: reading workflow roots %s: %w", src.Label, strings.Join(pending, ", "), err)
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
	return pending, nil
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
	v, err := j.prerequisiteFact(ctx, convoyID, live)
	if err != nil {
		return convoyFact{}, fmt.Errorf("reading prerequisites of input convoy %s: %w", convoyID, err)
	}
	j.convoys[convoyID] = v
	return v, nil
}

// prerequisiteFact judges members' ready-blocking prerequisites. Each member's
// edges are read from the store that holds it, live, when the convoy is
// judged, so a prerequisite added after the workflow was materialized counts.
// A target is judged from every copy the sources hold
// (beads.DependencySatisfiedByCopies): "external:<id>" names the bead <id>,
// and a capability ref never withholds. The first member, in their order,
// with a target the sources hold unsatisfied withholds the convoy.
//
// A target no source holds does not withhold. Absent anything that
// withholds, the first member with one leaves the convoy unresolved
// (Unresolved). A judge reads only its sources — the hook's reads the store
// it claims from, the ready reader's every store of the city — so absence
// from them cannot tell a prerequisite another store holds from a dangling
// edge. Over every store it is a dangling edge, and the step is admitted,
// bd's answer for an absent target: withholding would strand the workflow for
// good on a deleted target. Over fewer stores it is a question for the
// federated reader, which judges every target any store holds: such a judge
// reports the step Unresolved rather than deciding it on its own read, and
// its caller takes that reader's verdict.
func (j *Judge) prerequisiteFact(ctx context.Context, convoyID string, members []convoy.HeldMember) (convoyFact, error) {
	targetsOf, targets, err := j.prerequisiteTargets(ctx, members)
	if err != nil || len(targets) == 0 {
		return convoyFact{}, err
	}
	copies, err := j.copiesOf(ctx, targets)
	if err != nil {
		return convoyFact{}, err
	}
	var fact convoyFact
	for _, m := range members {
		for _, target := range targetsOf[m.ID] {
			held := copies[target]
			switch {
			case len(held) > 0 && !beads.DependencySatisfiedByCopies(held):
				return convoyFact{withheld: true, withhold: Withhold{Reason: ReasonPrerequisite, ConvoyID: convoyID, InputID: m.ID, BlockerID: target}}, nil
			case len(held) == 0 && !fact.unresolved:
				fact = convoyFact{unresolved: true, unknown: Unresolved{ConvoyID: convoyID, InputID: m.ID, TargetID: target}}
			}
		}
	}
	return fact, nil
}

// prerequisiteTargets reads members' down edges, each from the store that
// holds the member, and returns the targets of each member's ready-blocking
// prerequisites, keyed by member id, and all of them together.
func (j *Judge) prerequisiteTargets(ctx context.Context, members []convoy.HeldMember) (map[string][]string, []string, error) {
	byReader := make(map[int][]string)
	for _, m := range members {
		byReader[m.Reader] = append(byReader[m.Reader], m.ID)
	}
	edges := make(map[string][]beads.Dep, len(members))
	for i, ids := range byReader {
		deps, err := j.depsFrom(ctx, i, ids)
		if err != nil {
			return nil, nil, err
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
	return targetsOf, targets, nil
}

// copiesOf reads every copy of targets the sources hold, keyed by id: one
// batched read per source (beads.GetMany), then, for each target no batch
// answered, one read by id per source (beads.GetEach). A batch read can miss
// the wisp tier (bd's is `bd show`), and a target read as absent leaves its
// row unresolved rather than withheld, so a target is absent only once no
// source holds it in either tier.
func (j *Judge) copiesOf(ctx context.Context, targets []string) (map[string][]beads.Bead, error) {
	copies := make(map[string][]beads.Bead, len(targets))
	if err := j.readCopies(ctx, targets, beads.GetMany, copies); err != nil {
		return nil, err
	}
	var unanswered []string
	for _, id := range targets {
		if len(copies[id]) == 0 {
			unanswered = append(unanswered, id)
		}
	}
	if err := j.readCopies(ctx, unanswered, beads.GetEach, copies); err != nil {
		return nil, err
	}
	return copies, nil
}

// readCopies reads ids from every source with read and adds each copy found to
// copies.
func (j *Judge) readCopies(ctx context.Context, ids []string, read idRead, copies map[string][]beads.Bead) error {
	if len(ids) == 0 {
		return nil
	}
	for _, src := range j.sources {
		if src.Reader == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		found, err := read(src.Reader, ids)
		if err != nil {
			return fmt.Errorf("%s store: reading prerequisites %s: %w", src.Label, strings.Join(ids, ", "), err)
		}
		for id, b := range found {
			copies[id] = append(copies[id], b)
		}
	}
	return nil
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
