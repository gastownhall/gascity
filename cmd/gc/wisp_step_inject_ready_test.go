package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// The fixtures below are the live shape behind ga-qi3glw4 (MCDClient,
// 2026-10-15). On every prompt and wake, a PM session's step reminder named
// mc-3oj8r, "Clarify acceptance criteria", as its current active work
// assignment:
//
//	mc-8fzh7  content task: open, unassigned, gc.routed_to=MCDClient/pm,
//	          molecule_id=mc-5386h, blocked by mc-3c4a8 (open)
//	mc-5386h  mol-plan-breakdown root: open, unassigned
//	mc-yfp32  intake          open, no blocker          created 19:44:04
//	mc-j0uoy  tracker-import  open, blocked by intake   created 19:44:04
//	mc-3oj8r  clarify         open, blocked by import   created 19:44:04
//	mc-2fhfx  decompose       open, blocked by clarify  created 19:44:05
//
// The session had claimed nothing. It then claimed mc-rpte74, an unrelated
// decision bead, and the reminder did not change. created_at is stored to the
// second, so the first three steps tie, and the tie breaks by id: mc-3oj8r
// sorts first.

var planIdentities = []string{"MCDClient/pm", "MCDClient--pm", "gm-wisp-lkhv98"}

const (
	planContent   = "mc-8fzh7"
	planBlocker   = "mc-3c4a8"
	planRoot      = "mc-5386h"
	planIntake    = "mc-yfp32"
	planImport    = "mc-j0uoy"
	planClarify   = "mc-3oj8r"
	planDecompose = "mc-2fhfx"
	planDecision  = "mc-rpte74"
	planGate      = "mc-gate1"
	noReminder    = "<none>"
)

// planBreakdown holds the fixture's rows by class: the work beads (the content
// bead, its blocker and any claimed work) and the graph beads (the molecule),
// each with its dependency edges.
type planBreakdown struct {
	work, graph         []beads.Bead
	workDeps, graphDeps []beads.Dep
}

func newPlanBreakdown() *planBreakdown {
	at := func(minute, second int) time.Time {
		return time.Date(2026, 10, 9, 19, minute, second, 0, time.UTC)
	}
	step := func(id, title string, created time.Time) beads.Bead {
		return beads.Bead{ID: id, Title: title, Description: title + ": do it", Type: "step", Status: "open", ParentID: planRoot, CreatedAt: created}
	}
	blocks := func(id, blocker string) beads.Dep {
		return beads.Dep{IssueID: id, DependsOnID: blocker, Type: "blocks"}
	}
	parent := func(id string) beads.Dep {
		return beads.Dep{IssueID: id, DependsOnID: planRoot, Type: "parent-child"}
	}
	return &planBreakdown{
		work: []beads.Bead{
			{
				ID: planBlocker, Title: "ReplayDriver (6b-C2b)", Type: "task", Status: "open", CreatedAt: at(32, 46),
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "MCDClient/builder"},
			},
			{
				ID: planContent, Title: "Go recorder canonical trace schema (6b-C3)", Description: "the content bead", Type: "task", Status: "open", CreatedAt: at(32, 51),
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "MCDClient/pm", beadmeta.MoleculeIDMetadataKey: planRoot},
			},
		},
		workDeps: []beads.Dep{
			blocks(planContent, planBlocker),
			{IssueID: planContent, DependsOnID: "mc-9t1.14", Type: "discovered-from"},
		},
		graph: []beads.Bead{
			{ID: planRoot, Title: "mol-plan-breakdown", Type: "molecule", Status: "open", CreatedAt: at(44, 4)},
			step(planIntake, "Read the goal bead", at(44, 4)),
			step(planImport, "Import issues from external trackers", at(44, 4)),
			step(planClarify, "Clarify acceptance criteria", at(44, 4)),
			step(planDecompose, "Decompose the goal", at(44, 5)),
		},
		graphDeps: []beads.Dep{
			parent(planIntake), parent(planImport), parent(planClarify), parent(planDecompose),
			blocks(planImport, planIntake),
			blocks(planClarify, planImport),
			blocks(planDecompose, planClarify),
		},
	}
}

