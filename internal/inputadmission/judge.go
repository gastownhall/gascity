// Package inputadmission judges whether a native workflow's rows may be
// admitted to NEW execution given the CURRENT authored readiness facts of the
// input its root's input convoy tracks (#8832).
//
// Every readiness decision below the hook reads only the row itself and its
// own graph edges, so a step whose input is parked on a dispatch hold, blocked,
// deferred or waiting on a prerequisite was served, claimed, counted as
// demand, woken and nudged anyway. This package reads the input's facts live —
// across every store its caller federates, from current relations rather than
// any snapshot copied at materialization — and returns one typed withheld
// verdict the ready reader, the controller's demand pass and the hook's claim
// path all share, so they cannot disagree row for row.
//
// A row is SUBJECT to admission only when it is open work of a workflow whose
// root carries gc.input_convoy_id: an in_progress row is existing ownership
// and is never judged, control kinds keep the dispatcher bookkeeping running,
// and a row of a STOPPED workflow is left to the stop gates and the teardown
// tail that own it. A subject row is WITHHELD when any non-terminal member of
// its input convoy carries a canonical dispatch hold, a raw blocked status, a
// deferral, or an unsatisfied ready-blocking prerequisite. Everything else —
// open dependency-free planning inputs, in_progress, hooked, pinned, in-review
// or in-testing inputs, terminal members — stays eligible: a report must not
// have to close its input to run.
//
// A missing root, an absent or dangling convoy and unresolved placeholder
// members never withhold: retirement and its purge proofs own those cases,
// and hiding such rows from retirement is how a dead workflow loops.
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

// The withholding reasons, one per authored readiness fact an input can carry.
const (
	// ReasonHeld: the input carries a canonical dispatch hold
	// (beadmeta.HasDispatchHold) — hold:mayor or hold:external.
	ReasonHeld = "held"
	// ReasonBlocked: the input's raw status is blocked.
	ReasonBlocked = "blocked"
	// ReasonDeferred: the input is deferred (beads.IsDeferred) — bd's
	// deferred status or a future defer_until.
	ReasonDeferred = "deferred"
	// ReasonPrerequisite: the input has an unsatisfied ready-blocking
	// dependency (beads.IsReadyBlockingDependencyType) whose target is not
	// closed in every store that holds it.
	ReasonPrerequisite = "prerequisite"
)

// Withhold is the typed verdict input admission returns for a withheld row:
// the reason, the workflow root and input convoy it was read from, the input
// member that carried the reason, and — for a prerequisite — the blocker.
// The zero Withhold (an empty Reason) is an admissible row.
type Withhold struct {
	// Reason is one of the Reason constants above.
	Reason string
	// RootID is the workflow root the row was resolved to.
	RootID string
	// ConvoyID is the root's gc.input_convoy_id convoy.
	ConvoyID string
	// InputID is the convoy member whose fact withheld the row.
	InputID string
	// BlockerID is the unsatisfied prerequisite target, for ReasonPrerequisite.
	BlockerID string
}

// String renders the verdict for a stderr line: the reason and the ids an
// operator needs to release the work.
func (w Withhold) String() string {
	switch w.Reason {
	case ReasonHeld:
		return fmt.Sprintf("input %s carries a dispatch hold", w.InputID)
	case ReasonBlocked:
		return fmt.Sprintf("input %s is blocked", w.InputID)
	case ReasonDeferred:
		return fmt.Sprintf("input %s is deferred", w.InputID)
	case ReasonPrerequisite:
		return fmt.Sprintf("input %s waits on prerequisite %s", w.InputID, w.BlockerID)
	default:
		return fmt.Sprintf("input %s is not ready", w.InputID)
	}
}

// Source is one store an input's facts are read from: its live read handle
// and its work-class view, labeled so a read failure can name it. The handles
// are minted where the store was resolved — the reader's leg boundary, the
// controller's city stores — never here. A zero Source (no work view)
// contributes nothing.
type Source struct {
	Label string
	Live  beads.LiveReader
	Work  beads.WorkStore
}

// empty reports whether a source holds no store, so every read skips it
// without touching a nil reader.
func (s Source) empty() bool {
	return s.Work == (beads.WorkStore{})
}

