package main

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The release_orphaned_pool_assignments lane (ga-8uf72n, ruled in ga-57er0d).
//
// The sweep that reopens pool work whose assignee has no open session used to
// run on every reconciler tick. It is a CONVERGENCE repair of a rare failure, so
// it now follows the delta/backstop split the detached-orphan and route-recovery
// lanes already use (ga-l7jdg): the tick pays only for assignees whose session
// just went away, and a low-minutes backstop pass converges everything else off
// the tick, from the inputs the last tick published.
//
// The fixture is the production-call-site control's three beads plus a control
// that proves the sweep can still release anything at all:
//
//   - W1: assignee live ONLY in the relocated sessions-class store.
//   - W2: bare-template assignee, wake-reachable this tick (protected).
//   - W3: assignee "worker-dead" has no session bead anywhere — a true orphan.
type orphanReleaseFixture struct {
	cr           *CityRuntime
	work         *orphanReleaseCountingStore
	sessionStore beads.Store
	cfg          *config.City
	snapshot     *sessionBeadSnapshot
	w1, w2, w3   beads.Bead
}

// orphanReleaseCountingStore counts the reads and writes a lane issues against
// the work store, so a test can pin that a steady tick touches it zero times.
type orphanReleaseCountingStore struct {
	beads.Store
	reads  atomic.Int32
	writes atomic.Int32
}

func (s *orphanReleaseCountingStore) Get(id string) (beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.Get(id)
}

func (s *orphanReleaseCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.List(q)
}

func (s *orphanReleaseCountingStore) Update(id string, opts beads.UpdateOpts) error {
	s.writes.Add(1)
	return s.Store.Update(id, opts)
}

func (s *orphanReleaseCountingStore) SetMetadata(id, k, v string) error {
	s.writes.Add(1)
	return s.Store.SetMetadata(id, k, v)
}

func (s *orphanReleaseCountingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.writes.Add(1)
	return s.Store.SetMetadataBatch(id, kvs)
}

func newOrphanReleaseFixture(t *testing.T) *orphanReleaseFixture {
	t.Helper()
	mem := beads.NewMemStore()
	work := &orphanReleaseCountingStore{Store: mem}
	sessionStore := beads.NewMemStore()
	inProgress := "in_progress"

	mkWork := func(title, assignee string) beads.Bead {
		b, err := mem.Create(beads.Bead{
			Title:    title,
			Assignee: assignee,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		if err := mem.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("%s in_progress: %v", title, err)
		}
		got, err := mem.Get(b.ID)
		if err != nil {
			t.Fatalf("get %s: %v", title, err)
		}
		return got
	}
	fx := &orphanReleaseFixture{
		work:         work,
		sessionStore: sessionStore,
		w1:           mkWork("w1 assignee live in the relocated sessions store", "worker-w1-live"),
		w2:           mkWork("w2 wake-protected bare-template assignee", "worker"),
		w3:           mkWork("w3 true orphan, assignee has no session bead", "worker-dead"),
	}
	if _, err := sessionStore.Create(beads.Bead{
		Title:  "live session for w1, resident in the sessions class only",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name": "worker-w1-live",
			"template":     "worker",
			"agent_name":   "worker",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("create w1 session bead: %v", err)
	}
	fx.snapshot = newSessionBeadSnapshot([]beads.Bead{{
		ID:     "sc-snap-worker",
		Title:  "open worker session in the pre-tick snapshot",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"template":     "worker",
			"agent_name":   "worker",
			"state":        "active",
		},
	}})
	fx.cfg = &config.City{Agents: []config.Agent{{
		Name:              "worker",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(2),
	}}}
	fx.cr = &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "orphan-release-city",
		cfg:                 fx.cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: work,
		storageRoutes: &storageRoutes{stores: map[coordclass.Class]beads.Store{
			coordclass.ClassSessions: sessionStore,
		}},
		sessionDrains: newDrainTracker(),
		rec:           events.Discard,
		stdout:        io.Discard,
		stderr:        io.Discard,
	}
	return fx
}

func (fx *orphanReleaseFixture) result() DesiredStateResult {
	return DesiredStateResult{
		State:                 map[string]TemplateParams{},
		AssignedWorkBeads:     []beads.Bead{fx.w1, fx.w2, fx.w3},
		AssignedWorkStores:    []beads.Store{fx.work, fx.work, fx.work},
		AssignedWorkStoreRefs: []string{"", "", ""},
	}
}

