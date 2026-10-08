package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The session differential (EFFECT-STRUCTURE §2 item 3; it generalizes the
// allocation differential, P3-5c, to rows and effects). Each fixture builds
// twin worlds over the simulator's provider from the same rows, work and
// runtimes. Copy A runs legacy ticks until one changes nothing: each is the
// desired-state build, the session-bead sync, then the session reconciler
// (reconcileSessionBeadsAtPath's named-demand form, as the controller's
// ticks call it). Copy B runs v2 on the simulator: passes with every effect
// released synchronously until a pass admits nothing and writes nothing.
// Both then compare row status, type and metadata, work assignees and
// status, the provider's action log, and the recorded events by type,
// subject and payload keys (the plan's event-parity rule, §2.3).
//
// A difference is never whitelisted by shape. Each one a fixture shows must
// be accounted to an entry in one of three tables, and an entry that no
// longer shows fails as loudly as an unaccounted difference:
//   - parityAccepted: a CONTRACT v5 §12.2 item, by number;
//   - parityFindings: a divergence the contract does not explain, pinned
//     while the owner decides or its fix lands (a finding is not an
//     explanation);
//   - parityUnported: legacy behavior of an arm or effect kind not yet
//     registered on main. An entry fails once its arm or kind registers, so
//     the registering PR turns it into parity, a §12.2 entry or a finding.
//
// Fixtures are quiescent shapes. A behavior that is a race (INC-024's stale
// heal, POOL-051's locked re-census) is pinned here in its settled form; the
// simulator's scenarios own the interleaving (R5) until effect seams let a
// fixture inject one (EFFECT-STRUCTURE §2 item 5).
//
// TestEveryKeptBehaviorOfARegisteredArmHasAParityFixture gates the fixtures
// on BEHAVIORS.md: every KEPT row whose owner names only registered arms and
// effect kinds must be named by a fixture.

// parityFixture is one twin-world fixture. Rows and Work seed the city leg
// on both sides; each runtime runs under the session name of the row whose
// ID it carries. City is workerCity(3) when nil. Explain maps each aspect the
// fixture differs on to the entry that accounts for it.
type parityFixture struct {
	Name      string
	Behaviors []string
	Rows      []parityRow
	Work      []parityRow
	Runtimes  []simRuntime
	City      func() *config.City
	// Drains is a drain requested at generation 1, by row ID to reason:
	// legacy's drain tracker holds it, and the row its v2 stop request
	// (CONTRACT v5 D1), which legacy ignores. Legacy's outcome projects its
	// tracker back onto those keys, so the two compare.
	Drains map[string]string
	// Pending names the rows whose runtime has a pending interaction:
	// legacy's provider answers it, and v2's observation cache holds the
	// probe's fact.
	Pending []string
	Explain map[string]string
}

// parityRow is a bead; a type alias keeps fixtures short.
type parityRow = beads.Bead

// parityEntry is one accounted difference: its clause or owning PR, why,
// and for an unported arm the keys it writes.
type parityEntry struct {
	ref, reason string
	keys        []string // the row fields a wildcard "row:<id>:*<label>" Explain accounts to it
}

// parityAccepted are CONTRACT v5 §12.2's items the fixtures show; the rest
// come with the arms that own them (#6 with A3, #15-#17 with A21).
var parityAccepted = map[string]parityEntry{
	"§12.2#3 R16": {
		"CONTRACT v5 §12.2 #3 (ruling 4)", "a resume voids a suspended drain; legacy signals, stops and restarts the row",
		append([]string{"slept_at"}, parityStartKeys...),
	},
}

// parityStartKeys are the row keys a legacy start writes.
var parityStartKeys = []string{
	"state", "state_reason", "generation", "instance_token", "last_woke_at", "awake_started_at", "creation_complete_at", "primed_at", "prompt_hash",
	"core_hash_breakdown", "live_hash", "started_config_hash", "started_launch_hash", "started_live_hash", "started_provision_hash", "pending_create_claim", "pending_create_started_at",
}

// parityFindings are divergences between legacy and merged v2 that CONTRACT
// v5 does not explain. Delete an entry when its fix lands; its fixture then
// fails until the fixture's Explain drops it too.
var parityFindings = map[string]parityEntry{
	"sleep-policy-keys": {
		"BEHAVIORS SESS-601 (KEPT: AL1 allocate)", "legacy persists the seven sleep-policy keys on every visited row; the allocate is pure and no v2 arm or effect writes them, and neither CONTRACT v5 nor the plan names a writer",
		[]string{"requested_sleep_after_idle", "effective_sleep_after_idle", "sleep_policy_source", "sleep_capability", "sleep_policy_adjustment_reason", "sleep_policy_fingerprint", "config_wake_suppressed"},
	},
	"current-bead-stamp": {"C5d (#7315), SESS-613", "legacy stamps currently_processing_bead_id on an alive Wake row; v2's A6 does not yet", []string{"currently_processing_bead_id"}},
	"unknown-state-diagnostic": {
		"CONTRACT v5 A5; ORCH-NOTES C2c1 (sync SESS-045/046)", "legacy stamps the unknown_state_* markers and emits session.unknown_state (escalating at 30 minutes); v2 writes no marker and records reconciler.alert instead. Not a §12.2 item, and against the plan's event-parity rule (§2.3)",
		[]string{"unknown_state_first_seen", "unknown_state_value", "unknown_state_escalated_at"},
	},
	"creating-row-heal": {"C5d (#7315), SESS-062", "legacy heals a creating row with no claim and its runtime gone to asleep; v2's A6 does not yet", []string{"state"}},
	"advisory-state-heal": {
		"C5d (#7315) in part (CONTRACT v5.6 note), SESS-531", "legacy heals an awake row whose runtime is gone to asleep and resets its continuation; v2's A6 does neither",
		[]string{"state", "sleep_reason", "session_key", "started_config_hash", "continuation_reset_pending"},
	},
	"detached-at":           {"BEHAVIORS SESS-533..536 (KEPT: A6 timer heals + row write)", "legacy stamps detached_at on a detached interactive row and clears it otherwise; v2's A6 has no such heal, and neither CONTRACT v5 A6 nor the plan names one", []string{"detached_at"}},
	"wake-failure-clear":    {"BEHAVIORS SESS-539/540 (KEPT: A6 timer heals + row write)", "legacy clears wake_attempts, an unexpired quarantine and churn_count on a row alive past the stability and productivity thresholds; v2 has no such heal, and neither CONTRACT v5 A6 nor the plan names one", []string{"wake_attempts", "quarantined_until", "churn_count"}},
	"stranded-marker-clear": {"C5d (#7315), SESS-603", "legacy clears stranded_event_emitted_at on an alive row; v2's A6 does not yet", []string{"stranded_event_emitted_at"}},
	"empty-type-repair":     {"BEHAVIORS SESS-701 (KEPT: A6 timer heals + row write)", "legacy repairs a session row's empty type to session; v2 has no such repair, and neither CONTRACT v5 A6 nor the plan names one", []string{"type"}},
	"named-trigger-clear":   {"POOL-056 follow-up (ORCH-NOTES C5d ruling)", "legacy clears a preserved named row's stale trigger stamp; v2's A6 does not yet", []string{"gc.trigger_bead_id", "gc.trigger_bead_store_ref", "brain_parent_sid"}},
	"drain-cancel-on-probe-error": {
		"CONTRACT v5 O3, BEHAVIORS DRAIN-042", "legacy skips a drain whose running probe errors; v2's inventory still classifies the row, so A19's wake lens cancels the drain. No completion either way, but §12.2 does not list the cancel",
		[]string{drainIntentReasonKey, drainIntentAtKey, drainIntentIncarnationKey},
	},
	"idle-respawn-void": {
		"C6d, BEHAVIORS DRAIN-044", "legacy cancels an idle-respawn drain no longer eligible; A19 holds it until C6d completes its policy row",
		[]string{drainIntentReasonKey, drainIntentAtKey, drainIntentIncarnationKey},
	},
	"mislabelled-close": {"CONTRACT v5 AL1, A1", "legacy closes a row with no template and no session name as orphaned; v2's A1 leaves it open as None, which §12.2 does not list", []string{"status", "state", "close_reason", "closed_at"}},
}

// parityUnported is legacy behavior that belongs to an arm or effect kind
// not yet registered. The key's arm ("A18") or kind must stay unregistered.
var parityUnported = map[string]parityEntry{
	"A18 start":       {"C5a1 (#7320, #7321), C5b", "legacy starts a wanted row in the same tick; v2 registers no start arm yet", parityStartKeys},
	"A21 close":       {"C5c1 (#7314), C5c1b (#7330)", "legacy closes an unwanted dead row; v2 registers no close arm yet", []string{"status", "state", "close_reason", "closed_at"}},
	"A21 stranded":    {"C5c1 (#7314)", "legacy stamps the stranded marker of a dead row with assigned work and records session.stranded; v2 registers no close arm yet", []string{"stranded_event_emitted_at"}},
	"A7 row-metadata": {"C7d", "legacy's session-bead sync stamps the row metadata of a row created or changed this tick; v2 registers no row-metadata arm yet", []string{"synced_at", "command", "work_dir"}},
}

// parityKindOwners maps each registered effect kind to the BEHAVIORS owner
// phrases it implements; a PR registering a kind adds its phrases.
var parityKindOwners = map[string]*regexp.Regexp{
	intentRowHeal: regexp.MustCompile(`\brow write\b`),
	intentCreate:  regexp.MustCompile(`\bC1 create\b|\bC2 named reopen\b`),
	// A19's two kinds own exactly its rows.
	intentDrainCancel: regexp.MustCompile(`\bA19\b`),
	intentDrainVoid:   regexp.MustCompile(`\bA19\b`),
}

var parityArmRef = regexp.MustCompile(`\bA(\d{1,2})\b`)

// twinProvider is the simulator's provider with the reads legacy makes
// through runtime.Fake's own session table answered from the simulator's
// runtimes instead, so both copies see one runtime model.
type twinProvider struct{ *simProvider }

func (p twinProvider) IsRunning(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rts[name] != nil
}

func (p twinProvider) ProcessAlive(name string, names []string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rts[name]
	return rt != nil && (len(names) == 0 || !rt.corpse && !rt.zombie)
}

func (p twinProvider) GetMeta(name, key string) (string, error) {
	p.mu.Lock()
	rt := p.rts[name]
	p.mu.Unlock()
	if rt != nil {
		switch key {
		case "GC_SESSION_ID":
			return rt.id, nil
		case "GC_RUNTIME_EPOCH":
			return rt.epoch, nil
		case "GC_INSTANCE_TOKEN":
			return rt.token, nil
		}
	}
	return p.Fake.GetMeta(name, key)
}

func (p twinProvider) IsAttachedWithError(name string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt := p.rts[name]; rt != nil && rt.probeErr {
		return false, fmt.Errorf("sim: attach probe of %s: %w", name, runtime.ErrRuntimeUnavailable)
	}
	return p.rts[name] != nil && p.rts[name].attached, nil
}

// parityMutators are runtime.Fake's recorded calls that change a runtime;
// Start and the destructive calls are the simulator's own records.
var parityMutators = map[string]bool{
	"Interrupt": true, "Nudge": true, "NudgeNow": true, "SetMeta": true, "RemoveMeta": true, "SendKeys": true,
	"Relaunch": true, "TerminateRuntime": true, "ClearScrollback": true, "RunLive": true, "Respond": true, "CopyTo": true,
}

// actions is the provider's action log as a multiset of "Method name".
func (p twinProvider) actions() map[string]int {
	out := map[string]int{}
	p.mu.Lock()
	for _, s := range p.starts {
		out["Start "+s.Name]++
	}
	for _, k := range p.kills {
		out[k.Method+" "+k.Name]++
	}
	p.mu.Unlock()
	for _, c := range p.SnapshotCalls() {
		if parityMutators[c.Method] {
			out[c.Method+" "+c.Name]++
		}
	}
	return out
}

// place puts f's runtimes on p, each under its row's session name.
func (f parityFixture) place(p *simProvider) {
	for _, rt := range f.Runtimes {
		for _, b := range f.Rows {
			if b.ID == rt.id {
				p.put(b.Metadata["session_name"], rt.id, rt.epoch, rt.token)
				placed := *p.rts[b.Metadata["session_name"]]
				placed.corpse, placed.zombie, placed.attached, placed.probeErr = rt.corpse, rt.zombie, rt.attached, rt.probeErr
				*p.rts[b.Metadata["session_name"]] = placed
				if slices.Contains(f.Pending, rt.id) {
					p.SetPendingInteraction(b.Metadata["session_name"], &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval"})
				}
			}
		}
	}
}

func (f parityFixture) city() *config.City {
	if f.City == nil {
		return workerCity(3)
	}
	return f.City()
}

func (f parityFixture) beads() []beads.Bead {
	return cloneDiffBeads(append(slices.Clone(f.Rows), f.Work...))
}

// parityOutcome is one copy's end state, normalized for comparison.
type parityOutcome struct {
	rows    map[string]beads.Bead // session rows by ID
	work    map[string]beads.Bead // other beads by ID
	actions map[string]int
	events  map[string]int // "type subject [payload keys]"
}

// parityRandom are metadata keys whose values each side mints at random; only
// whether they are set is compared.
var parityRandom = map[string]bool{"instance_token": true}

func newParityOutcome(all []beads.Bead, sp twinProvider, rec *memRecorder) parityOutcome {
	out := parityOutcome{rows: map[string]beads.Bead{}, work: map[string]beads.Bead{}, actions: sp.actions(), events: map[string]int{}}
	for _, b := range all {
		b.Metadata = maps.Clone(b.Metadata)
		for k, v := range b.Metadata {
			if parityRandom[k] && v != "" {
				b.Metadata[k] = "<set>"
			}
		}
		if slices.Contains(b.Labels, session.LabelSession) || b.Type == session.BeadType {
			out.rows[b.ID] = b
		} else {
			out.work[b.ID] = b
		}
	}
	rec.mu.Lock()
	for _, e := range rec.events {
		var payload map[string]any
		_ = json.Unmarshal(e.Payload, &payload)
		out.events[fmt.Sprintf("%s %s %v", e.Type, e.Subject, slices.Sorted(maps.Keys(payload)))]++
	}
	rec.mu.Unlock()
	return out
}

// mismatches lists the aspects on which legacy and v2 differ, each as
// "<aspect>" with both sides' values.
func (legacy parityOutcome) mismatches(v2 parityOutcome) map[string]string {
	out := map[string]string{}
	differ := func(aspect, l, v string) {
		if l != v {
			out[aspect] = fmt.Sprintf("legacy %q, v2 %q", l, v)
		}
	}
	for _, id := range unionKeys(legacy.rows, v2.rows) {
		l, lok := legacy.rows[id]
		v, vok := v2.rows[id]
		if lok != vok {
			differ("row:"+id, fmt.Sprint(lok), fmt.Sprint(vok))
			continue
		}
		differ("row:"+id+":status", l.Status, v.Status)
		differ("row:"+id+":type", l.Type, v.Type)
		for _, k := range unionKeys(l.Metadata, v.Metadata) {
			differ("row:"+id+":"+k, l.Metadata[k], v.Metadata[k])
		}
	}
	for _, id := range unionKeys(legacy.work, v2.work) {
		l, v := legacy.work[id], v2.work[id]
		differ("work:"+id+":assignee", l.Assignee, v.Assignee)
		differ("work:"+id+":status", l.Status, v.Status)
	}
	for _, a := range unionKeys(legacy.actions, v2.actions) {
		differ("provider:"+a, fmt.Sprint(legacy.actions[a]), fmt.Sprint(v2.actions[a]))
	}
	for _, e := range unionKeys(legacy.events, v2.events) {
		differ("event:"+e, fmt.Sprint(legacy.events[e]), fmt.Sprint(v2.events[e]))
	}
	return out
}

// parityTicks bounds both copies' runs to a fixed point.
const parityTicks = 6

// parityNow is both copies' clock: the simulator's after its first second.
var parityNow = plannerT0.Add(time.Second)

// legacyWorld is copy A: a store, the provider, and the state legacy's
// ticks carry between them.
type legacyWorld struct {
	cfg      *config.City
	cityPath string
	store    *beads.MemStore
	sp       twinProvider
	clk      *clock.Fake
	rec      *memRecorder
	dt       *drainTracker
}

func newLegacyWorld(f parityFixture, cityPath string, rows []beads.Bead) *legacyWorld {
	sessionCircuitBreakerMu.Lock()
	sessionCircuitBreakerSingleton = newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	sessionCircuitBreakerMu.Unlock()
	sim := &simProvider{Fake: runtime.NewFake(), rts: make(map[string]*simRuntime), changed: make(map[string]uint64), now: func() time.Time { return parityNow }}
	f.place(sim)
	w := &legacyWorld{
		cfg: f.city(), cityPath: cityPath, store: beads.NewMemStoreFrom(0, rows, nil), sp: twinProvider{sim},
		clk: &clock.Fake{Time: parityNow}, rec: &memRecorder{}, dt: newDrainTracker(),
	}
	for id, reason := range f.Drains {
		w.dt.set(id, &drainState{startedAt: parityDrainAt, reason: reason, generation: 1})
	}
	return w
}

// parityDrainAt is when every fixture's drain began.
var parityDrainAt = parityNow.Add(-time.Minute)

// projectDrains rewrites each row's stop-request keys from legacy's drain
// tracker: the drain it still holds, or none.
func (w *legacyWorld) projectDrains(all []beads.Bead) {
	for i, b := range all {
		if !slices.Contains(b.Labels, session.LabelSession) {
			continue
		}
		meta := maps.Clone(b.Metadata)
		meta[drainIntentReasonKey], meta[drainIntentAtKey], meta[drainIntentIncarnationKey] = "", "", ""
		if ds := w.dt.get(b.ID); ds != nil {
			meta[drainIntentReasonKey], meta[drainIntentAtKey] = ds.reason, ds.startedAt.UTC().Format(time.RFC3339)
			meta[drainIntentIncarnationKey] = strconv.Itoa(ds.generation)
		}
		all[i].Metadata = meta
	}
}

func (w *legacyWorld) list(t *testing.T) []beads.Bead {
	all, err := w.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// tick is one legacy tick, as the controller's: the desired-state build
// (which also creates pool rows), the session-bead sync, and, when
// reconcile is set, the session reconciler over the synced rows.
func (w *legacyWorld) tick(t *testing.T, reconcile bool) {
	cfg, now := w.cfg, w.clk.Now()
	snap, err := loadSessionBeadSnapshot(w.store)
	if err != nil {
		t.Fatal(err)
	}
	result := buildDesiredStateWithSessionBeadsAt(cfg.Workspace.Name, w.cityPath, now, now, cfg, w.sp, w.store, nil, snap, nil, io.Discard)
	cfgNames := configuredSessionNamesWithSnapshot(cfg, cfg.Workspace.Name, snap)
	_, updated := syncSessionBeadsWithSnapshotAndRigStores(w.cityPath, beads.SessionStore{Store: w.store}, nil, result.State, w.sp, cfgNames, cfg, w.clk, io.Discard, true, snap, nil)
	if !reconcile {
		return
	}
	open := updated.OpenInfos()
	work := filterAssignedWorkBeadsForPoolDemand(cfg, w.cityPath, w.store, open, result.AssignedWorkBeads, result.AssignedWorkStoreRefs)
	poolDesired := retainScaleCheckPartialPoolDesired(cfg, PoolDesiredCounts(ComputePoolDesiredStatesAt(cfg, work, open, result.ScaleCheckCounts, now)),
		updated, effectivePoolPartialRetentionTemplates(result))
	if poolDesired == nil {
		poolDesired = map[string]int{}
	}
	mergeNamedSessionDemand(poolDesired, result.NamedSessionDemand, cfg)
	reconcileSessionBeadsAtPathWithNamedDemand(context.Background(), w.cityPath, updated.OpenForReconcile(), updated, result.State, cfgNames, cfg, w.sp, w.store,
		nil, result.AssignedWorkBeads, nil, nil, w.dt, nil, poolDesired, result.NamedSessionDemand, result.NamedSessionRoutedDemand,
		result.snapshotQueryPartial(), nil, cfg.Workspace.Name, nil, w.clk, w.rec, cfg.Session.StartupTimeoutDuration(), cfg.Daemon.DriftDrainTimeoutDuration(),
		io.Discard, io.Discard,
		withStartStabilityWaiter(immediateStartStabilityWaiter), withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter))
}

// sameParityBead is sameBead plus the type and assignee.
func sameParityBead(a, b beads.Bead) bool {
	return a.ID == b.ID && a.Type == b.Type && a.Assignee == b.Assignee && sameBead(a, b)
}

// converged is f's beads with each row's metadata as legacy's session-bead
// sync leaves it, the fixture's own keys kept: a city at cutover has
// converged rows, so a fixture shows the arms under test rather than the
// metadata convergence that arm A7 (C7d) owns. The rows the build creates are
// dropped; the copies' runs create their own.
func (f parityFixture) converged(t *testing.T, cityPath string) []beads.Bead {
	w := newLegacyWorld(f, cityPath, f.beads())
	w.tick(t, false)
	synced := map[string]beads.Bead{}
	for _, b := range w.list(t) {
		synced[b.ID] = b
	}
	out := f.beads()
	for i, b := range out {
		if !slices.Contains(b.Labels, session.LabelSession) {
			continue
		}
		meta := maps.Clone(synced[b.ID].Metadata)
		maps.Copy(meta, b.Metadata)
		if reason, ok := f.Drains[b.ID]; ok {
			meta[drainIntentReasonKey], meta[drainIntentAtKey], meta[drainIntentIncarnationKey] = reason, parityDrainAt.Format(time.RFC3339), "1"
		}
		out[i].Metadata = meta
	}
	return out
}

// legacyParity runs copy A from rows until a tick changes no bead and calls
// no provider verb.
func legacyParity(t *testing.T, f parityFixture, cityPath string, rows []beads.Bead) parityOutcome {
	t.Helper()
	w := newLegacyWorld(f, cityPath, cloneDiffBeads(rows))
	for tick := 0; ; tick++ {
		before, acted := w.list(t), w.sp.actions()
		w.tick(t, true)
		if after := w.list(t); slices.EqualFunc(before, after, sameParityBead) && maps.Equal(acted, w.sp.actions()) {
			w.projectDrains(after)
			return newParityOutcome(after, w.sp, w.rec)
		}
		if tick == parityTicks {
			t.Fatalf("%s: legacy reached no fixed point in %d ticks", f.Name, parityTicks)
		}
	}
}

// v2Parity runs copy B from rows on the simulator, every effect released at
// once, until a pass admits nothing and writes nothing. The simulator's
// invariants hold throughout.
func v2Parity(t *testing.T, f parityFixture, cityPath string, rows []beads.Bead) parityOutcome {
	t.Helper()
	s := newSim(t, 1, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
		f.place(s.sp)
		return cloneDiffBeads(rows), nil
	}})
	s.lag, s.env.CityPath = false, cityPath
	sp := twinProvider{s.sp}
	cfg := f.city()
	cfg.Daemon.PatrolInterval, cfg.Daemon.ProbeConcurrency, cfg.Rigs = s.cfg.Daemon.PatrolInterval, s.cfg.Daemon.ProbeConcurrency, s.cfg.Rigs
	*s.cfg = *cfg
	s.env.Env().SP = sp
	rec := &memRecorder{}
	s.p.rec = rec
	creates, err := newCreateEffects(createEffectHost{cityPath: cityPath, cityName: s.env.CityName, lookPath: s.env.LookPath, now: s.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	s.p.creates = creates
	s.advance(parityNow.Sub(s.clk.Now()))
	for pass := 0; ; pass++ {
		before := s.snapshot()
		s.inventory()
		for _, id := range f.Pending {
			s.lane.cache.Note("s-"+id, FactPending, ObsYes, s.clk.Now(), SourceProbe, "")
		}
		s.pass()
		admitted := intentKeys(s.p.out.record.Load().Admitted)
		for len(s.parked) > 0 {
			s.release(0)
		}
		s.audit("v2")
		if len(admitted) == 0 && maps.EqualFunc(before, s.snapshot(), sameParityBead) {
			break
		}
		if pass == parityTicks {
			t.Fatalf("%s: v2 reached no fixed point in %d passes: admitted %v", f.Name, parityTicks, admitted)
		}
	}
	s.noViolations(t)
	all, err := s.legs[0].backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	return newParityOutcome(all, sp, rec)
}

// parityEntryOf finds id in the three tables.
func parityEntryOf(id string) (table string, e parityEntry, ok bool) {
	for name, tbl := range map[string]map[string]parityEntry{"accepted": parityAccepted, "finding": parityFindings, "unported": parityUnported} {
		if e, ok := tbl[id]; ok {
			return name, e, true
		}
	}
	return "", parityEntry{}, false
}

// explains is the Explain key that accounts for aspect: the aspect itself,
// or a wildcard "row:<id>:*<label>" whose entry lists the aspect's field.
func (f parityFixture) explains(aspect string) (string, bool) {
	if _, ok := f.Explain[aspect]; ok {
		return aspect, true
	}
	i := strings.LastIndex(aspect, ":")
	for _, key := range slices.Sorted(maps.Keys(f.Explain)) {
		if prefix, _, wild := strings.Cut(key, "*"); wild && strings.HasPrefix(aspect, "row:") && prefix == aspect[:i+1] {
			if _, e, ok := parityEntryOf(f.Explain[key]); ok && slices.Contains(e.keys, aspect[i+1:]) {
				return key, true
			}
		}
	}
	return "", false
}

// checkParity compares f's two copies: f must account for every mismatch
// with an entry, and every aspect it accounts for must show.
func checkParity(t *testing.T, f parityFixture) {
	t.Helper()
	cityPath := t.TempDir()
	rows := f.converged(t, cityPath)
	got := legacyParity(t, f, cityPath, rows).mismatches(v2Parity(t, f, cityPath, rows))
	shown := map[string]bool{}
	for _, aspect := range slices.Sorted(maps.Keys(got)) {
		key, ok := f.explains(aspect)
		if !ok {
			t.Errorf("%s: unexplained difference %s (%s)", f.Name, aspect, got[aspect])
			continue
		}
		shown[key] = true
		switch table, e, ok := parityEntryOf(f.Explain[key]); {
		case !ok:
			t.Errorf("%s: %s accounted to %q, which no table lists", f.Name, aspect, f.Explain[key])
		case table == "finding":
			t.Logf("%s: %s is open finding %s (%s): %s", f.Name, aspect, f.Explain[key], e.ref, e.reason)
		}
	}
	for key, id := range f.Explain {
		if !shown[key] {
			t.Errorf("%s: %s, accounted to %s, no longer shows: legacy and v2 agree", f.Name, key, id)
		}
	}
}

// parityFixtures are the fixtures, one or more per KEPT row of a registered
// arm or effect kind.
func parityFixtures() []parityFixture {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	live := simRuntime{id: "gc-1", epoch: "1", token: "tok-gc-1"}
	awake := func(meta ...string) parityRow {
		return poolRow("gc-1", "worker", 1, "awake", append([]string{"instance_token", "tok-gc-1", "last_woke_at", at(-time.Minute)}, meta...)...)
	}
	assigned := []parityRow{{ID: "gw-1", Title: "gw-1", Type: "task", Status: "in_progress", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}}}
	napping := func() *config.City {
		cfg := workerCity(3)
		cfg.Agents[0].SleepAfterIdle = "5m"
		return cfg
	}
	// chat is a city with an always named session; legacy preserves its
	// row outside the desired state only while the city is suspended.
	chat := func(suspended bool) func() *config.City {
		return func() *config.City {
			cfg := workerCity(3)
			cfg.Workspace.Suspended = suspended
			cfg.Agents = append(cfg.Agents, config.Agent{Name: "chat", StartCommand: "true"})
			cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
			return cfg
		}
	}
	// explain merges the alive wanted row gc-1's standing findings with more.
	explain := func(more map[string]string) map[string]string {
		out := map[string]string{"row:gc-1:*policy": "sleep-policy-keys", "row:gc-1:*bead": "current-bead-stamp"}
		maps.Copy(out, more)
		return out
	}
	// started is legacy's start of row id under name, which v2 leaves to A18.
	started := func(id, name, subject string, more map[string]string) map[string]string {
		out := map[string]string{
			"row:" + id + ":*start": "A18 start", "row:" + id + ":*policy": "sleep-policy-keys",
			"provider:Start " + name: "A18 start", "provider:RemoveMeta " + name: "A18 start", "event:session.woke " + subject + " []": "A18 start",
		}
		maps.Copy(out, more)
		return out
	}
	// created is a row v2's create wrote, which legacy's sync then stamps.
	created := func(id string) map[string]string { return map[string]string{"row:" + id + ":*sync": "A7 row-metadata"} }
	return []parityFixture{
		{
			Name: "an expired hold heals on an awake row with assigned work", Behaviors: []string{"SESS-009", "SESS-613"},
			Rows: []parityRow{awake("held_until", at(-time.Minute), "sleep_reason", "user-hold")}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "an expired hold, then an expired quarantine against the post-hold reason", Behaviors: []string{"SESS-010"},
			Rows: []parityRow{awake("held_until", at(-2*time.Minute), "quarantined_until", at(-time.Minute), "sleep_reason", "user-hold", "wake_attempts", "3")}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "an expired hold heals on a suspended row and keeps the suspend", Behaviors: []string{"INC-024"},
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "suspended", "held_until", at(-time.Minute), "sleep_reason", "user-hold", "suspended_at", at(-time.Hour))},
			Explain: map[string]string{"row:gc-1:*close": "A21 close", "row:gc-1:*sync": "A7 row-metadata"},
		},
		{
			Name: "an unknown state is skipped", Behaviors: []string{"SESS-044", "SESS-045"},
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "archived-v9")},
			Explain: map[string]string{"row:gc-1:*marker": "unknown-state-diagnostic", "event:session.unknown_state gc-1 [escalated first_seen session_id session_name state]": "unknown-state-diagnostic", "event:reconciler.alert gc-1 [alert]": "unknown-state-diagnostic"},
		},
		{
			Name: "an unknown state first seen 31 minutes ago", Behaviors: []string{"SESS-046"},
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "archived-v9", "unknown_state_first_seen", at(-31*time.Minute), "unknown_state_value", "archived-v9")},
			Explain: map[string]string{"row:gc-1:*marker": "unknown-state-diagnostic", "event:session.unknown_state gc-1 [escalated first_seen session_id session_name state]": "unknown-state-diagnostic", "event:reconciler.alert gc-1 [alert]": "unknown-state-diagnostic"},
		},
		{
			Name: "a known state clears the unknown-state markers", Behaviors: []string{"SESS-047"},
			Rows: []parityRow{awake("unknown_state_first_seen", at(-time.Hour), "unknown_state_value", "archived-v9")}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "a creating row with no claim whose runtime is gone", Behaviors: []string{"SESS-062"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "creating", "instance_token", "tok-gc-1", "quarantined_until", at(time.Hour))}, Work: assigned,
			Explain: map[string]string{"row:gc-1:*heal": "creating-row-heal", "row:gc-1:*policy": "sleep-policy-keys"},
		},
		{
			Name: "an awake row whose runtime is gone", Behaviors: []string{"SESS-531"},
			Rows: []parityRow{awake("last_woke_at", at(-2*time.Hour), "quarantined_until", at(time.Hour), "session_key", "conversation-1", "started_config_hash", "v6:abc")}, Work: assigned,
			Explain: map[string]string{
				"row:gc-1:*heal": "advisory-state-heal", "row:gc-1:*policy": "sleep-policy-keys",
				"row:gc-1:*stranded": "A21 stranded", "event:session.stranded gc-1 [session_id session_name template work_bead_ids]": "A21 stranded",
			},
		},
		{
			Name: "detached_at clears when idle sleep is off", Behaviors: []string{"SESS-533"},
			Rows: []parityRow{awake("detached_at", at(-time.Hour))}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(map[string]string{"row:gc-1:*detach": "detached-at"}),
		},
		{
			Name: "detached_at clears while attached", Behaviors: []string{"SESS-535"}, City: napping,
			Rows: []parityRow{awake("detached_at", at(-time.Hour))}, Work: assigned, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", attached: true}},
			Explain: explain(map[string]string{"row:gc-1:*detach": "detached-at"}),
		},
		{
			Name: "detached_at is stamped on detach", Behaviors: []string{"SESS-536"}, City: napping,
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(map[string]string{"row:gc-1:*detach": "detached-at"}),
		},
		{
			Name: "an ambiguous probe defers the lifecycle and writes nothing else", Behaviors: []string{"SESS-534", "GUAR-011"}, City: napping,
			Rows: []parityRow{awake("held_until", at(-time.Minute), "sleep_reason", "user-hold")}, Work: assigned, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", probeErr: true}},
		},
		{
			Name: "a row alive past the thresholds clears wake failures and churn", Behaviors: []string{"SESS-539", "SESS-540"},
			Rows: []parityRow{awake("last_woke_at", at(-2*time.Hour), "wake_attempts", "2", "quarantined_until", at(time.Hour), "churn_count", "2")}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(map[string]string{"row:gc-1:*clear": "wake-failure-clear"}),
		},
		{
			Name: "an alive row clears its stranded marker", Behaviors: []string{"SESS-603"},
			Rows: []parityRow{awake("stranded_event_emitted_at", at(-time.Hour))}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(map[string]string{"row:gc-1:*stranded": "stranded-marker-clear"}),
		},
		{
			Name: "wake reasons cancel an idle drain", Behaviors: []string{"SESS-615", "SESS-632", "DRAIN-047"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Drains: map[string]string{"gc-1": "idle"}, Explain: explain(nil),
		},
		{
			Name: "assigned work cancels an orphaned drain", Behaviors: []string{"DRAIN-046"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Drains: map[string]string{"gc-1": "orphaned"}, Explain: explain(nil),
		},
		{
			Name: "a pending interaction cancels a config-drift drain", Behaviors: []string{"DRAIN-045"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Pending: []string{"gc-1"}, Drains: map[string]string{"gc-1": "config-drift"}, Explain: explain(nil),
		},
		{
			Name: "a probe error keeps a drain", Behaviors: []string{"DRAIN-042"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", probeErr: true}}, Drains: map[string]string{"gc-1": "idle"},
			Explain: map[string]string{"row:gc-1:*drain": "drain-cancel-on-probe-error"},
		},
		{
			Name: "an idle-respawn drain no longer eligible", Behaviors: []string{"DRAIN-044"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Drains: map[string]string{"gc-1": "idle-respawn"},
			Explain: explain(map[string]string{"row:gc-1:*drain": "idle-respawn-void"}),
		},
		{
			Name: "a resume voids a suspended drain (R16)",
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Drains: map[string]string{"gc-1": "suspended"},
			Explain: explain(map[string]string{
				"row:gc-1:*r16": "§12.2#3 R16", "provider:SetMeta s-gc-1": "§12.2#3 R16", "provider:Stop s-gc-1": "§12.2#3 R16",
				"provider:Start s-gc-1": "§12.2#3 R16", "provider:RemoveMeta s-gc-1": "§12.2#3 R16", "event:session.woke worker-1 []": "§12.2#3 R16",
			}),
		},
		{
			Name: "an empty row type is repaired", Behaviors: []string{"SESS-701"},
			Rows: []parityRow{func() parityRow { b := awake(); b.Type = ""; return b }()}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(map[string]string{"row:gc-1:*type": "empty-type-repair"}),
		},
		{
			Name: "a preserved named row's closed trigger is cleared", Behaviors: []string{"POOL-056"}, City: chat(true),
			Rows:    []parityRow{chatRow("gc-c", "1", "session_name", "chat", "gc.trigger_bead_id", "gw-9")},
			Work:    []parityRow{{ID: "gw-9", Title: "gw-9", Type: "task", Status: "closed", Metadata: map[string]string{}}},
			Explain: map[string]string{"row:gc-c:*trigger": "named-trigger-clear", "row:gc-c:*policy": "sleep-policy-keys"},
		},
		{
			Name: "routed demand creates a pool row", Behaviors: []string{"POOL-051", "GUAR-042", "INC-018", "SESS-711"},
			Work: []parityRow{routedDemandBead("gw-1")}, Explain: started("gc-1", "worker-gc-1", "worker-1", created("gc-1")),
		},
		{
			Name: "no second create beside an unconfirmed create", Behaviors: []string{"SESS-710"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "creating", "pending_create_claim", "true", "pending_create_started_at", at(-10*time.Second))},
			Work: []parityRow{routedDemandBead("gw-1")}, Explain: started("gc-1", "s-gc-1", "worker-1", nil),
		},
		{Name: "an always named session is created", Behaviors: []string{"SESS-711"}, City: chat(false), Explain: started("gc-1", "chat", "chat", nil)},
		{
			Name: "a closed named row is reopened", Behaviors: []string{"SESS-709"}, City: chat(false),
			Rows:    []parityRow{func() parityRow { b := chatRow("gc-c", "1", "session_name", "chat"); b.Status = "closed"; return b }()},
			Explain: started("gc-c", "chat", "chat", nil),
		},
		{
			Name: "a mislabelled row is left alone", Rows: []parityRow{sessionRow("gc-1", "state", "asleep")},
			Explain: map[string]string{"row:gc-1:*close": "mislabelled-close", "row:gc-1:*sync": "A7 row-metadata"},
		},
		{Name: "a kill fence holds its row while demand creates another", Rows: []parityRow{func() parityRow {
			b := poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-gc-1")
			maps.Copy(b.Metadata, session.KillPendingPatch(parityNow.Add(-time.Minute)))
			return b
		}()}, Work: []parityRow{routedDemandBead("gw-1")}, Explain: started("gc-2", "worker-gc-2", "worker-2", created("gc-2"))},
	}
}

// Kills drift between v2's merged arms and effects and legacy's tick on the
// same rows: any row write, assignee, provider call or event the two
// disagree on that no entry accounts for.
func TestSessionDifferentialFixtures(t *testing.T) {
	for _, f := range parityFixtures() {
		t.Run(f.Name, func(t *testing.T) { checkParity(t, f) })
	}
}

// Kills an entry that accounts for nothing, and an unported entry whose
// arm or kind has registered: every entry is shown by a fixture, and every
// unported entry's owner is still unregistered.
func TestSessionDifferentialEntriesHaveFixtures(t *testing.T) {
	shown := map[string]bool{}
	for _, f := range parityFixtures() {
		for _, id := range f.Explain {
			shown[id] = true
		}
	}
	for _, tbl := range []map[string]parityEntry{parityAccepted, parityFindings, parityUnported} {
		for _, id := range slices.Sorted(maps.Keys(tbl)) {
			if !shown[id] {
				t.Errorf("entry %s has no fixture", id)
			}
		}
	}
	arms, kinds := parityRegistered()
	for _, id := range slices.Sorted(maps.Keys(parityUnported)) {
		owner, _, _ := strings.Cut(id, " ")
		if arms[owner] || kinds[owner] {
			t.Errorf("unported entry %s: %s has registered; make it parity, a §12.2 entry or a finding", id, owner)
		}
	}
}

// parityRegistered is the arms in rowArms and the kinds in effectRegistry.
func parityRegistered() (arms, kinds map[string]bool) {
	arms, kinds = map[string]bool{}, map[string]bool{}
	for _, a := range rowArms {
		arms[a.name] = true
	}
	for k := range effectRegistry {
		kinds[k] = true
	}
	return arms, kinds
}

// keptOwners reads the vendored KEPT owner cells: owner → row IDs.
func keptOwners(t *testing.T) map[string][]string {
	t.Helper()
	file, err := os.Open("testdata/session_differential/kept_owners.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // read-only
	out := map[string][]string{}
	sc := bufio.NewScanner(file)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			owner, ids, ok := strings.Cut(line, "\t")
			if !ok {
				t.Fatalf("kept_owners.tsv: malformed line %q", line)
			}
			out[owner] = strings.Split(ids, ",")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// keptOwnerUnits is the arms and effect kinds an owner cell names.
func keptOwnerUnits(owner string) []string {
	var units []string
	for _, m := range parityArmRef.FindAllStringSubmatch(owner, -1) {
		units = append(units, "A"+m[1])
	}
	for _, kind := range slices.Sorted(maps.Keys(parityKindOwners)) {
		if parityKindOwners[kind].MatchString(owner) {
			units = append(units, kind)
		}
	}
	return units
}

// Kills a registered arm or effect kind landing without parity fixtures for
// the KEPT behaviors it owns: every KEPT row whose owner cell names only
// registered arms and kinds is named by a fixture. A row naming an arm not
// yet registered is exempt until that arm registers.
func TestEveryKeptBehaviorOfARegisteredArmHasAParityFixture(t *testing.T) {
	arms, kinds := parityRegistered()
	for kind := range kinds {
		if parityKindOwners[kind] == nil {
			t.Errorf("effect kind %s is registered with no BEHAVIORS owner phrase in parityKindOwners", kind)
		}
	}
	covered := map[string]bool{}
	for _, f := range parityFixtures() {
		for _, id := range f.Behaviors {
			covered[id] = true
		}
	}
	owners := keptOwners(t)
	var gated, exempt int
	for _, owner := range slices.Sorted(maps.Keys(owners)) {
		units := keptOwnerUnits(owner)
		registered := len(units) > 0
		for _, u := range units {
			registered = registered && (arms[u] || kinds[u])
		}
		if !registered {
			exempt += len(owners[owner])
			continue
		}
		for _, id := range owners[owner] {
			gated++
			if !covered[id] {
				t.Errorf("KEPT %s (owner %q) has no parity fixture", id, owner)
			}
		}
	}
	t.Logf("KEPT rows gated %d, exempt %d", gated, exempt)
}
