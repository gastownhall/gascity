package inputadmission

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// workflow is one input-gated workflow in a single store: an input tracked by
// a convoy, a root naming the convoy, and an open step of the root.
type workflow struct {
	input, convoy, root, step beads.Bead
}

// newWorkflow creates the workflow in store with input as the input bead.
func newWorkflow(t *testing.T, store beads.Store, input beads.Bead) workflow {
	t.Helper()
	if input.Title == "" {
		input.Title = "input"
	}
	in := mustCreate(t, store, input)
	conv := mustCreate(t, store, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := store.DepAdd(conv.ID, in.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd tracks: %v", err)
	}
	root := mustCreate(t, store, beads.Bead{Title: "root", Type: "molecule", Metadata: map[string]string{"gc.input_convoy_id": conv.ID}})
	step := mustCreate(t, store, beads.Bead{Title: "step", Type: "task", Metadata: map[string]string{"gc.root_bead_id": root.ID}})
	return workflow{input: in, convoy: conv, root: root, step: step}
}

// memStore is an in-memory store minting ids under prefix, as two real
// stores of a city mint under their own.
func memStore(prefix string) *beads.MemStore {
	m := beads.NewMemStore()
	m.IDPrefix = prefix
	return m
}

func mustCreate(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	if b.Type == "" {
		b.Type = "task"
	}
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("creating %q: %v", b.Title, err)
	}
	return created
}

// sourcesOf is stores as a Judge's sources, labeled by index, own first.
func sourcesOf(stores ...beads.Store) []Source {
	sources := make([]Source, len(stores))
	for i, s := range stores {
		sources[i] = Source{Label: []string{"work", "other"}[i], Reader: s}
	}
	return sources
}

// judge judges rows over stores, own first.
func judge(t *testing.T, rows []beads.Bead, stores ...beads.Store) map[string]Withhold {
	t.Helper()
	verdicts, err := New(sourcesOf(stores...), time.Now()).Judge(context.Background(), 0, rows)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	return verdicts
}

// verdictsOf judges rows over stores, own first, unresolved rows included.
func verdictsOf(t *testing.T, rows []beads.Bead, stores ...beads.Store) Verdicts {
	t.Helper()
	v, err := New(sourcesOf(stores...), time.Now()).Verdicts(context.Background(), 0, rows)
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	return v
}

// TestSubjectAndReadsInput pins which rows input admission judges and which
// it can settle without reading an input: an open, non-control row naming a
// workflow root is a subject, and a subject reads its input exactly when its
// root names an input convoy or cannot be found. Kills: a reader serving a
// step on its own store's verdict while its input lives elsewhere
// (OwnVerdictStands), and judging existing ownership or dispatcher control.
func TestSubjectAndReadsInput(t *testing.T) {
	inputRoot := beads.Bead{ID: "r-1", Status: "open", Metadata: map[string]string{"gc.input_convoy_id": "c-1"}}
	plainRoot := beads.Bead{ID: "r-2", Status: "open"}
	roots := map[string]beads.Bead{inputRoot.ID: inputRoot, plainRoot.ID: plainRoot}
	find := func(id string) (beads.Bead, bool) {
		b, ok := roots[id]
		return b, ok
	}
	step := func(status, root string, meta ...string) beads.Bead {
		m := map[string]string{"gc.root_bead_id": root}
		for i := 0; i+1 < len(meta); i += 2 {
			m[meta[i]] = meta[i+1]
		}
		return beads.Bead{ID: "s-1", Status: status, Metadata: m}
	}
	for _, tc := range []struct {
		name                string
		row                 beads.Bead
		root                func(string) (beads.Bead, bool)
		subject, readsInput bool
	}{
		{"open step of an input-gated root", step("open", "r-1"), find, true, true},
		{"open step of a root with no input", step("open", "r-2"), find, true, false},
		{"open step of a root no store holds", step("open", "r-9"), find, true, true},
		{"open step judged with no root lookup", step("open", "r-1"), nil, true, true},
		{"in_progress step: existing ownership", step("in_progress", "r-1"), find, false, false},
		{"control step", step("open", "r-1", "gc.kind", "check"), find, false, false},
		{"row naming no root", beads.Bead{ID: "s-1", Status: "open"}, find, false, false},
		{"root naming its own input convoy", inputRoot, find, true, true},
		{"row whose root id is itself, with no input", step("open", "s-1"), find, false, false},
	} {
		if got := Subject(tc.row); got != tc.subject {
			t.Errorf("%s: Subject = %t, want %t", tc.name, got, tc.subject)
		}
		if got := ReadsInput(tc.row, tc.root); got != tc.readsInput {
			t.Errorf("%s: ReadsInput = %t, want %t", tc.name, got, tc.readsInput)
		}
	}
}