// protected is the wake-protected set the tick computes for W2.
func (fx *orphanReleaseFixture) protected() map[storeScopedBeadKey]struct{} {
	return map[storeScopedBeadKey]struct{}{{StoreRef: "", ID: fx.w2.ID}: {}}
}

func (fx *orphanReleaseFixture) inputs(protected map[storeScopedBeadKey]struct{}) orphanReleaseInputs {
	return orphanReleaseInputs{
		store:        beads.WorkStore{Store: fx.work},
		sessionStore: beads.SessionStore{Store: fx.sessionStore},
		cfg:          fx.cfg,
		cityPath:     fx.cr.cityPath,
		openInfos:    fx.snapshot.OpenInfos(),
		result:       fx.result(),
		protected:    protected,
	}
}

// status reports whether a work bead still carries its original claim.
func (fx *orphanReleaseFixture) held(t *testing.T, b beads.Bead) bool {
	t.Helper()
	got, err := fx.work.Store.Get(b.ID)
	if err != nil {
		t.Fatalf("get %s: %v", b.ID, err)
	}
	return got.Assignee == b.Assignee && got.Status == "in_progress"
}

func releasedIDs(released []releasedPoolAssignment, all []beads.Bead) []string {
	ids := make([]string, 0, len(released))
	for _, r := range released {
		ids = append(ids, all[r.Index].ID)
	}
	sort.Strings(ids)
	return ids
}

// TestOrphanReleaseLane_CadenceIsLowMinutes pins the ruling that the backstop is
// load-bearing and so must run in low minutes: the delta leg under-fires until
// ga-2fpf9z ships, and an hourly cadence would leave a true orphan stranded for
// up to an hour.
func TestOrphanReleaseLane_CadenceIsLowMinutes(t *testing.T) {
	lane := newOrphanReleaseLane()
	if lane.interval <= 0 || lane.interval > 10*time.Minute {
		t.Fatalf("backstop interval = %s, want low minutes (0 < d <= 10m)", lane.interval)
	}
	if lane.interval >= detachedOrphanBackstopInterval {
		t.Fatalf("backstop interval = %s, want well below the hourly precedent %s", lane.interval, detachedOrphanBackstopInterval)
	}
	if lane.retry <= 0 || lane.retry >= lane.interval {
		t.Fatalf("retry = %s, want 0 < retry < interval (%s): a dark leg must not wait out the full cadence", lane.retry, lane.interval)
	}
	if orphanReleaseBackstopInterval != lane.interval || orphanReleaseBackstopRetryInterval != lane.retry {
		t.Fatal("the lane does not default to the named cadence constants")
	}
	fx := newOrphanReleaseFixture(t)
	first, second := fx.cr.orphanReleaseLaneOf(), fx.cr.orphanReleaseLaneOf()
	if first != second {
		t.Fatal("orphanReleaseLaneOf must return one lane per runtime")
	}
}

// TestOrphanReleaseLane_BackstopScheduling mirrors the sibling lanes' cadence
// contract: due at startup, quiet until the interval elapses, and rescheduled on
// the short retry cadence after a pass that could not read everything.
func TestOrphanReleaseLane_BackstopScheduling(t *testing.T) {
	lane := newOrphanReleaseLane()
	now := time.Now()
	if reason, due := lane.backstopDue(now); !due || reason != backstopReasonStartup {
		t.Fatalf("fresh lane backstopDue = (%q, %t), want (startup, true)", reason, due)
	}
	if _, _, ran := lane.lastBackstop(); ran {
		t.Fatal("a lane that never ran reports that it did")
	}
	lane.noteBackstopRan(now, backstopReasonStartup, false)
	if _, due := lane.backstopDue(now.Add(lane.interval / 2)); due {
		t.Fatal("backstop due before its interval elapsed")
	}
	if reason, due := lane.backstopDue(now.Add(lane.interval)); !due || reason != backstopReasonCadence {
		t.Fatalf("backstopDue at interval = (%q, %t), want (cadence, true)", reason, due)
	}
	lane.noteBackstopRan(now, backstopReasonCadence, true) // partial pass
	if _, due := lane.backstopDue(now.Add(lane.retry)); !due {
		t.Fatal("a partial pass must be retried on the short retry cadence")
	}
	lane.force(backstopReasonLegDegrade)
	if reason, due := lane.backstopDue(now); !due || reason != backstopReasonLegDegrade {
		t.Fatalf("forced backstopDue = (%q, %t), want (leg-degrade, true)", reason, due)
	}
	at, reason, ran := lane.lastBackstop()
	if !ran || reason != backstopReasonCadence || !at.Equal(now) {
		t.Fatalf("lastBackstop = (%s, %q, %t), want the cadence pass at %s", at, reason, ran, now)
	}
}

