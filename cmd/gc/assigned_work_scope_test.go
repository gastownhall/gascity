package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// sessionInfosFromBeads projects raw session beads to session.Info through the
// session store front door (via the shared seedSessionInfo seeder), matching how
// the reconciler feeds snapshot.OpenInfos() into the pool-demand/session-wake
// filters. Every caller passes session-shaped fixtures and no consumer reads
// Info.Type, so the seeder's type-stamp is behavior-neutral here.
func sessionInfosFromBeads(bs []beads.Bead) []sessionpkg.Info {
	if bs == nil {
		return nil
	}
	infos := make([]sessionpkg.Info, len(bs))
	for i, b := range bs {
		infos[i] = seedSessionInfo(b)
	}
	return infos
}

// TestFilterAssignedWorkBeadsForSessionWakeKeepsOnlyReachableAssigneeSources
// pins the store scoping of the wake filter for a rig-scoped session.
//
// Two arms are reachable and one is not. Its OWN rig store is reachable, as it
// always was; the LEADING arm (ref "") is now reachable too, because that is
// where claim-time class routing writes an assignee on a split city and where
// the agent's own hook fan-out reads even on a single-store one
// (appendCityHookStore) — dropping a claim the session holds there is what left
// a live worker with no wake reason at all (ga-whzrt). Another rig's store stays
// unreachable, which is what keeps this a scoping rule rather than
// cross-store-for-everyone.
func TestFilterAssignedWorkBeadsForSessionWakeKeepsOnlyReachableAssigneeSources(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "riga", Path: filepath.Join(cityPath, "riga")},
			{Name: "rigb", Path: filepath.Join(cityPath, "rigb")},
		},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "riga",
			Mode:     "on_demand",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":                  "riga/worker",
			"session_name":              "worker-session",
			"configured_named_identity": "riga/worker",
		},
	}}
	work := []beads.Bead{
		{ID: "city-named", Status: "open", Assignee: "riga/worker"},
		{ID: "rig-named", Status: "open", Assignee: "riga/worker"},
		{ID: "city-session", Status: "in_progress", Assignee: "session-1"},
		{ID: "rig-session", Status: "in_progress", Assignee: "session-1"},
		{ID: "other-rig-session", Status: "in_progress", Assignee: "session-1"},
		{ID: "other-rig-named", Status: "open", Assignee: "riga/worker"},
	}
	storeRefs := []string{"", "riga", "", "riga", "rigb", "rigb"}

	got, gotRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, nil, sessionInfosFromBeads(sessions), work, storeRefs)

	wantIDs := []string{"city-named", "rig-named", "city-session", "rig-session"}
	wantRefs := []string{"", "riga", "", "riga"}
	if len(got) != len(wantIDs) {
		t.Fatalf("filtered work length = %d, want %d: %#v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("filtered work IDs = %v, want %v", assignedWorkIDs(got), wantIDs)
		}
		if gotRefs[i] != wantRefs[i] {
			t.Fatalf("filtered store refs = %#v, want %#v aligned with beads", gotRefs, wantRefs)
		}
	}
}

func assignedWorkIDs(work []beads.Bead) []string {
	ids := make([]string, 0, len(work))
	for _, wb := range work {
		ids = append(ids, wb.ID)
	}
	return ids
}

