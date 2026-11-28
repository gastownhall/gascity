package main

import (
	"context"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

const adoptionTestCityPool = "ccc-researcher"

// adoptionTestConfig is a city whose HQ store (prefix "ct") adopts unrouted
// work into the city-scoped pool adoptionTestCityPool.
func adoptionTestConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "city", Prefix: "ct", UnroutedWorkTarget: adoptionTestCityPool},
		Agents: []config.Agent{
			{Name: adoptionTestCityPool, MinActiveSessions: ptr(0), MaxActiveSessions: ptr(3)},
		},
	}
}

// plainCreatedBead is what a plain `bd create` leaves behind: open,
// unassigned, a task, and no metadata at all.
func plainCreatedBead(id string) beads.Bead {
	return beads.Bead{ID: id, Title: "hand-made work", Type: "task", Status: "open"}
}

// adoptionTestStore is a bead store an adoption test writes through, counting
// the writes it took.
type adoptionTestStore interface {
	beads.Store
	writeCount() int
}

func (s *countingRouteStore) writeCount() int { return s.writes }

// conditionalRouteStore embeds *beads.MemStore, so its conditional writer and
// metadata compare-and-set are promoted as they are on the production stores
// and adoption takes its fenced write. With listBlind its List reads carry no
// revision, as BdStore's do (`bd list --json` publishes none, mc-3c8h5), while
// its point reads keep theirs.
type conditionalRouteStore struct {
	*beads.MemStore
	listBlind bool
	writes    int
}

func (s *conditionalRouteStore) writeCount() int { return s.writes }

func (s *conditionalRouteStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(q)
	if s.listBlind {
		for i := range rows {
			rows[i].Revision = 0
		}
	}
	return rows, err
}

func (s *conditionalRouteStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	s.writes++
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

func (s *conditionalRouteStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	s.writes++
	return s.MemStore.CompareAndSetMetadataKey(id, key, expected, next)
}

func (s *conditionalRouteStore) SetMetadata(id, key, value string) error {
	s.writes++
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *conditionalRouteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.writes++
	return s.MemStore.SetMetadataBatch(id, kvs)
}

// openConditionalRouteStore opens a conditionalRouteStore seeded with seed.
func openConditionalRouteStore(seed []beads.Bead) adoptionTestStore {
	return &conditionalRouteStore{MemStore: beads.NewMemStoreFrom(0, seed, nil)}
}

// openBlindRouteStore opens a countingRouteStore seeded with seed. It hides
// every optional capability, so adoption takes the blind fallback write.
func openBlindRouteStore(seed []beads.Bead) adoptionTestStore {
	return &countingRouteStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
}

// adoptionStoreKinds are the two ways adoption writes: fenced, through the
// conditional writer the production stores offer, and blind, through the
// fallback a store without one leaves.
var adoptionStoreKinds = []struct {
	name string
	open func(seed []beads.Bead) adoptionTestStore
}{
	{"conditional", openConditionalRouteStore},
	{"blind", openBlindRouteStore},
}

// adoptionRuntime builds a runtime over one city store opened by open with
// seed, configured with cfg.
func adoptionRuntime(t *testing.T, cfg *config.City, open func([]beads.Bead) adoptionTestStore, seed ...beads.Bead) (*CityRuntime, adoptionTestStore) {
	t.Helper()
	store := open(seed)
	cr := &CityRuntime{
		cityName:            "city",
		cityPath:            t.TempDir(),
		cfg:                 cfg,
		standaloneCityStore: store,
		stderr:              io.Discard,
	}
	return cr, store
}

// TestRouteAdoptionDeltaPassAdoptsPlainCreatedCityBead is AC-1: a bead made
// with plain `bd create` in a city whose workspace names an
// unrouted_work_target is routed by the delta pass to exactly the value
// built-in `gc sling <target>` writes, and it is reported as adopted, not
// restored, on either write path.
func TestRouteAdoptionDeltaPassAdoptsPlainCreatedCityBead(t *testing.T) {
	for _, kind := range adoptionStoreKinds {
		t.Run(kind.name, func(t *testing.T) {
			cfg := adoptionTestConfig()
			bead := plainCreatedBead("ct-1")
			cr, store := adoptionRuntime(t, cfg, kind.open, bead)
			lane := cr.routeRecoveryLaneOf()

			lane.observe(beadCreatedEvent(t, bead))
			report := cr.recoverUnroutedWorkRoutesDelta()

			if got, want := mustRoutedTo(t, store, "ct-1"), slingRouteFor(t, cfg, "city:city", adoptionTestCityPool, "ct-2"); got != want {
				t.Fatalf("ct-1 gc.routed_to = %q, want %q (what gc sling writes)", got, want)
			}
			if report.adopted != 1 {
				t.Fatalf("delta pass adopted %d, want 1", report.adopted)
			}
			if report.restored != 0 {
				t.Fatalf("delta pass restored %d, want 0 (adoption is not a restore)", report.restored)
			}
			if store.writeCount() != 1 {
				t.Fatalf("delta pass wrote %d time(s), want 1", store.writeCount())
			}
		})
	}
}

// TestRouteAdoptionCarriedRouteWins pins that a carried gc.run_target is
// restored by the existing recovery path and never overwritten by the target.
func TestRouteAdoptionCarriedRouteWins(t *testing.T) {
	cfg := adoptionTestConfig()
	bead := plainCreatedBead("ct-2")
	bead.Metadata = map[string]string{beadmeta.RunTargetMetadataKey: "other-pool"}
	cr, store := adoptionRuntime(t, cfg, openConditionalRouteStore, bead)

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)

	if got := mustRoutedTo(t, store, "ct-2"); got != "other-pool" {
		t.Fatalf("ct-2 gc.routed_to = %q, want the carried route %q", got, "other-pool")
	}
	if report.restored != 1 || report.adopted != 0 {
		t.Fatalf("backstop restored=%d adopted=%d, want restored=1 adopted=0", report.restored, report.adopted)
	}
}

