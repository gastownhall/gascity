package main

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/poolplan"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

func freshSlotTestInfo(id, template, agentName string, slot int) sessionpkg.Info {
	return sessionpkg.Info{
		ID:                  id,
		Template:            template,
		AgentName:           agentName,
		PoolSlot:            strconv.Itoa(slot),
		PoolManaged:         true,
		MetadataState:       string(sessionpkg.StateAwake),
		SessionNameMetadata: agentName + "-runtime",
	}
}

func freshSlotTestParams(cfg *config.City, infos ...sessionpkg.Info) *agentBuildParams {
	return &agentBuildParams{
		city:                             cfg,
		agents:                           cfg.Agents,
		beadStore:                        beads.NewMemStore(),
		sessionBeads:                     newSessionBeadSnapshotFromInfos(infos),
		sessionOccupancyInfos:            infos,
		sessionSnapshotComplete:          true,
		sessionSnapshotCompletenessKnown: true,
	}
}

func TestClaimFreshPoolSlotInfoCapacityMatrix(t *testing.T) {
	max0, max1, max2, max3, max5, unlimited := 0, 1, 2, 3, 5, -1
	tests := []struct {
		name      string
		agent     config.Agent
		workspace *int
		infos     []sessionpkg.Info
		used      map[int]bool
		want      int
		wantErr   bool
	}{
		{
			name:    "max zero disables even namepool",
			agent:   config.Agent{Name: "worker", MaxActiveSessions: &max0, NamepoolNames: []string{"ada"}},
			wantErr: true,
		},
		{
			name:  "canonical singleton uses slot zero",
			agent: config.Agent{Name: "worker", MaxActiveSessions: &max1},
			want:  0,
		},
		{
			name:  "positive max bounds numbered identity",
			agent: config.Agent{Name: "worker", MaxActiveSessions: &max2},
			used:  map[int]bool{1: true},
			want:  2,
		},
		{
			name:    "positive max exhausts",
			agent:   config.Agent{Name: "worker", MaxActiveSessions: &max2},
			used:    map[int]bool{1: true, 2: true},
			wantErr: true,
		},
		{
			name:  "namepool is lower bound",
			agent: config.Agent{Name: "worker", MaxActiveSessions: &max5, NamepoolNames: []string{"ada", "grace"}},
			used:  map[int]bool{1: true},
			want:  2,
		},
		{
			name:    "namepool exhausts before larger max",
			agent:   config.Agent{Name: "worker", MaxActiveSessions: &max5, NamepoolNames: []string{"ada", "grace"}},
			used:    map[int]bool{1: true, 2: true},
			wantErr: true,
		},
		{
			name:  "nil max is unlimited",
			agent: config.Agent{Name: "worker"},
			used:  map[int]bool{1: true, 2: true},
			want:  3,
		},
		{
			name:  "negative max is unlimited",
			agent: config.Agent{Name: "worker", MaxActiveSessions: &unlimited},
			used:  map[int]bool{1: true, 2: true},
			want:  3,
		},
		{
			name:      "workspace cap does not shape agent identity",
			agent:     config.Agent{Name: "worker"},
			workspace: &max1,
			used:      map[int]bool{1: true},
			want:      2,
		},
		{
			name:  "cap shrink ignores holder above current bound",
			agent: config.Agent{Name: "worker", MaxActiveSessions: &max2},
			infos: []sessionpkg.Info{freshSlotTestInfo("slot-3", "worker", "worker-3", 3)},
			used:  map[int]bool{1: true},
			want:  2,
		},
		{
			name:  "namepool bounds unlimited max",
			agent: config.Agent{Name: "worker", MaxActiveSessions: &unlimited, NamepoolNames: []string{"ada", "grace", "lin"}},
			used:  map[int]bool{1: true, 2: true},
			want:  3,
		},
		{
			name:  "positive bound retains lowest gap",
			agent: config.Agent{Name: "worker", MaxActiveSessions: &max3},
			used:  map[int]bool{1: true, 3: true},
			want:  2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Workspace: config.Workspace{MaxActiveSessions: tc.workspace}, Agents: []config.Agent{tc.agent}}
			bp := freshSlotTestParams(cfg, tc.infos...)
			used := make(map[int]bool, len(tc.used))
			for slot, value := range tc.used {
				used[slot] = value
			}
			got, err := claimFreshPoolSlotInfo(bp, &cfg.Agents[0], used)
			if tc.wantErr {
				if !errors.Is(err, errPoolSessionNameUnavailable) {
					t.Fatalf("claimFreshPoolSlotInfo error = %v, want errPoolSessionNameUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("claimFreshPoolSlotInfo: %v", err)
			}
			if got != tc.want {
				t.Fatalf("slot = %d, want %d", got, tc.want)
			}
			if got > 0 && !used[got] {
				t.Fatalf("selected slot %d not published to shared usedSlots", got)
			}
		})
	}
}

