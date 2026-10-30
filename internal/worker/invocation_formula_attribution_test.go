package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/usage"
)

// The formula a model fact carries is the formula the acting bead's run was
// compiled from. The acting bead is the one the controller stamped on the
// session (session.CurrentBeadIDKey, then gc.trigger_bead_id); the formula
// name sits on the run root the acting bead belongs to, never on the session
// bead. These tests drive both telemetry seams (the prompt-op path and the
// sweep) against that contract.

const (
	attrFormulaA = "mol-attribution-a"
	attrFormulaB = "mol-attribution-b"
)

// countingResolver is a bead resolver that counts and can fail by-id reads.
type countingResolver struct {
	store beads.Store

	mu    sync.Mutex
	reads []string
	fail  map[string]error
}

func newCountingResolver(store beads.Store) *countingResolver {
	return &countingResolver{store: store, fail: make(map[string]error)}
}

func (c *countingResolver) get(id string) (beads.Bead, error) {
	c.mu.Lock()
	c.reads = append(c.reads, id)
	err := c.fail[id]
	c.mu.Unlock()
	if err != nil {
		return beads.Bead{}, err
	}
	return c.store.Get(id)
}

func (c *countingResolver) failWith(id string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail[id] = err
}

func (c *countingResolver) clearFailure(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.fail, id)
}

func (c *countingResolver) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reads)
}

func (c *countingResolver) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = nil
}

// logRecorder captures slog records so a test can count the warnings a seam
// emits.
type logRecorder struct {
	mu      sync.Mutex
	records []string
	levels  []slog.Level
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, b.String())
	l.levels = append(l.levels, r.Level)
	return nil
}

func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

// warningsMentioning returns the warn-or-worse records containing every needle.
func (l *logRecorder) warningsMentioning(needles ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for i, rec := range l.records {
		if l.levels[i] < slog.LevelWarn {
			continue
		}
		match := true
		for _, n := range needles {
			if !strings.Contains(rec, n) {
				match = false
				break
			}
		}
		if match {
			out = append(out, rec)
		}
	}
	return out
}

