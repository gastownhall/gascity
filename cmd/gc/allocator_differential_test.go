package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The allocation differential (P3-5c): legacy's desired state against the
// v2 decide, city by city. Each city's beads seed two stores. Legacy runs
// buildDesiredStateWithSessionBeadsAt on one, as its tick does, and writes
// its creates and bindings there; the v2 side runs gather over a primed
// cache of the other, census, demand and the K1 recording's scale_check
// included, then decideAllocation. They must agree on the accepted requests
// per template, the existing rows in desired, the identities planned for
// creation (pool slots and named sessions), and the partial retention.
//
// Legacy plans nothing in planOnly mode outside the decide (its template
// resolution and creates refuse there), so it runs with its effects on its
// own copy and the test reads what it wrote. The decide is planOnly by
// construction (allocator_plan.go).
//
// A difference is never whitelisted by shape. Each one the test accepts is
// listed below with its reason; a fixture names the aspects it shows and the
// entry that accounts for each, and an entry that no longer shows fails as
// loudly as an unaccounted mismatch. Every entry has a fixture.
//
// Not differences, so not listed (each was once proposed as one):
//   - control rows outside their root scope: legacy's default probe applies
//     the ownership rule since W-H1 (mc-zndi7.41, #7206), so both sides drop
//     them; the dispatcher fixture shows both counting only its own row;
//   - sticky bindings: B3 deleted them, and v5 AL1 (C6) recomputes bindings
//     every pass, as legacy rebinds every tick. Bindings are not compared
//     here; the smoke test checks one;
//   - WorkSet and its scaled: wake stage (POOL-071): legacy's build never
//     fills WorkSet and its demand snapshot forces it empty, so production
//     legacy has no WorkSet either;
//   - custom scale_check counts and partials (ruling 1): K1's run of
//     legacy's own evaluation, read through gather, agrees on its fixture.

// diffNow is the shared row helpers' clock (censusSession).
var diffNow = censusNow

// allocDifference is one difference the test accounts for.
type allocDifference struct {
	clause string
	reason string
}

// allocDifferences are the explained differences, by ID, each citing
// CONTRACT v5 (AL1 keeps v4.3 §2-§3 and §6 except as it notes).
var allocDifferences = map[string]allocDifference{
	"create-budget": {
		"v5 P4",
		"the decide plans every create; admission caps creates in flight at 8, where legacy's build stops fresh creates at max_wakes_per_tick per tick",
	},
	"suspended-rig": {
		"v5 AL1 (POOL-002; P3 spec V-D1)",
		"a template in a suspended rig gets no requests; legacy still gives it min-fill and resume requests that count as accepted",
	},
	"dependency-floor": {
		"v5 §12.1 PAR-DEP; v4.3 §3.1",
		"no dependency floors: depends_on refuses the latch, so v2 never plans legacy's dependency-only row",
	},
	"suspended-city-named": {
		"v5 AL1 (v4.3 C2.1)",
		"a suspended city's canonical named row stays InDesired and sleeps; legacy's build returns an empty desired state",
	},
	"stale-pending-create": {
		"v5 C3, arm A10 (P3 spec F8)",
		"an expired pending create with no runtime is None(rollback-candidate) and planned around now; legacy reuses it, then rolls it back",
	},
	"create-fence": {
		"v5 C1 (v4.3 C7.2; P3 spec F3)",
		"a plan for an identity a failed-create row holds is compared before the create effect's fenced re-census refuses it, where legacy's create refused the same identity in its tick; the fence backoff then moves v2 to the next slot",
	},
	"wisp-tier": {
		"v5 AL1",
		"the census reads beads.FederatedReadTier, so a wisp-tier session row is in v2's census; legacy's snapshot does not read that tier",
	},
}

// allocFindings are divergences the differential found that CONTRACT v5
// does not explain. They are pinned so the suite stays honest while the owner
// decides; an entry is deleted when its fix lands (the fixture then fails
// until its explain entry goes too). A finding is not an explanation.
var allocFindings = map[string]allocDifference{}

