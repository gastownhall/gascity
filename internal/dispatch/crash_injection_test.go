package dispatch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// Crash-injection harness for control processing.
//
// Every control kind promises crash-safe, idempotent progress: the bead that
// guards a piece of work stays open until that work is durable, so a
// dispatcher killed between any two store writes is finished by the next serve
// cycle. The harness checks that promise mechanically. For a fixture it first
// records an uninterrupted run, then for every write position N the control
// processing makes it rebuilds the fixture, faults write N, lets the
// dispatcher serve cycles run to quiescence, and requires the final ledger to
// equal the uninterrupted one. A final extra cycle must change nothing.
//
// Two fault models are exercised:
//   - crash: write N and every later write are lost and the dispatcher process
//     dies (a panic unwinds out of ProcessControl); a fresh process then
//     serves the ledger as it was left.
//   - transient: write N alone fails with an availability-tier error (a
//     timed-out write) and the dispatcher keeps serving; later writes succeed.
//
// Only writes issued while ProcessControl runs are counted and faulted; the
// fixture's own setup writes are never touched.

var errInjectedWriteFault = errors.New("injected store write fault")

type writeFaultMode int

const (
	writeFaultCrash writeFaultMode = iota
	writeFaultTransient
)

func (m writeFaultMode) String() string {
	if m == writeFaultTransient {
		return "transient"
	}
	return "crash"
}

// injectedCrash is the panic value that models the dispatcher process dying at
// a store write.
type injectedCrash struct {
	write int
	op    string
}

// writeFaultInjector counts mutating store calls made during control
// processing and faults the configured one. It is shared by every store a
// fixture wraps so a fixture spanning several stores (cross-store source
// chains) sees one global write order.
type writeFaultInjector struct {
	mode    writeFaultMode
	faultAt int // 1-based write position to fault; 0 disables faults
	armed   bool
	writes  int
	ops     []string
	fired   bool
}

func (f *writeFaultInjector) beforeWrite(op string) error {
	if !f.armed {
		return nil
	}
	f.writes++
	f.ops = append(f.ops, op)
	if f.faultAt == 0 || f.writes < f.faultAt {
		return nil
	}
	switch f.mode {
	case writeFaultCrash:
		f.fired = true
		panic(injectedCrash{write: f.writes, op: op})
	case writeFaultTransient:
		if f.writes == f.faultAt {
			f.fired = true
			// A store write that timed out: the availability-tier error a
			// real backend returns under contention or a slow commit.
			return fmt.Errorf("%w: write #%d %s: %w", errInjectedWriteFault, f.writes, op, context.DeadlineExceeded)
		}
	}
	return nil
}

// faultStore wraps a store and routes every mutating call through a shared
// writeFaultInjector. Reads pass straight through. A transaction counts as one
// write because real backends commit it atomically.
type faultStore struct {
	beads.Store
	f *writeFaultInjector
}

func (s faultStore) Create(b beads.Bead) (beads.Bead, error) {
	if err := s.f.beforeWrite("Create"); err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Create(b)
}

func (s faultStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.f.beforeWrite("Update " + id); err != nil {
		return err
	}
	return s.Store.Update(id, opts)
}

func (s faultStore) Close(id string) error {
	if err := s.f.beforeWrite("Close " + id); err != nil {
		return err
	}
	return s.Store.Close(id)
}

func (s faultStore) Reopen(id string) error {
	if err := s.f.beforeWrite("Reopen " + id); err != nil {
		return err
	}
	return s.Store.Reopen(id)
}

func (s faultStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if err := s.f.beforeWrite("CloseAll " + strings.Join(ids, ",")); err != nil {
		return 0, err
	}
	return s.Store.CloseAll(ids, metadata)
}

func (s faultStore) SetMetadata(id, key, value string) error {
	if err := s.f.beforeWrite("SetMetadata " + id + " " + key); err != nil {
		return err
	}
	return s.Store.SetMetadata(id, key, value)
}