// RowRootID is the workflow root input admission resolves row to: the root its
// gc.root_bead_id names, or row itself when it carries gc.input_convoy_id —
// the same resolution the claim path's input-done walk uses. It returns ""
// for a row that is not part of a workflow whose input is tracked (no root, or
// its own root without an input convoy).
func RowRootID(row beads.Bead) string {
	if rootID := strings.TrimSpace(row.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != "" && rootID != row.ID {
		return rootID
	}
	if strings.TrimSpace(row.Metadata[beadmeta.InputConvoyIDMetadataKey]) != "" {
		return row.ID
	}
	return ""
}

// Subject reports whether row is subject to input admission: an open row —
// NEW execution; in_progress is existing ownership — that is not a control
// kind and belongs to a workflow whose root records an input convoy. Whether
// the workflow has stopped is a store read the Judge makes, not a fact the
// row carries.
func Subject(row beads.Bead) bool {
	if row.Status != "open" {
		return false
	}
	if beadmeta.IsControlKind(strings.TrimSpace(row.Metadata[beadmeta.KindMetadataKey])) {
		return false
	}
	return RowRootID(row) != ""
}

// Judge is input admission over one set of sources, for ONE read, controller
// pass or hook invocation: every root, convoy verdict and prerequisite target
// it resolves is memoized for the Judge's life, and nothing is cached across
// calls. Build one per pass; never share one across passes.
type Judge struct {
	sources []Source
	now     time.Time

	mu sync.Mutex
	// roots memoizes each resolved root id: found, and the bead when found.
	roots map[string]rootRead
	// stops memoizes the stop verdict for each walked row id.
	stops map[string]workflowstop.WorkflowStop
	// stopDone marks rows whose stop walk has run.
	stopDone map[string]bool
	// verdicts memoizes each judged convoy id; done marks judged convoys so a
	// convoy with no withholding verdict is not re-read.
	verdicts map[string]Withhold
	judged   map[string]bool
	// withheldRows records every row a Judge call withheld, keyed by row id,
	// for the caller that must report them (the demand pass's InputWithheld).
	withheldRows map[string]Withhold
	// satisfied memoizes each prerequisite target's copy-rule verdict.
	satisfied map[string]bool
}

// rootRead is a memoized root read: whether any source holds the bead.
type rootRead struct {
	bead  beads.Bead
	found bool
}

// New returns a Judge reading input facts from sources, judging deferrals
// against now.
func New(sources []Source, now time.Time) *Judge {
	return &Judge{
		sources:      sources,
		now:          now,
		roots:        make(map[string]rootRead),
		stops:        make(map[string]workflowstop.WorkflowStop),
		stopDone:     make(map[string]bool),
		verdicts:     make(map[string]Withhold),
		judged:       make(map[string]bool),
		withheldRows: make(map[string]Withhold),
		satisfied:    make(map[string]bool),
	}
}

// WithheldRows returns every row the Judge's calls have withheld so far,
// keyed by row id, with its verdict. The demand pass records it beside the
// gated reads so wake and keep-awake counting see the same rows the reader
// refused.
func (j *Judge) WithheldRows() map[string]Withhold {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string]Withhold, len(j.withheldRows))
	for id, w := range j.withheldRows {
		out[id] = w
	}
	return out
}

// Judge returns which of rows — every one served by sources[own], whose store
// the root reads and the stop walk prefer — input admission withholds, keyed
// by row id. A row with no entry is admissible. A row that could not be judged
// is an error naming the bead and the source that failed; it is never silently
// served.
func (j *Judge) Judge(ctx context.Context, own int, rows []beads.Bead) (map[string]Withhold, error) {
	out := make(map[string]Withhold, len(rows))
	for _, row := range rows {
		if !Subject(row) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w, err := j.judgeRow(ctx, own, row)
		if err != nil {
			return nil, fmt.Errorf("judging input admission for %s: %w", row.ID, err)
		}
		if w.Reason != "" {
			out[row.ID] = w
			j.mu.Lock()
			j.withheldRows[row.ID] = w
			j.mu.Unlock()
		}
	}
	return out, nil
}

// judgeRow resolves one subject row's verdict.
func (j *Judge) judgeRow(ctx context.Context, own int, row beads.Bead) (Withhold, error) {
	stop, err := j.stopped(ctx, own, row)
	if err != nil || stop.Stopped() {
		// A stopped workflow's rows belong to the stop gates and the teardown
		// tail; withholding them would starve both.
		return Withhold{}, err
	}
	rootID := RowRootID(row)
	root, found, err := j.root(own, rootID, row)
	if err != nil || !found {
		// A missing root is retirement's case, never a withheld row.
		return Withhold{}, err
	}
	convoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if convoyID == "" {
		return Withhold{}, nil
	}
	return j.convoyVerdict(ctx, own, rootID, convoyID)
}

