package main

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

func TestPoolMembershipContributionUsesCanonicalLifecycleCapacity(t *testing.T) {
	cfg := poolMembershipTestConfig("worker", "reviewer")
	tests := []struct {
		name       string
		state      string
		sleep      string
		closed     bool
		wantMember bool
		wantCap    bool
		wantErr    bool
	}{
		{name: "start pending", state: string(sessionpkg.StateStartPending), wantMember: true, wantCap: true},
		{name: "creating", state: string(sessionpkg.StateCreating), wantMember: true, wantCap: true},
		{name: "active", state: string(sessionpkg.StateActive), wantMember: true, wantCap: true},
		{name: "legacy awake", state: string(sessionpkg.StateAwake), wantMember: true, wantCap: true},
		{name: "draining", state: string(sessionpkg.StateDraining), wantMember: true, wantCap: true},
		{name: "quarantined", state: string(sessionpkg.StateQuarantined), wantMember: true, wantCap: true},
		{name: "asleep", state: string(sessionpkg.StateAsleep), wantMember: true},
		{name: "drained compatibility", state: string(sessionpkg.StateAsleep), sleep: string(sessionpkg.SleepReasonDrained), wantMember: true},
		{name: "suspended", state: string(sessionpkg.StateSuspended), wantMember: true},
		{name: "failed create", state: string(sessionpkg.StateFailedCreate), wantMember: true},
		{name: "archived", state: string(sessionpkg.StateArchived), wantMember: true},
		{name: "closed status wins", state: string(sessionpkg.StateActive), closed: true},
		{name: "missing state is uncertified", wantErr: true},
		{name: "unknown state is uncertified", state: "future-state", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := poolMembershipInfo(t, "session-1", "worker", test.state, test.sleep, test.closed)
			contribution, member, err := poolMembershipContributionFromInfo(cfg, info)
			if (err != nil) != test.wantErr {
				t.Fatalf("contribution error = %v, want error=%t", err, test.wantErr)
			}
			if member != test.wantMember {
				t.Fatalf("member = %t, want %t (contribution=%+v)", member, test.wantMember, contribution)
			}
			if !member {
				return
			}
			if contribution.poolTarget != "worker" || contribution.sessionID != info.ID {
				t.Fatalf("contribution identity = %+v, want worker/%s", contribution, info.ID)
			}
			if contribution.countsAgainstCap != test.wantCap {
				t.Fatalf("countsAgainstCap = %t, want %t (base=%s)", contribution.countsAgainstCap, test.wantCap, contribution.baseState)
			}
		})
	}
}

func TestPoolMembershipContributionHandlesCanonicalSingletonAndNamedCollapse(t *testing.T) {
	cfg := poolMembershipTestConfig("worker")
	cfg.Agents[0].MaxActiveSessions = poolMembershipInt(1)
	if cfg.Agents[0].SupportsInstanceExpansion() {
		t.Fatal("fixture must exercise the canonical singleton identity")
	}

	canonical := poolMembershipInfo(t, "session-pool", "worker", "active", "", false)
	canonical.PoolSlot = ""
	contribution, present, err := poolMembershipContributionFromInfo(cfg, canonical)
	if err != nil || !present || contribution.poolTarget != "worker" || !contribution.countsAgainstCap {
		t.Fatalf("canonical singleton contribution = (%+v, %t, %v), want occupied worker member", contribution, present, err)
	}

	named := poolMembershipInfo(t, "session-named", "worker", "active", "", false)
	named.ConfiguredNamedSession = true
	named.ConfiguredNamedIdentity = "worker"
	named.SessionOrigin = "named"
	named.PoolManaged = false
	named.PoolSlot = ""
	if contribution, present, err := poolMembershipContributionFromInfo(cfg, named); err != nil || present {
		t.Fatalf("configured named contribution = (%+v, %t, %v), want non-member", contribution, present, err)
	}
}

func TestPoolMembershipContributionKeepsCanonicalSingletonSlotZeroWhenPoolExpansionIsEnabled(t *testing.T) {
	one := 1
	cfg := poolMembershipTestConfig("worker")
	cfg.Agents[0].MaxActiveSessions = &one
	cfg.Agents[0].MinActiveSessions = &one
	if !cfg.Agents[0].SupportsInstanceExpansion() || !cfg.Agents[0].UsesCanonicalSingletonPoolIdentity() {
		t.Fatalf("fixture must be an expanding canonical singleton pool: %+v", cfg.Agents[0])
	}

	info := poolMembershipInfo(t, "session-pool", "worker", string(sessionpkg.StateActive), "", false)
	info.PoolSlot = ""
	contribution, present, err := poolMembershipContributionFromInfo(cfg, info)
	if err != nil || !present || contribution.slot != 0 {
		t.Fatalf("canonical singleton contribution = (%+v, %t, %v), want slot-0 member", contribution, present, err)
	}
}