func (s faultStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.f.beforeWrite("SetMetadataBatch " + id); err != nil {
		return err
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s faultStore) SetLocalString(id, key, value string) error {
	if err := s.f.beforeWrite("SetLocalString " + id + " " + key); err != nil {
		return err
	}
	return s.Store.SetLocalString(id, key, value)
}

func (s faultStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	if err := s.f.beforeWrite("Tx " + commitMsg); err != nil {
		return err
	}
	return s.Store.Tx(commitMsg, fn)
}

func (s faultStore) Delete(id string) error {
	if err := s.f.beforeWrite("Delete " + id); err != nil {
		return err
	}
	return s.Store.Delete(id)
}

func (s faultStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.f.beforeWrite("DepAdd " + issueID + "->" + dependsOnID); err != nil {
		return err
	}
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

func (s faultStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.f.beforeWrite("DepRemove " + issueID + "->" + dependsOnID); err != nil {
		return err
	}
	return s.Store.DepRemove(issueID, dependsOnID)
}

// bdClosePolicyStore mirrors production's two close paths: an unforced status
// update into closed is refused while a blocker or child is still open (the
// strictCloseStore rule, beads #5206), but Close is "bd close --force" in
// BdStore and a forced storage close in NativeDoltStore, so it never refuses.
// Fixtures whose shape relies on that policy (a logical bead blocked by the
// control that settles it) are built on this store so a reorder that only
// works on a permissive MemStore cannot pass.
type bdClosePolicyStore struct{ *strictCloseStore }

func newBdClosePolicyStore() bdClosePolicyStore {
	return bdClosePolicyStore{newStrictCloseStore()}
}

func (s bdClosePolicyStore) Close(id string) error {
	s.recordClose(id)
	return s.MemStore.Close(id)
}

// crashFixture is one freshly built ledger to serve. ledger lists every store
// whose end state is compared; dispatch is the (wrapped) store whose ready
// controls the dispatcher serves.
type crashFixture struct {
	ledger   []*beads.MemStore
	dispatch beads.Store
	opts     ProcessOptions
	// work, when set, plays the agents: it closes ready work beads once the
	// dispatcher has nothing left to do and reports whether it closed any.
	// Its writes are never faulted.
	work func(t *testing.T) bool
}

// crashScenario builds a fixture. wrap must be applied to every store the
// dispatcher can reach (the dispatch store and anything a resolver returns)
// so their writes are counted and faulted.
type crashScenario struct {
	name  string
	build func(t *testing.T, wrap func(beads.Store) beads.Store) crashFixture
	// check asserts scenario-specific facts about the uninterrupted end state
	// so the comparison cannot pass vacuously.
	check func(t *testing.T, ledger []ledgerBead)
	// ignoreMetadataKeys are excluded from the comparison for a known,
	// tracked divergence. Each use must cite its bead.
	ignoreMetadataKeys []string
}

type ledgerBead struct {
	Store    int
	ID       string
	Title    string
	Status   string
	Type     string
	Assignee string
	ParentID string
	Labels   []string
	Metadata map[string]string
	Blocks   []string
	// rawID is the store ID before canonicalization; lookup only, never
	// compared.
	rawID string
}

func (b ledgerBead) String() string {
	keys := slices.Sorted(maps.Keys(b.Metadata))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+b.Metadata[k])
	}
	return fmt.Sprintf("store%d/%s status=%s deps=%v labels=%v meta={%s}", b.Store, b.ID, b.Status, b.Blocks, b.Labels, strings.Join(parts, " "))
}

// crashVolatileMetadataKeys differ between two otherwise identical runs
// (wall-clock measurements) and are excluded from the comparison.
var crashVolatileMetadataKeys = map[string]bool{
	beadmeta.DurationMsMetadataKey: true,
}

func snapshotLedger(t *testing.T, stores []*beads.MemStore, ignoreKeys ...string) []ledgerBead {
	t.Helper()
	var out []ledgerBead
	for i, store := range stores {
		all, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
		if err != nil {
			t.Fatalf("snapshot store %d: %v", i, err)
		}
		for _, b := range all {
			meta := make(map[string]string, len(b.Metadata))
			for k, v := range b.Metadata {
				// An empty value and an absent key are the same state: control
				// completion clears keys by writing "".
				if v == "" || crashVolatileMetadataKeys[k] || slices.Contains(ignoreKeys, k) {
					continue
				}
				meta[k] = v
			}
			deps, err := store.DepList(b.ID, "down")
			if err != nil {
				t.Fatalf("snapshot deps of %s: %v", b.ID, err)
			}
			blocks := make([]string, 0, len(deps))
			for _, d := range deps {
				blocks = append(blocks, d.Type+":"+d.DependsOnID)
			}
			slices.Sort(blocks)
			labels := slices.Clone(b.Labels)
			slices.Sort(labels)
			out = append(out, ledgerBead{
				Store: i, ID: b.ID, Title: b.Title, Status: b.Status, Type: b.Type,
				Assignee: b.Assignee, ParentID: b.ParentID, Labels: labels,
				Metadata: meta, Blocks: blocks,
			})
		}
	}
	return canonicalizeLedger(out)
}