// diffCity is one fixture city: its config, the beads both stores start
// from, the runtime names alive on both sides, and the K1 scale_check run.
type diffCity struct {
	name  string
	cfg   *config.City
	beads []beads.Bead
	alive []string
	// scaleCheck publishes K1's scale_check run (ruling 1): legacy runs each
	// custom check in its shell (shellScaleCheck, which its build hard-wires:
	// one sh per check), and the lane's run answers each "echo X" check with
	// X, as the shell does.
	scaleCheck bool
	// explain maps each aspect this city differs on to its difference.
	explain map[string]string
}

// allocOutcome is one side's allocation, normalized for comparison.
type allocOutcome struct {
	accepted map[string]int  // template → accepted requests (zeros dropped)
	selected map[string]bool // pre-existing row IDs in desired
	creates  map[string]bool // "pool:<template>/<instance>#<slot>", "named:<identity>"
	retained map[string]bool // templates whose rows partial retention keeps
}

func newAllocOutcome() allocOutcome {
	return allocOutcome{accepted: map[string]int{}, selected: map[string]bool{}, creates: map[string]bool{}, retained: map[string]bool{}}
}

// mismatches lists the aspects on which legacy and v2 differ, each as
// "<aspect>:<key>" with both sides' values.
func (legacy allocOutcome) mismatches(v2 allocOutcome) map[string]string {
	out := map[string]string{}
	for _, t := range unionKeys(legacy.accepted, v2.accepted) {
		if legacy.accepted[t] != v2.accepted[t] {
			out["accepted:"+t] = fmt.Sprintf("legacy %d, v2 %d", legacy.accepted[t], v2.accepted[t])
		}
	}
	sets := []struct {
		aspect   string
		leg, v2s map[string]bool
	}{
		{"selected", legacy.selected, v2.selected},
		{"create", legacy.creates, v2.creates},
		{"retain", legacy.retained, v2.retained},
	}
	for _, s := range sets {
		for _, k := range unionKeys(s.leg, s.v2s) {
			if s.leg[k] != s.v2s[k] {
				out[s.aspect+":"+k] = fmt.Sprintf("legacy %t, v2 %t", s.leg[k], s.v2s[k])
			}
		}
	}
	return out
}

func unionKeys[V any](a, b map[string]V) []string {
	keys := maps.Keys(a)
	all := slices.Collect(keys)
	for k := range b {
		if _, ok := a[k]; !ok {
			all = append(all, k)
		}
	}
	slices.Sort(all)
	return all
}

// cloneDiffBeads deep-copies bs, so neither side's writes reach the other.
func cloneDiffBeads(bs []beads.Bead) []beads.Bead {
	out := make([]beads.Bead, len(bs))
	for i, b := range bs {
		b.Metadata = maps.Clone(b.Metadata)
		b.Labels = slices.Clone(b.Labels)
		out[i] = b
	}
	return out
}

func diffProvider(t *testing.T, alive []string) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	for _, name := range alive {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("fake start %s: %v", name, err)
		}
	}
	return sp
}