func TestClaimFreshPoolSlotInfoHolderAttributionAndLowestGap(t *testing.T) {
	maxSessions := 5
	agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
	cfg := &config.City{Agents: []config.Agent{agent}}

	t.Run("unrelated stray pool slot is ignored", func(t *testing.T) {
		stray := freshSlotTestInfo("stray", "other", "other-1", 1)
		got, err := claimFreshPoolSlotInfo(freshSlotTestParams(cfg, stray), &cfg.Agents[0], map[int]bool{})
		if err != nil || got != 1 {
			t.Fatalf("slot = %d, err = %v, want free slot 1", got, err)
		}
	})

	t.Run("foreign row holding exact concrete identity reserves it", func(t *testing.T) {
		foreign := freshSlotTestInfo("foreign", "other", "worker-1", 1)
		got, err := claimFreshPoolSlotInfo(freshSlotTestParams(cfg, foreign), &cfg.Agents[0], map[int]bool{})
		if err != nil || got != 2 {
			t.Fatalf("slot = %d, err = %v, want slot 2 after exact foreign holder", got, err)
		}
	})

	t.Run("legacy template alias and session-name holders reserve slots", func(t *testing.T) {
		legacyTemplate := freshSlotTestInfo("legacy-template", "worker", "", 1)
		legacyAlias := sessionpkg.Info{ID: "legacy-alias", Template: "other", Alias: "worker-2", PoolManaged: true, MetadataState: "awake", SessionNameMetadata: "legacy-alias-runtime"}
		legacySessionName := sessionpkg.Info{ID: "legacy-name", Template: "worker", PoolManaged: true, MetadataState: "awake", SessionNameMetadata: "worker-3"}
		got, err := claimFreshPoolSlotInfo(freshSlotTestParams(cfg, legacyTemplate, legacyAlias, legacySessionName), &cfg.Agents[0], map[int]bool{})
		if err != nil || got != 4 {
			t.Fatalf("slot = %d, err = %v, want slot 4 after legacy holders", got, err)
		}
	})

	t.Run("wait hold held and quarantine occupy with deterministic gap", func(t *testing.T) {
		future := time.Now().Add(time.Hour).Format(time.RFC3339)
		wait := freshSlotTestInfo("wait", "worker", "worker-1", 1)
		wait.WaitHold = "dependency"
		held := freshSlotTestInfo("held", "worker", "worker-2", 2)
		held.HeldUntil = future
		quarantined := freshSlotTestInfo("quarantine", "worker", "worker-4", 4)
		quarantined.QuarantinedUntil = future
		used := map[int]bool{}
		bp := freshSlotTestParams(cfg, wait, held, quarantined)
		got, err := claimFreshPoolSlotInfo(bp, &cfg.Agents[0], used)
		if err != nil || got != 3 {
			t.Fatalf("first slot = %d, err = %v, want lowest gap 3", got, err)
		}
		got, err = claimFreshPoolSlotInfo(bp, &cfg.Agents[0], used)
		if err != nil || got != 5 {
			t.Fatalf("second slot = %d, err = %v, want 5", got, err)
		}
		if _, err := claimFreshPoolSlotInfo(bp, &cfg.Agents[0], used); !errors.Is(err, errPoolSessionNameUnavailable) {
			t.Fatalf("exhaustion error = %v, want errPoolSessionNameUnavailable", err)
		}
	})
}