// adoptionRigRuntime builds a runtime over a city store and one rig store
// ("hello-world", prefix "hw"), both opened by open, configured with cfg.
func adoptionRigRuntime(t *testing.T, cfg *config.City, open func([]beads.Bead) adoptionTestStore, citySeed, rigSeed []beads.Bead) (*CityRuntime, adoptionTestStore, adoptionTestStore) {
	t.Helper()
	cityStore, rigStore := open(citySeed), open(rigSeed)
	cr := &CityRuntime{
		cityName:            "city",
		cityPath:            t.TempDir(),
		cfg:                 cfg,
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"hello-world": rigStore},
		stderr:              io.Discard,
	}
	return cr, cityStore, rigStore
}

// adoptionRigConfig is a city with a "hello-world" rig (prefix "hw") and a
// rig-scoped researcher pool. rigTarget is the rig's unrouted_work_target.
func adoptionRigConfig(t *testing.T, rigTarget string) *config.City {
	t.Helper()
	return &config.City{
		Workspace: config.Workspace{Name: "city", Prefix: "ct"},
		Rigs: []config.Rig{
			{Name: "hello-world", Path: t.TempDir(), Prefix: "hw", UnroutedWorkTarget: rigTarget},
		},
		Agents: []config.Agent{
			{Name: "researcher", Dir: "hello-world", MinActiveSessions: ptr(0), MaxActiveSessions: ptr(3)},
		},
	}
}

// TestRouteAdoptionRigTargetAdoptsOnlyItsOwnScope is AC-2's first half: a
// rig's unrouted_work_target adopts the rig's plain-created bead and leaves
// the city's unrouted, since the workspace sets no target.
func TestRouteAdoptionRigTargetAdoptsOnlyItsOwnScope(t *testing.T) {
	for _, kind := range adoptionStoreKinds {
		t.Run(kind.name, func(t *testing.T) {
			cfg := adoptionRigConfig(t, "hello-world/researcher")
			cityBead, rigBead := plainCreatedBead("ct-9"), plainCreatedBead("hw-9")
			cr, cityStore, rigStore := adoptionRigRuntime(t, cfg, kind.open, []beads.Bead{cityBead}, []beads.Bead{rigBead})

			report := cr.runRouteRecoveryBackstop(backstopReasonCadence)

			if got := mustRoutedTo(t, rigStore, "hw-9"); got != "hello-world/researcher" {
				t.Fatalf("hw-9 gc.routed_to = %q, want %q", got, "hello-world/researcher")
			}
			if got := mustRoutedTo(t, cityStore, "ct-9"); got != "" {
				t.Fatalf("ct-9 gc.routed_to = %q, want empty (the workspace sets no target)", got)
			}
			if report.adopted != 1 {
				t.Fatalf("backstop adopted %d, want 1", report.adopted)
			}
		})
	}
}