// legacyAllocation runs legacy's desired-state build over c.
func legacyAllocation(t *testing.T, c diffCity) allocOutcome {
	t.Helper()
	cityPath := t.TempDir()
	store := beads.NewMemStoreFrom(0, cloneDiffBeads(c.beads), nil)
	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatal(err)
	}
	existed := map[string]bool{}
	for _, info := range snap.OpenInfos() {
		existed[info.ID] = true
	}
	cfg := c.cfg
	result := buildDesiredStateWithSessionBeadsAt(cfg.Workspace.Name, cityPath, diffNow, diffNow, cfg, diffProvider(t, c.alive), store, nil, snap, nil, io.Discard)

	out := newAllocOutcome()
	// The reconciler's accepted counts: the demand snapshot's second
	// compute over the build's result (city_runtime.go, POOL-063).
	open := snap.OpenInfos()
	work := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, store, open, result.AssignedWorkBeads, result.AssignedWorkStoreRefs)
	counts := retainScaleCheckPartialPoolDesired(cfg, PoolDesiredCounts(ComputePoolDesiredStatesTracedAt(cfg, work, open, result.ScaleCheckCounts, diffNow, nil)),
		snap, effectivePoolPartialRetentionTemplates(result))
	if counts == nil {
		counts = map[string]int{}
	}
	mergeNamedSessionDemand(counts, result.NamedSessionDemand, cfg)
	for t, n := range counts {
		if n != 0 {
			out.accepted[t] = n
		}
	}
	after, err := sessionFrontDoor(store).ListAll(session.ListAllOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rowNames := map[string]bool{}
	for _, info := range after {
		if info.Closed {
			continue
		}
		rowNames[info.SessionNameMetadata] = true
		switch _, desired := result.State[info.SessionNameMetadata]; {
		case existed[info.ID] && desired:
			out.selected[info.ID] = true
		case !existed[info.ID]:
			out.creates[fmt.Sprintf("pool:%s/%s#%s", info.Template, info.AgentName, info.PoolSlot)] = true
		}
	}
	// A named session with no row is materialized by the session-bead sync
	// from its desired entry.
	for name, tp := range result.State {
		if !rowNames[name] && tp.ConfiguredNamedIdentity != "" {
			out.creates["named:"+tp.ConfiguredNamedIdentity] = true
		}
	}
	for t := range effectivePoolPartialRetentionTemplates(result) {
		out.retained[t] = true
	}
	return out
}

// v2Allocation gathers c's World as the planner does and runs the decide.
func v2Allocation(t *testing.T, c diffCity) allocOutcome {
	t.Helper()
	cache, _ := newDemandCache(t, true, cloneDiffBeads(c.beads)...)
	sp := diffProvider(t, c.alive)
	cityPath := t.TempDir()
	env := &reconcileEnv{Gen: 1, Cfg: c.cfg, SP: sp, ConfigRev: "rev-1"}
	clk := &clock.Fake{Time: diffNow}
	obs := NewObservationCache(clk, time.Minute, "e1")
	attrs := make(map[string]InventoryAttrs, len(c.alive))
	for _, name := range c.alive {
		attrs[name] = liveAttrs("")
	}
	obs.PublishInventory(InventoryPass{
		Epoch: "e1", Seq: 1, ProviderGen: 1, StartedAt: diffNow, FinishedAt: diffNow,
		MergedNames: c.alive, Backends: []BackendPass{completeBackend("", c.alive...)},
	}, attrs)
	rec := diffRecording(t, c, cityPath, cache)
	p := newPlanner(realPlannerClock{}, func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, io.Discard)
	w, err := gather(gatherEnv{
		CityPath: cityPath, CityName: c.cfg.Workspace.Name,
		Env:          func() *reconcileEnv { return env },
		Sessions:     func() beads.Store { return cache },
		RigStores:    func() map[string]beads.Store { return nil },
		Recording:    func() *externalReadsRecording { return rec },
		Observations: func() *ObservationCache { return obs },
		Episodes:     func() (map[string]session.StartupHealthEpisode, error) { return readStartupHealthEpisodes(cache) },
	}, p, diffNow)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	d := mustDecide(t, allocInputsOf(w, cityPath, c.cfg.Workspace.Name))

	out := newAllocOutcome()
	for t, n := range d.Snapshot.PoolDesired {
		if n != 0 {
			out.accepted[t] = n
		}
	}
	for k, e := range d.Snapshot.Entries {
		if e.InDesired {
			out.selected[k.ID] = true
		}
	}
	for _, plan := range d.Plans {
		switch plan.Kind {
		case createNamed:
			out.creates["named:"+plan.Named.Identity] = true
		default:
			slot := ""
			if plan.Plan.poolSlot > 0 {
				slot = fmt.Sprint(plan.Plan.poolSlot)
			}
			out.creates[fmt.Sprintf("pool:%s/%s#%s", plan.Template, plan.Plan.qualifiedInstance, slot)] = true
		}
	}
	for t, tp := range d.Snapshot.Partial.Templates {
		if tp.Retain {
			out.retained[t] = true
		}
	}
	return out
}