func TestCanonicalSingletonFreshSelectionIsUnique(t *testing.T) {
	maxSessions := 1
	agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
	cfg := &config.City{Agents: []config.Agent{agent}}
	bp := freshSlotTestParams(cfg)
	usedSlots := map[int]bool{}

	first, err := claimFreshPoolSlotInfo(bp, &cfg.Agents[0], usedSlots)
	if err != nil || first != 0 || !usedSlots[0] {
		t.Fatalf("first canonical selection = %d used=%#v err=%v, want reserved slot 0", first, usedSlots, err)
	}
	if _, err := claimFreshPoolSlotInfo(bp, &cfg.Agents[0], usedSlots); !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("second canonical selection error = %v, want unavailable", err)
	}

	holder := freshSlotTestInfo("canonical-holder", "worker", "worker", 0)
	holder.PoolSlot = ""
	holder.SessionNameMetadata = "worker"
	bp = freshSlotTestParams(cfg, holder)
	if _, err := claimFreshPoolSlotInfo(bp, &cfg.Agents[0], map[int]bool{}); !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("canonical holder error = %v, want unavailable", err)
	}
}

func TestCanonicalSingletonCannotPlanTwoFreshRows(t *testing.T) {
	maxSessions := 1
	agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
	cfg := &config.City{Agents: []config.Agent{agent}}
	bp := freshSlotTestParams(cfg)
	usedSlots := map[int]bool{}
	used := map[string]bool{}

	_, _, first, err := selectOrPlanPoolSessionBead(bp, &cfg.Agents[0], "worker", nil, SessionRequest{Template: "worker", Tier: "new"}, time.Now(), used, usedSlots)
	if err != nil || first == nil || first.slot != 0 {
		t.Fatalf("first canonical plan = %#v err=%v, want slot-0 plan", first, err)
	}
	_, _, second, err := selectOrPlanPoolSessionBead(bp, &cfg.Agents[0], "worker", nil, SessionRequest{Template: "worker", Tier: "new"}, time.Now(), used, usedSlots)
	if !errors.Is(err, errPoolSessionNameUnavailable) || second != nil {
		t.Fatalf("second canonical plan = %#v err=%v, want unavailable", second, err)
	}
}

func TestFreshSlotIdentityIgnoresWorkspaceAndRigCaps(t *testing.T) {
	inheritedCap := 1
	agent := config.Agent{Name: "worker", Dir: "rig-a"}
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &inheritedCap},
		Rigs:      []config.Rig{{Name: "rig-a", MaxActiveSessions: &inheritedCap}},
		Agents:    []config.Agent{agent},
	}
	got, err := claimFreshPoolSlotInfo(freshSlotTestParams(cfg), &cfg.Agents[0], map[int]bool{1: true})
	if err != nil || got != 2 {
		t.Fatalf("slot = %d, err = %v, want slot 2 despite inherited nested caps", got, err)
	}
}

func TestSessionOccupancyInfosForRefreshPreservesCrossStoreOccupancy(t *testing.T) {
	primary := freshSlotTestInfo("primary", "worker", "worker-1", 1)
	foreign := freshSlotTestInfo("foreign", "worker", "worker-2", 2)
	result := DesiredStateResult{
		SessionSnapshotComplete: true,
		SessionOccupancyInfos:   []sessionpkg.Info{primary, foreign},
	}
	infos := sessionOccupancyInfosForRefresh(result, newSessionBeadSnapshotFromInfos([]sessionpkg.Info{primary}))
	if len(infos) != 2 || infos[0].ID != primary.ID || infos[1].ID != foreign.ID {
		t.Fatalf("refresh occupancy = %#v, want original cross-store rows", infos)
	}
	infos[0].ID = "mutated"
	if result.SessionOccupancyInfos[0].ID != primary.ID {
		t.Fatal("refresh occupancy aliases DesiredStateResult storage")
	}

	maxSessions := 5
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: &maxSessions}}}
	createdThisBuild := freshSlotTestInfo("created", "worker", "worker-3", 3)
	bp := freshSlotTestParams(cfg, primary, createdThisBuild)
	bp.sessionOccupancyInfos = []sessionpkg.Info{primary, foreign}
	got, err := claimFreshPoolSlotInfo(bp, &cfg.Agents[0], map[int]bool{})
	if err != nil || got != 4 {
		t.Fatalf("unioned refresh/current occupancy slot = %d err=%v, want 4", got, err)
	}
}

