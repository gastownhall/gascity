package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// Creates on the session executor (C4b; CONTRACT v5 C1, P3, P5, P7) and
// C0.7's boot refusal.

// withCreates turns f's planner's effects on, on a clock stopped at
// gatherNow (the pass's deadlines run from it), with a create runner whose
// identifier locks are a no-op, and returns the executor.
func withCreates(t *testing.T, f *gatherFixture) *effectExecutor {
	t.Helper()
	x := newEffectExecutor(f.p.settlements.post, io.Discard)
	x.clock = newFakePlannerClock(gatherNow)
	creates, err := newCreateEffects(createEffectHost{
		cityPath: f.env.CityPath, cityName: f.env.CityName, lookPath: f.env.LookPath,
		withLocks: func(_ string, _ []string, fn func() error) error { return fn() },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.p.effects, f.p.creates = x, creates
	return x
}

// createEntries are v's create entries.
func createEntries(v inflightView) []inflightEntry {
	var out []inflightEntry
	for _, e := range v.Entries {
		if e.Kind == inflightCreate {
			out = append(out, e)
		}
	}
	return out
}

// Kills over-creating in the next pass: the planner records each admitted
// create's entry, with its identity and planning stand-in, before it submits
// the effect, so a pass that runs before the effect settles proposes no
// second create for the same demand.
func TestPlannerRecordsCreateBeforeSubmit(t *testing.T) {
	f := newGatherFixture(t, routedDemandBead("gc-r1"), routedDemandBead("gc-r2"))
	x := withCreates(t, f)
	x.spawn = func(func()) {} // no effect ever runs or settles
	f.passRecord(t)
	entries := createEntries(f.inflight.view())
	if len(entries) != 2 {
		t.Fatalf("create entries = %+v, want one per admitted create", entries)
	}
	for _, e := range entries {
		if e.Token == "" || e.Identity == "" || e.Template == "" || e.Leg == "" || e.Seq == 0 {
			t.Fatalf("entry %+v, want its token, identity, stand-in and sessions leg", e)
		}
	}
	if rec := f.passRecord(t); slices.Contains(intentKeys(rec.Admitted), intentCreate) {
		t.Fatalf("the next pass admitted %v while both creates run, want no second create", intentKeys(rec.Admitted))
	}
}

// Kills a create without a token, a token reused across plans, and a second
// worker pool: the planner mints a distinct token per create at submit, each
// row lands carrying its entry's token through the executor (a closed
// executor writes no row), and the landings clear the entries.
func TestPlannerMintsCreateTokenAtSubmit(t *testing.T) {
	f := newGatherFixture(t, routedDemandBead("gc-r1"), routedDemandBead("gc-r2"))
	x := withCreates(t, f)
	f.passRecord(t)
	var tokens []string
	for _, e := range createEntries(f.inflight.view()) {
		tokens = append(tokens, e.Token)
	}
	x.stop(time.Now().Add(time.Minute))
	var rowTokens []string
	for _, row := range sessionRows(t, f.cache) {
		rowTokens = append(rowTokens, row.InstanceToken)
	}
	slices.Sort(tokens)
	slices.Sort(rowTokens)
	if len(tokens) != 2 || tokens[0] == tokens[1] || tokens[0] == "" || !slices.Equal(tokens, rowTokens) {
		t.Fatalf("entry tokens %q, row tokens %q; want two distinct minted tokens, one per landed row", tokens, rowTokens)
	}
	f.p.drainSettlements(gatherNow)
	if v := f.inflight.view(); len(v.Entries) != 0 {
		t.Fatalf("in flight after the creates landed = %+v, want none", v.Entries)
	}

	closed := newGatherFixture(t, routedDemandBead("gc-r1"))
	withCreates(t, closed).close()
	closed.passRecord(t)
	if rows := sessionRows(t, closed.cache); len(rows) != 0 || len(closed.inflight.view().Entries) != 0 {
		t.Fatalf("rows %+v, entries %+v; want no create outside the executor, and the refused submit settled", rows, closed.inflight.view().Entries)
	}
}

// Kills a create keyed by its (empty) row in the single-flight, which runs
// one create at a time and refuses the rest: creates are keyed by token, so
// two run together and only the same token is busy.
func TestExecutorKeysCreatesByToken(t *testing.T) {
	x, _ := fakeClockExecutor(newFakePlannerClock(plannerT0))
	release := make(chan struct{})
	defer close(release)
	create := func(token string) sessionEffect {
		e := hungEffect(intentCreate, 1, plannerT0.Add(time.Minute), release)
		e.Token = token
		return e
	}
	if err := x.submit(rowKey{}, create("tok-a")); err != nil {
		t.Fatal(err)
	}
	if err := x.submit(rowKey{}, create("tok-b")); err != nil {
		t.Fatalf("a second create with its own token = %v, want it running", err)
	}
	if err := x.submit(rowKey{}, create("tok-a")); !errors.Is(err, errEffectBusy) {
		t.Fatalf("a second effect for tok-a = %v, want busy", err)
	}
}

// Kills a create abandoned at its deadline settling failed (its entry would
// clear while its row may still land), an executor-built settlement without
// the token (the entry would never clear), and a deadline settlement without
// its alert: it settles ambiguous with its token and seq, raises
// effect-deadline, and its entry holds until the census shows the token.
func TestAbandonedCreateSettlesAmbiguousWithToken(t *testing.T) {
	clk := newFakePlannerClock(plannerT0)
	x, posted := fakeClockExecutor(clk)
	release := make(chan struct{})
	defer close(release)
	m := newInflightMap()
	seq := m.add(inflightEntry{Kind: inflightCreate, Token: "tok-1", Identity: "worker/worker-1", Leg: "sessions"})
	e := hungEffect(intentCreate, seq, plannerT0.Add(time.Minute), release)
	e.Token = "tok-1"
	if err := x.submit(rowKey{}, e); err != nil {
		t.Fatal(err)
	}
	waitTimersAt(t, clk, plannerT0.Add(time.Minute), 2)
	clk.Advance(time.Minute)
	s := receive(t, posted)
	if s.Outcome != settledAmbiguous || s.Token != "tok-1" || s.Seq != seq || s.Cause != causeDeadline {
		t.Fatalf("settlement %+v, want ambiguous at the deadline with tok-1", s)
	}
	var stderr strings.Builder
	newPlanner(clk, func() time.Duration { return time.Minute }, nil, m, nil, &stderr).observeSettlement(s)
	if !strings.Contains(stderr.String(), "alert "+alertEffectDeadline) {
		t.Errorf("stderr = %q, want the %s alert for the abandoned create", stderr.String(), alertEffectDeadline)
	}
	s.At = plannerT0.Add(time.Minute) // as the planner's drain stamps it
	m.settle(s)
	if got := m.clearVisible(inflightCensus{}, plannerT0.Add(2*time.Minute)); len(got) != 0 || len(m.view().Entries) != 1 {
		t.Fatalf("cleared %+v, entries %+v; want the ambiguous entry held", got, m.view().Entries)
	}
	if got := m.clearVisible(inflightCensus{Tokens: map[string]bool{"tok-1": true}}, plannerT0.Add(2*time.Minute)); len(got) != 1 || got[0].HardBound {
		t.Fatalf("cleared %+v, want the entry cleared by its token", got)
	}
}

// Kills clearVisible left unwired from the pass, or its hard-bound clears
// left off OBS1's alert (P5): an ambiguous create whose row never showed
// clears at three minutes from its settlement with one ambiguous-create-bound
// alert naming its identity and leg; one whose token the census shows clears
// silently.
func TestPassClearsAmbiguousCreatesAndAlertsAtHardBound(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-seen"))
	var stderr strings.Builder
	f.p.stderr = &stderr
	for _, tok := range []string{"tok-seen", "tok-lost"} {
		seq := f.inflight.add(inflightEntry{Kind: inflightCreate, Token: tok, Identity: "worker/" + tok, Leg: "city"})
		f.inflight.settle(settlement{Kind: inflightCreate, Seq: seq, Token: tok, Outcome: settledAmbiguous, At: gatherNow.Add(-inflightHardBound)})
	}
	f.passRecord(t)
	if v := f.inflight.view(); len(v.Entries) != 0 {
		t.Fatalf("in flight = %+v, want both ambiguous creates cleared", v.Entries)
	}
	got := stderr.String()
	if strings.Count(got, "alert "+alertAmbiguousBound) != 1 || !strings.Contains(got, "worker/tok-lost on leg \"city\"") || strings.Contains(got, "tok-seen") {
		t.Fatalf("stderr %q, want one %s alert, for worker/tok-lost on leg city", got, alertAmbiguousBound)
	}
}

// Kills a false ambiguous-create-bound alert for a named create (P5 as
// amended): a named identity has one open row (the flock, C11), so a named
// ambiguous create clears once the census shows an open canonical row of its
// identity, whatever token the row carries. That covers a reopen (the row
// keeps its own token), an AdoptLive whose runtime's token was re-stamped on
// the row, and a create abandoned before its write while another writer
// made the row. A named create whose identity has no row, or a pool create,
// still waits for its token or the bound.
func TestNamedAmbiguousCreateClearsOnItsIdentityRow(t *testing.T) {
	for _, tc := range []struct{ name, rowToken string }{
		{"reopen keeps the row's token", "tok-row-own"},
		{"adopt-live re-stamps the runtime's token", "tok-runtime"},
		{"abandoned before its write, row made elsewhere", "tok-other-writer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGatherFixture(t, chatRow("gc-1", "3", "instance_token", tc.rowToken))
			var stderr strings.Builder
			f.p.stderr = &stderr
			for _, e := range []inflightEntry{
				{Kind: inflightCreate, Token: "tok-plan", Identity: "named:chat", Leg: "city"},
				{Kind: inflightCreate, Token: "tok-boss", Identity: "named:boss", Leg: "city"},
				{Kind: inflightCreate, Token: "tok-pool", Identity: "chat/chat-1", Leg: "city"},
			} {
				seq := f.inflight.add(e)
				f.inflight.settle(settlement{Kind: inflightCreate, Seq: seq, Token: e.Token, Outcome: settledAmbiguous, At: gatherNow.Add(-time.Minute)})
			}
			f.passRecord(t)
			var left []string
			for _, e := range createEntries(f.inflight.view()) {
				left = append(left, e.Identity)
			}
			slices.Sort(left)
			if !slices.Equal(left, []string{"chat/chat-1", "named:boss"}) || strings.Contains(stderr.String(), "alert "+alertAmbiguousBound) {
				t.Fatalf("left in flight %v, stderr %q; want named:chat cleared by its row, silently, and the rest held", left, stderr.String())
			}
		})
	}
}

// Kills a create admitted before a provider swap's pause running during it
// (P7, the C2c2 swap race): while starts are closed a create runs nothing
// and settles refused with cause swap-pause and its token, which backs
// nothing off.
func TestExecutorRefusesCreatesWhileStartsClosed(t *testing.T) {
	x, posted := fakeClockExecutor(newFakePlannerClock(plannerT0))
	x.closeStarts()
	ran := false
	if err := x.submit(rowKey{}, sessionEffect{Kind: intentCreate, Seq: 4, Token: "tok-1", Deadline: plannerT0.Add(time.Minute), Run: func(context.Context) settlement {
		ran = true
		return settlement{Outcome: settledLanded}
	}}); err != nil {
		t.Fatal(err)
	}
	if s := receive(t, posted); ran || s.Outcome != settledRefused || s.Cause != causeSwapPause || s.Token != "tok-1" || s.Seq != 4 {
		t.Fatalf("settlement %+v (ran %v), want a swap-pause refusal carrying tok-1", s, ran)
	}
}

// Kills a shutdown that abandons creates in flight (GUAR-013): the
// planner's stop waits, through executor.stop, for a create parked inside
// the identifier locks, and returns once it landed.
func TestPlannerStopJoinsCreateEffects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := &gateLocker{release: make(chan struct{})}
		creates, err := newCreateEffects(createEffectHost{cityPath: t.TempDir(), cityName: "test-city", withLocks: gate.withLocks})
		if err != nil {
			t.Fatal(err)
		}
		x := newEffectExecutor(func(settlement) {}, io.Discard)
		p := newPlanner(realPlannerClock{}, func() time.Duration { return time.Minute }, nil, newInflightMap(), x.stop, io.Discard)
		cfg, store := workerCity(2), beads.NewMemStore()
		plan := workerPlan(cfg, "c1", 1)
		plan.Token = "tok-1"
		pass := &effectPass{create: &createPass{cfg: cfg, store: store}, creates: creates}
		if err := x.submitIntent(pass, intent{Kind: intentCreate, CreatePlan: plan, Deadline: time.Now().Add(time.Minute)}, 1); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		stopped := make(chan struct{})
		go func() {
			p.stop(time.Now().Add(time.Hour))
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stop returned while a create was in flight")
		default:
		}
		close(gate.release)
		<-stopped
		if rows := sessionRows(t, store); len(rows) != 1 || rows[0].InstanceToken != "tok-1" {
			t.Fatalf("rows = %+v, want the joined create landed", rows)
		}
	})
}