// diffRecording is K1's recording for c: the scale_check source holds the
// lane's run of every custom scale_check (runCustomScaleChecks), its runner
// answering each command with the output legacy's shell gives it. Nil
// without custom checks, as before the lane's first publish.
func diffRecording(t *testing.T, c diffCity, cityPath string, store beads.Store) *externalReadsRecording {
	t.Helper()
	if !c.scaleCheck {
		return nil
	}
	runner := func(command, _ string, _ map[string]string) (string, error) {
		i := strings.LastIndex(command, "echo ")
		if i < 0 {
			return "", fmt.Errorf("unexpected scale_check %q", command)
		}
		return command[i+len("echo "):] + "\n", nil
	}
	run := runCustomScaleChecks(externalReadsEnv{CityPath: cityPath, CityName: c.cfg.Workspace.Name, Cfg: c.cfg, CityStore: store},
		runner, controllerQueryRuntimeEnv, io.Discard)
	return &externalReadsRecording{
		Seq: 1, FreshFor: time.Minute,
		Sources: map[sourceKey]sourceResult{
			{kind: sourceScaleCheck, cfg: c.cfg}: {StartedAt: diffNow, EndedAt: diffNow, sourcePayload: sourcePayload{ScaleCheck: run}},
		},
	}
}

// allocInputsOf is the decide's inputs from a gathered World.
func allocInputsOf(w World, cityPath, cityName string) allocInputs {
	return allocInputs{
		Now: w.Now, Epoch: "e1", Cfg: w.Env.Cfg, ConfigRev: w.Env.ConfigRev, EnvGen: w.Env.Gen,
		CityPath: cityPath, CityName: cityName,
		CitySuspended: w.CitySuspended, SuspendedRigPaths: w.SuspendedRigPaths,
		Census: w.Census, Demand: w.Demand, ScaleCheck: w.ScaleCheck, Obs: w.Obs, ObsMaxAge: w.ObsMaxAge,
		Endpoints: w.Gates, ProviderHealth: w.ProviderHealth, Episodes: w.Episodes,
		SleepPolicies: w.SleepPolicies, TransportRefused: w.TransportRefused, ReadyWaits: w.ReadyWaits,
		InFlight: w.InFlight, Backoff: w.Backoff,
	}
}

// checkDifferential compares c's two allocations: c must account for every
// mismatch with a listed difference or finding, and every entry must show.
func checkDifferential(t *testing.T, c diffCity) {
	t.Helper()
	legacy, v2 := legacyAllocation(t, c), v2Allocation(t, c)
	got := legacy.mismatches(v2)
	for aspect, values := range got {
		id, ok := c.explain[aspect]
		if !ok {
			t.Errorf("%s: unexplained difference %s (%s)", c.name, aspect, values)
			continue
		}
		_, explained := allocDifferences[id]
		finding, found := allocFindings[id]
		switch {
		case found:
			t.Logf("%s: %s is open finding %s: %s", c.name, aspect, id, finding.reason)
		case !explained:
			t.Errorf("%s: %s accounted to %q, which neither allocDifferences nor allocFindings lists", c.name, aspect, id)
		}
	}
	for aspect, id := range c.explain {
		if _, ok := got[aspect]; !ok {
			t.Errorf("%s: %s, accounted to %s, no longer shows: legacy and v2 agree", c.name, aspect, id)
		}
	}
	if t.Failed() {
		t.Logf("%s: legacy %+v\nv2 %+v", c.name, legacy, v2)
	}
}

// diffWork is an open work bead routed to template.
func diffWork(id, template string, meta ...string) beads.Bead {
	m := map[string]string{"gc.routed_to": template}
	for i := 0; i+1 < len(meta); i += 2 {
		m[meta[i]] = meta[i+1]
	}
	return beads.Bead{ID: id, Title: id, Type: "task", Status: "open", CreatedAt: diffNow.Add(-time.Hour), Metadata: m}
}