// stopped walks row's workflow ancestry for a stopped root, resolving each
// link from whichever source holds it, the row's own first: a stop any
// source's chain establishes stops the row, and a link no source holds ends
// the walk without a stop, exactly as the single-store walk does.
func (j *Judge) stopped(ctx context.Context, own int, row beads.Bead) (workflowstop.WorkflowStop, error) {
	if v, done := j.stopMemo(row.ID); done {
		return v, nil
	}
	if err := ctx.Err(); err != nil {
		return workflowstop.WorkflowStop{}, err
	}
	stop, err := workflowstop.StoppedWorkflowAncestor(j.chainGet(own), row)
	if err != nil {
		return workflowstop.WorkflowStop{}, err
	}
	j.setStopMemo(row.ID, stop)
	return stop, nil
}

// chainGet loads one ancestry link from the first source that holds it,
// preferring the serving source, so a chain that crosses stores — a graph
// step under a work drain control, a relic copy only one leg kept — is read
// from the store that holds each link.
func (j *Judge) chainGet(own int) workflowstop.BeadGetter {
	return func(id string) (beads.Bead, error) {
		b, i, err := j.probe(own, id)
		if err != nil {
			return beads.Bead{}, err
		}
		if i < 0 {
			return beads.Bead{}, beads.ErrNotFound
		}
		return b, nil
	}
}

// root reads the workflow root rootID, preferring the store that served the
// row. The row itself answers when it IS the root.
func (j *Judge) root(own int, rootID string, row beads.Bead) (beads.Bead, bool, error) {
	if rootID == row.ID {
		return row, true, nil
	}
	if r, done := j.rootMemo(rootID); done {
		return r.bead, r.found, nil
	}
	var bead beads.Bead
	found := false
	b, i, err := j.probe(own, rootID)
	if err != nil {
		return beads.Bead{}, false, err
	}
	bead, found = b, i >= 0
	j.setRootMemo(rootID, bead, found)
	return bead, found, nil
}

// probe reads id from the first source that holds it, preferring the serving
// source, and returns the source's index, or -1 when no source holds it. Each
// leg is a by-id residence probe — the row may be of the other leg's class on
// a split city, and the next source covers the miss — so it runs under
// beads.FederatedGet (reviewedMarkers).
func (j *Judge) probe(own int, id string) (beads.Bead, int, error) {
	for _, i := range j.sourceOrder(own) {
		src := j.sources[i]
		if src.empty() {
			continue
		}
		b, err := beads.FederatedGet(src.Work, id)
		if err == nil {
			return b, i, nil
		}
		if !errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, -1, fmt.Errorf("%s store: reading %s: %w", src.Label, id, err)
		}
	}
	return beads.Bead{}, -1, nil
}

// convoyVerdict judges one input convoy, memoized by its id. The members are
// read live as the union of what every source records; the first non-terminal
// member carrying a withholding fact wins, in the members' own order.
func (j *Judge) convoyVerdict(ctx context.Context, own int, rootID, convoyID string) (Withhold, error) {
	j.mu.Lock()
	if done := j.judged[convoyID]; done {
		v := j.verdicts[convoyID]
		j.mu.Unlock()
		return v, nil
	}
	j.mu.Unlock()

	scopes := make([]convoy.MemberScope, 0, len(j.sources))
	for _, src := range j.sources {
		if src.empty() {
			continue
		}
		scopes = append(scopes, convoy.MemberScope{
			Classes: convoy.MemberClasses{Convoy: src.Work},
		})
	}
	members, err := convoy.MembersAcross(scopes, convoyID, false)
	if err != nil {
		return Withhold{}, fmt.Errorf("reading input convoy %s: %w", convoyID, err)
	}
	var w Withhold
	for _, member := range members {
		if convoy.IsTerminalStatus(member.Status) || convoy.IsUnresolvedTrackedItem(member) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Withhold{}, err
		}
		mw, err := j.memberVerdict(ctx, own, member)
		if err != nil {
			return Withhold{}, err
		}
		if mw.Reason != "" {
			w = Withhold{
				Reason:    mw.Reason,
				RootID:    rootID,
				ConvoyID:  convoyID,
				InputID:   member.ID,
				BlockerID: mw.BlockerID,
			}
			break
		}
	}
	j.mu.Lock()
	j.judged[convoyID] = true
	j.verdicts[convoyID] = w
	j.mu.Unlock()
	return w, nil
}