// canonicalizeLedger makes two converged ledgers comparable when recovery
// legitimately minted different bead IDs. A partially appended retry is
// closed as skipped residue (gc.partial_retry) and re-appended under fresh
// IDs, so residue is dropped and every bead carrying a gc.step_ref that is
// unique within its store is keyed by that ref instead of its ID, in the bead
// itself, in its dependency edges, and in metadata values naming it.
func canonicalizeLedger(in []ledgerBead) []ledgerBead {
	kept := make([]ledgerBead, 0, len(in))
	for _, b := range in {
		if b.Status == "closed" && b.Metadata[beadmeta.PartialRetryMetadataKey] == "true" {
			continue
		}
		kept = append(kept, b)
	}
	type key struct {
		store int
		ref   string
	}
	refCount := make(map[key]int)
	for _, b := range kept {
		if ref := b.Metadata[beadmeta.StepRefMetadataKey]; ref != "" {
			refCount[key{b.Store, ref}]++
		}
	}
	rename := make(map[int]map[string]string)
	for _, b := range kept {
		ref := b.Metadata[beadmeta.StepRefMetadataKey]
		if ref == "" || refCount[key{b.Store, ref}] != 1 {
			continue
		}
		if rename[b.Store] == nil {
			rename[b.Store] = make(map[string]string)
		}
		rename[b.Store][b.ID] = "ref:" + ref
	}
	out := make([]ledgerBead, 0, len(kept))
	for _, b := range kept {
		names := rename[b.Store]
		mapID := func(id string) string {
			if name, ok := names[id]; ok {
				return name
			}
			return id
		}
		b.rawID = b.ID
		b.ID = mapID(b.ID)
		b.ParentID = mapID(b.ParentID)
		for k, v := range b.Metadata {
			b.Metadata[k] = mapID(v)
		}
		for i, dep := range b.Blocks {
			depType, target, _ := strings.Cut(dep, ":")
			b.Blocks[i] = depType + ":" + mapID(target)
		}
		slices.Sort(b.Blocks)
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b ledgerBead) int {
		if a.Store != b.Store {
			return a.Store - b.Store
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func withoutRawID(b ledgerBead) ledgerBead {
	b.rawID = ""
	return b
}

func diffLedgers(want, got []ledgerBead) string {
	index := func(l []ledgerBead) map[string]ledgerBead {
		m := make(map[string]ledgerBead, len(l))
		for _, b := range l {
			m[fmt.Sprintf("%d/%s", b.Store, b.ID)] = b
		}
		return m
	}
	w, g := index(want), index(got)
	keys := slices.Sorted(maps.Keys(w))
	for k := range g {
		if _, ok := w[k]; !ok {
			keys = append(keys, k)
		}
	}
	var lines []string
	for _, k := range keys {
		wb, wok := w[k]
		gb, gok := g[k]
		switch {
		case !gok:
			lines = append(lines, "  missing: "+wb.String())
		case !wok:
			lines = append(lines, "  extra:   "+gb.String())
		case !reflect.DeepEqual(withoutRawID(wb), withoutRawID(gb)):
			lines = append(lines, "  want:    "+wb.String(), "  got:     "+gb.String())
		}
	}
	return strings.Join(lines, "\n")
}

// serveOutcome reports how a serve run ended.
type serveOutcome struct {
	crashed  bool
	errs     []error
	pending  []string
	progress bool
}

// serveCycle mirrors one control-dispatcher sweep: every open control bead
// whose blockers are all closed is handed to ProcessControl once, in ID order.
// A ProcessControl error is logged and the sweep moves on, as the serve loop
// does; an injected crash ends the sweep (the process is gone).
func serveCycle(t *testing.T, fx crashFixture, inj *writeFaultInjector) (out serveOutcome) {
	t.Helper()
	open, err := fx.dispatch.ListOpen("open")
	if err != nil {
		t.Fatalf("listing open beads: %v", err)
	}
	slices.SortFunc(open, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
	for _, candidate := range open {
		current, err := fx.dispatch.Get(candidate.ID)
		if err != nil {
			t.Fatalf("reloading %s: %v", candidate.ID, err)
		}
		if current.Status != "open" || !beadmeta.IsControlKind(current.Metadata[beadmeta.KindMetadataKey]) {
			continue
		}
		deps, err := fx.dispatch.DepList(current.ID, "down")
		if err != nil {
			t.Fatalf("dep list %s: %v", current.ID, err)
		}
		ready := true
		for _, dep := range deps {
			if dep.Type != "blocks" {
				continue
			}
			blocker, err := fx.dispatch.Get(dep.DependsOnID)
			if err != nil || blocker.Status != "closed" {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		result, crashed, err := processControlUnderFault(fx, current, inj)
		if crashed {
			out.crashed = true
			return out
		}
		if errors.Is(err, ErrControlPending) {
			out.pending = append(out.pending, current.ID)
			continue
		}
		if err != nil {
			out.errs = append(out.errs, fmt.Errorf("ProcessControl(%s kind=%s): %w", current.ID, current.Metadata[beadmeta.KindMetadataKey], err))
			continue
		}
		if result.Processed {
			out.progress = true
		}
	}
	return out
}

func processControlUnderFault(fx crashFixture, bead beads.Bead, inj *writeFaultInjector) (result ControlResult, crashed bool, err error) {
	inj.armed = true
	defer func() {
		inj.armed = false
		if r := recover(); r != nil {
			if _, ok := r.(injectedCrash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	result, err = ProcessControl(fx.dispatch, bead, fx.opts)
	return result, false, err
}

// serveToQuiescence runs serve cycles until one makes no progress and leaves
// nothing pending or failing. A crash disarms the injector (the store
// recovers) and serving resumes as a fresh process would. Errors and pending
// controls are re-served on later cycles as the serve loop does; if they
// persist without progress, serving is wedged and they are returned.
func serveToQuiescence(t *testing.T, fx crashFixture, inj *writeFaultInjector) []error {
	t.Helper()
	const (
		maxCycles      = 60
		maxStuckCycles = 3
	)
	stuck := 0
	for cycle := 0; cycle < maxCycles; cycle++ {
		out := serveCycle(t, fx, inj)
		switch {
		case out.crashed:
			inj.faultAt = 0
			stuck = 0
		case out.progress:
			stuck = 0
		case len(out.errs) == 0 && fx.work != nil && fx.work(t):
			// Agents only get a turn once the dispatcher has nothing to
			// retry: it ticks in seconds, an agent takes minutes.
			stuck = 0
		case len(out.errs) == 0 && len(out.pending) == 0:
			return nil
		default:
			stuck++
			if stuck >= maxStuckCycles {
				errs := slices.Clone(out.errs)
				for _, id := range out.pending {
					errs = append(errs, fmt.Errorf("control %s still pending", id))
				}
				return errs
			}
		}
	}
	t.Fatalf("serve loop did not reach quiescence within %d cycles", maxCycles)
	return nil
}

func runCrashScenario(t *testing.T, sc crashScenario) {
	t.Helper()

	baseInj := &writeFaultInjector{}
	baseFx := sc.build(t, func(s beads.Store) beads.Store { return faultStore{Store: s, f: baseInj} })
	if errs := serveToQuiescence(t, baseFx, baseInj); len(errs) != 0 {
		t.Fatalf("uninterrupted run left errors: %v", errors.Join(errs...))
	}
	want := snapshotLedger(t, baseFx.ledger, sc.ignoreMetadataKeys...)
	if sc.check != nil {
		sc.check(t, want)
	}
	totalWrites := baseInj.writes
	if totalWrites == 0 {
		t.Fatal("uninterrupted run made no control writes; the scenario exercises nothing")
	}
	writeOps := slices.Clone(baseInj.ops)

	for _, mode := range []writeFaultMode{writeFaultCrash, writeFaultTransient} {
		for n := 1; n <= totalWrites; n++ {
			inj := &writeFaultInjector{mode: mode, faultAt: n}
			fx := sc.build(t, func(s beads.Store) beads.Store { return faultStore{Store: s, f: inj} })
			errs := serveToQuiescence(t, fx, inj)
			if !inj.fired {
				// The faulted run took a shorter path; nothing was injected.
				continue
			}
			label := fmt.Sprintf("%s fault at write %d/%d (%s)", mode, n, totalWrites, writeOps[n-1])
			if len(errs) != 0 {
				t.Errorf("%s: serving did not converge cleanly: %v", label, errors.Join(errs...))
				continue
			}
			got := snapshotLedger(t, fx.ledger, sc.ignoreMetadataKeys...)
			if diff := diffLedgers(want, got); diff != "" {
				t.Errorf("%s: end state differs from uninterrupted run:\n%s", label, diff)
				continue
			}
			// Idempotence: another serve cycle must neither progress nor
			// change anything.
			again := serveCycle(t, fx, inj)
			if again.progress || len(again.errs) != 0 || len(again.pending) != 0 {
				t.Errorf("%s: extra serve cycle was not a no-op: progress=%v errs=%v pending=%v", label, again.progress, again.errs, again.pending)
			}
			if diff := diffLedgers(want, snapshotLedger(t, fx.ledger, sc.ignoreMetadataKeys...)); diff != "" {
				t.Errorf("%s: extra serve cycle changed the ledger:\n%s", label, diff)
			}
		}
	}
}

func ledgerBeadByID(t *testing.T, ledger []ledgerBead, store int, id string) ledgerBead {
	t.Helper()
	for _, b := range ledger {
		if b.Store == store && b.rawID == id {
			return b
		}
	}
	t.Fatalf("bead store%d/%s not in ledger", store, id)
	return ledgerBead{}
}

func requireClosedWithOutcome(t *testing.T, ledger []ledgerBead, store int, id, outcome string) {
	t.Helper()
	b := ledgerBeadByID(t, ledger, store, id)
	if b.Status != "closed" || b.Metadata[beadmeta.OutcomeMetadataKey] != outcome {
		t.Fatalf("uninterrupted run: %s status=%s outcome=%q, want closed/%s", id, b.Status, b.Metadata[beadmeta.OutcomeMetadataKey], outcome)
	}
}

// --- check (ralph.go) ---

func ralphCheckCrashScenario(name, script string, maxAttempts int, hardSubject bool, check func(t *testing.T, ledger []ledgerBead, logicalID, checkID string)) crashScenario {
	var logicalID, checkID string
	return crashScenario{
		name: name,
		build: func(t *testing.T, wrap func(beads.Store) beads.Store) crashFixture {
			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "check.sh", script)
			store := newBdClosePolicyStore()
			logical, run1, check1 := newSimpleRalphLoopInStore(t, store, "implement", checkPath, maxAttempts)
			subjectMeta := map[string]string{
				beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
				beadmeta.OutputJSONMetadataKey: `{"verdict":"green"}`,
			}
			if hardSubject {
				subjectMeta = map[string]string{
					beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
					beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
					beadmeta.FailureReasonMetadataKey: "external_live_head_changed",
				}
			}
			if err := store.SetMetadataBatch(run1.ID, subjectMeta); err != nil {
				t.Fatalf("stamp subject: %v", err)
			}
			if err := store.Close(run1.ID); err != nil {
				t.Fatalf("close subject: %v", err)
			}
			logicalID, checkID = logical.ID, check1.ID
			return crashFixture{
				ledger:   []*beads.MemStore{store.MemStore},
				dispatch: wrap(store),
				opts:     ProcessOptions{CityPath: cityPath},
			}
		},
		check: func(t *testing.T, ledger []ledgerBead) { check(t, ledger, logicalID, checkID) },
	}
}

func TestCrashInjectionRalphCheck(t *testing.T) {
	t.Parallel()
	scenarios := []crashScenario{
		ralphCheckCrashScenario("pass", "#!/bin/bash\nexit 0\n", 3, false, func(t *testing.T, ledger []ledgerBead, logicalID, checkID string) {
			requireClosedWithOutcome(t, ledger, 0, logicalID, beadmeta.OutcomePass)
			requireClosedWithOutcome(t, ledger, 0, checkID, beadmeta.OutcomePass)
			if got := ledgerBeadByID(t, ledger, 0, logicalID).Metadata[beadmeta.OutputJSONMetadataKey]; got != `{"verdict":"green"}` {
				t.Fatalf("logical gc.output_json = %q, want propagated subject output", got)
			}
		}),
		ralphCheckCrashScenario("hard-fail", "#!/bin/bash\nexit 0\n", 3, true, func(t *testing.T, ledger []ledgerBead, logicalID, checkID string) {
			requireClosedWithOutcome(t, ledger, 0, logicalID, beadmeta.OutcomeFail)
			requireClosedWithOutcome(t, ledger, 0, checkID, beadmeta.OutcomeFail)
		}),
		ralphCheckCrashScenario("exhausted", "#!/bin/bash\nexit 1\n", 1, false, func(t *testing.T, ledger []ledgerBead, logicalID, checkID string) {
			requireClosedWithOutcome(t, ledger, 0, logicalID, beadmeta.OutcomeFail)
			requireClosedWithOutcome(t, ledger, 0, checkID, beadmeta.OutcomeFail)
		}),
		ralphCheckCrashScenario("retry", "#!/bin/bash\nexit 1\n", 3, false, func(t *testing.T, ledger []ledgerBead, logicalID, checkID string) {
			requireClosedWithOutcome(t, ledger, 0, checkID, beadmeta.OutcomeFail)
			if got := ledgerBeadByID(t, ledger, 0, logicalID).Status; got != "open" {
				t.Fatalf("logical status after retry = %s, want open for attempt 2", got)
			}
		}),
	}
	// ga-5ysi6e: a transient spawn error recorded on check 1 is cloned into
	// check 2 because clearRetryEphemera keeps the controller-error keys.
	scenarios[3].ignoreMetadataKeys = []string{
		beadmeta.ControllerErrorMetadataKey,
		beadmeta.ControllerErrorClassMetadataKey,
		beadmeta.ControllerRetryableMetadataKey,
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runCrashScenario(t, sc)
		})
	}
}

// --- workflow-finalize (runtime.go) ---

func workflowFinalizeCrashScenario(name, stepOutcome string, check func(t *testing.T, ledger []ledgerBead, f sourceChainFinalizeFixture, remainingID string)) crashScenario {
	var fixture sourceChainFinalizeFixture
	var remainingID string
	return crashScenario{
		name: name,
		build: func(t *testing.T, wrap func(beads.Store) beads.Store) crashFixture {
			f := newSourceChainFinalizeFixtureWithOutcome(t, stepOutcome)
			// A generated member the run never reached: the finalizer's sweep
			// must close it as skipped.
			remaining := mustCreateWorkflowBead(t, f.rigStore, beads.Bead{
				Title: "unreached step",
				Type:  "task",
				Metadata: map[string]string{
					beadmeta.RootBeadIDMetadataKey: f.workflow.ID,
					beadmeta.StepRefMetadataKey:    "unreached",
				},
			})
			fixture, remainingID = f, remaining.ID
			city, rig := wrap(f.cityStore), wrap(f.rigStore)
			return crashFixture{
				ledger:   []*beads.MemStore{f.rigStore, f.cityStore},
				dispatch: rig,
				opts: ProcessOptions{
					ResolveStoreRef: func(ref string) (beads.Store, error) {
						switch ref {
						case "city:test":
							return city, nil
						case "rig:test":
							return rig, nil
						default:
							return nil, fmt.Errorf("unknown store ref: %s", ref)
						}
					},
					SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
						return []SourceWorkflowStore{
							{Store: city, StoreRef: "city:test"},
							{Store: rig, StoreRef: "rig:test"},
						}, nil
					},
				},
			}
		},
		check: func(t *testing.T, ledger []ledgerBead) { check(t, ledger, fixture, remainingID) },
	}
}

func TestCrashInjectionWorkflowFinalize(t *testing.T) {
	t.Parallel()
	scenarios := []crashScenario{
		workflowFinalizeCrashScenario("pass", beadmeta.OutcomePass, func(t *testing.T, ledger []ledgerBead, f sourceChainFinalizeFixture, remainingID string) {
			requireClosedWithOutcome(t, ledger, 0, f.workflow.ID, beadmeta.OutcomePass)
			requireClosedWithOutcome(t, ledger, 0, f.finalizer.ID, beadmeta.OutcomePass)
			requireClosedWithOutcome(t, ledger, 0, remainingID, beadmeta.OutcomeSkipped)
			requireClosedWithOutcome(t, ledger, 0, f.rigLaunch.ID, beadmeta.OutcomePass)
			requireClosedWithOutcome(t, ledger, 1, f.citySource.ID, beadmeta.OutcomePass)
		}),
		workflowFinalizeCrashScenario("fail", beadmeta.OutcomeFail, func(t *testing.T, ledger []ledgerBead, f sourceChainFinalizeFixture, remainingID string) {
			requireClosedWithOutcome(t, ledger, 0, f.workflow.ID, beadmeta.OutcomeFail)
			requireClosedWithOutcome(t, ledger, 0, f.finalizer.ID, beadmeta.OutcomePass)
			requireClosedWithOutcome(t, ledger, 0, remainingID, beadmeta.OutcomeSkipped)
			if got := ledgerBeadByID(t, ledger, 0, f.rigLaunch.ID).Status; got != "open" {
				t.Fatalf("failed workflow's source status = %s, want open for investigation", got)
			}
		}),
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runCrashScenario(t, sc)
		})
	}
}

// --- retry-eval (retry.go) ---
//
// retry-eval has the ordering ga-up0tmv fixed for check: it closes the eval
// before the logical bead, and its stale-attempt guard turns an interrupted
// settle into an open, unblocked logical bead that the next sweep quarantines
// as malformed. Fixing it changes the guard's behavior; tracked in ga-wzqjs4.

func retryEvalCrashScenario(name string, runOutcome map[string]string) crashScenario {
	return crashScenario{
		name: name,
		build: func(t *testing.T, wrap func(beads.Store) beads.Store) crashFixture {
			store, _, _ := newRetryEvalOrderingFixture(t, runOutcome)
			policy := bdClosePolicyStore{store}
			return crashFixture{
				ledger:   []*beads.MemStore{store.MemStore},
				dispatch: wrap(policy),
			}
		},
	}
}

func TestCrashInjectionRetryEval(t *testing.T) {
	t.Parallel()
	t.Skip("ga-wzqjs4: retry-eval closes the eval before its logical bead")
	scenarios := []crashScenario{
		retryEvalCrashScenario("pass", map[string]string{"gc.outcome": "pass", "gc.output_json": `{"ok":true}`}),
		retryEvalCrashScenario("hard", map[string]string{"gc.outcome": "fail", "gc.failure_class": "hard", "gc.failure_reason": "boom"}),
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runCrashScenario(t, sc)
		})
	}
}

// --- compiled molecule: retry, scope-check, teardown tail, workflow-finalize ---

func teardownMoleculeCrashScenario(name, formulaDir, workOutcome string) crashScenario {
	return crashScenario{
		name: name,
		build: func(t *testing.T, wrap func(beads.Store) beads.Store) crashFixture {
			store := newBdClosePolicyStore()
			cooked, err := molecule.Cook(context.Background(), store, "teardown-settlement", []string{formulaDir}, molecule.Options{})
			if err != nil {
				t.Fatalf("Cook: %v", err)
			}
			rootID := cooked.RootID
			return crashFixture{
				ledger:   []*beads.MemStore{store.MemStore},
				dispatch: wrap(store),
				work: func(t *testing.T) bool {
					members, err := molecule.ListSubtree(store, rootID)
					if err != nil {
						t.Fatalf("listing molecule members: %v", err)
					}
					teardownSteps := teardownStepIDs(members)
					slices.SortFunc(members, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
					for _, member := range members {
						kind := member.Metadata[beadmeta.KindMetadataKey]
						if member.Status != "open" || member.ID == rootID || kind == beadmeta.KindSpec ||
							kind == beadmeta.KindScope || beadmeta.IsControlKind(kind) {
							continue
						}
						if !allBlockersClosed(t, store, member.ID) {
							continue
						}
						closeAsWorker(t, store, member, rootID, workOutcome, teardownSteps)
						return true
					}
					return false
				},
			}
		},
		check: func(t *testing.T, ledger []ledgerBead) {
			for _, b := range ledger {
				if b.Status != "closed" {
					t.Fatalf("uninterrupted run left %s open", b)
				}
			}
		},
	}
}

func TestCrashInjectionCompiledMolecule(t *testing.T) {
	t.Parallel()
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "teardown-settlement.toml"), []byte(teardownSettlementFormula), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	for _, sc := range []crashScenario{
		teardownMoleculeCrashScenario("pass", formulaDir, beadmeta.OutcomePass),
		teardownMoleculeCrashScenario("fail", formulaDir, beadmeta.OutcomeFail),
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runCrashScenario(t, sc)
		})
	}
}