// row returns the fixture row with id, for a case to adjust.
func (p *planBreakdown) row(t *testing.T, id string) *beads.Bead {
	t.Helper()
	for _, rows := range [][]beads.Bead{p.work, p.graph} {
		for i := range rows {
			if rows[i].ID == id {
				return &rows[i]
			}
		}
	}
	t.Fatalf("fixture has no row %s", id)
	return nil
}

// claim puts id in progress under the session's alias.
func (p *planBreakdown) claim(t *testing.T, id string) {
	t.Helper()
	b := p.row(t, id)
	b.Status = "in_progress"
	b.Assignee = planIdentities[0]
}

// claimDecision adds the unrelated decision bead the session claimed through
// gc hook --claim. It carries no molecule_id.
func (p *planBreakdown) claimDecision() {
	p.work = append(p.work, beads.Bead{
		ID: planDecision, Title: "Human decision: should a frozen client close a healthy socket?",
		Description: "decide the frozen-client question", Type: "decision", Status: "in_progress",
		Assignee: planIdentities[0], CreatedAt: time.Date(2026, 10, 15, 1, 26, 8, 0, time.UTC),
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "MCDClient/pm"},
	})
}

// stores seeds the rows into one store, which the reminder answers from a
// single listing, or splits them by class into two, which it answers one
// question at a time across the class boundary.
func (p *planBreakdown) stores(split bool) (work, graph beads.Store) {
	if !split {
		rows := append(append([]beads.Bead{}, p.work...), p.graph...)
		deps := append(append([]beads.Dep{}, p.workDeps...), p.graphDeps...)
		one := beads.NewMemStoreFrom(1000, rows, deps)
		return one, one
	}
	return beads.NewMemStoreFrom(1000, p.work, p.workDeps), beads.NewMemStoreFrom(2000, p.graph, p.graphDeps)
}

// assertStepReminder resolves the reminder with the rows in one store and split
// across two, and fails unless both name want. The one-store resolution must
// also keep asking a single in-progress question.
func assertStepReminder(t *testing.T, p *planBreakdown, want string) {
	t.Helper()
	for _, split := range []bool{false, true} {
		work, graph := p.stores(split)
		var rec *beadstest.OpRecordingStore
		if !split {
			rec = beadstest.NewOpRecordingStore(work)
			work, graph = rec, rec
		}
		got, err := resolveActiveWispStep(work, graph, planIdentities)
		if err != nil {
			t.Fatalf("split=%v: resolveActiveWispStep: %v", split, err)
		}
		if stepID(got) != want {
			t.Errorf("split=%v: the reminder names %s, want %s", split, describeStep(got), want)
		}
		if rec == nil {
			continue
		}
		claimedLists := 0
		for _, op := range rec.Ops() {
			if op.Kind == beadstest.OpList && op.Query.Status == "in_progress" && len(op.Query.Assignees) > 0 {
				claimedLists++
			}
		}
		if claimedLists != 1 {
			t.Errorf("the one-store resolution asked %d in-progress questions for the identities, want 1", claimedLists)
		}
	}
}

func describeStep(b *beads.Bead) string {
	if b == nil {
		return noReminder
	}
	return b.ID + " (" + b.Title + ")"
}

// Before the claim: nothing is claimed, and the only bead routed to the session
// waits on an open blocker. bd ready would not offer it, so neither it nor any
// step of its molecule is the session's assignment.
func TestStepReminderSkipsARoutedBeadStillBlocked(t *testing.T) {
	assertStepReminder(t, newPlanBreakdown(), noReminder)
}