// memberVerdict judges one input member's authored facts, in the reason order
// the design fixes: hold, blocked, deferred, prerequisite.
func (j *Judge) memberVerdict(ctx context.Context, own int, member beads.Bead) (Withhold, error) {
	if beadmeta.HasDispatchHold(member.Labels) {
		return Withhold{Reason: ReasonHeld}, nil
	}
	if member.Status == "blocked" || member.SourceStatus == "blocked" {
		return Withhold{Reason: ReasonBlocked}, nil
	}
	if beads.IsDeferred(member, j.now) {
		return Withhold{Reason: ReasonDeferred}, nil
	}
	blocker, err := j.unsatisfiedPrerequisite(ctx, own, member)
	if err != nil {
		return Withhold{}, err
	}
	if blocker != "" {
		return Withhold{Reason: ReasonPrerequisite, BlockerID: blocker}, nil
	}
	return Withhold{}, nil
}

// unsatisfiedPrerequisite returns the first ready-blocking dependency target
// of member that no store holds satisfied, or "". The member's edges are read
// live from the store that holds the member; each target is judged from every
// copy the sources hold (beads.DependencySatisfiedByCopies), the ready
// reader's own copy rule. An external capability ref names no bead and never
// blocks.
func (j *Judge) unsatisfiedPrerequisite(ctx context.Context, own int, member beads.Bead) (string, error) {
	deps, err := j.memberDeps(ctx, own, member)
	if err != nil {
		return "", err
	}
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) {
			continue
		}
		target, capability := beads.ExternalBlockerTarget(dep.DependsOnID)
		if capability {
			continue
		}
		satisfied, err := j.targetSatisfied(ctx, target)
		if err != nil {
			return "", err
		}
		if !satisfied {
			return dep.DependsOnID, nil
		}
	}
	return "", nil
}

// memberDeps reads member's dependency edges live from the store that holds
// the member.
func (j *Judge) memberDeps(ctx context.Context, own int, member beads.Bead) ([]beads.Dep, error) {
	live, err := j.holdingLive(own, member.ID)
	if err != nil || live == nil {
		// A member no source now holds has no edges to read; the other reasons
		// judged it, and absence is retirement's case.
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return live.DepList(member.ID, "down")
}

// holdingLive returns the live reader of the source that holds id, preferring
// the serving source, or nil when no source does.
func (j *Judge) holdingLive(own int, id string) (beads.LiveReader, error) {
	_, i, err := j.probe(own, id)
	if err != nil || i < 0 {
		return nil, err
	}
	return j.sources[i].Live, nil
}

// targetSatisfied judges one prerequisite target from every copy the sources
// hold, memoized: an id no store holds releases nothing, as the ready reader
// holds an unresolved blocker.
func (j *Judge) targetSatisfied(ctx context.Context, target string) (bool, error) {
	j.mu.Lock()
	if v, ok := j.satisfied[target]; ok {
		j.mu.Unlock()
		return v, nil
	}
	j.mu.Unlock()
	var copies []beads.Bead
	for _, src := range j.sources {
		if src.empty() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		b, err := src.Live.Get(target)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return false, fmt.Errorf("%s store: reading prerequisite %s: %w", src.Label, target, err)
		}
		copies = append(copies, b)
	}
	satisfied := beads.DependencySatisfiedByCopies(copies)
	j.mu.Lock()
	j.satisfied[target] = satisfied
	j.mu.Unlock()
	return satisfied, nil
}

// sourceOrder lists the source indexes to read, the serving source first.
func (j *Judge) sourceOrder(own int) []int {
	order := make([]int, 0, len(j.sources))
	if own >= 0 && own < len(j.sources) {
		order = append(order, own)
	}
	for i := range j.sources {
		if i == own {
			continue
		}
		order = append(order, i)
	}
	return order
}

func (j *Judge) stopMemo(id string) (workflowstop.WorkflowStop, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stops[id], j.stopDone[id]
}

func (j *Judge) setStopMemo(id string, stop workflowstop.WorkflowStop) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stops[id] = stop
	j.stopDone[id] = true
}

func (j *Judge) rootMemo(id string) (rootRead, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.roots[id]
	return r, ok
}

func (j *Judge) setRootMemo(id string, bead beads.Bead, found bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.roots[id] = rootRead{bead: bead, found: found}
}