func captureLogs(t *testing.T) *logRecorder {
	t.Helper()
	rec := &logRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

// formulaRig is one started claude session whose transcript the test grows,
// swept or prompted through a real Factory.
type formulaRig struct {
	t          *testing.T
	store      *beads.MemStore // the city store: holds the session bead
	factory    *Factory
	handle     *SessionHandle
	sessions   *sessionpkg.Store
	sessionID  string
	sinkPath   string
	transcript string
	entries    []map[string]any
}

func newFormulaRig(t *testing.T, cfg FactoryConfig) *formulaRig {
	t.Helper()
	searchBase := t.TempDir()
	workDir := t.TempDir()
	r := &formulaRig{
		t:        t,
		store:    beads.NewMemStore(),
		sinkPath: filepath.Join(t.TempDir(), "usage.jsonl"),
	}
	cfg.CityPath = t.TempDir()
	cfg.Store = r.store
	cfg.Provider = runtime.NewFake()
	cfg.SearchPaths = []string{searchBase}
	cfg.UsageSink = usage.NewLocalSink(r.sinkPath)
	factory, err := NewFactory(cfg)
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	r.factory = factory
	h, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background(), handleActor(t, h, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.handle = h
	r.sessionID = h.sessionID
	r.sessions = h.manager.PersistedStore()
	info, err := h.manager.Get(r.sessionID)
	if err != nil {
		t.Fatalf("Get(%q): %v", r.sessionID, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	r.transcript = filepath.Join(slugDir, info.SessionKey+".jsonl")
	return r
}

// point records the work bead the session is processing, through the session
// front door the reconciler uses.
func (r *formulaRig) point(beadID string) {
	r.t.Helper()
	if err := r.sessions.RecordCurrentBead(r.sessionID, beadID); err != nil {
		r.t.Fatalf("RecordCurrentBead(%q, %q): %v", r.sessionID, beadID, err)
	}
}

// addUsage appends one model invocation to the transcript.
func (r *formulaRig) addUsage(messageID string) {
	r.t.Helper()
	n := len(r.entries) + 1
	r.entries = append(r.entries, usageEntryWithMessageID(fmt.Sprintf("u%d", n), messageID, 100*n, 50*n, 0, 0))
	writeWorkerTestJSONL(r.t, r.transcript, r.entries)
}

// sweep runs the controller-tick sweep over the session's current metadata.
func (r *formulaRig) sweep() int {
	r.t.Helper()
	return r.sweepWith(nil)
}

// sweepWith sweeps with extra metadata layered over the session bead's.
func (r *formulaRig) sweepWith(extra map[string]string) int {
	r.t.Helper()
	b, err := r.store.Get(r.sessionID)
	if err != nil {
		r.t.Fatalf("Get session bead: %v", err)
	}
	meta := make(map[string]string, len(b.Metadata)+len(extra))
	for k, v := range b.Metadata {
		meta[k] = v
	}
	for k, v := range extra {
		meta[k] = v
	}
	emitted, settled, err := r.factory.SweepSessionModelUsage(context.Background(), r.sessionID, meta, time.Unix(1, 0).UTC())
	if err != nil {
		r.t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if !settled {
		r.t.Fatal("sweep must report settled")
	}
	return emitted
}

// message sends one prompt through the handle, which runs the prompt-op
// telemetry seam.
func (r *formulaRig) message() {
	r.t.Helper()
	if _, err := r.handle.Message(context.Background(), handleActor(r.t, r.handle, sessionpkg.ActorAgent), MessageRequest{Text: "hello"}); err != nil {
		r.t.Fatalf("Message: %v", err)
	}
}

func (r *formulaRig) facts() []usage.Fact {
	r.t.Helper()
	facts, _, err := usage.ReadFacts(r.sinkPath)
	if err != nil {
		r.t.Fatalf("ReadFacts: %v", err)
	}
	return facts
}

// formulaNames returns the FormulaName of every fact, in sink order.
func (r *formulaRig) formulaNames() []string {
	var out []string
	for _, f := range r.facts() {
		out = append(out, f.FormulaName)
	}
	return out
}

// seedRun creates a run root stamped with formula and a step bead that belongs
// to it, in store, and returns the step (the acting bead).
func seedRun(t *testing.T, store beads.Store, formula string) (root, step beads.Bead) {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: "root of " + formula, Metadata: map[string]string{beadmeta.FormulaNameMetadataKey: formula}})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	step, err = store.Create(beads.Bead{Title: "step of " + formula, Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}})
	if err != nil {
		t.Fatalf("Create step: %v", err)
	}
	return root, step
}

func wantNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("formula names = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("formula names = %q, want %q", got, want)
		}
	}
}

// T1 (worker half): the controller stamps the acting bead on the session; the
// prompt-op path carries the formula its run root was compiled from. The
// session bead itself carries no molecule_id and no gc.formula_name.
func TestMessageEmitsModelUsageFactWithFormulaName(t *testing.T) {
	rig := newFormulaRig(t, FactoryConfig{})
	_, step := seedRun(t, rig.store, attrFormulaA)
	rig.point(step.ID)
	rig.addUsage("msg-1")
	rig.message()
	wantNames(t, rig.formulaNames(), attrFormulaA)
}

// T1 (worker half): the same chain through the sweep seam.
func TestSweepSessionModelUsageCarriesFormulaName(t *testing.T) {
	rig := newFormulaRig(t, FactoryConfig{})
	_, step := seedRun(t, rig.store, attrFormulaA)
	rig.point(step.ID)
	rig.addUsage("msg-1")
	if got := rig.sweep(); got != 1 {
		t.Fatalf("emitted = %d, want 1", got)
	}
	wantNames(t, rig.formulaNames(), attrFormulaA)
}

// The label is a property of the whole batch: every fact swept together
// carries the same name.
func TestSweepBatchCarriesOneNameOnEveryFact(t *testing.T) {
	rig := newFormulaRig(t, FactoryConfig{})
	_, step := seedRun(t, rig.store, attrFormulaA)
	rig.point(step.ID)
	rig.addUsage("msg-1")
	rig.addUsage("msg-2")
	rig.addUsage("msg-3")
	if got := rig.sweep(); got != 3 {
		t.Fatalf("emitted = %d, want 3", got)
	}
	wantNames(t, rig.formulaNames(), attrFormulaA, attrFormulaA, attrFormulaA)
}

