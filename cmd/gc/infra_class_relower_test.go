package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// relowerExec is a runtime whose probe the test sets and whose replacement
// steps settle at a boundary.
type relowerExec struct {
	mu         sync.Mutex
	probe      opjournal.Probe
	interrupt  opjournal.ControlResult
	interrupts []string
}

func (e *relowerExec) Probe(context.Context, opjournal.ConversationView) (opjournal.Probe, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.probe, nil
}

func (e *relowerExec) Deliver(context.Context, opjournal.Delivery) opjournal.DeliveryResult {
	return opjournal.Delivered{Record: opjournal.DeliveryRecord{At: time.Now().UTC(), Ack: opjournal.AckDelivered}, Via: "test"}
}

func (e *relowerExec) Interrupt(_ context.Context, c opjournal.InterruptCall) opjournal.ControlResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.interrupts = append(e.interrupts, c.Op.ID)
	return e.interrupt
}

func (e *relowerExec) Respond(context.Context, opjournal.RespondCall) opjournal.ControlResult {
	return opjournal.Settled{}
}

// relowerProbe is a live runtime whose turn t1 runs (busy), or one that is
// not live, on which no unit is taken.
func relowerProbe(live bool) opjournal.Probe {
	return opjournal.Probe{
		Available: true, Running: true, Boundary: opjournal.TriNo, TurnLive: opjournal.TriYes, Turn: "t1",
		Caps: opjournal.Caps{Live: live, Input: opjournal.FenceAtomic},
	}
}

// relowerView is conversation conv's view with a session awake.
func relowerView(conv string) opjournal.ConversationView {
	return opjournal.ConversationView{ID: conv, Generation: 1, Epoch: 1, Token: "tok"}
}

// relowerNow is a deliver-now prompt on conversation conv.
func relowerNow(conv, key string) opjournal.Request {
	return opjournal.Request{
		Conversation: conv, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane, ClientKey: key,
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorBackground},
		Payload:  "stop and do this",
		Policy:   opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RolePrompt, When: opjournal.WhenNow}},
	}
}

// relowerCity is a city whose work store holds lane ops before a migrate:
// m1 lowered with its unit open, m2 not lowered, m3 lowered with its unit
// over (i3 closed), and a plain notice.
type relowerCity struct {
	request            storageOperatorRequest
	target             infraBindingTarget
	source             *beads.MemStore
	clk                *clock.Fake
	m1, i1, m2, m3, i3 opjournal.Op
	notice             opjournal.Op
	// rows are the op rows as the work store held them before the migrate.
	rows map[string]beads.Bead
}