// diffAgent is a pool agent with a start command, so legacy resolves its
// template without a provider.
func diffAgent(name string, maxActive int) config.Agent {
	return config.Agent{Name: name, StartCommand: "true", MaxActiveSessions: intPtr(maxActive)}
}

func diffConfig(agents ...config.Agent) *config.City {
	return &config.City{Workspace: config.Workspace{Name: "diff-city"}, Agents: agents}
}

func diffCities(t *testing.T) []diffCity {
	chat := func(mode string) *config.City {
		cfg := diffConfig(diffAgent("worker", 3), diffAgent("chat", 1))
		cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: mode}}
		return cfg
	}
	floor := diffAgent("worker", 4)
	floor.MinActiveSessions = intPtr(2)
	suspended := diffAgent("worker", 3)
	suspended.Suspended = true
	budget := diffConfig(diffAgent("worker", 10))
	budget.Daemon.MaxWakesPerTick = intPtr(2)
	rigged := diffConfig(diffAgent("worker", 2))
	rigged.Rigs = []config.Rig{{Name: "away", Path: t.TempDir(), Suspended: true}}
	away := diffAgent("helper", 2)
	away.Dir, away.MinActiveSessions = "away", intPtr(1)
	rigged.Agents = append(rigged.Agents, away)
	app := diffAgent("app", 2)
	app.DependsOn = []string{"db"}
	deps := diffConfig(app, diffAgent("db", 1))
	idle := chat("always")
	idle.Workspace.Suspended = true
	custom, flaky := diffAgent("custom", 3), diffAgent("flaky", 2)
	custom.ScaleCheck, flaky.ScaleCheck = "echo 2", "echo unknown"
	dispatch := cityOnlyDispatcherFixtureConfig(t)
	dispatcher := dispatch.Agents[0].QualifiedName()
	control := func(id, root string) beads.Bead {
		return diffWork(id, dispatcher, beadmeta.KindMetadataKey, beadmeta.KindWorkflowFinalize, beadmeta.RootStoreRefMetadataKey, root)
	}
	return []diffCity{
		{
			name: "routed demand reuses an asleep row and plans the rest",
			cfg:  diffConfig(diffAgent("worker", 3)),
			beads: []beads.Bead{
				poolRow("dr-1", "worker", 1, "asleep"),
				diffWork("dw-1", "worker"), diffWork("dw-2", "worker"),
			},
		},
		{
			name:  "demand over max_active_sessions stops at the cap",
			cfg:   diffConfig(diffAgent("worker", 2)),
			beads: []beads.Bead{diffWork("dw-1", "worker"), diffWork("dw-2", "worker"), diffWork("dw-3", "worker"), diffWork("dw-4", "worker")},
		},
		{
			name: "assigned work counts its row; alive rows stay in desired",
			cfg:  diffConfig(diffAgent("worker", 4)),
			beads: []beads.Bead{
				poolRow("dr-1", "worker", 1, "asleep"), poolRow("dr-2", "worker", 2, "active"), poolRow("dr-3", "worker", 3, "active"),
				{ID: "dw-1", Title: "dw-1", Type: "task", Status: "in_progress", Assignee: "dr-1", CreatedAt: diffNow.Add(-time.Hour), Metadata: map[string]string{"gc.routed_to": "worker"}},
			},
			alive: []string{"s-dr-2", "s-dr-3"},
		},
		{
			name:  "min_active_sessions fills the floor with no demand",
			cfg:   diffConfig(floor),
			beads: []beads.Bead{poolRow("dr-1", "worker", 1, "asleep")},
		},
		{
			name: "two templates share nothing",
			cfg:  diffConfig(diffAgent("worker", 2), diffAgent("reviewer", 2)),
			beads: []beads.Bead{
				poolRow("dr-1", "reviewer", 1, "active"),
				diffWork("dw-1", "worker"), diffWork("dw-2", "reviewer"), diffWork("dw-3", "reviewer"),
			},
			alive: []string{"s-dr-1"},
		},
		{
			name:  "a singleton pool plans its canonical identity",
			cfg:   diffConfig(diffAgent("solo", 1)),
			beads: []beads.Bead{diffWork("dw-1", "solo")},
		},
		{
			name:  "an always named session with no row is materialized",
			cfg:   chat("always"),
			beads: []beads.Bead{diffWork("dw-1", "worker")},
		},
		{
			name:  "an always named session's canonical row is selected",
			cfg:   chat("always"),
			beads: []beads.Bead{chatRow("dr-c", "1")},
		},
		{
			name:  "an on_demand named session without work stays unmaterialized",
			cfg:   chat("on_demand"),
			beads: []beads.Bead{poolRow("dr-1", "worker", 1, "asleep")},
		},
		{
			name: "an on_demand named session with assigned work is materialized",
			cfg:  chat("on_demand"),
			beads: []beads.Bead{
				{ID: "dw-1", Title: "dw-1", Type: "task", Status: "open", Assignee: "chat", CreatedAt: diffNow.Add(-time.Hour), Metadata: map[string]string{}},
			},
		},
		{
			name: "a manual session row joins desired by the overlay",
			cfg:  diffConfig(diffAgent("worker", 2)),
			beads: []beads.Bead{
				sessionRow("dr-m", "template", "worker", "state", "asleep", "session_name", "s-dr-m", "session_origin", "manual", "alias", "mine", "generation", "1"),
			},
		},
		{
			name: "a suspended agent gets no requests and keeps no rows",
			cfg:  diffConfig(suspended),
			beads: []beads.Bead{
				poolRow("dr-1", "worker", 1, "active"), diffWork("dw-1", "worker"),
			},
			alive: []string{"s-dr-1"},
		},
		{
			name:    "creates over the wake budget",
			explain: map[string]string{"create:pool:worker/worker-3#3": "create-budget", "create:pool:worker/worker-4#4": "create-budget"},
			cfg:     budget,
			beads:   []beads.Bead{diffWork("dw-1", "worker"), diffWork("dw-2", "worker"), diffWork("dw-3", "worker"), diffWork("dw-4", "worker")},
		},
		{
			name:    "a template in a suspended rig",
			explain: map[string]string{"accepted:away/helper": "suspended-rig"},
			cfg:     rigged,
			beads:   []beads.Bead{diffWork("dw-1", "worker")},
		},
		{
			name:    "a depends_on dependency floor",
			explain: map[string]string{"create:pool:db/db#": "dependency-floor"},
			cfg:     deps,
			beads:   []beads.Bead{diffWork("dw-1", "app")},
		},
		{
			name:    "a suspended city keeps its named row",
			explain: map[string]string{"selected:dr-c": "suspended-city-named"},
			cfg:     idle,
			beads:   []beads.Bead{chatRow("dr-c", "1"), poolRow("dr-1", "worker", 1, "active")},
			alive:   []string{"s-dr-1"},
		},
		{
			name:       "custom scale_check counts and a partial check retain (ruling 1)",
			cfg:        diffConfig(custom, flaky),
			beads:      []beads.Bead{poolRow("dr-1", "flaky", 1, "asleep"), poolRow("dr-2", "custom", 1, "asleep")},
			scaleCheck: true,
		},
		{
			name: "control rows count only in their own root scope (W-H1, not a difference)",
			cfg:  dispatch,
			beads: []beads.Bead{
				control("dc-gap", "rig:fixture"), control("dc-own", ""),
			},
		},
		{
			name:    "an expired pending create with no runtime",
			explain: map[string]string{"selected:dr-1": "stale-pending-create", "create:pool:worker/worker-2#2": "stale-pending-create"},
			cfg:     diffConfig(diffAgent("worker", 3)),
			beads: []beads.Bead{
				poolRow("dr-1", "worker", 1, "creating", "pending_create_claim", "true", "pending_create_started_at", diffNow.Add(-time.Hour).Format(time.RFC3339)),
				diffWork("dw-1", "worker"),
			},
		},
		{
			name: "a stale creating row with no claim and no runtime (v5 C3, B5: reused, not rolled back)",
			cfg:  diffConfig(diffAgent("worker", 3)),
			beads: []beads.Bead{
				poolRow("dr-1", "worker", 1, "creating"),
				diffWork("dw-1", "worker"),
			},
		},
		{
			name:    "a failed create, an orphaned template and a phantom singleton slot",
			explain: map[string]string{"create:pool:worker/worker-1#1": "create-fence"},
			cfg:     diffConfig(diffAgent("worker", 2), diffAgent("solo", 1)),
			beads: []beads.Bead{
				poolRow("dr-1", "worker", 1, string(session.StateFailedCreate)),
				poolRow("dr-2", "retired", 1, "asleep"),
				poolRow("dr-3", "solo", 1, "asleep", "agent_name", "solo-1"),
				diffWork("dw-1", "worker"), diffWork("dw-2", "solo"),
			},
		},
		{
			name:  "an on_demand named session's canonical row without work",
			cfg:   chat("on_demand"),
			beads: []beads.Bead{chatRow("dr-c", "1", "configured_named_mode", "on_demand")},
		},
		v5HashedCity(),
	}
}