// A bead that is itself the run root (a standalone formula run) carries its
// own name.
func TestSweepActingBeadThatIsItsOwnRootCarriesItsOwnName(t *testing.T) {
	rig := newFormulaRig(t, FactoryConfig{})
	root, err := rig.store.Create(beads.Bead{Title: "root", Metadata: map[string]string{beadmeta.FormulaNameMetadataKey: " " + attrFormulaA + " "}})
	if err != nil {
		t.Fatal(err)
	}
	rig.point(root.ID)
	rig.addUsage("msg-1")
	rig.sweep()
	wantNames(t, rig.formulaNames(), attrFormulaA)
}

// T2: one session, two formulas. The pointer moves from A's step to B's step
// between sweeps; the facts carry A, then B.
func TestSweepPointerMovesBetweenFormulas(t *testing.T) {
	rig := newFormulaRig(t, FactoryConfig{})
	_, stepA := seedRun(t, rig.store, attrFormulaA)
	_, stepB := seedRun(t, rig.store, attrFormulaB)

	rig.point(stepA.ID)
	rig.addUsage("msg-1")
	rig.sweep()

	rig.point(stepB.ID)
	rig.addUsage("msg-2")
	rig.sweep()

	wantNames(t, rig.formulaNames(), attrFormulaA, attrFormulaB)
}

// T2 with a shared memo: the memo is keyed by the acting bead, so a pointer
// that moves still re-resolves.
func TestSweepPointerMovesBetweenFormulasWithWarmMemo(t *testing.T) {
	rig := newFormulaRig(t, FactoryConfig{FormulaMemo: NewFormulaNameMemo()})
	_, stepA := seedRun(t, rig.store, attrFormulaA)
	_, stepB := seedRun(t, rig.store, attrFormulaB)

	rig.point(stepA.ID)
	rig.addUsage("msg-1")
	rig.sweep()
	rig.point(stepB.ID)
	rig.addUsage("msg-2")
	rig.sweep()
	rig.point(stepA.ID)
	rig.addUsage("msg-3")
	rig.sweep()

	wantNames(t, rig.formulaNames(), attrFormulaA, attrFormulaB, attrFormulaA)
}

// T3: the session bead lives in the city store; the acting bead and its root
// live in a rig store. The injected resolver reaches them; with no resolver the
// factory store (city-resident only) cannot, and the label is empty.
func TestSweepResolvesActingBeadInAnotherStore(t *testing.T) {
	rigStore := beads.NewMemStore()
	rigStore.IDPrefix = "rg"
	_, step := seedRun(t, rigStore, attrFormulaA)

	t.Run("resolver attributes", func(t *testing.T) {
		resolver := newCountingResolver(rigStore)
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get})
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.sweep()
		wantNames(t, rig.formulaNames(), attrFormulaA)
	})
	t.Run("nil resolver is empty", func(t *testing.T) {
		rig := newFormulaRig(t, FactoryConfig{})
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.sweep()
		wantNames(t, rig.formulaNames(), "")
	})
}

// T4: beads.ErrNotFound (a purged or burned wisp) is the normal miss: empty
// label, no warning. Any other error: empty label and exactly one warning for
// the sweep. The fact is emitted whole either way.
func TestSweepResolverErrorsLeaveTheFactWhole(t *testing.T) {
	t.Run("not found is quiet", func(t *testing.T) {
		logs := captureLogs(t)
		rig := newFormulaRig(t, FactoryConfig{})
		rig.point("gone-1")
		rig.addUsage("msg-1")
		rig.addUsage("msg-2")
		if got := rig.sweep(); got != 2 {
			t.Fatalf("emitted = %d, want 2", got)
		}
		wantNames(t, rig.formulaNames(), "", "")
		if warns := logs.warningsMentioning("gone-1"); len(warns) != 0 {
			t.Fatalf("ErrNotFound must not warn, got %q", warns)
		}
	})
	t.Run("other error warns once per sweep", func(t *testing.T) {
		logs := captureLogs(t)
		backend := beads.NewMemStore()
		_, step := seedRun(t, backend, attrFormulaA)
		resolver := newCountingResolver(backend)
		boom := errors.New("backend unavailable")
		resolver.failWith(step.ID, boom)
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get})
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.addUsage("msg-2")
		if got := rig.sweep(); got != 2 {
			t.Fatalf("emitted = %d, want 2", got)
		}
		wantNames(t, rig.formulaNames(), "", "")
		facts := rig.facts()
		for i, f := range facts {
			if f.InputTokens == 0 || f.OutputTokens == 0 || f.SessionID != rig.sessionID || f.Model == "" {
				t.Errorf("fact %d is not whole: %+v", i, f)
			}
		}
		warns := logs.warningsMentioning(rig.sessionID, step.ID, boom.Error())
		if len(warns) != 1 {
			t.Fatalf("want exactly one warning naming session, acting bead and error; got %d: %q", len(warns), warns)
		}
	})
	t.Run("prompt op leaves the fact whole too", func(t *testing.T) {
		backend := beads.NewMemStore()
		_, step := seedRun(t, backend, attrFormulaA)
		resolver := newCountingResolver(backend)
		resolver.failWith(step.ID, errors.New("backend unavailable"))
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get})
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.message()
		facts := rig.facts()
		if len(facts) != 1 || facts[0].FormulaName != "" || facts[0].InputTokens == 0 {
			t.Fatalf("want one whole unattributed fact, got %+v", facts)
		}
	})
}