// TestOrphanReleaseLane_ObserveSessionsQueuesVanishedAssignees is the delta
// trigger: a session that was open and is no longer — closed or confirmed dead —
// makes its assignee identities candidates. The first observation has no prior
// set to diff against and queues nothing; a session that stays open queues
// nothing.
func TestOrphanReleaseLane_ObserveSessionsQueuesVanishedAssignees(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	mkInfos := func(names ...string) []beads.Bead {
		out := make([]beads.Bead, 0, len(names))
		for _, n := range names {
			out = append(out, beads.Bead{
				ID: "sc-" + n, Type: sessionBeadType, Status: "open",
				Labels: []string{sessionBeadLabel, "agent:worker"},
				Metadata: map[string]string{
					"session_name": n, "template": "worker", "agent_name": "worker", "state": "active",
				},
			})
		}
		return out
	}
	lane := newOrphanReleaseLane()
	lane.observeSessions(newSessionBeadSnapshot(mkInfos("worker-a", "worker-b")).OpenInfos(), fx.cfg)
	if got := lane.takePending(); len(got) != 0 {
		t.Fatalf("first observation queued %v, want nothing (no prior set)", got)
	}
	lane.observeSessions(newSessionBeadSnapshot(mkInfos("worker-a", "worker-b")).OpenInfos(), fx.cfg)
	if got := lane.takePending(); len(got) != 0 {
		t.Fatalf("an unchanged session set queued %v, want nothing — a steady tick must name no candidate", got)
	}
	lane.observeSessions(newSessionBeadSnapshot(mkInfos("worker-b")).OpenInfos(), fx.cfg)
	got := lane.takePending()
	if !containsString(got, "worker-a") {
		t.Fatalf("pending = %v after worker-a vanished, want it to include worker-a", got)
	}
	if containsString(got, "worker-b") {
		t.Fatalf("pending = %v includes worker-b, which is still open", got)
	}
	if again := lane.takePending(); len(again) != 0 {
		t.Fatalf("takePending did not drain: %v", again)
	}
}

// TestOrphanReleaseLane_SteadyDeltaIssuesZeroStoreReads is the slice's headline
// property: with no session transition there is no candidate, no plan and no
// store read — and the same inputs, handed to the backstop one line later, DO
// release the true orphan. Without that control, "zero reads" would be satisfied
// by a lane that had simply stopped working.
func TestOrphanReleaseLane_SteadyDeltaIssuesZeroStoreReads(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	lane := fx.cr.orphanReleaseLaneOf()
	in := fx.inputs(fx.protected())

	for range 4 {
		report := lane.deltaPass(in, nil)
		if report.candidates != 0 || len(report.released) != 0 {
			t.Fatalf("steady delta pass = %+v, want no candidates and no release", report)
		}
	}
	if r, w := fx.work.reads.Load(), fx.work.writes.Load(); r != 0 || w != 0 {
		t.Fatalf("steady delta passes issued %d read(s) and %d write(s) on the work store, want 0 and 0", r, w)
	}
	if !fx.held(t, fx.w3) {
		t.Fatal("W3 was released by a steady delta pass; the sweep is still running on the tick")
	}

	lane.publish(in)
	report := lane.backstopPass(backstopReasonCadence, nil)
	if got := releasedIDs(report.released, in.result.AssignedWorkBeads); len(got) != 1 || got[0] != fx.w3.ID {
		t.Fatalf("backstop released %v, want exactly the true orphan %s — the zero-read assertion above measured a lane that cannot release", got, fx.w3.ID)
	}
	if fx.work.reads.Load() == 0 {
		t.Fatal("backstop issued no live re-verification read; it is releasing from the published snapshot alone")
	}
}