// newRelowerCity builds the city and stubs the migration's source and
// controller probe.
func newRelowerCity(t *testing.T) *relowerCity {
	t.Helper()
	cfg := infraSplitConfig(t.TempDir() + "/store")
	c := &relowerCity{request: storageTestRequest(t, cfg)}
	c.target = mustResolveInfraTarget(t, c.request.CityPath, cfg)
	c.source = &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return c.source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	stubInfraControllerPing(t, 0)

	clk := &clock.Fake{Time: time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)}
	c.clk = clk
	store, err := opjournal.NewLaneStore(beads.SessionStore{Store: c.source}, clk, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("NewLaneStore: %v", err)
	}
	lane, err := opjournal.NewLane(store, "ctl", clk)
	if err != nil {
		t.Fatalf("NewLane: %v", err)
	}
	propose := func(req opjournal.Request) opjournal.Op {
		t.Helper()
		clk.Advance(time.Millisecond)
		op, created, err := store.Propose(req)
		if err != nil || !created {
			t.Fatalf("Propose: created %v, %v", created, err)
		}
		return op
	}
	pass := func(conv string, exec *relowerExec) opjournal.PassResult {
		t.Helper()
		res, err := lane.Pass(context.Background(), relowerView(conv), exec)
		if err != nil {
			t.Fatalf("Pass(%s): %v", conv, err)
		}
		return res
	}
	c.m1 = propose(relowerNow("gc-1", "m1"))
	if res := pass("gc-1", &relowerExec{probe: relowerProbe(false)}); !slices.Equal(res.Lowered, []string{c.m1.ID}) {
		t.Fatalf("lowering pass %+v", res)
	}
	c.m3 = propose(relowerNow("gc-3", "m3"))
	over := &relowerExec{probe: relowerProbe(true), interrupt: opjournal.Settled{Turn: "t1", After: opjournal.TurnAfter{Boundary: opjournal.TriNo, Live: opjournal.TriYes, Turn: "t2"}}}
	if res := pass("gc-3", over); res.Took != opjournal.TakeNow {
		t.Fatalf("unit pass %+v", res)
	}
	c.m2 = propose(relowerNow("gc-2", "m2"))
	c.notice = propose(opjournal.Request{
		Conversation: "gc-1", Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane, ClientKey: "n",
		Producer: opjournal.Producer{Principal: "gc:mail", Actor: opjournal.ActorBackground}, Payload: "fyi",
		Policy: opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RoleNotice}},
	})
	get := func(id string) opjournal.Op {
		t.Helper()
		op, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		return op
	}
	i1, err := opjournal.NowInterruptID("gc", c.m1)
	if err != nil {
		t.Fatal(err)
	}
	i3, err := opjournal.NowInterruptID("gc", c.m3)
	if err != nil {
		t.Fatal(err)
	}
	c.m1, c.i1, c.m2, c.m3, c.i3 = get(c.m1.ID), get(i1), get(c.m2.ID), get(c.m3.ID), get(i3)
	if c.m1.Status != opjournal.StatusAccepted || c.i1.Status != opjournal.StatusSubmitted || c.m2.Status != opjournal.StatusSubmitted ||
		c.m3.Status != opjournal.StatusAccepted || c.i3.Status != opjournal.StatusClosed {
		t.Fatalf("fixture: m1 %s i1 %s m2 %s m3 %s i3 %s", c.m1.Status, c.i1.Status, c.m2.Status, c.m3.Status, c.i3.Status)
	}
	c.rows = map[string]beads.Bead{}
	for _, id := range []string{c.m1.ID, c.i1.ID, c.m2.ID, c.m3.ID, c.i3.ID, c.notice.ID} {
		b, err := c.source.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		c.rows[id] = b
	}
	return c
}