// A routed bead is checked the way bd ready checks it: a closed blocker
// satisfies it, one closed with gc.work_outcome=blocked does not, and a
// bead assigned elsewhere, deferred, budget-deferred or dispatch-held is not
// offered at all.
func TestStepReminderRoutedBridgeOffersOnlyWhatAClaimCouldTake(t *testing.T) {
	cases := []struct {
		name   string
		adjust func(t *testing.T, p *planBreakdown)
		want   string
	}{
		{name: "blocker closed", want: planIntake, adjust: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
		}},
		{name: "blocker closed blocked", want: noReminder, adjust: func(t *testing.T, p *planBreakdown) {
			blocker := p.row(t, planBlocker)
			blocker.Status = "closed"
			blocker.Metadata[beadmeta.WorkOutcomeMetadataKey] = beadmeta.WorkOutcomeBlocked
		}},
		{name: "content deferred", want: noReminder, adjust: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
			until := time.Now().Add(24 * time.Hour)
			p.row(t, planContent).DeferUntil = &until
		}},
		{name: "content held external", want: noReminder, adjust: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
			content := p.row(t, planContent)
			content.Labels = append(content.Labels, beadmeta.HoldExternalLabel)
		}},
		{name: "content held mayor", want: noReminder, adjust: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
			content := p.row(t, planContent)
			content.Labels = append(content.Labels, beadmeta.HoldMayorLabel)
		}},
		{name: "content assigned elsewhere", want: noReminder, adjust: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
			p.row(t, planContent).Assignee = "MCDClient/builder"
		}},
		{name: "content budget deferred", want: noReminder, adjust: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
			p.row(t, planContent).Metadata[beadmeta.BudgetDeferredUntilMetadataKey] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanBreakdown()
			tc.adjust(t, p)
			assertStepReminder(t, p, tc.want)
		})
	}
}

// After the claim: the session holds mc-rpte74, which has no molecule. Claimed
// work is the assignment; a routed bead is at most what comes after it, ready
// or not.
func TestStepReminderPrefersClaimedWorkOverARoutedMolecule(t *testing.T) {
	for _, contentReady := range []bool{false, true} {
		name := "content blocked"
		if contentReady {
			name = "content ready"
		}
		t.Run(name, func(t *testing.T) {
			p := newPlanBreakdown()
			if contentReady {
				p.row(t, planBlocker).Status = "closed"
			}
			p.claimDecision()
			assertStepReminder(t, p, planDecision)
		})
	}
}

// Every path that falls back to a molecule's entry step lands on the first
// step whose blockers are satisfied, not the first open step in created order.
func TestStepReminderEntryStepIsTheFirstUnblockedStep(t *testing.T) {
	paths := []struct {
		name  string
		reach func(t *testing.T, p *planBreakdown)
	}{
		{name: "routed bead before the claim", reach: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
		}},
		{name: "claimed content bead", reach: func(t *testing.T, p *planBreakdown) {
			p.claim(t, planContent)
		}},
		{name: "molecule root claimed", reach: func(t *testing.T, p *planBreakdown) {
			p.claim(t, planRoot)
		}},
	}
	progress := []struct {
		name   string
		closed []string
		want   string
	}{
		{name: "fresh molecule", want: planIntake},
		{name: "intake closed", closed: []string{planIntake}, want: planImport},
		{name: "intake and import closed", closed: []string{planIntake, planImport}, want: planClarify},
	}
	for _, path := range paths {
		for _, stage := range progress {
			t.Run(path.name+"/"+stage.name, func(t *testing.T) {
				p := newPlanBreakdown()
				path.reach(t, p)
				for _, id := range stage.closed {
					p.row(t, id).Status = "closed"
				}
				assertStepReminder(t, p, stage.want)
			})
		}
	}
}

// A molecule whose every open step still waits on something has no step the
// session can start, so the reminder names none.
func TestStepReminderNamesNoStepWhileEveryOpenStepIsBlocked(t *testing.T) {
	p := newPlanBreakdown()
	p.claim(t, planRoot)
	p.graph = append(p.graph, beads.Bead{ID: planGate, Title: "DP1 decision gate", Type: "gate", Status: "open", CreatedAt: time.Date(2026, 10, 9, 19, 44, 3, 0, time.UTC)})
	p.graphDeps = append(p.graphDeps, beads.Dep{IssueID: planIntake, DependsOnID: planGate, Type: "blocks"})
	assertStepReminder(t, p, noReminder)
}