// T5: the pointer is currently_processing_bead_id, then gc.trigger_bead_id.
// With both set, processing wins. The trigger key is a controller-written
// launch-config field; the real binder is covered in cmd/gc, and here it is
// layered onto the metadata the sweep is handed.
func TestSweepPointerPrecedence(t *testing.T) {
	t.Run("trigger only", func(t *testing.T) {
		rig := newFormulaRig(t, FactoryConfig{})
		_, step := seedRun(t, rig.store, attrFormulaA)
		rig.addUsage("msg-1")
		rig.sweepWith(map[string]string{beadmeta.TriggerBeadIDMetadataKey: step.ID})
		wantNames(t, rig.formulaNames(), attrFormulaA)
	})
	t.Run("processing wins over trigger", func(t *testing.T) {
		rig := newFormulaRig(t, FactoryConfig{})
		_, stepA := seedRun(t, rig.store, attrFormulaA)
		_, stepB := seedRun(t, rig.store, attrFormulaB)
		rig.point(stepA.ID)
		rig.addUsage("msg-1")
		rig.sweepWith(map[string]string{beadmeta.TriggerBeadIDMetadataKey: stepB.ID})
		wantNames(t, rig.formulaNames(), attrFormulaA)
	})
	t.Run("claim pointer is never read", func(t *testing.T) {
		rig := newFormulaRig(t, FactoryConfig{})
		_, step := seedRun(t, rig.store, attrFormulaA)
		rig.addUsage("msg-1")
		rig.sweepWith(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: step.ID})
		wantNames(t, rig.formulaNames(), "")
	})
}

// T6: the walk takes the run root's name only. A root that was not compiled
// from a formula gives empty, even when the acting bead carries a stamp of its
// own; an unreadable differing root gives empty.
func TestSweepRootThatIsNotAFormulaGivesEmpty(t *testing.T) {
	t.Run("non-formula parent", func(t *testing.T) {
		rig := newFormulaRig(t, FactoryConfig{})
		parent, err := rig.store.Create(beads.Bead{Title: "plain parent"})
		if err != nil {
			t.Fatal(err)
		}
		step, err := rig.store.Create(beads.Bead{Title: "step", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:  parent.ID,
			beadmeta.FormulaNameMetadataKey: attrFormulaB, // an own stamp must not stand in for the root's
		}})
		if err != nil {
			t.Fatal(err)
		}
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.sweep()
		wantNames(t, rig.formulaNames(), "")
	})
	t.Run("unreadable root", func(t *testing.T) {
		rig := newFormulaRig(t, FactoryConfig{})
		step, err := rig.store.Create(beads.Bead{Title: "step", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:  "vanished-root",
			beadmeta.FormulaNameMetadataKey: attrFormulaB,
		}})
		if err != nil {
			t.Fatal(err)
		}
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.sweep()
		wantNames(t, rig.formulaNames(), "")
	})
}

// T7: no liveness check. A pointer naming a closed bead (the terminal sweep
// recovers usage after the work has finished) is still attributed.
func TestSweepPointerNamingClosedBeadIsStillAttributed(t *testing.T) {
	rig := newFormulaRig(t, FactoryConfig{})
	root, step := seedRun(t, rig.store, attrFormulaA)
	rig.point(step.ID)
	rig.addUsage("msg-1")
	if err := rig.store.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	if err := rig.store.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	rig.sweep()
	wantNames(t, rig.formulaNames(), attrFormulaA)
}