// TestOrphanReleaseLane_DeltaNarrowsToQueuedAssignees: only assigned work whose
// assignee just lost its session is evaluated. W1 and W2 are queued too so the
// sweep must evaluate and RETAIN them (the cures), while W3 is released; a bead
// whose assignee was never queued is not even re-read.
func TestOrphanReleaseLane_DeltaNarrowsToQueuedAssignees(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	lane := fx.cr.orphanReleaseLaneOf()
	in := fx.inputs(fx.protected())

	lane.queueAssignees("worker-w1-live", "worker")
	report := lane.deltaPass(in, nil)
	if len(report.released) != 0 {
		t.Fatalf("delta released %v, want none: W1 is live in the sessions store and W2 is wake-protected, and W3's assignee was not queued",
			releasedIDs(report.released, in.result.AssignedWorkBeads))
	}
	if report.candidates != 2 {
		t.Fatalf("delta evaluated %d candidate(s), want 2 (W1, W2) — it must narrow to the queued assignees", report.candidates)
	}
	if !fx.held(t, fx.w3) {
		t.Fatal("W3's assignee was never queued but the delta released it")
	}

	lane.queueAssignees("worker-dead")
	report = lane.deltaPass(in, nil)
	got := releasedIDs(report.released, in.result.AssignedWorkBeads)
	if len(got) != 1 || got[0] != fx.w3.ID {
		t.Fatalf("delta released %v, want exactly %s", got, fx.w3.ID)
	}
	for _, r := range report.released {
		if in.result.AssignedWorkBeads[r.Index].ID != fx.w3.ID {
			t.Fatalf("released Index %d does not point into the FULL assigned-work slice", r.Index)
		}
	}
	if fx.held(t, fx.w3) {
		t.Fatal("W3 still held after delta released it")
	}
	if !fx.held(t, fx.w1) || !fx.held(t, fx.w2) {
		t.Fatal("delta released W1 or W2: a lost cure")
	}
}

// TestOrphanReleaseLane_BackstopRetainsLiveAndProtectedWork is the third
// reachable-red scenario from the ruling: a backstop pass that runs AFTER the
// computing tick returned must still defer to the wake-protected set that tick
// published. The control publishes an empty set and shows W2 IS released — so
// the retention above is the published set's doing, not an accident of the
// fixture.
func TestOrphanReleaseLane_BackstopRetainsLiveAndProtectedWork(t *testing.T) {
	t.Run("defers to the published protected set", func(t *testing.T) {
		fx := newOrphanReleaseFixture(t)
		lane := fx.cr.orphanReleaseLaneOf()
		lane.publish(fx.inputs(fx.protected()))
		report := lane.backstopPass(backstopReasonCadence, nil)
		got := releasedIDs(report.released, fx.result().AssignedWorkBeads)
		if len(got) != 1 || got[0] != fx.w3.ID {
			t.Fatalf("backstop released %v, want only the true orphan %s", got, fx.w3.ID)
		}
		if !fx.held(t, fx.w1) {
			t.Fatal("backstop released W1: the liveness read missed the relocated sessions store")
		}
		if !fx.held(t, fx.w2) {
			t.Fatal("backstop released W2, which the published wake-protected set covers")
		}
	})
	t.Run("control: a stale empty published set releases W2", func(t *testing.T) {
		fx := newOrphanReleaseFixture(t)
		lane := fx.cr.orphanReleaseLaneOf()
		lane.publish(fx.inputs(nil))
		report := lane.backstopPass(backstopReasonCadence, nil)
		got := releasedIDs(report.released, fx.result().AssignedWorkBeads)
		if !containsString(got, fx.w2.ID) {
			t.Fatalf("backstop released %v with an empty protected set, want W2 (%s) among them — the protected-set assertion is vacuous", got, fx.w2.ID)
		}
		if !fx.held(t, fx.w1) {
			t.Fatal("W1 was released: the liveness read must hit the sessions class regardless of the protected set")
		}
	})
	t.Run("protected set is consulted before every write, not only at publish", func(t *testing.T) {
		fx := newOrphanReleaseFixture(t)
		lane := fx.cr.orphanReleaseLaneOf()
		in := fx.inputs(fx.protected())
		lane.publish(in)
		// The caller's map is mutated after publish: the lane must have its own
		// copy, so neither a later mutation can silently widen NOR narrow what
		// the backstop protects.
		delete(in.protected, storeScopedBeadKey{ID: fx.w2.ID})
		report := lane.backstopPass(backstopReasonCadence, nil)
		got := releasedIDs(report.released, fx.result().AssignedWorkBeads)
		if containsString(got, fx.w2.ID) {
			t.Fatal("backstop released W2 after the caller mutated its protected map; the lane aliases the caller's state")
		}
		// Control: the pass did run and did release the true orphan, so W2's
		// retention is a decision rather than a lane that does nothing.
		if !containsString(got, fx.w3.ID) {
			t.Fatalf("backstop released %v, want the true orphan %s — W2's retention proves nothing from a lane that releases nothing", got, fx.w3.ID)
		}
	})
}