func TestSessionOccupancyInfosForRefreshBlocksFreshCreatesWithoutFullRecensus(t *testing.T) {
	result := DesiredStateResult{
		SessionSnapshotComplete: true,
		SessionOccupancyInfos:   []sessionpkg.Info{},
	}
	for _, tc := range []struct {
		name     string
		snapshot *sessionBeadSnapshot
	}{
		{name: "healthy primary still lacks foreign-leg recensus", snapshot: newSessionBeadSnapshotFromInfos(nil)},
		{name: "degraded primary", snapshot: newSessionBeadSnapshotWithError(errors.New("refresh scan failed"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			infos := sessionOccupancyInfosForRefresh(result, tc.snapshot)
			if infos == nil || len(infos) != 0 {
				t.Fatalf("preserved cross-store census = %#v, want explicit complete-empty evidence", infos)
			}

			maxSessions := 2
			agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
			cfg := &config.City{Agents: []config.Agent{agent}}
			store := beads.NewMemStore()
			bp := &agentBuildParams{
				city:                             cfg,
				cityPath:                         t.TempDir(),
				agents:                           cfg.Agents,
				beadStore:                        store,
				sessionBeads:                     tc.snapshot,
				sessionOccupancyInfos:            infos,
				sessionSnapshotCompletenessKnown: true,
				sessionSnapshotComplete:          false,
			}
			_, _, plan, err := selectOrPlanPoolSessionBead(bp, &cfg.Agents[0], "worker", nil, SessionRequest{Template: "worker", Tier: "new"}, time.Now(), map[string]bool{}, map[int]bool{})
			if !errors.Is(err, errPoolSessionCreatePartial) || plan != nil {
				t.Fatalf("ordinary fresh plan = %#v err=%v, want partial-census refusal", plan, err)
			}
			if _, _, err := selectOrCreateDependencyPoolSessionBeadWithSlot(bp, &cfg.Agents[0], "worker"); !errors.Is(err, errPoolSessionCreatePartial) {
				t.Fatalf("dependency fresh create error = %v, want partial-census refusal", err)
			}
			rows, err := store.ListByLabel(sessionBeadLabel, 0)
			if err != nil || len(rows) != 0 {
				t.Fatalf("refresh created session rows: rows=%#v err=%v", rows, err)
			}
		})
	}
}

func TestSelectOrPlanPoolSessionBeadFreshUsesLocalHolderUnion(t *testing.T) {
	maxSessions := 3
	agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
	cfg := &config.City{Agents: []config.Agent{agent}}
	holder := freshSlotTestInfo("holder-1", "worker", "worker-1", 1)
	holder.HeldUntil = time.Now().Add(time.Hour).Format(time.RFC3339)
	bp := freshSlotTestParams(cfg, holder)
	usedSlots := map[int]bool{}
	used := map[string]bool{}
	decisionTime := time.Now()

	_, _, plan, err := selectOrPlanPoolSessionBead(bp, &cfg.Agents[0], "worker", nil, SessionRequest{Template: "worker", Tier: "new", BeadPriority: 10}, decisionTime, used, usedSlots)
	if err != nil {
		t.Fatalf("anonymous fresh plan: %v", err)
	}
	if plan == nil || plan.slot != 2 {
		t.Fatalf("anonymous plan = %#v, want slot 2", plan)
	}
	if usedSlots[1] || !usedSlots[2] || len(usedSlots) != 1 {
		t.Fatalf("shared usedSlots = %#v, want only newly selected slot 2", usedSlots)
	}

	got, slot, nextPlan, err := selectOrPlanPoolSessionBead(bp, &cfg.Agents[0], "worker", &holder, SessionRequest{Template: "worker", Tier: "resume", SessionBeadID: holder.ID, BeadPriority: 1}, decisionTime, used, usedSlots)
	if err != nil {
		t.Fatalf("concrete resume: %v", err)
	}
	if nextPlan != nil || got.ID != holder.ID || slot != 1 {
		t.Fatalf("resume = info %q slot %d plan %#v, want holder slot 1 reuse", got.ID, slot, nextPlan)
	}
}

func TestIncompleteSnapshotStillReusesProvenPoolRow(t *testing.T) {
	maxSessions := 3
	agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
	cfg := &config.City{Agents: []config.Agent{agent}}
	proven := freshSlotTestInfo("proven-1", "worker", "worker-1", 1)
	bp := freshSlotTestParams(cfg, proven)
	bp.sessionSnapshotComplete = false

	got, slot, plan, err := selectOrPlanPoolSessionBead(bp, &cfg.Agents[0], "worker", nil, SessionRequest{Template: "worker", Tier: "new"}, time.Now(), map[string]bool{}, map[int]bool{})
	if err != nil {
		t.Fatalf("proven reuse under partial snapshot: %v", err)
	}
	if plan != nil || got.ID != proven.ID || slot != 1 {
		t.Fatalf("reuse = info %q slot %d plan %#v, want proven slot-1 row", got.ID, slot, plan)
	}
}

func TestFreshPoolPlanningRequiresCompleteSessionSnapshot(t *testing.T) {
	maxSessions := 3
	agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
	cfg := &config.City{Agents: []config.Agent{agent}}
	tests := []struct {
		name     string
		snapshot *sessionBeadSnapshot
		known    bool
		complete bool
	}{
		{name: "nil snapshot", snapshot: nil},
		{name: "snapshot load error", snapshot: newSessionBeadSnapshotWithError(errors.New("scan failed"))},
		{name: "known partial census", snapshot: newSessionBeadSnapshotFromInfos(nil), known: true, complete: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			budget := poolplan.NewCreateBudget(1)
			bp := &agentBuildParams{
				city:                             cfg,
				agents:                           cfg.Agents,
				beadStore:                        beads.NewMemStore(),
				sessionBeads:                     tc.snapshot,
				sessionSnapshotCompletenessKnown: tc.known,
				sessionSnapshotComplete:          tc.complete,
				poolSessionCreateBudget:          budget,
			}
			usedSlots := map[int]bool{3: true}
			_, _, plan, err := selectOrPlanPoolSessionBead(bp, &cfg.Agents[0], "worker", nil, SessionRequest{Template: "worker", Tier: "new"}, time.Now(), map[string]bool{}, usedSlots)
			if !errors.Is(err, errPoolSessionCreatePartial) || plan != nil {
				t.Fatalf("plan = %#v, error = %v, want no plan and errPoolSessionCreatePartial", plan, err)
			}
			if len(usedSlots) != 1 || !usedSlots[3] {
				t.Fatalf("usedSlots mutated before completeness gate: %#v", usedSlots)
			}
			if !budget.TryClaim("worker") {
				t.Fatal("fresh-create budget was consumed before completeness gate")
			}
		})
	}
}