// T9: the resolver runs lazily, once per non-empty batch, with a bounded read
// count; a warm memo reads nothing.
func TestSweepFormulaResolutionReadCounts(t *testing.T) {
	t.Run("no pointer reads nothing", func(t *testing.T) {
		resolver := newCountingResolver(beads.NewMemStore())
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get, FormulaMemo: NewFormulaNameMemo()})
		rig.addUsage("msg-1")
		rig.sweep()
		if got := resolver.count(); got != 0 {
			t.Fatalf("reads with no pointer = %d, want 0", got)
		}
		wantNames(t, rig.formulaNames(), "")
	})
	t.Run("empty batch reads nothing", func(t *testing.T) {
		backend := beads.NewMemStore()
		resolver := newCountingResolver(backend)
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get, FormulaMemo: NewFormulaNameMemo()})
		_, step := seedRun(t, backend, attrFormulaA)
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.sweep() // drains the batch; cursor now current
		resolver.reset()
		rig.sweep() // empty batch
		if got := resolver.count(); got != 0 {
			t.Fatalf("reads on an empty batch = %d, want 0", got)
		}
	})
	t.Run("cold non-empty batch reads at most two", func(t *testing.T) {
		backend := beads.NewMemStore()
		resolver := newCountingResolver(backend)
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get, FormulaMemo: NewFormulaNameMemo()})
		_, step := seedRun(t, backend, attrFormulaA)
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.addUsage("msg-2")
		rig.sweep()
		wantNames(t, rig.formulaNames(), attrFormulaA, attrFormulaA)
		if got := resolver.count(); got == 0 || got > 2 {
			t.Fatalf("cold reads = %d, want 1 or 2 for a two-batch of one run", got)
		}
	})
	t.Run("warm memo reads nothing across factories", func(t *testing.T) {
		backend := beads.NewMemStore()
		resolver := newCountingResolver(backend)
		memo := NewFormulaNameMemo()
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get, FormulaMemo: memo})
		_, step := seedRun(t, backend, attrFormulaA)
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.sweep()
		resolver.reset()

		// The API builds a fresh Factory per request: a memo injected from a
		// long-lived owner must still hit.
		next, err := NewFactory(FactoryConfig{
			CityPath:    t.TempDir(),
			Store:       rig.store,
			Provider:    runtime.NewFake(),
			SearchPaths: []string{filepath.Dir(filepath.Dir(rig.transcript))},
			UsageSink:   usage.NewLocalSink(rig.sinkPath),
			ResolveBead: resolver.get,
			FormulaMemo: memo,
		})
		if err != nil {
			t.Fatal(err)
		}
		rig.factory = next
		rig.addUsage("msg-2")
		rig.sweep()
		if got := resolver.count(); got != 0 {
			t.Fatalf("warm reads = %d, want 0", got)
		}
		wantNames(t, rig.formulaNames(), attrFormulaA, attrFormulaA)
	})
	t.Run("not found is not cached", func(t *testing.T) {
		resolver := newCountingResolver(beads.NewMemStore())
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get, FormulaMemo: NewFormulaNameMemo()})
		rig.point("gone-1")
		rig.addUsage("msg-1")
		rig.sweep()
		first := resolver.count()
		if first == 0 {
			t.Fatal("the first sweep must read the pointed bead")
		}
		rig.addUsage("msg-2")
		rig.sweep()
		if got := resolver.count(); got <= first {
			t.Fatalf("an ErrNotFound must be re-read on the next batch: reads %d -> %d", first, got)
		}
		wantNames(t, rig.formulaNames(), "", "")
	})
	t.Run("other errors are not cached", func(t *testing.T) {
		backend := beads.NewMemStore()
		_, step := seedRun(t, backend, attrFormulaA)
		resolver := newCountingResolver(backend)
		resolver.failWith(step.ID, errors.New("backend unavailable"))
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get, FormulaMemo: NewFormulaNameMemo()})
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.sweep()
		resolver.clearFailure(step.ID)
		rig.addUsage("msg-2")
		rig.sweep()
		wantNames(t, rig.formulaNames(), "", attrFormulaA)
	})
	t.Run("prompt op is lazy and memoized too", func(t *testing.T) {
		backend := beads.NewMemStore()
		resolver := newCountingResolver(backend)
		rig := newFormulaRig(t, FactoryConfig{ResolveBead: resolver.get, FormulaMemo: NewFormulaNameMemo()})
		_, step := seedRun(t, backend, attrFormulaA)
		rig.message() // no transcript, no pointer
		if got := resolver.count(); got != 0 {
			t.Fatalf("reads with no batch = %d, want 0", got)
		}
		rig.point(step.ID)
		rig.addUsage("msg-1")
		rig.message()
		cold := resolver.count()
		if cold == 0 || cold > 2 {
			t.Fatalf("cold prompt-op reads = %d, want 1 or 2", cold)
		}
		rig.addUsage("msg-2")
		rig.message()
		if got := resolver.count(); got != cold {
			t.Fatalf("warm prompt-op reads: %d -> %d, want no new reads", cold, got)
		}
		wantNames(t, rig.formulaNames(), attrFormulaA, attrFormulaA)
	})
}