// TestOrphanReleaseLane_BackstopSkipsWithoutCompleteInputs: a backstop with no
// published snapshot, or one built from a partial query, must release nothing —
// missing work or sessions make active work look orphaned.
func TestOrphanReleaseLane_BackstopSkipsWithoutCompleteInputs(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	lane := fx.cr.orphanReleaseLaneOf()

	report := lane.backstopPass(backstopReasonStartup, nil)
	if len(report.released) != 0 || report.skipped == "" {
		t.Fatalf("unpublished backstop = %+v, want no release and a skip reason", report)
	}

	for name, mutate := range map[string]func(*DesiredStateResult){
		"store partial":   func(r *DesiredStateResult) { r.StoreQueryPartial = true },
		"session partial": func(r *DesiredStateResult) { r.SessionQueryPartial = true },
	} {
		in := fx.inputs(fx.protected())
		mutate(&in.result)
		lane.publish(in)
		report := lane.backstopPass(backstopReasonCadence, nil)
		if len(report.released) != 0 || report.skipped == "" {
			t.Fatalf("%s: backstop = %+v, want no release and a skip reason", name, report)
		}
		if !fx.held(t, fx.w3) {
			t.Fatalf("%s: W3 released from a partial snapshot", name)
		}
	}
}

// TestOrphanReleaseLane_PublishedSnapshotIsACopy: the tick keeps filtering and
// reslicing its assigned-work slices after the call site, so the lane must own
// what it published.
func TestOrphanReleaseLane_PublishedSnapshotIsACopy(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	lane := fx.cr.orphanReleaseLaneOf()
	in := fx.inputs(fx.protected())
	lane.publish(in)
	for i := range in.result.AssignedWorkBeads {
		in.result.AssignedWorkBeads[i] = beads.Bead{}
	}
	report := lane.backstopPass(backstopReasonCadence, nil)
	if got := releasedIDs(report.released, fx.result().AssignedWorkBeads); len(got) != 1 || got[0] != fx.w3.ID {
		t.Fatalf("backstop released %v after the caller clobbered its slice, want %s: the lane aliases the caller's slice", got, fx.w3.ID)
	}
}

// TestOrphanReleaseLane_BackstopAlwaysReportsItselfAndItsAge: the backstop runs
// off-tick on a goroutine nothing else watches, so a quiet pass must still say
// that it ran.
func TestOrphanReleaseLane_BackstopAlwaysReportsItselfAndItsAge(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	var stderr bytes.Buffer
	fx.cr.stderr = &stderr
	fx.cr.logPrefix = "gc"
	fx.cr.orphanReleaseLaneOf().publish(fx.inputs(fx.protected()))

	report := fx.cr.runOrphanReleaseBackstop(backstopReasonCadence)
	if len(report.released) != 1 {
		t.Fatalf("backstop released %d, want 1 (W3)", len(report.released))
	}
	line := stderr.String()
	if !strings.Contains(line, "orphan release sweep (backstop): pass reason=cadence") {
		t.Fatalf("backstop logged %q, want a pass line naming why it was due", line)
	}
	for _, want := range []string{"candidates=", "released=1", "took="} {
		if !strings.Contains(line, want) {
			t.Fatalf("the backstop line %q is missing %q", line, want)
		}
	}
	at, reason, ran := fx.cr.orphanReleaseLaneOf().lastBackstop()
	if !ran || reason != backstopReasonCadence || time.Since(at) > time.Minute {
		t.Fatalf("lastBackstop = (%s, %q, %t), want a recent cadence pass", at, reason, ran)
	}

	fields := report.fields()
	if fields["released_count"] != 1 || fields["lane"] != "backstop" {
		t.Fatalf("trace fields = %v, want released_count=1 lane=backstop", fields)
	}
	addBackstopAgeFields(fields, at, reason, ran)
	if fields["backstop_ran"] != true {
		t.Fatalf("trace fields = %v, want backstop_ran=true", fields)
	}
}