// migrate runs the operator command once.
func (c *relowerCity) migrate(t *testing.T) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageMigrate(context.Background(), c.request, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// bootStore opens the binding's sessions store as boot routes it, and its op
// store; both close with the test.
func (c *relowerCity) bootStore(t *testing.T) (beads.Store, *opjournal.Store) {
	t.Helper()
	plan, err := resolveCityStoragePlan(c.request.CityPath, c.request.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := openStorageRoutes(plan, c.target, c.request.Cfg, c.request.CityPath, nil)
	if err != nil {
		t.Fatalf("openStorageRoutes: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	sessions, ok := routes.storeFor(coordclass.ClassSessions)
	if !ok {
		t.Fatal("the routes serve no sessions class")
	}
	store, err := opjournal.NewLaneStore(beads.SessionStore{Store: sessions}, c.clk)
	if err != nil {
		t.Fatalf("NewLaneStore: %v", err)
	}
	return sessions, store
}

// faultingRelowerEngine routes an engine's guarded batches through a test
// batcher; every other capability resolves to the engine.
type faultingRelowerEngine struct {
	beads.Store
	batcher beads.GuardedBatcher
}

func (e faultingRelowerEngine) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return e.batcher, true
}

func (e faultingRelowerEngine) ConditionalWritesResolveTarget() beads.Store { return e.Store }

// countingBatcher counts batches and fails them while fail is set, without
// committing.
type countingBatcher struct {
	inner beads.GuardedBatcher
	n     *int
	fail  bool
}

func (b countingBatcher) ApplyGuarded(ctx context.Context, batch beads.GuardedBatch) (beads.GuardedResult, error) {
	*b.n++
	if b.fail {
		return beads.GuardedResult{}, errors.New("link dropped mid-batch")
	}
	return b.inner.ApplyGuarded(ctx, batch)
}

// stubRelowerBatches routes the step's batches through a countingBatcher
// that fails while *fail is set, and returns the batch count.
func stubRelowerBatches(t *testing.T, fail *bool) *int {
	t.Helper()
	n := new(int)
	stubRelowerStore(t, func(engine beads.Store, open func(beads.Store) (*opjournal.Store, error)) (*opjournal.Store, error) {
		inner, ok := beads.GuardedBatcherFor(engine)
		if !ok {
			t.Fatal("the binding engine applies no guarded batches")
		}
		return open(faultingRelowerEngine{Store: engine, batcher: countingBatcher{inner: inner, n: n, fail: *fail}})
	})
	return n
}

// stubRelowerStore replaces the step's lane-store constructor with wrap,
// which is handed the engine the step opened and the constructor it
// replaces.
func stubRelowerStore(t *testing.T, wrap func(engine beads.Store, open func(beads.Store) (*opjournal.Store, error)) (*opjournal.Store, error)) {
	t.Helper()
	prev := newInfraRelowerStore
	newInfraRelowerStore = func(engine beads.Store) (*opjournal.Store, error) { return wrap(engine, prev) }
	t.Cleanup(func() { newInfraRelowerStore = prev })
}

// opMetadata is an op row's metadata without the migration's stamp: the
// fields the copy proved.
func opMetadata(t *testing.T, b beads.Bead) map[string]string {
	t.Helper()
	op, err := opjournal.DecodeOp(b)
	if err != nil {
		t.Fatalf("DecodeOp(%s): %v", b.ID, err)
	}
	enc, err := opjournal.EncodeOp(op)
	if err != nil {
		t.Fatal(err)
	}
	return enc.Metadata
}

// migratedLinePattern is the success line as this command printed it before
// the re-lowering step existed.
var migratedLinePattern = regexp.MustCompile("^Migrated\\. infra now serves [^ ]+ from [^;]+; ([^\\n]*cop\\(ies\\) cleared[^\\n]*)\\. Start the city with `gc start`\\.\\n$")

// A migrate re-lowers the open deliver-now unit it carried: the success line
// says so, stderr names the message and both ids, the binding holds the
// interrupt at its home id with a pointer at the old one, every other op row
// is as the copy proved it, and a lane opened as boot opens it runs the
// unit under the new id and lowers the unlowered message natively.
func TestStorageMigrateRelowersDeliverNowUnits(t *testing.T) {
	c := newRelowerCity(t)
	code, stdout, stderr := c.migrate(t)
	if code != 0 {
		t.Fatalf("migrate exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		t.Fatal(err)
	}
	home, err := opjournal.NowInterruptID(prefix, c.m1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "; re-lowered 1 deliver-now unit under "+prefix+". Start the city with") {
		t.Errorf("success line %q does not report the unit", stdout)
	}
	if want := "gc storage migrate: re-lowered deliver-now message " + c.m1.ID + " under " + prefix + ": its interrupt " + c.i1.ID + " is now " + home + "\n"; !strings.Contains(stderr, want) {
		t.Errorf("stderr %q does not carry %q", stderr, want)
	}
	if strings.Contains(stderr, "left deliver-now message") {
		t.Errorf("stderr %q reports a unit left; none should be", stderr)
	}
	sessions, store := c.bootStore(t)
	if store.Prefix() != prefix {
		t.Fatalf("the boot lane store mints under %q, want %q", store.Prefix(), prefix)
	}
	moved, err := store.Get(home)
	if err != nil || moved.Status != opjournal.StatusSubmitted || moved.Causation != c.m1.ID {
		t.Fatalf("Get(%s) = %+v, %v; want the open interrupt", home, moved, err)
	}
	pointer, err := store.Get(c.i1.ID)
	if err != nil || pointer.Status != opjournal.StatusClosed || pointer.Outcome.Reason != opjournal.ReasonCoalescedInto || pointer.Outcome.Result.CoalescedInto != home {
		t.Fatalf("Get(%s) = %+v, %v; want a pointer to %s", c.i1.ID, pointer.Outcome, err, home)
	}
	for _, id := range []string{c.m1.ID, c.m2.ID, c.m3.ID, c.i3.ID, c.notice.ID} {
		b, err := sessions.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got, want := opMetadata(t, b), opMetadata(t, c.rows[id]); !maps.Equal(got, want) {
			t.Errorf("row %s changed past the copy:\n got %v\nwant %v", id, got, want)
		}
	}
	lane, err := opjournal.NewLane(store, "boot", c.clk)
	if err != nil {
		t.Fatal(err)
	}
	exec := &relowerExec{probe: relowerProbe(true), interrupt: opjournal.Settled{Turn: "t1", After: opjournal.TurnAfter{Boundary: opjournal.TriYes, Live: opjournal.TriNo, Known: []string{"t1"}}}}
	res, err := lane.Pass(context.Background(), relowerView("gc-1"), exec)
	if err != nil || res.Took != opjournal.TakeNow || !slices.Equal(res.Taken, []string{home, c.m1.ID}) {
		t.Fatalf("Pass = %+v, %v; want the unit [%s %s]", res, err, home, c.m1.ID)
	}
	if !slices.Equal(exec.interrupts, []string{home}) {
		t.Fatalf("interrupt calls %v, want one on %s", exec.interrupts, home)
	}
	res, err = lane.Pass(context.Background(), relowerView("gc-2"), &relowerExec{probe: relowerProbe(false)})
	if err != nil || !slices.Equal(res.Lowered, []string{c.m2.ID}) {
		t.Fatalf("Pass = %+v, %v; want %s lowered", res, err, c.m2.ID)
	}
	native, err := opjournal.NowInterruptID(prefix, c.m2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(native); err != nil {
		t.Fatalf("Get(%s): %v; want the lowering under %s", native, err, prefix)
	}
}

// A step that does not finish exits 1 on a converged city and names the
// re-run; the re-run finishes it, and a third run prints exactly what the
// command printed before the step existed, with no batch.
func TestStorageMigrateRerunFinishesTheRelowering(t *testing.T) {
	c := newRelowerCity(t)
	fail := true
	batches := stubRelowerBatches(t, &fail)
	code, stdout, stderr := c.migrate(t)
	if code != 1 || strings.Contains(stdout, "Migrated.") {
		t.Fatalf("faulted migrate exited %d with stdout %q, want 1 and no Migrated line", code, stdout)
	}
	if !strings.Contains(stderr, "re-lowering deliver-now units did not finish") || !strings.Contains(stderr, "`gc storage migrate --from-work --fleet-stopped` again") {
		t.Errorf("stderr %q does not name the re-run", stderr)
	}
	if _, err := os.Stat(c.target.MarkerPath()); err != nil {
		t.Fatalf("the marker is gone after the step failed: %v", err)
	}
	fail = false
	code, stdout, stderr = c.migrate(t)
	if code != 0 || !strings.Contains(stdout, "; re-lowered 1 deliver-now unit under ") {
		t.Fatalf("re-run exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	*batches = 0
	code, stdout, stderr = c.migrate(t)
	if code != 0 || !migratedLinePattern.MatchString(stdout) || strings.Contains(stdout, "re-lowered") || strings.Contains(stderr, "deliver-now") {
		t.Fatalf("third run exited %d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	if *batches != 0 {
		t.Fatalf("third run issued %d batches, want none", *batches)
	}
}

// The step never runs before the convergence marker: a migrate whose marker
// does not land leaves no row under the binding's op prefix, and a healthy
// re-run converges and re-lowers.
func TestStorageMigrateRelowersOnlyAConvergedBinding(t *testing.T) {
	c := newRelowerCity(t)
	prev := infraMigrationRename
	infraMigrationRename = func(from, to string) error {
		if to == c.target.MarkerPath() {
			return errors.New("the marker did not land")
		}
		return prev(from, to)
	}
	code, stdout, stderr := c.migrate(t)
	infraMigrationRename = prev
	if code != 1 {
		t.Fatalf("migrate exited %d with the marker failing\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertNoConvergenceMarker(t, c.target, "the marker failed")
	if strings.Contains(stderr, "re-lowered") {
		t.Fatalf("stderr %q: the step ran before the marker", stderr)
	}
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		t.Fatal(err)
	}
	dest := openMigratedDestination(t, c.target)
	for _, id := range infraStoreFingerprint(t, dest) {
		if strings.HasPrefix(id, prefix+"-op-") {
			t.Fatalf("row %s minted under %s before the marker", id, prefix)
		}
	}
	_ = closeBeadStoreHandle(dest)
	code, stdout, stderr = c.migrate(t)
	if code != 0 || !strings.Contains(stdout, "; re-lowered 1 deliver-now unit under ") {
		t.Fatalf("re-run exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

// The step's store is the runtime's: the engine the step writes through is
// fenced to the binding's namespaces, the lane store over it mints under the
// binding's prefix, and what it writes reads back through boot's routes.
func TestRelowerOpensTheBindingAsTheRuntimeDoes(t *testing.T) {
	c := newRelowerCity(t)
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	stubRelowerStore(t, func(engine beads.Store, open func(beads.Store) (*opjournal.Store, error)) (*opjournal.Store, error) {
		opened++
		batcher, ok := beads.GuardedBatcherFor(engine)
		if !ok {
			t.Fatal("the step's engine applies no guarded batches")
		}
		outside := beads.Bead{ID: "zz-op-aaaaaaaaaaaaaaaaaaaaaaaaaa", Title: "outside", Type: "task"}
		if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Reason: "test", Items: []beads.GuardedItem{{Create: &outside}}}); !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
			t.Errorf("a create outside the binding's namespaces through the step's engine = %v, want ErrPinnedIDOutsideNamespace", err)
		}
		store, err := open(engine)
		if err == nil && store.Prefix() != prefix {
			t.Errorf("the step's store mints under %q, want %q", store.Prefix(), prefix)
		}
		return store, err
	})
	if code, stdout, stderr := c.migrate(t); code != 0 {
		t.Fatalf("migrate exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if opened != 1 {
		t.Fatalf("the step opened %d lane stores, want one", opened)
	}
	home, err := opjournal.NowInterruptID(prefix, c.m1)
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := c.bootStore(t)
	if _, err := sessions.Get(home); err != nil {
		t.Fatalf("boot's sessions store cannot read %s: %v", home, err)
	}
}

// A unit the step leaves where it is (two open derived interrupts) is named
// with its reason, and the migrate still succeeds: the lane runs the unit by
// identity, and nothing was moved.
func TestStorageMigrateNamesAUnitItLeaves(t *testing.T) {
	c := newRelowerCity(t)
	store, err := opjournal.NewLaneStore(beads.SessionStore{Store: c.source}, c.clk, opjournal.WithIDPrefix("zz"))
	if err != nil {
		t.Fatal(err)
	}
	twinReq := opjournal.Request{
		Conversation: c.m1.Conversation, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane,
		ClientKey: c.m1.ID + ":interrupt", Causation: c.m1.ID, Producer: c.m1.Producer,
		Scope: opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
	}
	twin, created, err := store.Propose(twinReq)
	if err != nil || !created {
		t.Fatalf("Propose(twin): created %v, %v", created, err)
	}
	code, stdout, stderr := c.migrate(t)
	if code != 0 || !migratedLinePattern.MatchString(stdout) {
		t.Fatalf("migrate exited %d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	ids := []string{c.i1.ID, twin.ID}
	slices.Sort(ids)
	want := "gc storage migrate: left deliver-now message " + c.m1.ID + " where it is: it has 2 open derived interrupts (" +
		strings.Join(ids, ", ") + "); its lane runs it by identity\n"
	if got := relowerStepStderr(t, c.target, stderr); got != want {
		t.Fatalf("the step's stderr %q, want exactly %q", got, want)
	}
}

// A city without lane ops prints exactly what the command printed before
// the step existed, and opens no lane store.
func TestStorageMigrateWithoutLaneOpsPrintsNothingNew(t *testing.T) {
	cfg := infraSplitConfig(t.TempDir() + "/store")
	request := storageTestRequest(t, cfg)
	target := mustResolveInfraTarget(t, request.CityPath, cfg)
	source := stubInfraMigrationSource(t)
	stubInfraControllerPing(t, 0)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	mustCreateInfraBead(t, source, beads.Bead{Title: "ordinary work", Type: "task"})
	assertMigratePrintsWhatItAlwaysPrinted(t, request, target)
}

// A city without lane ops whose binding is the pre-revision layout (no
// revision column, so no guarded batches: it hosts no lane) migrates as it
// did before the step existed, on every run.
func TestStorageMigrateWithoutLaneOpsOverALegacyBinding(t *testing.T) {
	cfg := infraSplitConfig(t.TempDir() + "/store")
	request := storageTestRequest(t, cfg)
	target := mustResolveInfraTarget(t, request.CityPath, cfg)
	opened, err := beads.OpenSQLiteStore(target.Dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	if err := opened.(interface{ CloseStore() error }).CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	rewriteToPreRevisionLayout(t, target.Database)
	source := stubInfraMigrationSource(t)
	stubInfraControllerPing(t, 0)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	mustCreateInfraBead(t, source, beads.Bead{Title: "ordinary work", Type: "task"})
	for range 2 {
		assertMigratePrintsWhatItAlwaysPrinted(t, request, target)
	}
}

// A failed probe read is the step's error, naming the binding, never "no
// lowered message": the migrate does not report success over units it never
// read, and no lane store is opened.
func TestRelowerProbeReadErrorIsTheStepsError(t *testing.T) {
	opened := 0
	stubRelowerStore(t, func(engine beads.Store, open func(beads.Store) (*opjournal.Store, error)) (*opjournal.Store, error) {
		opened++
		return open(engine)
	})
	b := convergedInfraBinding{target: infraBindingTarget{Binding: "infra"}}
	_, err := relowerNowUnitsOn(context.Background(), b, failingListStore{beads.NewMemStore()})
	if !errors.Is(err, errDarkLeg{}) || !strings.Contains(err.Error(), `binding "infra"`) {
		t.Fatalf("relowerNowUnitsOn = %v, want the probe's read error naming the binding", err)
	}
	if opened != 0 {
		t.Fatalf("%d lane stores opened after a failed probe, want none", opened)
	}
}

// relowerStepStderr is a migrate's stderr without the lines the migration
// printed before the re-lowering step existed: the copy's line and the
// clear's.
func relowerStepStderr(t *testing.T, target infraBindingTarget, stderr string) string {
	t.Helper()
	own := []*regexp.Regexp{
		regexp.MustCompile("^gc storage migrate: infrastructure classes migrated to " + regexp.QuoteMeta(target.Database) + ` \(\d+ beads copied\)$`),
		regexp.MustCompile(`^gc storage migrate: \d+ retained work-store cop\(ies\) cleared \(backup: ` + regexp.QuoteMeta(target.RetainedBackupPath()) + `\)[^\n]*$`),
	}
	var rest strings.Builder
	for _, line := range strings.SplitAfter(stderr, "\n") {
		if line == "" || slices.ContainsFunc(own, func(re *regexp.Regexp) bool { return re.MatchString(strings.TrimSuffix(line, "\n")) }) {
			continue
		}
		rest.WriteString(line)
	}
	return rest.String()
}

// assertMigratePrintsWhatItAlwaysPrinted runs a migrate of a city with no
// lane ops and fails unless it exits 0 with exactly the success line the
// command printed before the step existed, nothing on stderr, and no lane
// store opened.
func assertMigratePrintsWhatItAlwaysPrinted(t *testing.T, request storageOperatorRequest, target infraBindingTarget) {
	t.Helper()
	opened := 0
	stubRelowerStore(t, func(engine beads.Store, open func(beads.Store) (*opjournal.Store, error)) (*opjournal.Store, error) {
		opened++
		return open(engine)
	})
	var stdout, stderr bytes.Buffer
	code := doStorageMigrate(context.Background(), request, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("migrate exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	m := migratedLinePattern.FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("stdout %q is not the line the command always printed", stdout.String())
	}
	want := fmt.Sprintf("Migrated. %s now serves %s from %s; %s. Start the city with `gc start`.\n",
		target.Binding, infraMigrationClassList(), target.Database, m[1])
	if stdout.String() != want || strings.Contains(m[1], "re-lowered") {
		t.Errorf("stdout %q, want exactly %q", stdout.String(), want)
	}
	if got := relowerStepStderr(t, target, stderr.String()); got != "" {
		t.Errorf("stderr %q carries %q past the migration's own lines, want nothing", stderr.String(), got)
	}
	if opened != 0 {
		t.Errorf("the step opened %d lane stores, want none", opened)
	}
}