// TestRouteAdoptionNoTargetAnywhereWritesNothing is AC-2's second half: with
// no unrouted_work_target configured, plain-created beads add no candidates
// and draw no writes from either pass.
func TestRouteAdoptionNoTargetAnywhereWritesNothing(t *testing.T) {
	for _, kind := range adoptionStoreKinds {
		t.Run(kind.name, func(t *testing.T) {
			cfg := adoptionRigConfig(t, "")
			cityBead, rigBead := plainCreatedBead("ct-9"), plainCreatedBead("hw-9")
			cr, cityStore, rigStore := adoptionRigRuntime(t, cfg, kind.open, []beads.Bead{cityBead}, []beads.Bead{rigBead})
			lane := cr.routeRecoveryLaneOf()

			lane.observe(beadCreatedEvent(t, cityBead))
			lane.observe(beadCreatedEvent(t, rigBead))
			delta := cr.recoverUnroutedWorkRoutesDelta()
			if delta.candidates != 0 || delta.legReads != 0 {
				t.Fatalf("delta candidates=%d reads=%d, want 0/0 with no target configured", delta.candidates, delta.legReads)
			}
			backstop := cr.runRouteRecoveryBackstop(backstopReasonCadence)
			if backstop.candidates != 0 || backstop.adopted != 0 {
				t.Fatalf("backstop candidates=%d adopted=%d, want 0/0 with no target configured", backstop.candidates, backstop.adopted)
			}
			if cityStore.writeCount() != 0 || rigStore.writeCount() != 0 {
				t.Fatalf("writes city=%d rig=%d, want 0", cityStore.writeCount(), rigStore.writeCount())
			}
		})
	}
}

// TestRouteAdoptionRefusesIneligibleBeads is AC-3: in a scope with a target,
// neither the delta nor the backstop pass adopts an open, unassigned,
// unrouted bead that the target's Tier 3 query would refuse, that is held,
// ephemeral, workflow or control topology, one of gc's own coordination
// beads, or that gc ever claimed, on either write path.
func TestRouteAdoptionRefusesIneligibleBeads(t *testing.T) {
	withMeta := func(key string) func(*beads.Bead) {
		return func(b *beads.Bead) { b.Metadata = map[string]string{key: "x"} }
	}
	withLabel := func(label string) func(*beads.Bead) {
		return func(b *beads.Bead) { b.Labels = []string{label} }
	}
	cases := []struct {
		name  string
		shape func(*beads.Bead)
	}{
		{"epic", func(b *beads.Bead) { b.Type = "epic" }},
		{"ready-excluded type", func(b *beads.Bead) { b.Type = "molecule" }},
		{"ready-excluded label", withLabel("gc:session")},
		{"dispatch hold", withLabel(beadmeta.DispatchHoldLabels[0])},
		{"human label", withLabel(beadmeta.HumanLabel)},
		{"ephemeral", func(b *beads.Bead) { b.Ephemeral = true }},
		{"gc.kind", withMeta(beadmeta.KindMetadataKey)},
		{"gc.execution_routed_to", withMeta(beadmeta.ExecutionRoutedToMetadataKey)},
		{"gc.deferred_routed_to", withMeta(beadmeta.DeferredRoutedToMetadataKey)},
		{"gc.deferred_execution_routed_to", withMeta(beadmeta.DeferredExecutionRoutedToMetadataKey)},
		{"gc.instantiating", withMeta(beadmeta.InstantiatingMetadataKey)},
		{"gc.claimed_at", withMeta(beadmeta.ClaimedAtMetadataKey)},
		{"route-recovery quarantined", func(b *beads.Bead) {
			b.Metadata = map[string]string{beadmeta.RouteQuarantineMetadataKey: "true"}
		}},
		// gc's own coordination beads, which the serving rules alone accept.
		{"queued-nudge shadow", func(b *beads.Bead) { b.Type, b.Labels = "chore", []string{"gc:nudge"} }},
		{"durable nudge queue", withLabel("gc:nudge-queue")},
		{"extmsg binding", withLabel("gc:extmsg-binding")},
		{"extmsg delivery", withLabel("gc:extmsg-delivery")},
		{"legacy session wait", func(b *beads.Bead) { b.Type, b.Labels = "wait", []string{"gc:wait"} }},
		{"wisp label", withLabel("wisp")},
		{"gc:wisp label", withLabel("gc:wisp")},
		{"graph node", withMeta(beadmeta.RootBeadIDMetadataKey)},
	}
	for _, kind := range adoptionStoreKinds {
		for _, tc := range cases {
			t.Run(kind.name+"/"+tc.name, func(t *testing.T) {
				bead := plainCreatedBead("ct-3")
				tc.shape(&bead)
				if adoptableUnroutedWork(bead) {
					t.Fatalf("adoptableUnroutedWork(%s) = true, want false", tc.name)
				}
				cr, store := adoptionRuntime(t, adoptionTestConfig(), kind.open, bead)
				cr.routeRecoveryLaneOf().observe(beadCreatedEvent(t, bead))

				delta := cr.recoverUnroutedWorkRoutesDelta()
				backstop := cr.runRouteRecoveryBackstop(backstopReasonCadence)

				if got := mustRoutedTo(t, store, "ct-3"); got != "" {
					t.Fatalf("gc.routed_to = %q, want empty", got)
				}
				if delta.adopted != 0 || backstop.adopted != 0 {
					t.Fatalf("adopted delta=%d backstop=%d, want 0/0", delta.adopted, backstop.adopted)
				}
			})
		}
	}
	t.Run("gc.run_target is restored, not adopted", func(t *testing.T) {
		bead := plainCreatedBead("ct-4")
		bead.Metadata = map[string]string{beadmeta.RunTargetMetadataKey: "carried-pool"}
		if adoptableUnroutedWork(bead) {
			t.Fatal("adoptableUnroutedWork(gc.run_target) = true, want false")
		}
		cr, store := adoptionRuntime(t, adoptionTestConfig(), openConditionalRouteStore, bead)
		cr.routeRecoveryLaneOf().observe(beadCreatedEvent(t, bead))
		delta := cr.recoverUnroutedWorkRoutesDelta()
		if got := mustRoutedTo(t, store, "ct-4"); got != "carried-pool" {
			t.Fatalf("gc.routed_to = %q, want the carried %q", got, "carried-pool")
		}
		if delta.adopted != 0 || delta.restored != 1 {
			t.Fatalf("delta adopted=%d restored=%d, want 0/1", delta.adopted, delta.restored)
		}
	})
}