// TestJudgeWithholdsOnEachInputFact pins memberReason through Judge: each "do
// not start yet" fact on the input withholds the step with its own reason,
// and an input carrying none (an assignee, another label, in_progress) admits
// it.
func TestJudgeWithholdsOnEachInputFact(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name   string
		input  beads.Bead
		status string
		want   Reason
	}{
		{name: "hold:external", input: beads.Bead{Labels: []string{"hold:external"}}, want: ReasonHeld},
		{name: "hold:mayor", input: beads.Bead{Labels: []string{"hold:mayor"}}, want: ReasonHeld},
		{name: "blocked status", status: "blocked", want: ReasonBlocked},
		{name: "future defer_until", input: beads.Bead{DeferUntil: &future}, want: ReasonDeferred},
		{name: "indefinite deferral", input: beads.Bead{IndefinitelyDeferred: true}, want: ReasonDeferred},
		{name: "assigned, labeled input", input: beads.Bead{Assignee: "someone", Labels: []string{"area:x"}}},
		{name: "in_progress input", status: "in_progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			wf := newWorkflow(t, store, tc.input)
			if tc.status != "" {
				status := tc.status
				if err := store.Update(wf.input.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatalf("setting status %s: %v", status, err)
				}
			}
			got, withheld := judge(t, []beads.Bead{wf.step}, store)[wf.step.ID]
			if tc.want == "" {
				if withheld {
					t.Fatalf("step withheld as %v, want admitted", got)
				}
				return
			}
			if !withheld || got.Reason != tc.want || got.InputID != wf.input.ID || got.ConvoyID != wf.convoy.ID || got.RootID != wf.root.ID {
				t.Fatalf("verdict = %+v (withheld %t), want %s on input %s of convoy %s, root %s", got, withheld, tc.want, wf.input.ID, wf.convoy.ID, wf.root.ID)
			}
		})
	}
}