// TestOrphanReleaseLane_BackstopHonorsQuiescence: a suspended city with nothing
// running reads no store until resume.
func TestOrphanReleaseLane_BackstopHonorsQuiescence(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	fx.cr.orphanReleaseLaneOf().publish(fx.inputs(fx.protected()))
	fx.cr.beadsQuiescent.Store(true)
	report := fx.cr.runOrphanReleaseBackstop(backstopReasonCadence)
	if len(report.released) != 0 || fx.work.reads.Load() != 0 {
		t.Fatalf("quiescent backstop released %d and read %d time(s), want 0 and 0", len(report.released), fx.work.reads.Load())
	}
	// Control: the same published inputs DO release once the city resumes, so
	// the zero above is the quiescence gate and not a lane that cannot release.
	fx.cr.beadsQuiescent.Store(false)
	if report := fx.cr.runOrphanReleaseBackstop(backstopReasonCadence); len(report.released) != 1 {
		t.Fatalf("resumed backstop released %d, want 1 (W3) — the quiescent assertion above measured a lane that cannot release", len(report.released))
	}
}

// TestOrphanReleaseLane_BackstopLoopReArmsPastItsStartupPass drives the real
// loop and waits on the lane's own always-emit line, as the sibling lanes'
// loop test does: a lane that converges only at boot is the hole reopened.
func TestOrphanReleaseLane_BackstopLoopReArmsPastItsStartupPass(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	lines := newBackstopLineWatcher("orphan release sweep (backstop): pass reason=" + backstopReasonCadence)
	fx.cr.stderr = lines
	fx.cr.logPrefix = "gc"
	lane := fx.cr.orphanReleaseLaneOf()
	lane.interval = time.Millisecond
	lane.poll = time.Millisecond
	lane.publish(fx.inputs(fx.protected()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fx.cr.runOrphanReleaseBackstopLoop(ctx, lane)
	lines.await(t)
}

// TestOrphanReleaseLane_TickPublishesAndDefersTheSweep drives the REAL tick.
// The tick must (a) publish its inputs for the backstop, (b) not run the sweep
// itself when no session transitioned, and (c) when a session did vanish, run
// the delta in-tick and release only the true orphan.
func TestOrphanReleaseLane_TickPublishesAndDefersTheSweep(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	lane := fx.cr.orphanReleaseLaneOf()

	fx.cr.beadReconcileTick(context.Background(), newSeatWork(fx.cr.workLegs()), fx.result(), fx.snapshot, nil, false)
	if !fx.held(t, fx.w3) {
		t.Fatal("a steady tick released W3: the sweep is still on the tick")
	}

	// The tick published what the backstop needs, including the protected set
	// for W2 it computed from the same snapshot.
	report := lane.backstopPass(backstopReasonCadence, nil)
	got := releasedIDs(report.released, fx.result().AssignedWorkBeads)
	if len(got) != 1 || got[0] != fx.w3.ID {
		t.Fatalf("backstop after a tick released %v, want only %s (W1 live, W2 protected by the tick's published set)", got, fx.w3.ID)
	}
	if !fx.held(t, fx.w1) || !fx.held(t, fx.w2) {
		t.Fatal("backstop after a tick released W1 or W2")
	}
}

// TestOrphanReleaseLane_TickDeltaFiresOnSessionTransition: the delta trigger is
// wired into the real tick — a session open on one tick and gone on the next
// queues its assignee, and that tick releases the orphan.
func TestOrphanReleaseLane_TickDeltaFiresOnSessionTransition(t *testing.T) {
	fx := newOrphanReleaseFixture(t)
	gone := beads.Bead{
		ID: "sc-gone", Type: sessionBeadType, Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name": "worker-dead", "template": "worker", "agent_name": "worker", "state": "active",
		},
	}
	open := beads.Bead{
		ID: "sc-snap-worker", Type: sessionBeadType, Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name": "worker-1", "template": "worker", "agent_name": "worker", "state": "active",
		},
	}
	sw := newSeatWork(fx.cr.workLegs())
	fx.cr.beadReconcileTick(context.Background(), sw, fx.result(), newSessionBeadSnapshot([]beads.Bead{open, gone}), nil, false)
	if !fx.held(t, fx.w3) {
		t.Fatal("W3 was released while its session was still open")
	}
	fx.cr.beadReconcileTick(context.Background(), sw, fx.result(), newSessionBeadSnapshot([]beads.Bead{open}), nil, false)
	if fx.held(t, fx.w3) {
		t.Fatal("W3 still held after its assignee's session vanished: the delta trigger is not wired into the tick")
	}
	if !fx.held(t, fx.w1) || !fx.held(t, fx.w2) {
		t.Fatal("the in-tick delta released W1 or W2: a lost cure")
	}
}