// mintCoordinationBeads mints gc's own coordination beads into store through
// their real constructors, as a city that relocates no class mints them into
// its work store: a queued-nudge shadow, and an extmsg binding with its
// delivery context. store must mint ids under the scope's prefix. It returns
// the open, unassigned, unrouted ones the Tier 3 serving rules alone would
// hand to a worker, and fails unless they include all three families.
func mintCoordinationBeads(t *testing.T, store *beads.MemStore) []beads.Bead {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, time.November, 28, 1, 0, 0, 0, time.UTC)
	if _, _, err := nudgequeue.NewStore(beads.NudgesStore{Store: store}).Save(nudgequeue.Item{
		ID: "nudge-1", Agent: adoptionTestCityPool, Source: "session", Message: "check the queue",
		CreatedAt: now, DeliverAfter: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("saving a queued nudge: %v", err)
	}
	sess, err := store.Create(beads.Bead{
		Title:    "session s-1",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "s-1"},
	})
	if err != nil {
		t.Fatalf("creating a session bead: %v", err)
	}
	fabric := extmsg.NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	ref := extmsg.ConversationRef{ScopeID: "city-1", Provider: "slack", AccountID: "acct-1", ConversationID: "C1", Kind: extmsg.ConversationRoom}
	binding, err := fabric.Bindings.Bind(ctx, caller, extmsg.BindInput{Conversation: ref, SessionID: sess.ID, Now: now})
	if err != nil {
		t.Fatalf("binding a conversation: %v", err)
	}
	if err := fabric.Delivery.Record(ctx, caller, extmsg.DeliveryContextRecord{
		Conversation: ref, SessionID: sess.ID, BindingGeneration: binding.BindingGeneration, LastPublishedAt: now,
	}); err != nil {
		t.Fatalf("recording a delivery context: %v", err)
	}
	rows, err := store.List(beads.ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("listing the minted beads: %v", err)
	}
	var minted []beads.Bead
	families := map[string]bool{}
	for _, b := range rows {
		if strings.TrimSpace(b.Assignee) != "" || b.Metadata[beadmeta.RoutedToMetadataKey] != "" ||
			beads.IsReadyExcludedBead(b) || !demandRowServable(b) {
			continue
		}
		if !coordclass.Classify(b).IsInfrastructure() {
			t.Fatalf("minted bead %s (type %q, labels %v) classifies as work", b.ID, b.Type, b.Labels)
		}
		for _, family := range []string{"gc:nudge", "gc:extmsg-binding", "gc:extmsg-delivery"} {
			families[family] = families[family] || slices.Contains(b.Labels, family)
		}
		minted = append(minted, b)
	}
	for _, family := range []string{"gc:nudge", "gc:extmsg-binding", "gc:extmsg-delivery"} {
		if !families[family] {
			t.Fatalf("no servable %s bead among %v: the test would prove nothing", family, rows)
		}
	}
	return minted
}