// TestJudgePrerequisiteTargets pins the prerequisite rule (#8832 review): a
// target is judged from every copy the judge's sources hold, in plain and
// "external:" form. A target no source holds does not withhold: the step is
// Unresolved, the outcome a judge over fewer stores than the city's
// federation (the hook's) leaves to the federated reader. Kills: such a judge
// admitting (or withholding) on its own read a step whose prerequisite lives
// in a store it does not read, and a deleted target stranding the workflow
// for good.
func TestJudgePrerequisiteTargets(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange returns the edge target, given the input's store and a
		// second store the federated judge also reads.
		arrange    func(t *testing.T, own, other beads.Store) string
		federated  bool // judge over both stores, else over own alone
		withheld   bool
		unresolved bool
	}{
		{"open prerequisite in the input's store", func(t *testing.T, own, _ beads.Store) string {
			return mustCreate(t, own, beads.Bead{Title: "prereq"}).ID
		}, false, true, false},
		{"closed prerequisite in the input's store", func(t *testing.T, own, _ beads.Store) string {
			return closedPrereq(t, own)
		}, false, false, false},
		{"prerequisite no store holds", func(*testing.T, beads.Store, beads.Store) string { return "zz-gone" }, true, false, true},
		{"external: prerequisite no store holds", func(*testing.T, beads.Store, beads.Store) string { return "external:zz-gone" }, true, false, true},
		{"open prerequisite another store holds, judged over the input's store alone", func(t *testing.T, _, other beads.Store) string {
			return mustCreate(t, other, beads.Bead{Title: "prereq"}).ID
		}, false, false, true},
		{"closed prerequisite another store holds, judged over the input's store alone", func(t *testing.T, _, other beads.Store) string {
			return closedPrereq(t, other)
		}, false, false, true},
		{"open prerequisite another store holds, judged federated", func(t *testing.T, _, other beads.Store) string {
			return mustCreate(t, other, beads.Bead{Title: "prereq"}).ID
		}, true, true, false},
		{"open external: prerequisite another store holds, judged federated", func(t *testing.T, _, other beads.Store) string {
			return "external:" + mustCreate(t, other, beads.Bead{Title: "prereq"}).ID
		}, true, true, false},
		{"closed prerequisite another store holds, judged federated", func(t *testing.T, _, other beads.Store) string {
			return closedPrereq(t, other)
		}, true, false, false},
		{"capability ref", func(*testing.T, beads.Store, beads.Store) string { return "external:rig:capability" }, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own, other := memStore("ga"), memStore("mc")
			wf := newWorkflow(t, own, beads.Bead{})
			target := tc.arrange(t, own, other)
			if err := own.DepAdd(wf.input.ID, target, "blocks"); err != nil {
				t.Fatalf("DepAdd blocks %s: %v", target, err)
			}
			stores := []beads.Store{own}
			if tc.federated {
				stores = append(stores, other)
			}
			got, withheld := judge(t, []beads.Bead{wf.step}, stores...)[wf.step.ID]
			if withheld != tc.withheld {
				t.Fatalf("withheld = %t (%v), want %t", withheld, got, tc.withheld)
			}
			if withheld && (got.Reason != ReasonPrerequisite || got.InputID != wf.input.ID) {
				t.Fatalf("verdict = %+v, want a prerequisite verdict on input %s", got, wf.input.ID)
			}
			v := verdictsOf(t, []beads.Bead{wf.step}, stores...)
			if _, ok := v.Withheld[wf.step.ID]; ok != tc.withheld {
				t.Fatalf("Verdicts withheld = %t, want %t, as Judge says", ok, tc.withheld)
			}
			unknown, unresolved := v.Unresolved[wf.step.ID]
			if unresolved != tc.unresolved {
				t.Fatalf("unresolved = %t (%+v), want %t", unresolved, unknown, tc.unresolved)
			}
			wantTarget, _ := beads.ExternalBlockerTarget(target)
			if unresolved && (unknown.InputID != wf.input.ID || unknown.TargetID != wantTarget || unknown.ConvoyID != wf.convoy.ID || unknown.RootID != wf.root.ID) {
				t.Fatalf("unresolved = %+v, want input %s waiting on %s (convoy %s, root %s)", unknown, wf.input.ID, wantTarget, wf.convoy.ID, wf.root.ID)
			}
		})
	}
}

// closedPrereq creates a closed prerequisite in store and returns its id.
func closedPrereq(t *testing.T, store beads.Store) string {
	t.Helper()
	b := mustCreate(t, store, beads.Bead{Title: "prereq"})
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	return b.ID
}

// TestVerdictsWithholdBeforeUnresolved pins the order of the two outcomes: a
// prerequisite the sources hold open withholds the step even when another
// edge of the input (listed first) names a target no source holds, so a
// narrower judge never hands a step it can refuse on its own read to the
// federated reader.
func TestVerdictsWithholdBeforeUnresolved(t *testing.T) {
	own := memStore("ga")
	wf := newWorkflow(t, own, beads.Bead{})
	open := mustCreate(t, own, beads.Bead{Title: "prereq"})
	for _, target := range []string{"mc-elsewhere", open.ID} {
		if err := own.DepAdd(wf.input.ID, target, "blocks"); err != nil {
			t.Fatalf("DepAdd blocks %s: %v", target, err)
		}
	}
	v := verdictsOf(t, []beads.Bead{wf.step}, own)
	if w, ok := v.Withheld[wf.step.ID]; !ok || w.BlockerID != open.ID {
		t.Fatalf("Withheld[%s] = %+v (%t), want the open prerequisite %s", wf.step.ID, w, ok, open.ID)
	}
	if u, ok := v.Unresolved[wf.step.ID]; ok {
		t.Fatalf("Unresolved[%s] = %+v, want a withheld step reported withheld only", wf.step.ID, u)
	}
}

