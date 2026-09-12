package main

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Codex round-6 pins (evidence 07-codex-r6.md).

// probeConditionalWriter is the bare capability probe tests inject for the
// fenced arms: a MemStore carries no beads.conditional_writes mode, so the
// production seam (beads.ResolveConditionalWriter) leaves it unfenced.
func probeConditionalWriter(store beads.Store) (beads.ConditionalWriter, error) {
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		return nil, nil
	}
	return writer, nil
}

// TestPoolStartBackoffClassRefChargesTheClassStoreCopy: a bead migrated to a
// relocated class binding keeps a retained copy in the work store. The
// trigger's "class:<token>" store ref names the binding, so the ACTIVE copy in
// the class store is the one charged — never the retained copy, and never
// skipped because the retained copy's route differs.
func TestPoolStartBackoffClassRefChargesTheClassStoreCopy(t *testing.T) {
	const id = "gg-active"
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}, {Name: "other", MaxActiveSessions: intPtr(1)}}}
	retained := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Title: "retained copy", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "other"}}}, nil)
	class := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Title: "active copy", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}}}, nil)
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore:   retained,
		extraStores: []beads.Store{class},
		classStoreByRef: func(ref string) beads.Store {
			if ref == "class:g" {
				return class
			}
			return nil
		},
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		stderr:            &stderr,
	}
	policy.recordStartFailure(workTrigger{BeadID: id, StoreRef: "class:g"}, "worker", errPreStartFailure, time.Now().UTC())
	active, err := class.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := readWorkStartFailureState(active.Metadata).Failures; got != 1 {
		t.Fatalf("the ACTIVE copy in the class store must be charged: count=%d\nstderr:\n%s", got, stderr.String())
	}
	copyRow, err := retained.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := readWorkStartFailureState(copyRow.Metadata).Failures; got != 0 {
		t.Fatalf("the retained work-store copy must be untouched: count=%d", got)
	}
	// Without the class-ref resolution the retained copy shadows the active
	// one: found first in the work store, routed "elsewhere", nothing charged.
	policy.classStoreByRef = nil
	policy.recordStartFailure(workTrigger{BeadID: id, StoreRef: "class:g"}, "worker", errPreStartFailure, time.Now().UTC())
	active, _ = class.Get(id)
	if got := readWorkStartFailureState(active.Metadata).Failures; got != 1 {
		t.Fatalf("control: with no class-ref resolution the active copy is shadowed (count stays 1), got %d", got)
	}
}

// TestPoolStartBackoffUnfencedReadsBypassTheCache: through the controller's
// policy wrapper over a caching store, an unfenced record write reads the
// row the backing holds NOW — a cache-served row would be the same stale
// row twice, and a re-read that cannot see a concurrent clear fences
// nothing. Here the cache still says four failures while the backing was
// cleared: the write computes from the backing (count 1, no park).
func TestPoolStartBackoffUnfencedReadsBypassTheCache(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1), MaxStartFailures: intPtr(5)}}}
	backing := beads.NewMemStore()
	caching := beads.NewCachingStoreForTest(backing, nil)
	work, err := caching.Create(beads.Bead{Title: "cached work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	four := map[string]string{beadmeta.StartFailuresMetadataKey: "4", beadmeta.StartFailedAtMetadataKey: time.Now().UTC().Format(time.RFC3339), beadmeta.StartFailureMetadataKey: "boom"}
	if err := caching.SetMetadataBatch(work.ID, four); err != nil {
		t.Fatal(err)
	}
	if err := caching.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The concurrent clear lands on the backing behind the cache's back.
	if err := backing.SetMetadataBatch(work.ID, workStartFailureClearPatch(four)); err != nil {
		t.Fatal(err)
	}
	wrapped := &beadPolicyStore{Store: caching, cfg: cfg}
	cached, err := wrapped.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cache-served count after the backing clear: %d", readWorkStartFailureState(cached.Metadata).Failures)
	live, err := liveWorkBead(wrapped, work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readWorkStartFailureState(live.Metadata).Failures; got != 0 {
		t.Fatalf("liveWorkBead must read the backing through the wrapper and the cache: count=%d, want 0", got)
	}
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore:         wrapped,
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		stderr:            &stderr,
	}
	policy.recordStartFailure(workTrigger{BeadID: work.ID, StoreRef: "city"}, "worker", errPreStartFailure, time.Now().UTC())
	row, err := backing.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := readWorkStartFailureState(row.Metadata)
	if state.Failures != 1 || state.Parked() {
		t.Fatalf("the charge must be computed from the LIVE row (count 1, no park), got %+v\nstderr:\n%s", state, stderr.String())
	}
}

// TestBuildDesiredState_FreshNamedHolderCarriesItsDirectWorkTrigger: an
// on-demand named holder with no bead yet, woken by work assigned to it,
// is created for that bead: the trigger rides on its TemplateParams and
// lands on the bead at creation (namedSessionTriggerMetadata), so the first
// failed start already charges the work bead.
func TestBuildDesiredState_FreshNamedHolderCarriesItsDirectWorkTrigger(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "solo",
			StartCommand:      "true",
			WorkQuery:         "printf ''",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{Template: "solo", Mode: "on_demand"}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	work, err := store.Create(beads.Bead{Title: "solo direct work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "solo"}})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &identity}); err != nil {
		t.Fatal(err)
	}
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if !dsResult.NamedSessionDemand[identity] {
		t.Fatalf("work assigned to %s must be its direct demand: %v", identity, dsResult.NamedSessionDemand)
	}
	var found *TemplateParams
	for _, tp := range dsResult.State {
		if tp.ConfiguredNamedIdentity == identity {
			tp := tp
			found = &tp
			break
		}
	}
	if found == nil {
		t.Fatalf("no desired state for the named holder %s: %v", identity, mapKeys(dsResult.State))
	}
	if found.TriggerBeadID != work.ID {
		t.Fatalf("the fresh holder's TemplateParams must carry the work bead as its trigger, got %q", found.TriggerBeadID)
	}
	// The city store's ref may be spelled "" in the assigned-work snapshot;
	// storeByRef reads "" as the work store, so only the id is required.
	meta := namedSessionTriggerMetadata(*found)
	if meta[beadmeta.TriggerBeadIDMetadataKey] != work.ID {
		t.Fatalf("the create/reopen metadata must carry the trigger: %v", meta)
	}
	t.Logf("trigger metadata on the fresh holder: %v", meta)
	if namedSessionTriggerMetadata(TemplateParams{}) != nil {
		t.Fatal("no trigger → no metadata")
	}
}