func TestPoolMembershipContributionRejectsNamedPoolIdentityConflict(t *testing.T) {
	cfg := poolMembershipTestConfig("worker")
	conflict := poolMembershipInfo(t, "session-conflict", "worker", "active", "", false)
	conflict.ConfiguredNamedSession = true
	conflict.ConfiguredNamedIdentity = "worker"

	if _, _, err := poolMembershipContributionFromInfo(cfg, conflict); err == nil {
		t.Fatal("named pool identity conflict was accepted")
	}
}

func TestPoolMembershipIndexMovesOnlyTheChangedSession(t *testing.T) {
	cfg := poolMembershipTestConfig("worker", "reviewer")
	worker1 := poolMembershipInfo(t, "session-1", "worker", "active", "", false)
	worker2 := poolMembershipInfo(t, "session-2", "worker", "asleep", "", false)
	worker2.PoolSlot = "2"
	reviewer := poolMembershipInfo(t, "session-3", "reviewer", "active", "", false)
	index := rebuiltPoolMembershipIndex(t, cfg, []sessionpkg.Info{worker1, worker2, reviewer})

	assertCertifiedPoolMembership(t, index.observe("worker"), 2, 1)
	assertCertifiedPoolMembership(t, index.observe("reviewer"), 1, 1)

	moved := poolMembershipInfo(t, worker1.ID, "reviewer", "draining", "", false)
	moved.PoolSlot = "2"
	if err := index.replace(cfg, moved); err != nil {
		t.Fatalf("replace moved session: %v", err)
	}
	assertCertifiedPoolMembership(t, index.observe("worker"), 1, 0)
	assertCertifiedPoolMembership(t, index.observe("reviewer"), 2, 2)

	index.remove(worker2.ID)
	assertCertifiedPoolMembership(t, index.observe("worker"), 0, 0)
}

func TestPoolMembershipIndexKeepsRevisionForUnchangedContribution(t *testing.T) {
	cfg := poolMembershipTestConfig("worker")
	active := poolMembershipInfo(t, "session-1", "worker", "active", "", false)
	index := rebuiltPoolMembershipIndex(t, cfg, []sessionpkg.Info{active})
	before, memberIDs, exact := index.observeMemberIDs("worker")
	if !exact {
		t.Fatalf("initial membership = (%+v, %v, %t), want certified exact state", before, memberIDs, exact)
	}

	updated := active
	updated.Title = "same membership, new provenance"
	updated.LastNudgeDeliveredAt = time.Now().UTC()
	if err := index.replace(cfg, updated); err != nil {
		t.Fatalf("replace unchanged membership contribution: %v", err)
	}

	after, afterIDs, exact := index.observeMemberIDs("worker")
	if !exact || after.revision != before.revision || !reflect.DeepEqual(afterIDs, memberIDs) ||
		after.members != before.members || after.occupied != before.occupied {
		t.Fatalf("membership after non-membership update = (%+v, %v, %t), want revision-stable (%+v, %v)", after, afterIDs, exact, before, memberIDs)
	}
}

func TestPoolMembershipIndexObservesMemberIDsOnlyFromCertifiedExactState(t *testing.T) {
	cfg := poolMembershipTestConfig("worker")
	first := poolMembershipInfo(t, "session-1", "worker", "active", "", false)
	second := poolMembershipInfo(t, "session-2", "worker", "active", "", false)
	second.PoolSlot = "2"

	index := rebuiltPoolMembershipIndex(t, cfg, []sessionpkg.Info{first})
	observation, ids, ok := index.observeMemberIDs("worker")
	if !ok || !reflect.DeepEqual(ids, []string{first.ID}) || !observation.certified || observation.members != 1 || observation.occupied != 1 {
		t.Fatalf("member observation = (%+v, %v, %t), want certified [%q]", observation, ids, ok, first.ID)
	}

	if err := index.replace(cfg, second); err != nil {
		t.Fatalf("add second member: %v", err)
	}
	observation, ids, ok = index.observeMemberIDs("worker")
	if !ok || !reflect.DeepEqual(ids, []string{first.ID, second.ID}) || observation.members != 2 || observation.occupied != 2 {
		t.Fatalf("two-member observation = (%+v, %v, %t), want both exact member IDs", observation, ids, ok)
	}
	ids[0] = "mutated-copy"
	_, ids, ok = index.observeMemberIDs("worker")
	if !ok || !reflect.DeepEqual(ids, []string{first.ID, second.ID}) {
		t.Fatalf("member observation after caller mutation = (%v, %t), want unchanged exact member IDs", ids, ok)
	}

	index.remove(second.ID)
	index.invalidate(poolMembershipUncertifiedSnapshotGap)
	if observation, ids, ok := index.observeMemberIDs("worker"); ok || ids != nil || observation.certified {
		t.Fatalf("uncertified member observation = (%+v, %v, %t), want refusal", observation, ids, ok)
	}
}