// TestFilterAssignedWorkBeadsForSessionWakeWithStoresProjectsSurvivingStores
// pins the store projection where alignment is CONSTRUCTED. Every other test of
// this contract exercises a consumer that is handed an already-aligned slice;
// this one drops a bead in the MIDDLE and asserts the legs move with it.
//
// The assertion is store IDENTITY, not length. A projection that dropped the
// bead but not its store yields a same-length pair that every downstream length
// check accepts, and the orphan release then writes the surviving bead through
// the leg that belonged to the dropped one — the exact cross-leg write ga-b0o6a
// exists to prevent. Distinct MemStore handles per index are what make that
// detectable.
func TestFilterAssignedWorkBeadsForSessionWakeWithStoresProjectsSurvivingStores(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "riga", Path: filepath.Join(cityPath, "riga")},
			{Name: "rigb", Path: filepath.Join(cityPath, "rigb")},
		},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "riga",
			Mode:     "on_demand",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":                  "riga/worker",
			"session_name":              "worker-session",
			"configured_named_identity": "riga/worker",
		},
	}}
	work := []beads.Bead{
		{ID: "leading-keep", Status: "in_progress", Assignee: "session-1"},
		{ID: "other-rig-drop", Status: "in_progress", Assignee: "session-1"},
		{ID: "own-rig-keep", Status: "in_progress", Assignee: "session-1"},
	}
	storeRefs := []string{"", "rigb", "riga"}
	stores := []beads.Store{beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()}

	got, gotRefs, gotStores := filterAssignedWorkBeadsForSessionWakeWithStores(
		cfg, cityPath, nil, sessionInfosFromBeads(sessions), work, storeRefs, stores,
	)

	wantIDs := []string{"leading-keep", "own-rig-keep"}
	if gotIDs := assignedWorkIDs(got); !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("filtered work IDs = %v, want %v — the middle bead must drop", gotIDs, wantIDs)
	}
	wantRefs := []string{"", "riga"}
	if !slices.Equal(gotRefs, wantRefs) {
		t.Fatalf("filtered store refs = %#v, want %#v aligned with beads", gotRefs, wantRefs)
	}
	if len(gotStores) != len(wantIDs) {
		t.Fatalf("filtered stores length = %d, want %d — a store must drop with its bead", len(gotStores), len(wantIDs))
	}
	wantStores := []beads.Store{stores[0], stores[2]}
	for i, want := range wantStores {
		if gotStores[i] != want {
			t.Fatalf("filtered store at index %d is not the input store for %q; the projection is misordered, so a release would write through another bead's leg", i, wantIDs[i])
		}
	}
}