// v5HashedCity is a production-shaped city (OPS-1, OPS-1b): rows carry
// instance tokens and v5: started hashes where main computes v6, one named
// row has empty config and live hashes, an awake row has no started hashes,
// a row drains with no sleep reason, and one row is on the wisp tier. Hashes
// are not an allocation input, so the two sides must agree on every row;
// that v2 drains none of them for drift is D2b's (Gate R R18).
func v5HashedCity() diffCity {
	cfg := diffConfig(diffAgent("reviewer", 4), diffAgent("worker", 3), diffAgent("olivia", 1))
	cfg.NamedSessions = []config.NamedSession{{Template: "olivia", Mode: "always"}}
	hashes := func(token string, meta ...string) []string {
		return append([]string{
			"instance_token", token, "generation", "3", "continuation_epoch", "2",
			"last_woke_at", diffNow.Add(-30 * time.Minute).Format(time.RFC3339),
			"started_config_hash", "v5:9f1c", "started_live_hash", "v5:2b7e", "live_hash", "v5:2b7e",
			"started_launch_hash", "v5:41aa", "started_provision_hash", "v5:c3d0",
		}, meta...)
	}
	olivia := chatRow("gcg--9223372036854775750", "4", hashes("tok-o")...)
	olivia.Metadata["template"], olivia.Metadata["configured_named_identity"] = "olivia", "olivia"
	olivia.Metadata["started_config_hash"], olivia.Metadata["started_live_hash"], olivia.Metadata["live_hash"] = "", "", ""
	wisp := poolRow("gcg-wisp-vownh35", "worker", 2, "awake", hashes("tok-w")...)
	wisp.Ephemeral = true
	return diffCity{
		name:    "production-shaped rows with v5 hashes",
		explain: map[string]string{"selected:gcg-wisp-vownh35": "wisp-tier"},
		cfg:     cfg,
		beads: []beads.Bead{
			poolRow("gcg-session-b2063", "reviewer", 1, "awake", hashes("tok-1")...),
			poolRow("gcg-session-b2064", "reviewer", 2, "draining", hashes("tok-2", "sleep_reason", "", "state_reason", session.DrainAckStopPendingReason)...),
			poolRow("gcg-session-b2065", "reviewer", 3, "asleep", hashes("tok-3")...),
			poolRow("gci-miai39", "worker", 1, "awake", "instance_token", "tok-4"),
			olivia, wisp,
			{ID: "gw-1", Title: "gw-1", Type: "task", Status: "in_progress", Assignee: "gci-miai39", CreatedAt: diffNow.Add(-time.Hour), Metadata: map[string]string{"gc.routed_to": "worker"}},
			diffWork("gw-2", "reviewer"), diffWork("gw-3", "reviewer"),
		},
		alive: []string{"s-gcg-session-b2063", "s-gcg-session-b2064", "s-gci-miai39", "s-gcg-wisp-vownh35"},
	}
}