func TestPoolMembershipIndexCertifiesExpandablePoolSlots(t *testing.T) {
	unlimited := -1
	cfg := poolMembershipTestConfig("worker")
	cfg.Agents[0].MaxActiveSessions = &unlimited

	withSlot := func(id, slot string) sessionpkg.Info {
		info := poolMembershipInfo(t, id, "worker", string(sessionpkg.StateActive), "", false)
		info.PoolSlot = slot
		return info
	}

	t.Run("selects lowest positive hole", func(t *testing.T) {
		index := rebuiltPoolMembershipIndex(t, cfg, []sessionpkg.Info{withSlot("slot-1", "1"), withSlot("slot-3", "3")})
		got := index.observe("worker")
		if !got.certified || got.members != 2 || got.occupied != 2 || got.nextFreeSlot != 2 {
			t.Fatalf("slot-hole observation = %+v, want certified occupied slots with next free 2", got)
		}
	})

	t.Run("incremental duplicate invalidates certification", func(t *testing.T) {
		index := rebuiltPoolMembershipIndex(t, cfg, []sessionpkg.Info{withSlot("first", "1")})
		if err := index.replace(cfg, withSlot("second", "1")); err == nil {
			t.Fatal("duplicate slot delta was accepted")
		}
		got := index.observe("worker")
		if got.certified || got.reason != poolMembershipUncertifiedInvalidDelta {
			t.Fatalf("membership after duplicate slot delta = %+v, want invalid_delta certification failure", got)
		}
	})

	for _, test := range []struct {
		name  string
		infos []sessionpkg.Info
	}{
		{name: "missing slot", infos: []sessionpkg.Info{withSlot("missing", "")}},
		{name: "invalid slot", infos: []sessionpkg.Info{withSlot("invalid", "not-a-slot")}},
		{name: "duplicate slot", infos: []sessionpkg.Info{withSlot("first", "1"), withSlot("second", "1")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := buildPoolMembershipState(cfg, test.infos); err == nil {
				t.Fatal("invalid expandable-pool slot census was certified")
			}
		})
	}
}

func TestPoolMembershipIndexIncrementalHistoryMatchesFreshRebuild(t *testing.T) {
	cfg := poolMembershipTestConfig("worker", "reviewer")
	unlimited := -1
	for i := range cfg.Agents {
		cfg.Agents[i].MaxActiveSessions = &unlimited
	}
	current := map[string]sessionpkg.Info{}
	incremental := rebuiltPoolMembershipIndex(t, cfg, nil)
	states := []string{"start-pending", "creating", "active", "asleep", "suspended", "draining", "quarantined", "archived"}

	for step := 0; step < 256; step++ {
		id := "session-" + string(rune('a'+step%17))
		if step%11 == 0 {
			delete(current, id)
			incremental.remove(id)
		} else {
			target := "worker"
			if step%3 == 0 {
				target = "reviewer"
			}
			info := poolMembershipInfo(t, id, target, states[step%len(states)], "", false)
			info.PoolSlot = fmt.Sprintf("%d", step+1)
			current[id] = info
			if err := incremental.replace(cfg, info); err != nil {
				t.Fatalf("step %d replace: %v", step, err)
			}
		}

		infos := make([]sessionpkg.Info, 0, len(current))
		for _, info := range current {
			infos = append(infos, info)
		}
		fresh := rebuiltPoolMembershipIndex(t, cfg, infos)
		for _, target := range []string{"worker", "reviewer"} {
			got, want := incremental.observe(target), fresh.observe(target)
			if got.members != want.members || got.occupied != want.occupied || got.nextFreeSlot != want.nextFreeSlot || !got.certified {
				t.Fatalf("step %d target %s incremental=%+v fresh=%+v", step, target, got, want)
			}
		}
	}
}