// TestRouteAdoptionLeavesCoordinationBeadsAlone pins that the lane never
// routes gc's own coordination beads into the pool, as built by their real
// constructors: a pool worker would claim and close a live nudge or
// conversation binding as work. The plain bead beside them is the control:
// it is adopted, so the scope does adopt. Where the scope sets no target, the
// backstop's backlog line counts the plain bead alone.
func TestRouteAdoptionLeavesCoordinationBeadsAlone(t *testing.T) {
	mint := func(t *testing.T) (*conditionalRouteStore, []beads.Bead, beads.Bead) {
		t.Helper()
		mem := beads.NewMemStore()
		mem.IDPrefix = "ct"
		minted := mintCoordinationBeads(t, mem)
		work, err := mem.Create(plainCreatedBead(""))
		if err != nil {
			t.Fatalf("creating the control bead: %v", err)
		}
		return &conditionalRouteStore{MemStore: mem}, minted, work
	}

	t.Run("adopting scope", func(t *testing.T) {
		store, minted, work := mint(t)
		cr := &CityRuntime{cityName: "city", cityPath: t.TempDir(), cfg: adoptionTestConfig(), standaloneCityStore: store, stderr: io.Discard}
		lane := cr.routeRecoveryLaneOf()
		for _, b := range append(slices.Clone(minted), work) {
			lane.observe(beadCreatedEvent(t, b))
		}

		delta := cr.recoverUnroutedWorkRoutesDelta()
		backstop := cr.runRouteRecoveryBackstop(backstopReasonCadence)

		for _, b := range minted {
			if got := mustRoutedTo(t, store, b.ID); got != "" {
				t.Fatalf("coordination bead %s (labels %v) gc.routed_to = %q, want empty", b.ID, b.Labels, got)
			}
		}
		if got := mustRoutedTo(t, store, work.ID); got != adoptionTestCityPool {
			t.Fatalf("control bead %s gc.routed_to = %q, want %q", work.ID, got, adoptionTestCityPool)
		}
		if delta.adopted != 1 || backstop.adopted != 0 {
			t.Fatalf("adopted delta=%d backstop=%d, want 1/0: only the control bead", delta.adopted, backstop.adopted)
		}
	})

	t.Run("scope without a target", func(t *testing.T) {
		store, _, _ := mint(t)
		cfg := adoptionTestConfig()
		cfg.Workspace.UnroutedWorkTarget = ""
		cr := &CityRuntime{cityName: "city", cityPath: t.TempDir(), cfg: cfg, standaloneCityStore: store, stderr: io.Discard}

		report := cr.runRouteRecoveryBackstop(backstopReasonCadence)

		if got, want := report.unroutedBacklog, map[string]int{"city": 1}; !maps.Equal(got, want) {
			t.Fatalf("backstop unroutedBacklog = %v, want %v: coordination beads are not stalled work", got, want)
		}
	})
}

// claimConcurrently is a session claiming the bead after the lane's live
// read: assigned and in progress, with gc.routed_to still empty.
func claimConcurrently(id string) func(*beads.MemStore) {
	return func(store *beads.MemStore) {
		assignee, status := "researcher-session", "in_progress"
		if err := store.Update(id, beads.UpdateOpts{Assignee: &assignee, Status: &status}); err != nil {
			panic(err)
		}
	}
}

// TestRouteAdoptionLosesToAWriteAfterItsLiveRead is AC-4: when another writer
// routes or a session claims an adoption candidate after the lane's live read,
// that route or claim stands, and the lost write is neither an adoption nor a
// restore charged to the flap limit. A row read with its revision loses at the
// fenced write; a legacy row read at revision 0 loses at the live re-read the
// fence takes first.
func TestRouteAdoptionLosesToAWriteAfterItsLiveRead(t *testing.T) {
	const id = "ct-race"
	races := map[string]func(string) func(*beads.MemStore){
		"another writer routes": routeConcurrently,
		"a session claims":      claimConcurrently,
	}
	passes := map[string]func(cr *CityRuntime) routeRecoveryReport{
		"backstop": func(cr *CityRuntime) routeRecoveryReport {
			return cr.runRouteRecoveryBackstop(backstopReasonCadence)
		},
		"delta": func(cr *CityRuntime) routeRecoveryReport {
			cr.routeRecoveryLaneOf().observe(beadCreatedEvent(t, plainCreatedBead(id)))
			return cr.recoverUnroutedWorkRoutesDelta()
		},
	}
	revisions := map[string]int64{
		"fenced write":            1,
		"revision-0 live re-read": 0,
	}
	for raceName, race := range races {
		for passName, pass := range passes {
			for revisionName, revision := range revisions {
				t.Run(raceName+"/"+passName+"/"+revisionName, func(t *testing.T) {
					seed := plainCreatedBead(id)
					seed.Revision = revision
					before := beads.NewMemStoreFrom(0, []beads.Bead{seed}, nil)
					race(id)(before)
					want, err := before.Get(id)
					if err != nil {
						t.Fatalf("Get: %v", err)
					}

					store := &raceAfterLiveReadStore{
						MemStore: beads.NewMemStoreFrom(0, []beads.Bead{seed}, nil),
						target:   id,
						race:     race(id),
					}
					cr := &CityRuntime{cityName: "city", cfg: adoptionTestConfig(), standaloneCityStore: store, stderr: io.Discard}

					report := pass(cr)
					if !store.fired {
						t.Fatal("the competing write never ran; the pass did not re-read the bead")
					}
					if report.err != nil {
						t.Fatalf("pass error = %v, want nil: losing the race is not an error", report.err)
					}
					if report.adopted != 0 || report.restored != 0 {
						t.Fatalf("pass adopted=%d restored=%d, want 0/0", report.adopted, report.restored)
					}
					got, err := store.MemStore.Get(id)
					if err != nil {
						t.Fatalf("Get: %v", err)
					}
					if got.Assignee != want.Assignee || got.Status != want.Status ||
						got.Metadata[beadmeta.RoutedToMetadataKey] != want.Metadata[beadmeta.RoutedToMetadataKey] {
						t.Fatalf("after the pass assignee=%q status=%q routed_to=%q, want the other writer's %q/%q/%q",
							got.Assignee, got.Status, got.Metadata[beadmeta.RoutedToMetadataKey],
							want.Assignee, want.Status, want.Metadata[beadmeta.RoutedToMetadataKey])
					}
					lane := cr.routeRecoveryLaneOf()
					lane.mu.Lock()
					defer lane.mu.Unlock()
					if n := lane.restores[id]; n != 0 {
						t.Fatalf("flap tally for a lost race = %d, want 0", n)
					}
				})
			}
		}
	}
}