func TestFilterAssignedWorkBeadsForSessionWakeCityScopedAgentIsCrossStoreEligible(t *testing.T) {
	// vp-kvp: a city-scoped singleton legitimately serves per-rig routed work.
	// Its assigned work may live in ANY store, so reachability must federate
	// across stores — gating it to its own configured rig is the cross-store
	// dead-drop this fixes. Rig-scoped agents stay single-store (unchanged).
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:  "auditor",
			Scope: "city",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "auditor",
			Scope:    "city",
			Mode:     "on_demand",
		}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	work := []beads.Bead{
		{ID: "city-work", Status: "open", Assignee: identity},
		{ID: "rig-work", Status: "open", Assignee: identity},
	}
	storeRefs := []string{"", "riga"} // city store + rig store

	got, gotRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, nil, nil, work, storeRefs)

	if len(got) != 2 {
		t.Fatalf("city-scoped %q must be reachable from BOTH stores; got %d: %#v", identity, len(got), got)
	}
	if len(gotRefs) != len(got) || gotRefs[0] != "" || gotRefs[1] != "riga" {
		t.Fatalf("filtered store refs = %#v, want [\"\" riga] aligned with beads", gotRefs)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsDirectAssigneeAfterTemplateFallback(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-session",
		},
	}}
	work := []beads.Bead{{
		ID:       "direct-assigned",
		Status:   "in_progress",
		Assignee: "session-1",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", nil, sessionInfosFromBeads(sessions), work, []string{""})

	if len(got) != 1 || got[0].ID != "direct-assigned" {
		t.Fatalf("filtered work = %#v, want direct-assigned work preserved through template fallback", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsLegacyWorkflowRunTarget(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	work := []beads.Bead{{
		ID:       "legacy-workflow-root",
		Status:   "in_progress",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": "worker",
		},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", nil, nil, work, []string{""})

	if len(got) != 1 || got[0].ID != "legacy-workflow-root" {
		t.Fatalf("filtered work = %#v, want legacy workflow root preserved through run_target fallback", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsPersistedBoundRoute(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity-packs")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "gascity-packs", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "implementation-worker",
			Dir:  "gascity-packs",
		}},
	}
	sessionName := "gc__implementation-worker-mc-xbvk5"
	sessions := []beads.Bead{{
		ID:     "mc-xbvk5",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "gascity-packs/gc.implementation-worker",
			"session_name": sessionName,
		},
	}}
	work := []beads.Bead{{
		ID:       "gp-qx0o",
		Status:   "in_progress",
		Assignee: sessionName,
		Metadata: map[string]string{
			"gc.routed_to": "gascity-packs/gc.implementation-worker",
		},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, nil, sessionInfosFromBeads(sessions), work, []string{"gascity-packs"})

	if len(got) != 1 || got[0].ID != "gp-qx0o" {
		t.Fatalf("filtered work = %#v, want persisted bound route preserved", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandNormalizesInstanceSuffixedRouteTarget(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MinActiveSessions: intPtr(1),
			MaxActiveSessions: intPtr(3),
		}},
	}
	work := []beads.Bead{{
		ID:       "instance-routed",
		Status:   "in_progress",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.routed_to": "worker-1",
		},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", nil, nil, work, []string{""})

	if len(got) != 1 || got[0].ID != "instance-routed" {
		t.Fatalf("filtered work = %#v, want instance-suffixed route target normalized to the base template and kept", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandLeavesUnmatchedInstanceSuffixAlone(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MinActiveSessions: intPtr(1),
			MaxActiveSessions: intPtr(3),
		}},
	}
	work := []beads.Bead{{
		ID:       "out-of-range-routed",
		Status:   "in_progress",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.routed_to": "worker-99",
		},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", nil, nil, work, []string{""})

	if len(got) != 0 {
		t.Fatalf("filtered work = %#v, want out-of-range instance suffix left unmatched and dropped", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandDropsDeferredRoutedBead(t *testing.T) {
	// A deferred bead retaining a stale gc.routed_to must not count as pool
	// demand. bd ready (and so scale_check) already hides it; the raw
	// List(status=open) pass this filter draws from does not. Without the
	// deferred exclusion it drives poolDesired=1 with no ready work behind it.
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	future := time.Now().UTC().Add(720 * time.Hour)
	work := []beads.Bead{
		{
			ID:       "deferred-routed-anchor",
			Status:   "open",
			Assignee: "worker-dead",
			Metadata: map[string]string{
				"gc.routed_to": "worker",
			},
			DeferUntil: &future,
		},
		{
			ID:       "live-routed-work",
			Status:   "in_progress",
			Assignee: "worker-dead",
			Metadata: map[string]string{
				"gc.routed_to": "worker",
			},
		},
	}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", nil, nil, work, []string{"", ""})

	if len(got) != 1 || got[0].ID != "live-routed-work" {
		t.Fatalf("filtered work = %#v, want only live-routed-work (deferred anchor dropped)", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsElapsedDeferRoutedBead(t *testing.T) {
	// A defer_until in the past is elapsed — the bead is ready again and must
	// still count as demand. Only a FUTURE defer_until parks it.
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	past := time.Now().UTC().Add(-1 * time.Hour)
	work := []beads.Bead{{
		ID:       "elapsed-defer-work",
		Status:   "open",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.routed_to": "worker",
		},
		DeferUntil: &past,
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", nil, nil, work, []string{""})

	if len(got) != 1 || got[0].ID != "elapsed-defer-work" {
		t.Fatalf("filtered work = %#v, want elapsed-defer bead preserved as demand", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandDropsDirectAssigneeFromUnreachableStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-session",
		},
	}}
	work := []beads.Bead{{
		ID:       "rig-direct-assigned",
		Status:   "in_progress",
		Assignee: "session-1",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, nil, sessionInfosFromBeads(sessions), work, []string{"riga"})

	if len(got) != 0 {
		t.Fatalf("filtered work = %#v, want unreachable rig-store direct assignment dropped", got)
	}
}

func TestSessionHasOpenAssignedWorkUsesOnlyReachableStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-session",
		},
	}
	if _, err := cityStore.Create(beads.Bead{
		ID:       "city-work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create city work: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, sessiontest.SeedBead(t, session))
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if has {
		t.Fatal("city-store assigned work should not count for a rig-scoped session")
	}

	if _, err := rigStore.Create(beads.Bead{
		ID:       "rig-work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create rig work: %v", err)
	}
	has, err = sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, sessiontest.SeedBead(t, session))
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("rig-store assigned work should count for a rig-scoped session")
	}
}

// TestSessionAssignedWorkGuardsFederateForCityScopedSession is the cross-store
// regression for the reconciler guard path. A city-scoped (cross-store-eligible)
// session legitimately owns rig-store-routed work (vp-kvp), so every reachable-store
// guard the drain/close/recycle/stranded paths consult — the open-work check, the
// awake check, the stranded-bead lookup, and the stranded-work collector — must
// federate across the city store AND every rig store for it, exactly like
// openSessionReachableStoreRef's cross-store wildcard. Before the fix these guards
// resolved the city-scoped session to a single configured store and missed its
// rig-store work, so a live holder could be closed/drained/recycled or
// under-reported (#3453 re-regression). Rig-scoped sessions stay single-store
// (covered by TestSessionHasOpenAssignedWorkUsesOnlyReachableStore).
func TestSessionAssignedWorkGuardsFederateForCityScopedSession(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:  "auditor",
			Scope: "city",
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"riga": rigStore}
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "auditor",
			"session_name": "auditor-session",
		},
	}
	// Work lives ONLY in the rig store, assigned to the city-scoped session.
	rigWork, err := rigStore.Create(beads.Bead{
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create rig work: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(rigWork.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark rig work in progress: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, rigStores, sessiontest.SeedBead(t, session))
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("city-scoped session must see its rig-store work across stores (close/drain guard)")
	}

	awake, err := sessionHasAwakeAssignedWorkForReachableStore(cityPath, cfg, cityStore, rigStores, sessiontest.SeedBead(t, session))
	if err != nil {
		t.Fatalf("sessionHasAwakeAssignedWorkForReachableStore: %v", err)
	}
	if !awake {
		t.Fatal("city-scoped session's in-progress rig-store work must keep it awake (recycle guard)")
	}

	bead, found, err := firstOpenAssignedWorkBeadForReachableStore(cityPath, cfg, cityStore, rigStores, sessiontest.SeedBead(t, session))
	if err != nil {
		t.Fatalf("firstOpenAssignedWorkBeadForReachableStore: %v", err)
	}
	if !found || bead.ID != rigWork.ID {
		t.Fatalf("stranded-bead lookup must find rig-store work for a city-scoped session; found=%v bead=%q want=%q", found, bead.ID, rigWork.ID)
	}

	stranded, err := collectSessionAssignedWorkInfo(cityPath, cfg, cityStore, rigStores, sessiontest.SeedBead(t, session))
	if err != nil {
		t.Fatalf("collectSessionAssignedWorkInfo: %v", err)
	}
	if len(stranded) != 1 || stranded[0].bead.ID != rigWork.ID {
		t.Fatalf("stranded-work collector must include rig-store work for a city-scoped session; got %#v", stranded)
	}
}

func TestSessionHasOpenAssignedWorkMatchesConfiguredNamedSessionRuntimeFallback(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "worker",
			BindingName: "pack",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "worker",
			BindingName: "pack",
			Mode:        "on_demand",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "pack.worker")
	store := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":                   "pack.worker",
			"session_name":               sessionName,
			namedSessionMetadataKey:      "true",
			namedSessionModeMetadata:     "on_demand",
			namedSessionIdentityMetadata: "",
		},
	}
	if _, err := store.Create(beads.Bead{
		ID:       "named-work",
		Type:     "task",
		Status:   "open",
		Assignee: "pack.worker",
	}); err != nil {
		t.Fatalf("Create named work: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore("", cfg, store, nil, sessiontest.SeedBead(t, session))
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("configured named-session runtime-name fallback assignment should count as open assigned work")
	}
}

func TestSessionAssignmentIdentifiersForConfigConfiguredNamedSessionFallbackIsConservative(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "worker",
			BindingName: "pack",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "worker",
			BindingName: "pack",
			Mode:        "on_demand",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "pack.worker")

	tests := []struct {
		name    string
		session beads.Bead
	}{
		{
			name: "identity metadata already present",
			session: beads.Bead{
				ID: "session-with-identity",
				Metadata: map[string]string{
					"template":                   "pack.worker",
					"session_name":               sessionName,
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "pack.other",
				},
			},
		},
		{
			name: "template mismatch",
			session: beads.Bead{
				ID: "session-template-mismatch",
				Metadata: map[string]string{
					"template":                   "pack.other",
					"session_name":               sessionName,
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "",
				},
			},
		},
		{
			name: "runtime name mismatch",
			session: beads.Bead{
				ID: "session-runtime-mismatch",
				Metadata: map[string]string{
					"template":                   "pack.worker",
					"session_name":               "different-session",
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, identifier := range sessionAssignmentIdentifiersForConfig(tt.session, cfg) {
				if identifier == "pack.worker" {
					t.Fatalf("identifiers include configured identity %q for conservative mismatch case: %v", identifier, sessionAssignmentIdentifiersForConfig(tt.session, cfg))
				}
			}
		})
	}
}

func TestAgentReachesWorkflowStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "alpha")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "alpha", Path: rigPath}},
	}
	hqAgent := &config.Agent{Name: "mayor"}
	rigAgent := &config.Agent{Name: "polecat", Dir: "alpha"}

	cases := []struct {
		name     string
		storeRef string
		agent    *config.Agent
		want     bool
	}{
		{name: "hq agent reaches city store", storeRef: "city:test-city", agent: hqAgent, want: true},
		{name: "hq agent cannot reach rig store", storeRef: "rig:alpha", agent: hqAgent, want: false},
		{name: "rig agent reaches own rig store", storeRef: "rig:alpha", agent: rigAgent, want: true},
		{name: "rig agent cannot reach city store", storeRef: "city:test-city", agent: rigAgent, want: false},
		{name: "rig agent cannot reach a different rig", storeRef: "rig:beta", agent: rigAgent, want: false},
		{name: "empty storeRef is unreachable for rig agent", storeRef: "", agent: rigAgent, want: false},
		{name: "empty storeRef is unreachable for hq agent", storeRef: "", agent: hqAgent, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentutil.AgentReachesWorkflowStore(tc.storeRef, tc.agent, cityPath, cfg); got != tc.want {
				t.Fatalf("AgentReachesWorkflowStore(%q, %q) = %v, want %v", tc.storeRef, tc.agent.Name, got, tc.want)
			}
		})
	}

	if !agentutil.AgentReachesWorkflowStore("city:test-city", nil, cityPath, cfg) {
		t.Fatal("nil agent should permissively reach any store")
	}
	if !agentutil.AgentReachesWorkflowStore("rig:alpha", rigAgent, cityPath, nil) {
		t.Fatal("nil cfg should permissively reach any store")
	}
}

func TestAgentReachableStoreLabel(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "alpha")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "alpha", Path: rigPath}},
	}
	hqAgent := &config.Agent{Name: "mayor"}
	rigAgent := &config.Agent{Name: "polecat", Dir: "alpha"}

	if got := agentutil.AgentReachableStoreLabel(hqAgent, cityPath, "test-city", cfg); got != "city:test-city" {
		t.Errorf("hq agent label = %q, want city:test-city", got)
	}
	if got := agentutil.AgentReachableStoreLabel(rigAgent, cityPath, "test-city", cfg); got != "rig:alpha" {
		t.Errorf("rig agent label = %q, want rig:alpha", got)
	}
	if got := agentutil.AgentReachableStoreLabel(hqAgent, cityPath, "", cfg); got != "city:city" {
		t.Errorf("hq agent label with empty cityName = %q, want city:city", got)
	}
	if got := agentutil.AgentReachableStoreLabel(nil, cityPath, "test-city", cfg); got != "" {
		t.Errorf("nil agent label = %q, want empty", got)
	}
	if got := agentutil.AgentReachableStoreLabel(hqAgent, cityPath, "test-city", nil); got != "" {
		t.Errorf("nil cfg label = %q, want empty", got)
	}
}