func TestPoolMembershipIndexRejectsStaleRebuildAndRecertifiesAfterGap(t *testing.T) {
	cfg := poolMembershipTestConfig("worker")
	active := poolMembershipInfo(t, "session-1", "worker", "active", "", false)
	index := rebuiltPoolMembershipIndex(t, cfg, []sessionpkg.Info{active})
	token := index.rebuildToken()
	stale, err := buildPoolMembershipState(cfg, nil)
	if err != nil {
		t.Fatalf("build stale candidate: %v", err)
	}

	asleep := poolMembershipInfo(t, active.ID, "worker", "asleep", "", false)
	if err := index.replace(cfg, asleep); err != nil {
		t.Fatalf("replace during rebuild: %v", err)
	}
	if index.publishRebuild(token, stale) {
		t.Fatal("stale rebuild published over a newer exact delta")
	}
	assertCertifiedPoolMembership(t, index.observe("worker"), 1, 0)

	index.invalidate(poolMembershipUncertifiedSnapshotGap)
	if got := index.observe("worker"); got.certified || got.reason != poolMembershipUncertifiedSnapshotGap {
		t.Fatalf("invalidated observation = %+v, want explicit snapshot gap", got)
	}
	state, err := buildPoolMembershipState(cfg, []sessionpkg.Info{asleep})
	if err != nil {
		t.Fatalf("build recovery candidate: %v", err)
	}
	if !index.publishRebuild(index.rebuildToken(), state) {
		t.Fatal("fresh recovery rebuild was rejected")
	}
	assertCertifiedPoolMembership(t, index.observe("worker"), 1, 0)
}

func TestPoolMembershipSnapshotRebuildRejectsControllerConfigDrift(t *testing.T) {
	cr := newSessionStartCityRuntimeForTest(t, rollout.Auto, true)
	cr.poolMembershipShadow = newPoolMembershipIndex()
	cr.cfg.Agents[0].MaxActiveSessions = poolMembershipInt(2)
	cr.cs.cfg = cr.cfg
	cr.sessionStartOwnership = sessionStartOwnershipKeyed

	if _, err := cr.cs.cityBeadStore.Create(beads.Bead{
		Title:  "worker slot",
		Type:   sessionpkg.BeadType,
		Status: "open",
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"template":       "worker",
			"session_name":   "s-worker-1",
			"pool_managed":   "true",
			"session_origin": "ephemeral",
			"pool_slot":      "1",
			"state":          "active",
		},
	}); err != nil {
		t.Fatalf("create pool session: %v", err)
	}
	if snapshot := cr.loadSessionBeadSnapshot(); snapshot == nil {
		t.Fatal("initial session snapshot is nil")
	}
	assertCertifiedPoolMembership(t, cr.poolMembershipShadow.observe("worker"), 1, 1)

	nextCfg := *cr.cfg
	nextCfg.Agents = append([]config.Agent(nil), cr.cfg.Agents...)
	cr.cs.mu.Lock()
	cr.cs.sessionStartGeneration++
	cr.cs.sessionStartStoreGeneration = cr.cs.sessionStartGeneration
	cr.cs.cfg = &nextCfg
	cr.cs.mu.Unlock()

	if snapshot := cr.loadSessionBeadSnapshot(); snapshot == nil {
		t.Fatal("drifted session snapshot is nil")
	}
	got := cr.poolMembershipShadow.observe("worker")
	if got.certified || got.reason != poolMembershipUncertifiedConfigChanged {
		t.Fatalf("membership after controller config drift = %+v, want config_changed uncertified", got)
	}
}