// TestRouteAdoptionFencesListBlindRowsOnTheirLiveRevision pins adoption on a
// store whose list reads carry no revision (BdStore). A pass that re-reads
// two or more candidates gets them from one List, so every one reads at
// revision 0; fenced there, each write failed its precondition and was
// dropped as a lost race, and nothing was ever adopted. The fence now comes
// from a live re-read of the row: its real revision, or 0 for a legacy row
// whose live revision is 0 too.
func TestRouteAdoptionFencesListBlindRowsOnTheirLiveRevision(t *testing.T) {
	ids := []string{"ct-1", "ct-2"}
	passes := map[string]func(cr *CityRuntime) routeRecoveryReport{
		"backstop": func(cr *CityRuntime) routeRecoveryReport {
			return cr.runRouteRecoveryBackstop(backstopReasonCadence)
		},
		"delta": func(cr *CityRuntime) routeRecoveryReport {
			for _, id := range ids {
				cr.routeRecoveryLaneOf().observe(beadCreatedEvent(t, plainCreatedBead(id)))
			}
			return cr.recoverUnroutedWorkRoutesDelta()
		},
	}
	revisions := map[string]int64{
		"rows with a revision":      4,
		"legacy rows at revision 0": 0,
	}
	for passName, pass := range passes {
		for revisionName, revision := range revisions {
			t.Run(passName+"/"+revisionName, func(t *testing.T) {
				var seed []beads.Bead
				for _, id := range ids {
					b := plainCreatedBead(id)
					b.Revision = revision
					seed = append(seed, b)
				}
				store := &conditionalRouteStore{MemStore: beads.NewMemStoreFrom(0, seed, nil), listBlind: true}
				cr := &CityRuntime{cityName: "city", cityPath: t.TempDir(), cfg: adoptionTestConfig(), standaloneCityStore: store, stderr: io.Discard}

				report := pass(cr)

				if report.err != nil {
					t.Fatalf("pass error = %v, want nil", report.err)
				}
				if report.adopted != len(ids) {
					t.Fatalf("pass adopted %d, want %d", report.adopted, len(ids))
				}
				for _, id := range ids {
					if got := mustRoutedTo(t, store, id); got != adoptionTestCityPool {
						t.Fatalf("%s gc.routed_to = %q, want %q", id, got, adoptionTestCityPool)
					}
				}
			})
		}
	}
}

// slingRouteFor is the gc.routed_to built-in `gc sling <target> <id>` writes
// for target on bead id, which lives in the store storeRef names: the agent
// the CLI resolves target to (resolveAgentIdentity, outside any rig
// directory), then the real sling core and router (doSling).
func slingRouteFor(t *testing.T, cfg *config.City, storeRef, target, id string) string {
	t.Helper()
	agent, ok := resolveAgentIdentity(cfg, target, "")
	if !ok {
		t.Fatalf("resolveAgentIdentity(%q) found no agent", target)
	}
	slingCfg := *cfg
	deps, stdout, stderr := testDeps(&slingCfg, runtime.NewFake(), newFakeRunner().run)
	deps.StoreRef = storeRef
	if code := doSling(testOpts(agent, id), deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("doSling(%q) = %d, want 0; stderr: %s", target, code, stderr.String())
	}
	bead, err := deps.Store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return bead.Metadata[beadmeta.RoutedToMetadataKey]
}