func TestDependencyFloorHeldSlotUsesNextSlotWithoutBudget(t *testing.T) {
	maxSessions := 3
	agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
	cfg := &config.City{Agents: []config.Agent{agent}}
	store := beads.NewMemStore()
	heldRaw, err := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker-1", "template:worker"},
		Metadata: map[string]string{
			"template":        "worker",
			"agent_name":      "worker-1",
			"pool_slot":       "1",
			"session_name":    "worker-1-pool",
			"state":           "awake",
			"pool_managed":    "true",
			"dependency_only": "true",
			"held_until":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	held := sessiontest.SeedBead(t, heldRaw)
	budget := poolplan.NewCreateBudget(1)
	bp := &agentBuildParams{
		city:                             cfg,
		cityPath:                         t.TempDir(),
		agents:                           cfg.Agents,
		beadStore:                        store,
		sessionBeads:                     newSessionBeadSnapshotFromInfos([]sessionpkg.Info{held}),
		sessionOccupancyInfos:            []sessionpkg.Info{held},
		sessionSnapshotCompletenessKnown: true,
		sessionSnapshotComplete:          true,
		poolSessionCreateBudget:          budget,
	}
	created, slot, err := selectOrCreateDependencyPoolSessionBeadWithSlot(bp, &cfg.Agents[0], "worker")
	if err != nil {
		t.Fatalf("dependency create: %v", err)
	}
	if slot != 2 || created.PoolSlot != "2" || created.AgentName != "worker-2" {
		t.Fatalf("dependency = slot %d info %+v, want worker-2 at slot 2", slot, created)
	}
	if !budget.TryClaim("worker") {
		t.Fatal("dependency-floor create consumed ordinary fresh-create budget")
	}
}

func TestDependencyFloorQuarantineAndWaitHoldUseNextSlot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{name: "quarantined", field: "quarantined_until", value: time.Now().Add(time.Hour).Format(time.RFC3339)},
		{name: "wait-held", field: "wait_hold", value: "dependency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			maxSessions := 3
			agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
			cfg := &config.City{Agents: []config.Agent{agent}}
			store := beads.NewMemStore()
			heldRaw, err := store.Create(beads.Bead{
				Title:  "worker-1",
				Type:   sessionBeadType,
				Status: "open",
				Labels: []string{sessionBeadLabel, "agent:worker-1", "template:worker"},
				Metadata: map[string]string{
					"template":        "worker",
					"agent_name":      "worker-1",
					"pool_slot":       "1",
					"session_name":    "worker-1-pool",
					"state":           "awake",
					"pool_managed":    "true",
					"dependency_only": "true",
					tc.field:          tc.value,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			held := sessiontest.SeedBead(t, heldRaw)
			bp := &agentBuildParams{
				city:                             cfg,
				cityPath:                         t.TempDir(),
				agents:                           cfg.Agents,
				beadStore:                        store,
				sessionBeads:                     newSessionBeadSnapshotFromInfos([]sessionpkg.Info{held}),
				sessionOccupancyInfos:            []sessionpkg.Info{held},
				sessionSnapshotCompletenessKnown: true,
				sessionSnapshotComplete:          true,
			}
			created, slot, err := selectOrCreateDependencyPoolSessionBeadWithSlot(bp, &cfg.Agents[0], "worker")
			if err != nil {
				t.Fatalf("dependency create: %v", err)
			}
			if slot != 2 || created.PoolSlot != "2" {
				t.Fatalf("dependency = slot %d info %+v, want slot 2", slot, created)
			}
		})
	}
}

func TestOpenFailedCreateRetriesStableSlotOnlyAfterClose(t *testing.T) {
	maxSessions := 2
	agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
	cfg := &config.City{Agents: []config.Agent{agent}}
	store := beads.NewMemStore()
	failedRaw, err := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   sessionBeadType,
		Status: "open", // Simulates the durable row left when Close itself fails.
		Labels: []string{sessionBeadLabel, "agent:worker-1", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"agent_name":   "worker-1",
			"pool_slot":    "1",
			"session_name": "worker-1-pool",
			"state":        string(sessionpkg.StateFailedCreate),
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := sessiontest.SeedBead(t, failedRaw)
	bp := &agentBuildParams{
		city:                             cfg,
		cityPath:                         t.TempDir(),
		agents:                           cfg.Agents,
		beadStore:                        store,
		sessionBeads:                     newSessionBeadSnapshotFromInfos([]sessionpkg.Info{failed}),
		sessionOccupancyInfos:            []sessionpkg.Info{failed},
		sessionSnapshotCompletenessKnown: true,
		sessionSnapshotComplete:          true,
	}
	if _, slot, err := selectOrCreateDependencyPoolSessionBeadWithSlot(bp, &cfg.Agents[0], "worker"); !errors.Is(err, errPoolSessionNameUnavailable) || slot != 1 {
		t.Fatalf("open failed-create retry = slot %d err %v, want stable slot 1 unavailable", slot, err)
	}
	rows, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("open failed-create minted replacement: rows=%d err=%v", len(rows), err)
	}

	closed := "closed"
	if err := store.Update(failedRaw.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close failed-create fixture: %v", err)
	}
	bp.sessionBeads = newSessionBeadSnapshotFromInfos(nil)
	bp.sessionOccupancyInfos = []sessionpkg.Info{}
	created, slot, err := selectOrCreateDependencyPoolSessionBeadWithSlot(bp, &cfg.Agents[0], "worker")
	if err != nil {
		t.Fatalf("retry after close: %v", err)
	}
	if slot != 1 || created.PoolSlot != "1" || created.SessionNameMetadata != "worker-1-pool" {
		t.Fatalf("retry after close = slot %d info %+v, want same slot/name", slot, created)
	}
}

func TestCrossStoreOpenFailedCreateBlocksStableSlotUntilClose(t *testing.T) {
	maxSessions := 2
	agent := config.Agent{Name: "worker", MaxActiveSessions: &maxSessions}
	cfg := &config.City{Agents: []config.Agent{agent}}
	store := beads.NewMemStore()
	failed := freshSlotTestInfo("foreign-failed", "worker", "worker-1", 1)
	failed.MetadataState = string(sessionpkg.StateFailedCreate)
	failed.SessionNameMetadata = "worker-1-pool"
	bp := &agentBuildParams{
		city:      cfg,
		cityPath:  t.TempDir(),
		agents:    cfg.Agents,
		beadStore: store,
		// The failed row deliberately exists only in the complete cross-store
		// census. It is absent from both the primary snapshot and primary store.
		sessionBeads:                     newSessionBeadSnapshotFromInfos(nil),
		sessionOccupancyInfos:            []sessionpkg.Info{failed},
		sessionSnapshotCompletenessKnown: true,
		sessionSnapshotComplete:          true,
	}

	if _, slot, err := selectOrCreateDependencyPoolSessionBeadWithSlot(bp, &cfg.Agents[0], "worker"); !errors.Is(err, errPoolSessionNameUnavailable) || slot != 1 {
		t.Fatalf("cross-store open failed-create retry = slot %d err %v, want stable slot 1 unavailable", slot, err)
	}
	rows, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("cross-store holder minted primary replacement: rows=%#v err=%v", rows, err)
	}
	if got := bp.sessionBeads.OpenInfos(); len(got) != 0 {
		t.Fatalf("blocked create wrote primary snapshot: %#v", got)
	}

	// A later complete census proves the failed row closed. The allocator retries
	// the same stable slot, the exact-name guard sees it free, and only then does
	// the primary store/snapshot receive the replacement.
	bp.sessionOccupancyInfos = []sessionpkg.Info{}
	created, slot, err := selectOrCreateDependencyPoolSessionBeadWithSlot(bp, &cfg.Agents[0], "worker")
	if err != nil {
		t.Fatalf("cross-store retry after close: %v", err)
	}
	if slot != 1 || created.PoolSlot != "1" || created.SessionNameMetadata != "worker-1-pool" {
		t.Fatalf("cross-store retry after close = slot %d info %+v, want same slot/name", slot, created)
	}
	rows, err = store.ListByLabel(sessionBeadLabel, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != created.ID {
		t.Fatalf("primary store rows after retry = %#v err=%v, want replacement %s", rows, err, created.ID)
	}
	if got := bp.sessionBeads.OpenInfos(); len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("primary writeback after retry = %#v, want replacement %s", got, created.ID)
	}
}