// Kills a C0.7 bypass, or the check left unwired from the city's host: over
// stores with conditional writes off, boot refuses at once, naming each class
// store, its mode and its missing capability, and the conditional_writes
// fix, raises boot-refused, and never starts the planner.
func TestBootRefusesWithoutConditionalWriter(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, true)
	stderr := &lockedBuffer{}
	cr.stderr = stderr
	rt := attachTestV2(t, cr)
	rt.host.capabilities = cr.newPlannerHost().capabilities
	primeTestV2(t, cr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cr.bootV2(ctx) || ctx.Err() != nil {
		t.Fatal("bootV2 reached ready, or retried, over stores without a conditional writer")
	}
	for _, want := range []string{
		"alert " + alertBootRefused, "C0.7", "conditional_writes=unset", `set [beads] conditional_writes = "auto"`,
		"sessions store (", "graph store (", "conditional writer false (conditional writes are off)",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	rt.mu.Lock()
	started := rt.started
	rt.mu.Unlock()
	if started || rt.census.Load() {
		t.Error("a C0.7 refusal ran the census or started the planner")
	}
}

// Kills a C0.7 that checks the writer alone, or names the wrong fix: a store
// that resolves a conditional writer but no atomic conditional closer (an
// auto-stamped MemStore) refuses boot, naming its kind, mode and closer, and
// a store that fences as the fix.
func TestBootRefusesWithoutAtomicCloser(t *testing.T) {
	mem := fencedMemStore(t)
	caps := v2ClassStoreCapabilities(mem, mem, false)
	if !caps[0].Writer || caps[0].Closer {
		t.Fatalf("capabilities %+v, want a writer and no closer", caps)
	}
	refusal := v2CapabilityRefusal(caps)
	if refusal == nil || !strings.Contains(refusal.Error(), "sessions store (MemStore, conditional_writes=auto): conditional writer true, atomic conditional closer false") ||
		!strings.Contains(refusal.Error(), "a store that fences") || strings.Contains(refusal.Error(), "conditional_writes = ") {
		t.Fatalf("C0.7 = %v, want a refusal naming the missing closer and a store that fences", refusal)
	}
	rt := newDefaultPlanner(io.Discard)
	rt.host.capabilities = func() error { return refusal }
	if err := rt.boot(context.Background()); !errors.Is(err, refusal) {
		t.Fatalf("boot = %v, want the C0.7 refusal", err)
	}
	if rt.census.Load() {
		t.Error("a C0.7 refusal ran the census")
	}
}

// Kills over-refusal: maintainer-city's shape, a SQLite store with the
// revision layout, bare or under the controller's CachingStore, and the
// native-Dolt wrapper chain (the bead policy store over the CachingStore
// over an atomic-close store, stamped auto) resolve both capabilities, read
// by boot and by doctor alike.
func TestBootAdmitsSQLiteRevisionLayout(t *testing.T) {
	sqlite := stampedSQLite(t, gate.Auto)
	atomic := beads.NewAtomicCloseMemStore()
	if err := beads.StampOpenedStore(atomic, "MemStore", gate.Auto, nil, nil); err != nil {
		t.Fatal(err)
	}
	chain := &beadPolicyStore{Store: beads.NewCachingStoreForTest(atomic, nil), cfg: workerCity(1)}
	for name, store := range map[string]beads.Store{
		"sqlite": sqlite, "cached sqlite": beads.NewCachingStoreForTest(sqlite, nil), "policy over cache over atomic close": chain,
	} {
		for _, inspect := range []bool{false, true} {
			if err := v2CapabilityRefusal(v2ClassStoreCapabilities(store, store, inspect)); err != nil {
				t.Errorf("%s (inspect=%v): %v", name, inspect, err)
			}
		}
	}
}

// Kills a doctor C0.7 read that resolves the writer as boot does, which
// fires the auto-mode degraded event from a read-only check: over an
// incapable auto-stamped store, doctor's read fires none and still reports no
// writer, with the store's reason; boot's read fires it.
func TestDoctorC07ReadFiresNoDegradeEvent(t *testing.T) {
	mem := beads.NewMemStore()
	mem.DisableConditionalWrites = true
	degraded := 0
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Auto, func(beads.ConditionalWritesDegrade) { degraded++ }, nil); err != nil {
		t.Fatal(err)
	}
	caps := v2ClassStoreCapabilities(mem, mem, true)
	if degraded != 0 || caps[0].Writer || !strings.Contains(caps[0].String(), "conditional writes disabled on this store instance") {
		t.Fatalf("doctor read: %d degrade events, capabilities %s; want none, no writer, and the store's reason", degraded, caps[0])
	}
	if v2ClassStoreCapabilities(mem, mem, false); degraded == 0 {
		t.Fatal("boot's read fired no degrade event: the test no longer tells the two reads apart")
	}
}