// TestRouteAdoptionAdoptedBeadIsServedClaimedAndDemanded is AC-6's Go half:
// for every spelling of a target that validation accepts, a bead the lane
// adopted carries exactly the route `gc sling <target>` writes for that
// spelling, and that route is the one the unchanged claim and demand
// predicates accept for a session of the target's pool. A bare name, a V2
// agent's name without its binding, or a bare pool-instance name is resolved
// to its agent first; stamped as typed, no worker would claim it. The shell
// half (Tier 3 query and count-form) is internal/config
// TestEffectiveWorkQueryAndCountFormServeAnAdoptedRoute, which pins the same
// route value.
func TestRouteAdoptionAdoptedBeadIsServedClaimedAndDemanded(t *testing.T) {
	rigPool := config.Agent{Name: "researcher", Dir: "hello-world", MinActiveSessions: ptr(0), MaxActiveSessions: ptr(3)}
	imported := config.Agent{Name: "researcher", BindingName: "gc", MinActiveSessions: ptr(0), MaxActiveSessions: ptr(3)}
	cases := []struct {
		name   string
		agent  config.Agent
		target string
		// rig sets the target on the hello-world rig rather than the
		// workspace.
		rig       bool
		wantRoute string
	}{
		{name: "qualified rig agent", agent: rigPool, target: "hello-world/researcher", rig: true, wantRoute: "hello-world/researcher"},
		{name: "bare rig agent name", agent: rigPool, target: "researcher", rig: true, wantRoute: "hello-world/researcher"},
		{name: "bare pool-instance name", agent: rigPool, target: "researcher-2", rig: true, wantRoute: "hello-world/researcher"},
		{name: "qualified pool-instance name", agent: rigPool, target: "hello-world/researcher-2", rig: true, wantRoute: "hello-world/researcher"},
		{name: "imported agent by its binding-qualified name", agent: imported, target: "gc.researcher", wantRoute: "gc.researcher"},
		{name: "imported agent by its name alone", agent: imported, target: "researcher", wantRoute: "gc.researcher"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := adoptionRigConfig(t, "")
			cfg.Agents = []config.Agent{tc.agent}
			// id is the bead the lane adopts; slingID, in the same store, is
			// the bead gc sling routes for comparison.
			id, slingID, storeRef := "ct-7", "ct-8", "city:city"
			if tc.rig {
				cfg.Rigs[0].UnroutedWorkTarget = tc.target
				id, slingID, storeRef = "hw-7", "hw-8", "rig:hello-world"
			} else {
				cfg.Workspace.UnroutedWorkTarget = tc.target
			}
			cr, cityStore, rigStore := adoptionRigRuntime(t, cfg, openConditionalRouteStore, []beads.Bead{plainCreatedBead("ct-7")}, []beads.Bead{plainCreatedBead("hw-7")})
			store := cityStore
			if tc.rig {
				store = rigStore
			}
			if err := validateUnroutedWorkTargets(cfg, cr.cityPath); err != nil {
				t.Fatalf("validateUnroutedWorkTargets() = %v, want nil", err)
			}
			agent := tc.agent
			routeTargets := hookClaimRouteTargets(hookClaimPrimaryRouteTarget(&agent), agent.QualifiedName()+"-1")
			template := agent.QualifiedName()

			// Control: before adoption the same bead is refused by both predicates.
			if hookClaimMatchesRoute(plainCreatedBead(id), routeTargets) {
				t.Fatal("hookClaimMatchesRoute(unrouted bead) = true, want false before adoption")
			}
			if _, ok := demandServableForTemplates(cfg, plainCreatedBead(id), map[string]struct{}{template: {}}); ok {
				t.Fatal("demandServableForTemplates(unrouted bead) = true, want false before adoption")
			}

			if report := cr.runRouteRecoveryBackstop(backstopReasonCadence); report.adopted != 1 {
				t.Fatalf("backstop adopted %d, want 1", report.adopted)
			}
			bead, err := store.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			got := bead.Metadata[beadmeta.RoutedToMetadataKey]
			if got != tc.wantRoute {
				t.Fatalf("adopted gc.routed_to = %q, want %q", got, tc.wantRoute)
			}
			if sling := slingRouteFor(t, cfg, storeRef, tc.target, slingID); got != sling {
				t.Fatalf("adopted gc.routed_to = %q, gc sling %s writes %q", got, tc.target, sling)
			}

			if !hookClaimMatchesRoute(bead, routeTargets) {
				t.Fatalf("hookClaimMatchesRoute(adopted bead, %v) = false, want true", routeTargets)
			}
			got, ok := demandServableForTemplates(cfg, bead, map[string]struct{}{template: {}})
			if !ok || got != template {
				t.Fatalf("demandServableForTemplates(adopted bead) = %q, %v; want %q, true", got, ok, template)
			}
			// Per-pool isolation holds for adopted routes too: a foreign target
			// neither claims nor demands the adopted bead.
			const foreignTarget = "hello-world/other"
			if hookClaimMatchesRoute(bead, []string{foreignTarget}) {
				t.Fatalf("hookClaimMatchesRoute(adopted bead, %q) = true, want false for a foreign target", foreignTarget)
			}
			if _, ok := demandServableForTemplates(cfg, bead, map[string]struct{}{foreignTarget: {}}); ok {
				t.Fatalf("demandServableForTemplates(adopted bead, %q) = true, want false for a foreign template", foreignTarget)
			}
		})
	}
}