func TestSessionMutationRefreshesPoolMembershipBeforeDemandShadow(t *testing.T) {
	cr := newSessionStartCityRuntimeForTest(t, rollout.Auto, true)
	cr.poolMembershipShadow = newPoolMembershipIndex()
	cr.cityPath = t.TempDir()
	cr.cityName = "test-city"
	cr.cs.cityPath = cr.cityPath
	cr.cs.cityName = cr.cityName
	cr.cfg.Agents[0].MaxActiveSessions = poolMembershipInt(2)
	cr.cs.cfg = cr.cfg
	store := cr.cs.cityBeadStore
	trace := newSessionReconcilerTraceManager(cr.cityPath, cr.cityName, io.Discard)
	cr.trace = trace
	t.Cleanup(func() { _ = trace.Close() })

	created, err := store.Create(beads.Bead{
		Title:  "worker slot",
		Type:   sessionpkg.BeadType,
		Status: "open",
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"template":       "worker",
			"session_name":   "s-worker-1",
			"pool_managed":   "true",
			"session_origin": "ephemeral",
			"pool_slot":      "1",
			"state":          "active",
		},
	})
	if err != nil {
		t.Fatalf("create pool session: %v", err)
	}
	seed, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load session seed: %v", err)
	}
	if err := cr.ensureSessionStartController(context.Background(), seed); err != nil {
		t.Fatalf("ensure keyed controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	if snapshot := cr.loadSessionBeadSnapshot(); snapshot == nil {
		t.Fatal("authoritative session rebuild returned nil")
	}
	assertCertifiedPoolMembership(t, cr.poolMembershipShadow.observe("worker"), 1, 1)

	if err := sessionFrontDoor(store).ApplyPatch(created.ID, sessionpkg.MetadataPatch{
		"state":        "asleep",
		"sleep_reason": string(sessionpkg.SleepReasonIdle),
	}); err != nil {
		t.Fatalf("sleep pool session: %v", err)
	}
	updated, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("read updated pool session: %v", err)
	}
	cr.cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, updated))
	assertCertifiedPoolMembership(t, cr.poolMembershipShadow.observe("worker"), 1, 0)

	now := time.Now().UTC()
	cr.recordReadyRoutedWorkDemandContribution(readyRoutedWorkDemandContribution{
		WorkID:              "ga-ready",
		PoolTarget:          "worker",
		SourceActor:         "bd-hook",
		SourceStore:         "city:test-city",
		ContributionPresent: true,
		EventAt:             now.Add(-time.Second),
		ObservedAt:          now,
		DecidedAt:           now,
	})

	records, err := ReadTraceRecords(traceCityRuntimeDir(cr.cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read pool membership shadow trace: %v", err)
	}
	for _, record := range records {
		if record.RecordType != TraceRecordOperation || record.SiteCode != TraceSitePoolDemandContributionShadow {
			continue
		}
		if record.Fields["pool_member_count"] != float64(1) ||
			record.Fields["pool_occupancy"] != float64(0) ||
			record.Fields["pool_membership_certified"] != true ||
			record.Fields["pool_membership_lookup_ns"].(float64) < 0 ||
			record.Fields["event_to_capacity_shadow_decision_ns"].(float64) <= 0 ||
			record.Fields["effect_applied"] != false {
			t.Fatalf("pool membership shadow record = %+v", record)
		}
		return
	}
	t.Fatal("pool membership shadow record not found")
}

func BenchmarkPoolMembershipIndexReplaceFleetSize(b *testing.B) {
	for _, size := range []int{1, 1_000, 10_000} {
		b.Run(fmt.Sprintf("fleet-%d", size), func(b *testing.B) {
			cfg := poolMembershipTestConfig("worker")
			unlimited := -1
			cfg.Agents[0].MaxActiveSessions = &unlimited
			infos := make([]sessionpkg.Info, size)
			for i := range infos {
				infos[i] = sessionpkg.Info{
					ID:            fmt.Sprintf("session-%d", i+1),
					Template:      "worker",
					PoolSlot:      strconv.Itoa(i + 1),
					MetadataState: "active",
					PoolManaged:   true,
					SessionOrigin: "ephemeral",
				}
			}
			index := rebuiltPoolMembershipIndex(b, cfg, infos)
			updated := infos[0]
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%2 == 0 {
					updated.MetadataState = "asleep"
				} else {
					updated.MetadataState = "active"
				}
				if err := index.replace(cfg, updated); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func rebuiltPoolMembershipIndex(t testing.TB, cfg *config.City, infos []sessionpkg.Info) *poolMembershipIndex {
	t.Helper()
	index := newPoolMembershipIndex()
	state, err := buildPoolMembershipState(cfg, infos)
	if err != nil {
		t.Fatalf("build membership state: %v", err)
	}
	if !index.publishRebuild(index.rebuildToken(), state) {
		t.Fatal("initial membership rebuild rejected")
	}
	return index
}

func assertCertifiedPoolMembership(t testing.TB, got poolMembershipObservation, members, occupied int) {
	t.Helper()
	if got.members != members || got.occupied != occupied || !got.certified {
		t.Fatalf("membership observation = %+v, want members=%d occupied=%d certified", got, members, occupied)
	}
}

func poolMembershipInfo(t testing.TB, id, template, state, sleep string, closed bool) sessionpkg.Info {
	t.Helper()
	status := "open"
	if closed {
		status = "closed"
	}
	return sessiontest.SeedBead(t, beads.Bead{
		ID:     id,
		Title:  id,
		Type:   sessionpkg.BeadType,
		Status: status,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"template":       template,
			"session_name":   "s-" + id,
			"pool_managed":   "true",
			"session_origin": "ephemeral",
			"pool_slot":      "1",
			"state":          state,
			"sleep_reason":   sleep,
		},
	})
}

func poolMembershipTestConfig(names ...string) *config.City {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	for _, name := range names {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: name, MaxActiveSessions: poolMembershipInt(10)})
	}
	return cfg
}

func poolMembershipInt(value int) *int { return &value }