// The entry step's readiness costs one batched edge read for the molecule's
// open steps, not one read per step: the reminder runs on every prompt.
func TestStepReminderReadsEntryStepEdgesInOneBatch(t *testing.T) {
	p := newPlanBreakdown()
	p.claim(t, planRoot)
	p.row(t, planIntake).Status = "closed"
	store, _ := p.stores(false)
	rec := beadstest.NewOpRecordingStore(store)
	got, err := resolveActiveWispStep(rec, rec, planIdentities)
	if err != nil {
		t.Fatalf("resolveActiveWispStep: %v", err)
	}
	if stepID(got) != planImport {
		t.Fatalf("the reminder names %s, want %s", describeStep(got), planImport)
	}
	batches, singles := 0, 0
	for _, op := range rec.Ops() {
		switch op.Kind {
		case beadstest.OpDepListBatch:
			batches++
		case beadstest.OpDepList:
			singles++
		}
	}
	if batches != 1 || singles != 0 {
		t.Errorf("read the open steps' edges in %d batches and %d single reads, want one batch", batches, singles)
	}
}

// edgeReadFaultStore fails every dependency read, the batched one included, the
// way a store does while its database is unreachable.
type edgeReadFaultStore struct{ beads.Store }

func (s edgeReadFaultStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, errors.New("dolt: connection refused")
}

func (s edgeReadFaultStore) DepListBatch([]string) (map[string][]beads.Dep, error) {
	return nil, errors.New("dolt: connection refused")
}

// blockerReadFaultStore fails the read of one bead, a blocker, and answers every
// other read as the store under it does.
type blockerReadFaultStore struct {
	beads.Store
	id string
}

func (s blockerReadFaultStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		return beads.Bead{}, errors.New("dolt: connection refused")
	}
	return s.Store.Get(id)
}

// edgeBatchFaultStore answers every batched dependency read with answer and err,
// and the single reads as the store under it does.
type edgeBatchFaultStore struct {
	beads.Store
	answer map[string][]beads.Dep
	err    error
}

func (s edgeBatchFaultStore) DepListBatch([]string) (map[string][]beads.Dep, error) {
	return s.answer, s.err
}

// assertStepReminderOver is assertStepReminder with each store the resolution
// reads wrapped by wrap: one wrapper over the one store, one over each of the
// two. The wrappers are pointers so the one-store resolution still sees one
// handle.
func assertStepReminderOver(t *testing.T, p *planBreakdown, wrap func(beads.Store) beads.Store, want string) {
	t.Helper()
	for _, split := range []bool{false, true} {
		work, graph := p.stores(split)
		work = wrap(work)
		if split {
			graph = wrap(graph)
		} else {
			graph = work
		}
		got, err := resolveActiveWispStep(work, graph, planIdentities)
		if err != nil {
			t.Fatalf("split=%v: resolveActiveWispStep: %v", split, err)
		}
		if stepID(got) != want {
			t.Errorf("split=%v: the reminder names %s, want %s", split, describeStep(got), want)
		}
	}
}

// The reminder is advisory: while the edges that decide whether a step can
// start cannot be read, it names nothing rather than a step the session may not
// be able to start. Both paths name planIntake while the store answers.
func TestStepReminderNamesNoStepWhenItsDependenciesCannotBeRead(t *testing.T) {
	paths := []struct {
		name  string
		reach func(t *testing.T, p *planBreakdown)
	}{
		{name: "routed bead before the claim", reach: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
		}},
		{name: "claimed molecule root", reach: func(t *testing.T, p *planBreakdown) {
			p.claim(t, planRoot)
		}},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			p := newPlanBreakdown()
			path.reach(t, p)
			assertStepReminder(t, p, planIntake)
			assertStepReminderOver(t, p, func(s beads.Store) beads.Store { return &edgeReadFaultStore{Store: s} }, noReminder)
		})
	}
}