// TestVerdictsServeAStoppedWorkflowUnresolved pins AC-7 for the unresolved
// outcome: a stopped workflow's rows are served as they would be without
// input admission, so a closed root's step is neither withheld nor left to
// the federated reader, whatever its input waits on.
func TestVerdictsServeAStoppedWorkflowUnresolved(t *testing.T) {
	own := memStore("ga")
	wf := newWorkflow(t, own, beads.Bead{})
	if err := own.DepAdd(wf.input.ID, "mc-elsewhere", "blocks"); err != nil {
		t.Fatal(err)
	}
	if v := verdictsOf(t, []beads.Bead{wf.step}, own); len(v.Unresolved) != 1 {
		t.Fatalf("live workflow: Unresolved = %v, want %s", v.Unresolved, wf.step.ID)
	}
	if err := own.Close(wf.root.ID); err != nil {
		t.Fatal(err)
	}
	v := verdictsOf(t, []beads.Bead{wf.step}, own)
	if len(v.Withheld) != 0 || len(v.Unresolved) != 0 {
		t.Fatalf("stopped workflow: Verdicts = %+v, want its step served", v)
	}
}

// TestJudgePrerequisiteCopies pins the copy rule: a closed copy of a
// prerequisite in one store does not release the input while another store
// holds an open copy.
func TestJudgePrerequisiteCopies(t *testing.T) {
	own, other := memStore("ga"), memStore("mc")
	other.HonorExplicitIDs = true
	wf := newWorkflow(t, own, beads.Bead{})
	prereq := mustCreate(t, own, beads.Bead{Title: "prereq"})
	if err := own.Close(prereq.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Create(beads.Bead{ID: prereq.ID, Title: "prereq relic", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	if err := own.DepAdd(wf.input.ID, prereq.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if _, withheld := judge(t, []beads.Bead{wf.step}, own, other)[wf.step.ID]; !withheld {
		t.Fatal("step admitted while a second store holds the prerequisite open; want it withheld")
	}
	if _, withheld := judge(t, []beads.Bead{wf.step}, own)[wf.step.ID]; withheld {
		t.Fatal("step withheld over the store that holds the prerequisite closed alone; want it admitted")
	}
}

// countingStore counts the reads a Judge sends a store and can fail one kind.
// onList, when set, runs at the start of every List.
type countingStore struct {
	beads.Store
	gets, lists, depLists atomic.Int64
	failList, failDepList error
	onList                func()
}

func (s *countingStore) Get(id string) (beads.Bead, error) {
	s.gets.Add(1)
	return s.Store.Get(id)
}

func (s *countingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	if s.onList != nil {
		s.onList()
	}
	if s.failList != nil {
		return nil, s.failList
	}
	return s.Store.List(q)
}

func (s *countingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depLists.Add(1)
	if s.failDepList != nil {
		return nil, s.failDepList
	}
	return s.Store.DepList(id, direction)
}

// TestJudgeMemoizesForItsLife pins the memo: a second judgment of the same
// workflow's rows by one Judge reads no store, so a controller pass judging
// many rows of one workflow reads its root and convoy once.
func TestJudgeMemoizesForItsLife(t *testing.T) {
	mem := beads.NewMemStore()
	wf := newWorkflow(t, mem, beads.Bead{Labels: []string{"hold:external"}})
	second := mustCreate(t, mem, beads.Bead{Title: "step 2", Metadata: map[string]string{"gc.root_bead_id": wf.root.ID}})
	store := &countingStore{Store: mem}
	j := New([]Source{{Label: "work", Reader: store}}, time.Now())
	first, err := j.Judge(context.Background(), 0, []beads.Bead{wf.step})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first[wf.step.ID]; !ok {
		t.Fatalf("first judgment = %v, want %s withheld", first, wf.step.ID)
	}
	before := store.gets.Load() + store.lists.Load() + store.depLists.Load()
	again, err := j.Judge(context.Background(), 0, []beads.Bead{wf.step, second})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again[second.ID]; !ok {
		t.Fatalf("second judgment = %v, want %s withheld too", again, second.ID)
	}
	if after := store.gets.Load() + store.lists.Load() + store.depLists.Load(); after != before {
		t.Fatalf("the second judgment of the same workflow read the store %d more times, want none", after-before)
	}
}

// TestJudgeFailureNamesBeadAndSource pins AC-8 at the package boundary: a read
// that fails fails the whole judgment, naming the row, its root and the
// source, so a row that could not be judged is never admitted silently.
func TestJudgeFailureNamesBeadAndSource(t *testing.T) {
	mem := beads.NewMemStore()
	wf := newWorkflow(t, mem, beads.Bead{})
	store := &countingStore{Store: mem, failList: errors.New("ledger unavailable")}
	_, err := New([]Source{{Label: "rig:alpha", Reader: store}}, time.Now()).Judge(context.Background(), 0, []beads.Bead{wf.step})
	if err == nil {
		t.Fatal("Judge over a failing store succeeded, want its error")
	}
	for _, want := range []string{wf.step.ID, wf.root.ID, "rig:alpha store", "ledger unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestJudgeAdmitsAStoppedWorkflowWhoseInputCannotBeRead pins AC-7 against
// AC-8: a stopped workflow's rows are admitted however its input stands, so
// an input read that fails (the convoy's membership listing or its edges)
// still admits the teardown of a closed or cancel-requested root, whose
// ancestry reads fine. A live workflow's row fails the judgment, naming the
// row, its root and the source.
func TestJudgeAdmitsAStoppedWorkflowWhoseInputCannotBeRead(t *testing.T) {
	readErr := errors.New("ledger unavailable")
	for _, tc := range []struct {
		name     string
		stop     func(t *testing.T, store beads.Store, rootID string)
		failEdge bool
	}{
		{name: "closed root, membership listing fails", stop: func(t *testing.T, store beads.Store, rootID string) {
			t.Helper()
			if err := store.Close(rootID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "cancel-requested root, membership listing fails", stop: func(t *testing.T, store beads.Store, rootID string) {
			t.Helper()
			if err := store.SetMetadata(rootID, "gc.cancel_requested", "true"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "closed root, convoy edges fail", failEdge: true, stop: func(t *testing.T, store beads.Store, rootID string) {
			t.Helper()
			if err := store.Close(rootID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "live root, membership listing fails"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			wf := newWorkflow(t, mem, beads.Bead{Labels: []string{"hold:external"}})
			teardown := mustCreate(t, mem, beads.Bead{Title: "teardown", Metadata: map[string]string{
				"gc.root_bead_id": wf.root.ID,
				"gc.scope_role":   "teardown",
			}})
			if tc.stop != nil {
				tc.stop(t, mem, wf.root.ID)
			}
			store := &countingStore{Store: mem}
			if tc.failEdge {
				store.failDepList = readErr
			} else {
				store.failList = readErr
			}
			v, err := New([]Source{{Label: "rig:alpha", Reader: store}}, time.Now()).Verdicts(context.Background(), 0, []beads.Bead{teardown})
			if tc.stop == nil {
				if err == nil {
					t.Fatalf("Verdicts over a live workflow whose input cannot be read = %+v, want its error", v)
				}
				for _, want := range []string{teardown.ID, wf.root.ID, "rig:alpha store", readErr.Error()} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Verdicts over a stopped workflow whose input cannot be read: %v, want its teardown admitted", err)
			}
			if len(v.Withheld) != 0 || len(v.Unresolved) != 0 {
				t.Fatalf("Verdicts = %+v, want the stopped workflow's teardown %s admitted", v, teardown.ID)
			}
		})
	}
}

// TestJudgeStartsNoAncestryReadOnceItsContextEnds pins the deadline side of
// that exemption: when the caller's context ends during the input read, the
// judgment fails with the read's error and starts no ancestry read, though
// the workflow's root has stopped.
func TestJudgeStartsNoAncestryReadOnceItsContextEnds(t *testing.T) {
	mem := beads.NewMemStore()
	wf := newWorkflow(t, mem, beads.Bead{})
	if err := mem.Close(wf.root.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &countingStore{Store: mem, failList: context.Canceled}
	var getsAtList int64
	store.onList = func() {
		cancel()
		getsAtList = store.gets.Load()
	}
	v, err := New([]Source{{Label: "rig:alpha", Reader: store}}, time.Now()).Verdicts(ctx, 0, []beads.Bead{wf.step})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Verdicts = %+v, %v; want the input read's error once the context ended", v, err)
	}
	if got := store.gets.Load(); got != getsAtList {
		t.Fatalf("the judgment read %d beads after its context ended, want none", got-getsAtList)
	}
}

// issueTierBatch answers its exact batch read from the issue tier alone, as
// bd's batch read (`bd show`) does: a wisp it holds is missing from the batch
// and found only by Get. It counts the Gets of each id and fails a Get of an
// id in failGet.
type issueTierBatch struct {
	*beads.MemStore
	failGet map[string]error

	mu   sync.Mutex
	gets map[string]int
}

func newIssueTierBatch(prefix string) *issueTierBatch {
	return &issueTierBatch{MemStore: memStore(prefix), failGet: map[string]error{}, gets: map[string]int{}}
}

func (s *issueTierBatch) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	found := make(map[string]beads.Bead, len(ids))
	var unresolved []string
	for _, id := range ids {
		if b, err := s.MemStore.Get(id); err == nil && !b.Ephemeral {
			found[id] = b
			continue
		}
		unresolved = append(unresolved, id)
	}
	return found, unresolved, nil
}

func (s *issueTierBatch) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	s.gets[id]++
	s.mu.Unlock()
	if err := s.failGet[id]; err != nil {
		return beads.Bead{}, err
	}
	return s.MemStore.Get(id)
}

func (s *issueTierBatch) getsOf(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[id]
}

// wispRootWorkflow is a workflow in work whose input carries input's facts,
// with a step whose root is a wisp in home (work itself, or another store)
// naming the input convoy.
func wispRootWorkflow(t *testing.T, work, home beads.Store, input beads.Bead) (workflow, beads.Bead) {
	t.Helper()
	wf := newWorkflow(t, work, input)
	root := mustCreate(t, home, beads.Bead{Title: "wisp root", Type: "molecule", Ephemeral: true, Metadata: map[string]string{"gc.input_convoy_id": wf.convoy.ID}})
	step := mustCreate(t, work, beads.Bead{Title: "step of the wisp root", Metadata: map[string]string{"gc.root_bead_id": root.ID}})
	return wf, step
}

// TestJudgeReadsTheWispsABatchReadMisses pins that admission reads a wisp
// root or prerequisite that a store's batch read leaves unresolved (#9552
// review, iteration 4). bd's batch read is `bd show`, which reads only the
// issue tier, so a wisp is found only by Get. A root read as absent admits
// its step unjudged, and a prerequisite read as absent leaves its step
// unresolved, which the ready reader admits as a dangling edge. So a held
// input behind a wisp root, or an open wisp prerequisite, started the step.
// A closed wisp prerequisite is read as satisfied, not as unresolved.
func TestJudgeReadsTheWispsABatchReadMisses(t *testing.T) {
	t.Run("held input behind a wisp root", func(t *testing.T) {
		work := newIssueTierBatch("ga")
		wf, step := wispRootWorkflow(t, work, work, beads.Bead{Labels: []string{"hold:external"}})
		v := judge(t, []beads.Bead{step}, work)
		if got := v[step.ID]; got.Reason != ReasonHeld || got.InputID != wf.input.ID {
			t.Fatalf("Judge = %v, want %s withheld as held by input %s", v, step.ID, wf.input.ID)
		}
	})
	t.Run("held input behind a wisp root another store holds", func(t *testing.T) {
		work, other := newIssueTierBatch("ga"), newIssueTierBatch("mc")
		wf, step := wispRootWorkflow(t, work, other, beads.Bead{Labels: []string{"hold:external"}})
		v := judge(t, []beads.Bead{step}, work, other)
		if got := v[step.ID]; got.Reason != ReasonHeld || got.InputID != wf.input.ID {
			t.Fatalf("Judge = %v, want %s withheld as held by input %s", v, step.ID, wf.input.ID)
		}
	})
	for _, tc := range []struct {
		name   string
		closed bool
	}{
		{name: "open wisp prerequisite"},
		{name: "closed wisp prerequisite", closed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := newIssueTierBatch("ga")
			wf := newWorkflow(t, work, beads.Bead{})
			prerequisite := mustCreate(t, work, beads.Bead{Title: "wisp prerequisite", Ephemeral: true})
			if err := work.DepAdd(wf.input.ID, prerequisite.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			if tc.closed {
				if err := work.Close(prerequisite.ID); err != nil {
					t.Fatal(err)
				}
			}
			v := verdictsOf(t, []beads.Bead{wf.step}, work)
			if len(v.Unresolved) != 0 {
				t.Fatalf("Verdicts = %+v: the wisp prerequisite %s, which the store holds, read as held by no store", v, prerequisite.ID)
			}
			got, withheld := v.Withheld[wf.step.ID]
			if tc.closed {
				if withheld {
					t.Fatalf("Verdicts = %+v, want %s admitted: its wisp prerequisite is closed", v, wf.step.ID)
				}
				return
			}
			if got.Reason != ReasonPrerequisite || got.BlockerID != prerequisite.ID {
				t.Fatalf("Verdicts = %+v, want %s withheld by the open wisp prerequisite %s", v, wf.step.ID, prerequisite.ID)
			}
		})
	}
}

// TestJudgeReadsByIDOnlyWhatNoBatchAnswered pins the cost of reading wisps:
// an id is read on its own only when no source's batch read answered it, so a
// prerequisite one store's batch holds is not read again by id in any store.
func TestJudgeReadsByIDOnlyWhatNoBatchAnswered(t *testing.T) {
	work, other := newIssueTierBatch("ga"), newIssueTierBatch("mc")
	wf := newWorkflow(t, work, beads.Bead{})
	prerequisite := mustCreate(t, other, beads.Bead{Title: "prerequisite"})
	if err := work.DepAdd(wf.input.ID, prerequisite.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	v := judge(t, []beads.Bead{wf.step}, work, other)
	if got := v[wf.step.ID]; got.Reason != ReasonPrerequisite || got.BlockerID != prerequisite.ID {
		t.Fatalf("Judge = %v, want %s withheld by the open prerequisite %s", v, wf.step.ID, prerequisite.ID)
	}
	if n := work.getsOf(prerequisite.ID) + other.getsOf(prerequisite.ID); n != 0 {
		t.Fatalf("the prerequisite %s, which the other store's batch answered, was read by id %d time(s), want none", prerequisite.ID, n)
	}
}

// TestJudgeFailsWhenAWispCannotBeRead pins that the read by id keeps the
// judgment's failure rule: a wisp root or prerequisite whose Get fails fails
// the judgment, naming the source, rather than reading as absent.
func TestJudgeFailsWhenAWispCannotBeRead(t *testing.T) {
	readErr := errors.New("ledger unavailable")
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, work *issueTierBatch) (step beads.Bead, wisp string)
	}{
		{name: "wisp root", build: func(t *testing.T, work *issueTierBatch) (beads.Bead, string) {
			t.Helper()
			_, step := wispRootWorkflow(t, work, work, beads.Bead{Labels: []string{"hold:external"}})
			return step, step.Metadata["gc.root_bead_id"]
		}},
		{name: "wisp prerequisite", build: func(t *testing.T, work *issueTierBatch) (beads.Bead, string) {
			t.Helper()
			wf := newWorkflow(t, work, beads.Bead{})
			prerequisite := mustCreate(t, work, beads.Bead{Title: "wisp prerequisite", Ephemeral: true})
			if err := work.DepAdd(wf.input.ID, prerequisite.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			return wf.step, prerequisite.ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := newIssueTierBatch("ga")
			step, wisp := tc.build(t, work)
			work.failGet[wisp] = readErr
			v, err := New([]Source{{Label: "rig:alpha", Reader: work}}, time.Now()).Verdicts(context.Background(), 0, []beads.Bead{step})
			if !errors.Is(err, readErr) {
				t.Fatalf("Verdicts = %+v, %v; want the failed read of %s", v, err, wisp)
			}
			if !strings.Contains(err.Error(), "rig:alpha store") {
				t.Fatalf("error %q does not name the source", err)
			}
		})
	}
}