// TestRouteAdoptionAdoptsNothingForATargetThatCannotAdopt pins the policy
// half of validation: a target the validator refuses, read around it (a
// config gc start or a reload would refuse), routes nothing rather than
// stamping a route no pool reading the scope's store would claim.
func TestRouteAdoptionAdoptsNothingForATargetThatCannotAdopt(t *testing.T) {
	cfg := adoptionRigConfig(t, "")
	cfg.Workspace.UnroutedWorkTarget = "hello-world/researcher"
	cr, cityStore, _ := adoptionRigRuntime(t, cfg, openConditionalRouteStore, []beads.Bead{plainCreatedBead("ct-7")}, nil)
	if err := validateUnroutedWorkTargets(cfg, cr.cityPath); err == nil {
		t.Fatal("validateUnroutedWorkTargets() = nil, want the workspace target refused")
	}

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)

	if got := mustRoutedTo(t, cityStore, "ct-7"); got != "" {
		t.Fatalf("ct-7 gc.routed_to = %q, want empty: rig hello-world's pool never reads the city store", got)
	}
	if report.adopted != 0 || cityStore.writeCount() != 0 {
		t.Fatalf("backstop adopted=%d writes=%d, want 0/0", report.adopted, cityStore.writeCount())
	}
}

// TestRouteAdoptionBackstopLogsUnroutedBacklogForScopesWithNoTarget pins the
// backlog line: the backstop counts the adoptable unrouted rows it already
// listed in each scope with no unrouted_work_target and logs one line per
// scope pointing at `gc doctor`, while a scope that adopts logs none.
func TestRouteAdoptionBackstopLogsUnroutedBacklogForScopesWithNoTarget(t *testing.T) {
	cfg := adoptionRigConfig(t, "")
	routed := plainCreatedBead("ct-8")
	routed.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: "elsewhere"}
	cr, cityStore, rigStore := adoptionRigRuntime(t, cfg, openConditionalRouteStore,
		[]beads.Bead{plainCreatedBead("ct-9"), plainCreatedBead("ct-10"), routed},
		[]beads.Bead{plainCreatedBead("hw-9")})
	var stderr strings.Builder
	cr.stderr, cr.logPrefix = &stderr, "gc"

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)

	if got, want := report.unroutedBacklog, map[string]int{"city": 2, "rig hello-world": 1}; !maps.Equal(got, want) {
		t.Fatalf("backstop unroutedBacklog = %v, want %v", got, want)
	}
	if cityStore.writeCount() != 0 || rigStore.writeCount() != 0 {
		t.Fatalf("writes city=%d rig=%d, want 0: the backlog is reported, never routed", cityStore.writeCount(), rigStore.writeCount())
	}
	for _, want := range []string{
		"gc: route recovery (backstop): 2 open bead(s) in city carry no gc.routed_to and the scope sets no unrouted_work_target; no pool will claim them (see gc doctor: unrouted-ready-work)\n",
		"gc: route recovery (backstop): 1 open bead(s) in rig hello-world carry no gc.routed_to and the scope sets no unrouted_work_target; no pool will claim them (see gc doctor: unrouted-ready-work)\n",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing backlog line %q:\n%s", want, stderr.String())
		}
	}

	adopting, _, _ := adoptionRigRuntime(t, adoptionRigConfig(t, "hello-world/researcher"), openConditionalRouteStore, nil, []beads.Bead{plainCreatedBead("hw-9")})
	if got := adopting.runRouteRecoveryBackstop(backstopReasonCadence).unroutedBacklog; len(got) != 0 {
		t.Fatalf("adopting scope unroutedBacklog = %v, want none", got)
	}
}