func TestSessionHasOpenAssignedWorkIncludesReachableAssignedWisp(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-session",
		},
	}
	wisp, err := rigStore.Create(beads.Bead{
		ID:        "rig-wisp-work",
		Title:     "active workflow step",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  session.Metadata["session_name"],
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create rig wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark rig wisp in progress: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, sessiontest.SeedBead(t, session))
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("reachable assigned wisp work should count before closing a session")
	}
}

func TestFirstOpenAssignedWorkBeadIncludesAssignedWisp(t *testing.T) {
	store := beads.NewMemStore()
	wisp, err := store.Create(beads.Bead{
		Title:     "active workflow step",
		Type:      "task",
		Assignee:  "worker-session",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark wisp in progress: %v", err)
	}

	got, found, err := firstOpenAssignedWorkBeadInStoreByIdentifiers(store, []string{"worker-session"})
	if err != nil {
		t.Fatalf("firstOpenAssignedWorkBeadInStoreByIdentifiers: %v", err)
	}
	if !found {
		t.Fatal("assigned wisp work should be found for session diagnostics")
	}
	if got.ID != wisp.ID {
		t.Fatalf("first assigned work ID = %q, want %q", got.ID, wisp.ID)
	}
}

func TestResolveTaskWorkDirIncludesAssignedWisp(t *testing.T) {
	workDir := t.TempDir()
	store := beads.NewMemStore()
	wisp, err := store.Create(beads.Bead{
		Title:     "active workflow step",
		Type:      "task",
		Assignee:  "worker-session",
		Metadata:  map[string]string{"work_dir": workDir},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark wisp in progress: %v", err)
	}

	if got := resolveTaskWorkDir("", store, "worker-session"); got != workDir {
		t.Fatalf("resolveTaskWorkDir = %q, want assigned wisp work_dir %q", got, workDir)
	}
}

func TestResolveTaskWorkDirPrefersPreparedDrainSourceAnchor(t *testing.T) {
	sourceWorkDir := t.TempDir()
	launcherWorkDir := t.TempDir()
	store := beads.NewMemStore()
	source, err := store.Create(beads.Bead{
		Title: "implementation source anchor",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.LegacyWorkDirMetadataKey: sourceWorkDir,
		},
	})
	if err != nil {
		t.Fatalf("Create source anchor: %v", err)
	}
	root, err := store.Create(beads.Bead{
		Title: "drain item workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.DrainMemberIDMetadataKey: source.ID,
			beadmeta.LegacyWorkDirMetadataKey: launcherWorkDir,
		},
	})
	if err != nil {
		t.Fatalf("Create item root: %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title:    "implementation step",
		Type:     "task",
		Assignee: "worker-session",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:    root.ID,
			beadmeta.WorkDirMetadataKey:       launcherWorkDir,
			beadmeta.LegacyWorkDirMetadataKey: launcherWorkDir,
		},
	})
	if err != nil {
		t.Fatalf("Create implementation step: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(step.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark implementation step in progress: %v", err)
	}

	if got := resolveTaskWorkDir("", store, "worker-session"); got != sourceWorkDir {
		t.Fatalf("resolveTaskWorkDir = %q, want prepared source work dir %q", got, sourceWorkDir)
	}
}

// TestResolveTaskWorkDirPrefersCreatorWorkDirOverStampedCanonical pins the
// key precedence for non-drain beads: legacy `work_dir` is written by the
// worktree creator, while `gc.work_dir` is an observability stamp
// reconciliation mirrors from an observed cwd. The creator's record wins.
func TestResolveTaskWorkDirPrefersCreatorWorkDirOverStampedCanonical(t *testing.T) {
	creatorDir := t.TempDir()
	observedDir := t.TempDir()
	store := beads.NewMemStore()
	task, err := store.Create(beads.Bead{
		Title:    "assigned task",
		Type:     "task",
		Assignee: "worker-session",
		Metadata: map[string]string{
			beadmeta.LegacyWorkDirMetadataKey: creatorDir,
			beadmeta.WorkDirMetadataKey:       observedDir,
		},
	})
	if err != nil {
		t.Fatalf("Create assigned task: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(task.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark assigned task in progress: %v", err)
	}

	if got := resolveTaskWorkDir("", store, "worker-session"); got != creatorDir {
		t.Fatalf("resolveTaskWorkDir = %q, want creator work_dir %q (not stamped %q)", got, creatorDir, observedDir)
	}
}