// A blocker that cannot be read is not known to be satisfied: the bead it
// blocks is not offered, though the blocker is closed. Both paths name
// planIntake while the blocker reads.
func TestStepReminderTreatsABlockerItCannotReadAsUnsatisfied(t *testing.T) {
	paths := []struct {
		name    string
		blocker string
		reach   func(t *testing.T, p *planBreakdown)
	}{
		{name: "routed bead before the claim", blocker: planBlocker, reach: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
		}},
		{name: "claimed molecule root", blocker: planGate, reach: func(t *testing.T, p *planBreakdown) {
			p.claim(t, planRoot)
			p.graph = append(p.graph, beads.Bead{ID: planGate, Title: "DP1 decision gate", Type: "gate", Status: "closed", CreatedAt: time.Date(2026, 10, 9, 19, 44, 3, 0, time.UTC)})
			p.graphDeps = append(p.graphDeps, beads.Dep{IssueID: planIntake, DependsOnID: planGate, Type: "blocks"})
		}},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			p := newPlanBreakdown()
			path.reach(t, p)
			assertStepReminder(t, p, planIntake)
			assertStepReminderOver(t, p, func(s beads.Store) beads.Store {
				return &blockerReadFaultStore{Store: s, id: path.blocker}
			}, noReminder)
		})
	}
}

// A batched edge read that does not answer for the open steps is not an answer
// that they have no edges: some stores omit an anchor, and bd answers a batch it
// declines with an empty map. Read that way, every open step looks startable and
// the first in created order, planClarify, is named again.
func TestStepReminderRereadsEdgesTheBatchDoesNotAnswer(t *testing.T) {
	batches := []struct {
		name   string
		answer map[string][]beads.Dep
		err    error
	}{
		{name: "empty answer", answer: map[string][]beads.Dep{}},
		{name: "unsupported", err: beads.ErrDepListBatchUnsupported},
	}
	for _, tc := range batches {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanBreakdown()
			p.claim(t, planRoot)
			assertStepReminderOver(t, p, func(s beads.Store) beads.Store {
				return &edgeBatchFaultStore{Store: s, answer: tc.answer, err: tc.err}
			}, planIntake)
		})
	}
}

// claimedListFaultStore fails the question "what has this session claimed", the
// in-progress listing for its identities, the way a store does while its database
// is unreachable. Every other read, the routed-bead listing included, is answered
// by the store under it.
type claimedListFaultStore struct{ beads.Store }

func (s claimedListFaultStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "in_progress" && len(q.Assignees) > 0 {
		return nil, errors.New("dolt: connection refused")
	}
	return s.Store.List(q)
}

// Claimed work wins over a routed bead, so a reminder that cannot list the
// claimed work cannot know the routed bead is the assignment: it names nothing,
// not planIntake, though the routed bead is one a claim could take now (its
// blocker is closed) and the reminder does name planIntake while the listing
// answers. In the split case only the work store fails; the graph store is asked
// for roots and steps, and failing it would end the resolution before the bridge.
func TestStepReminderNamesNoStepWhenItCannotListClaimedWork(t *testing.T) {
	p := newPlanBreakdown()
	p.row(t, planBlocker).Status = "closed"
	assertStepReminder(t, p, planIntake)
	for _, split := range []bool{false, true} {
		work, graph := p.stores(split)
		work = &claimedListFaultStore{Store: work}
		if !split {
			graph = work
		}
		// The error is the caller's to log; what the reminder owes is no step.
		got, _ := resolveActiveWispStep(work, graph, planIdentities)
		if got != nil {
			t.Errorf("split=%v: the reminder names %s, want %s", split, describeStep(got), noReminder)
		}
	}
}