// The memo has the DerivedActivityMemo shape: nil is valid, a failure is
// returned and never cached, a success is cached, and the map is cleared
// whole at the cap.
func TestFormulaNameMemo(t *testing.T) {
	derive := func(calls *int, name string, err error) func() (string, error) {
		return func() (string, error) {
			*calls++
			return name, err
		}
	}
	t.Run("nil memo derives every time", func(t *testing.T) {
		var m *FormulaNameMemo
		calls := 0
		for i := 0; i < 2; i++ {
			got, err := m.resolve("a", derive(&calls, "f", nil))
			if err != nil || got != "f" {
				t.Fatalf("resolve = %q, %v", got, err)
			}
		}
		if calls != 2 {
			t.Fatalf("derive calls = %d, want 2", calls)
		}
	})
	t.Run("success cached, failure not", func(t *testing.T) {
		m := NewFormulaNameMemo()
		calls := 0
		boom := errors.New("boom")
		if _, err := m.resolve("a", derive(&calls, "", boom)); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if _, err := m.resolve("a", derive(&calls, "", boom)); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if calls != 2 {
			t.Fatalf("a failure was cached: derive calls = %d, want 2", calls)
		}
		for i := 0; i < 3; i++ {
			got, err := m.resolve("a", derive(&calls, "f", nil))
			if err != nil || got != "f" {
				t.Fatalf("resolve = %q, %v", got, err)
			}
		}
		if calls != 3 {
			t.Fatalf("a success was not cached: derive calls = %d, want 3", calls)
		}
	})
	t.Run("an empty verdict is a cached verdict", func(t *testing.T) {
		m := NewFormulaNameMemo()
		calls := 0
		for i := 0; i < 3; i++ {
			if got, err := m.resolve("a", derive(&calls, "", nil)); err != nil || got != "" {
				t.Fatalf("resolve = %q, %v", got, err)
			}
		}
		if calls != 1 {
			t.Fatalf("derive calls = %d, want 1", calls)
		}
	})
	t.Run("cleared whole at the cap", func(t *testing.T) {
		m := NewFormulaNameMemo()
		calls := 0
		for i := 0; i < formulaNameMemoCap; i++ {
			if _, err := m.resolve(fmt.Sprintf("id-%d", i), derive(&calls, "f", nil)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := m.resolve("overflow", derive(&calls, "f", nil)); err != nil {
			t.Fatal(err)
		}
		before := calls
		// id-0 went with the whole-map clear; overflow survived it.
		if _, err := m.resolve("overflow", derive(&calls, "f", nil)); err != nil {
			t.Fatal(err)
		}
		if calls != before {
			t.Fatalf("the entry that triggered the clear must be cached: calls %d -> %d", before, calls)
		}
		if _, err := m.resolve("id-0", derive(&calls, "f", nil)); err != nil {
			t.Fatal(err)
		}
		if calls != before+1 {
			t.Fatalf("an entry from before the clear must be re-derived: calls %d -> %d", before, calls)
		}
		m.mu.Lock()
		size := len(m.entries)
		m.mu.Unlock()
		if size > formulaNameMemoCap {
			t.Fatalf("memo holds %d entries, cap %d", size, formulaNameMemoCap)
		}
	})
}