// Kills drift between the allocator and legacy's pool realization (P3-5c):
// any accepted count, selected row, planned identity or retention the two
// disagree on that allocDifferences does not explain.
func TestAllocationDifferentialFixtures(t *testing.T) {
	for _, c := range diffCities(t) {
		t.Run(c.name, func(t *testing.T) { checkDifferential(t, c) })
	}
}

// generatedCity is seed's city: one to four pool templates (singletons
// among them) with caps and floors, an optional named session, rows in the
// states both sides manage alike, some alive, and routed and assigned work.
// It stays clear of every explained difference: no suspension, depends_on,
// custom scale_check, stale create, wisp-tier row or create budget.
func generatedCity(seed uint64) diffCity {
	rng := rand.New(rand.NewPCG(seed, 7))
	cfg := diffConfig()
	cfg.Daemon.MaxWakesPerTick = intPtr(100)
	c := diffCity{name: fmt.Sprintf("seed %d", seed), cfg: cfg}
	states := []string{"asleep", "active", "awake", "drained", "suspended"}
	row := 0
	for i := range 1 + rng.IntN(4) {
		template := fmt.Sprintf("pool%d", i)
		maxActive := 1 + rng.IntN(4)
		agent := diffAgent(template, maxActive)
		if maxActive > 1 && rng.IntN(3) == 0 {
			agent.MinActiveSessions = intPtr(1 + rng.IntN(2))
		}
		cfg.Agents = append(cfg.Agents, agent)
		for slot := 1; slot <= maxActive && rng.IntN(3) > 0; slot++ {
			row++
			id, state := fmt.Sprintf("dg-%d", row), states[rng.IntN(len(states))]
			b := poolRow(id, template, slot, state)
			if maxActive == 1 {
				b = poolRow(id, template, 0, state, "agent_name", template, "pool_slot", "")
			}
			c.beads = append(c.beads, b)
			if (state == "active" || state == "awake") && rng.IntN(4) > 0 {
				c.alive = append(c.alive, "s-"+id)
			}
			if rng.IntN(4) == 0 {
				w := diffWork("dga-"+id, template)
				w.Status, w.Assignee = "in_progress", id
				c.beads = append(c.beads, w)
			}
		}
		for j := range rng.IntN(5) {
			c.beads = append(c.beads, diffWork(fmt.Sprintf("dgw-%d-%d", i, j), template))
		}
	}
	if rng.IntN(2) == 0 {
		mode := []string{"always", "on_demand"}[rng.IntN(2)]
		cfg.Agents = append(cfg.Agents, diffAgent("chat", 1))
		cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: mode}}
		if rng.IntN(2) == 0 {
			c.beads = append(c.beads, chatRow("dg-chat", "1", "configured_named_mode", mode))
		}
		if rng.IntN(2) == 0 {
			c.beads = append(c.beads, beads.Bead{ID: "dga-chat", Title: "dga-chat", Type: "task", Status: "open", Assignee: "chat", CreatedAt: diffNow.Add(-time.Hour), Metadata: map[string]string{}})
		}
	}
	return c
}

// Kills drift on cities no one hand-wrote (P3-5c): 200 seeded cities, each
// of which must show no difference at all.
func TestAllocationDifferentialGenerated(t *testing.T) {
	for seed := range uint64(200) {
		c := generatedCity(seed)
		t.Run(c.name, func(t *testing.T) { checkDifferential(t, c) })
	}
}

// Kills an entry that accounts for nothing: every explained difference and
// every open finding is shown by at least one fixture.
func TestAllocationDifferentialEntriesHaveFixtures(t *testing.T) {
	shown := map[string]bool{}
	for _, c := range diffCities(t) {
		for _, id := range c.explain {
			shown[id] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(allocDifferences)) {
		if !shown[id] {
			t.Errorf("explained difference %s has no fixture", id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(allocFindings)) {
		if !shown[id] {
			t.Errorf("finding %s has no fixture", id)
		}
	}
}