// assertWorkResidentStepReminder resolves the reminder on a split city whose work
// store holds the molecule too: a v1 poured molecule is work class, so its root,
// its steps and the edges between them live on the work ledger and the graph
// store holds none of them. It fails unless the reminder names want.
func assertWorkResidentStepReminder(t *testing.T, p *planBreakdown, want string) {
	t.Helper()
	rows := append(append([]beads.Bead{}, p.work...), p.graph...)
	deps := append(append([]beads.Dep{}, p.workDeps...), p.graphDeps...)
	work := beads.NewMemStoreFrom(1000, rows, deps)
	graph := beads.NewMemStoreFrom(2000, nil, nil)
	got, err := resolveActiveWispStep(work, graph, planIdentities)
	if err != nil {
		t.Fatalf("resolveActiveWispStep: %v", err)
	}
	if stepID(got) != want {
		t.Errorf("the reminder names %s, want %s", describeStep(got), want)
	}
}

// The entry step of a molecule on the work ledger is the first step whose
// blockers are satisfied, read from the store that holds the molecule. The graph
// store holds none of its edges, so an entry step read from it sees no edge at
// all and names planClarify, the first open step in created order, as the
// original report did. Each path reaches the molecule from a different bead.
func TestStepReminderEntryStepOfAWorkResidentMoleculeIsTheFirstUnblockedStep(t *testing.T) {
	paths := []struct {
		name  string
		reach func(t *testing.T, p *planBreakdown)
	}{
		{name: "routed bead before the claim", reach: func(t *testing.T, p *planBreakdown) {
			p.row(t, planBlocker).Status = "closed"
		}},
		{name: "claimed content bead", reach: func(t *testing.T, p *planBreakdown) {
			p.claim(t, planContent)
		}},
		{name: "molecule root claimed", reach: func(t *testing.T, p *planBreakdown) {
			p.claim(t, planRoot)
		}},
	}
	progress := []struct {
		name   string
		closed []string
		want   string
	}{
		{name: "fresh molecule", want: planIntake},
		{name: "intake closed", closed: []string{planIntake}, want: planImport},
		{name: "intake and import closed", closed: []string{planIntake, planImport}, want: planClarify},
	}
	for _, path := range paths {
		for _, stage := range progress {
			t.Run(path.name+"/"+stage.name, func(t *testing.T) {
				p := newPlanBreakdown()
				path.reach(t, p)
				for _, id := range stage.closed {
					p.row(t, id).Status = "closed"
				}
				assertWorkResidentStepReminder(t, p, stage.want)
			})
		}
	}
}

// bridgeClaimedListFaultStore fails the molecule_id bridge's question "what has
// this session claimed", the untyped in-progress listing for its identities, the
// way a store does while its database is unreachable. The typed listings that
// look for a claimed molecule root answer, so the resolution reaches the bridge
// before the failure; every other read, the routed-bead listing included, is
// answered by the store under it.
type bridgeClaimedListFaultStore struct{ beads.Store }

func (s bridgeClaimedListFaultStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "in_progress" && len(q.Assignees) > 0 && q.Type == "" {
		return nil, errors.New("dolt: connection refused")
	}
	return s.Store.List(q)
}

// On a split city the work store is asked what the session has claimed after
// the claimed-molecule listings have answered. The test above fails those
// listings first, so in the split case it never reaches this question. When the
// claimed-work listing fails here, the reminder cannot know the routed bead is
// the assignment: it names nothing, not planIntake, though the routed bead is
// one a claim could take now (its blocker is closed) and the reminder does name
// planIntake while the listing answers.
func TestStepReminderNamesNoStepWhenTheBridgeCannotListClaimedWork(t *testing.T) {
	p := newPlanBreakdown()
	p.row(t, planBlocker).Status = "closed"
	assertStepReminder(t, p, planIntake)
	work, graph := p.stores(true)
	// The error is the caller's to log; what the reminder owes is no step.
	got, _ := resolveActiveWispStep(&bridgeClaimedListFaultStore{Store: work}, graph, planIdentities)
	if got != nil {
		t.Errorf("the reminder names %s, want %s", describeStep(got), noReminder)
	}
}