// Kills a doctor that hides C0.7, or that disagrees with boot: the
// daemon-session-reconciler check lists both capabilities of each class
// store, and errors only when the latched mode is v2 and boot would refuse.
func TestSessionReconcilerDoctorReportsC07(t *testing.T) {
	mem := fencedMemStore(t)
	caps := func() ([]v2StoreCapability, error) { return v2ClassStoreCapabilities(mem, mem, true), nil }
	for _, tc := range []struct {
		name, mode string
		want       doctor.CheckStatus
	}{
		{"legacy", "", doctor.StatusOK},
		{"v2", "v2", doctor.StatusError},
	} {
		cfg := workerCity(1)
		cfg.Daemon.SessionReconciler = tc.mode
		c := newSessionReconcilerDoctorCheck(cfg, overrideEnv("1"))
		c.capabilities = caps
		r := c.Run(nil)
		details := strings.Join(r.Details, "\n")
		if r.Status != tc.want || !strings.Contains(details, "C0.7 sessions store (MemStore, conditional_writes=auto): conditional writer true, atomic conditional closer false") || !strings.Contains(details, "C0.7 graph store") {
			t.Errorf("%s: status %v, details %q; want %v with both stores' capabilities", tc.name, r.Status, details, tc.want)
		}
	}
}

// Kills the doctor's C0.7 read left unwired from buildDoctorChecks: on a
// real city with a file store, the registered daemon-session-reconciler
// check reports both class stores' capabilities, read through doctor's own
// store factory.
func TestBuildDoctorChecksWiresC07Capabilities(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore: %v", err)
	}
	var check doctor.Check
	for _, c := range buildDoctorChecks(cityDir, &config.City{}, nil, buildDoctorChecksOpts{Stderr: io.Discard}) {
		if c.Name() == "daemon-session-reconciler" {
			check = c
		}
	}
	if check == nil {
		t.Fatal("daemon-session-reconciler not registered")
	}
	details := strings.Join(check.Run(&doctor.CheckContext{CityPath: cityDir}).Details, "\n")
	if !strings.Contains(details, "C0.7 sessions store (") || !strings.Contains(details, "C0.7 graph store (") {
		t.Fatalf("details = %q, want both class stores' C0.7 capabilities", details)
	}
}
