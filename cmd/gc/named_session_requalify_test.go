package main

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Issue #5957: gc rig add --adopt requalifies a city's top-level named
// session golden-hon to contentbuild/lane.golden-hon, and its live row still
// carries the bare identity.
const (
	requalifiedBare      = "golden-hon"
	requalifiedQualified = "contentbuild/lane.golden-hon"
	requalifiedRuntime   = "contentbuild--lane__golden-hon"
)

// requalifiedCity is the adopted config: golden-hon bound as lane in the
// contentbuild rig, in mode, and, when ambiguous, also bound as ops.
func requalifiedCity(t *testing.T, mode string, ambiguous bool) func() *config.City {
	rigPath := t.TempDir()
	return func() *config.City {
		cfg := &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs:      []config.Rig{{Name: "contentbuild", Path: rigPath}},
			Agents:    []config.Agent{{Name: requalifiedBare, Dir: "contentbuild", BindingName: "lane", StartCommand: "true"}},
			NamedSessions: []config.NamedSession{
				{Template: requalifiedBare, Dir: "contentbuild", BindingName: "lane", Mode: mode},
			},
		}
		if ambiguous {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: requalifiedBare, Dir: "contentbuild", BindingName: "ops", StartCommand: "true"})
			cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: requalifiedBare, Dir: "contentbuild", BindingName: "ops", Mode: mode})
		}
		return cfg
	}
}

// requalifiedStaleHash is the pre-adopt start's config hash, which the
// requalified template's never equals.
var requalifiedStaleHash = runtime.FingerprintVersion + ":pre-adopt"

// preAdoptRow is the running, started top-level named session's row, under
// its pre-adopt bare identity.
func preAdoptRow(mode string) beads.Bead {
	return sessionRow("gc-1",
		"template", requalifiedBare, "state", "active", "session_name", requalifiedBare, "alias", requalifiedBare,
		"configured_named_session", "true", "configured_named_identity", requalifiedBare, "configured_named_mode", mode,
		"generation", "1", "started_config_hash", requalifiedStaleHash)
}

// requalifiedWork is in-progress work assigned under the bare identity.
func requalifiedWork() beads.Bead {
	return beads.Bead{ID: "gw-1", Title: "gw-1", Type: "task", Status: "in_progress", Assignee: requalifiedBare}
}

// requalifiedFixture is the adopted city with the pre-adopt row running and
// its work in progress.
func requalifiedFixture(t *testing.T, mode string, ambiguous bool) parityFixture {
	return parityFixture{
		Name:     "requalified-" + mode,
		Rows:     []parityRow{preAdoptRow(mode)},
		Work:     []parityRow{requalifiedWork()},
		Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: rowToken("gc-1")}},
		City:     requalifiedCity(t, mode, ambiguous),
	}
}

// requalifiedV2World is f's v2 world with the contentbuild rig, which
// newV2World's simulator rigs replace, configured beside them.
func requalifiedV2World(t *testing.T, f parityFixture) *v2World {
	w := newV2World(t, f, t.TempDir(), f.beads(), nil, "", nil)
	w.s.cfg.Rigs = append(w.s.cfg.Rigs, f.city().Rigs...)
	return w
}

// rowByID is the bead with id in all.
func rowByID(t *testing.T, all []beads.Bead, id string) beads.Bead {
	t.Helper()
	i := slices.IndexFunc(all, func(b beads.Bead) bool { return b.ID == id })
	if i < 0 {
		t.Fatalf("no bead %s", id)
	}
	return all[i]
}

// AC-1: one legacy tick renames the pre-adopt row in place to the qualified
// identity and rebaselines its drift hash, and mints no second row.
func TestLegacyRequalifiedNamedSessionMigratesBareRowInOneTick(t *testing.T) {
	f := requalifiedFixture(t, "always", false)
	cityPath := t.TempDir()
	w := newLegacyWorld(t, f, cityPath, f.beads())
	w.tick(t, true)

	all := w.list(t)
	row := rowByID(t, all, "gc-1")
	if row.Status != "open" {
		t.Fatalf("row status = %q, want open", row.Status)
	}
	for key, want := range map[string]string{
		"session_name":              requalifiedBare,
		"configured_named_identity": requalifiedQualified,
		"alias":                     requalifiedQualified,
	} {
		if got := row.Metadata[key]; got != want {
			t.Errorf("row %s = %q, want %q", key, got, want)
		}
	}
	if history := session.AliasHistory(row.Metadata); !slices.Contains(history, requalifiedBare) {
		t.Errorf("alias_history = %v, want it to contain %q", history, requalifiedBare)
	}
	for _, b := range all {
		if b.Metadata["session_name"] == requalifiedRuntime {
			t.Errorf("row %s (%s) was minted for the requalified session", b.ID, b.Status)
		}
	}

	snap, err := loadSessionBeadSnapshot(w.store)
	if err != nil {
		t.Fatal(err)
	}
	now := w.clk.Now()
	result := buildDesiredStateWithSessionBeadsAt(w.cfg.Workspace.Name, cityPath, now, now, w.cfg, w.sp, w.store, nil, snap, nil, io.Discard)
	tp, ok := result.State[requalifiedBare]
	if !ok {
		t.Fatalf("desired state has no %s entry: %v", requalifiedBare, mapKeys(result.State))
	}
	if tp.TemplateName != requalifiedQualified {
		t.Fatalf("desired template = %q, want %q", tp.TemplateName, requalifiedQualified)
	}
	want := runtime.CoreFingerprint(sessionCoreConfigForHashInfo(tp, sessionInfoFromBead(row)))
	if got := row.Metadata["started_config_hash"]; got != want {
		t.Errorf("started_config_hash = %q, want the requalified template's %q (stale %q)", got, want, requalifiedStaleHash)
	}
}

// AC-2: one v2 pass selects the pre-adopt row as the qualified named
// session's canonical row, and A7 renames it and rebaselines its drift hash
// in one patch.
func TestV2RequalifiedNamedSessionSelectsAndRenamesBareRowInOnePass(t *testing.T) {
	cfg := requalifiedCity(t, "always", false)()
	d := newAllocFixture(t, cfg).sessions(preAdoptRow("always")).alive(requalifiedBare, InventoryAttrs{}).decide()
	if e := entryOf(t, d, "gc-1"); !e.InDesired || e.DrainReason == drainOrphaned {
		t.Errorf("gc-1 entry: InDesired %v, drain %q; want the named session's canonical row", e.InDesired, e.DrainReason)
	}
	for _, p := range d.Plans {
		if p.Kind == createNamed {
			t.Errorf("planNamed planned a named create %+v beside the pre-adopt row", p.Named)
		}
	}

	f := requalifiedFixture(t, "always", false)
	w := requalifiedV2World(t, f)
	w.once(t)
	admitted := w.s.p.out.record.Load().Admitted
	if keys := intentKeys(admitted); !slices.Contains(keys, intentRowMetadata+":gc-1") {
		t.Errorf("v2 admitted %v, want A7's %s on gc-1", keys, intentRowMetadata)
	}
	for _, it := range admitted {
		if it.Kind == intentCreate {
			t.Errorf("v2 admitted a create %v beside the pre-adopt row", it.Key)
		}
	}
	snap := w.s.snapshot()
	row, ok := snap["city/gc-1"]
	if !ok || row.Status != "open" {
		t.Fatalf("row gc-1 = %+v, want it open", row)
	}
	for key, want := range map[string]string{
		"session_name":              requalifiedBare,
		"template":                  requalifiedQualified,
		"configured_named_identity": requalifiedQualified,
		"alias":                     requalifiedQualified,
	} {
		if got := row.Metadata[key]; got != want {
			t.Errorf("row %s = %q, want %q", key, got, want)
		}
	}
	if history := session.AliasHistory(row.Metadata); !slices.Contains(history, requalifiedBare) {
		t.Errorf("alias_history = %v, want it to contain %q", history, requalifiedBare)
	}
	if row.Metadata["started_config_hash"] == requalifiedStaleHash || row.Metadata["core_hash_breakdown"] == "" {
		t.Errorf("A7 left the drift baseline: started_config_hash %q, core_hash_breakdown %q", row.Metadata["started_config_hash"], row.Metadata["core_hash_breakdown"])
	}
	for k, b := range snap {
		if b.Metadata["session_name"] == requalifiedRuntime {
			t.Errorf("row %s (%s) was minted for the requalified session", k, b.Status)
		}
	}
}

// requalifiedTicker is one reconciler over a requalified fixture: tick runs
// one tick and moves the clock a patrol on, store is the sessions store,
// actions the runtime's action log, log the legacy reconciler's log (nil
// for v2).
type requalifiedTicker struct {
	tick    func()
	store   beads.Store
	actions func() map[string]int
	log     *strings.Builder
}

// requalifiedTickerNames are the reconcilers newRequalifiedTicker runs.
var requalifiedTickerNames = []string{"legacy", "v2"}

// newRequalifiedTicker is the named reconciler over f.
func newRequalifiedTicker(t *testing.T, name string, f parityFixture) requalifiedTicker {
	if name == "legacy" {
		w := newLegacyWorld(t, f, t.TempDir(), f.beads())
		log := &strings.Builder{}
		w.log = log
		return requalifiedTicker{tick: func() { w.tick(t, true); w.clk.Advance(simPatrol) }, store: w.store, actions: w.sp.actions, log: log}
	}
	w := requalifiedV2World(t, f)
	return requalifiedTicker{tick: func() { w.once(t); w.s.advance(simPatrol) }, store: w.s.legs[0].backing, actions: w.sp.actions}
}

// runtimeLifecycleActions are the actions that start or stop a runtime.
func runtimeLifecycleActions(actions map[string]int) []string {
	var out []string
	for k := range actions {
		method, _, _ := strings.Cut(k, " ")
		switch method {
		case "Start", "Stop", "Interrupt", "Relaunch", "TerminateRuntime", "KillCorpseObject", "KillZombieObject":
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// AC-3: across three ticks of either reconciler, in either mode, the
// requalified session keeps its bare-assigned work, its row and its runtime,
// and gains no second row.
func TestRequalifiedNamedSessionKeepsWorkRowAndRuntimeAcrossTicks(t *testing.T) {
	for _, name := range requalifiedTickerNames {
		for _, mode := range []string{"always", "on_demand"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				w := newRequalifiedTicker(t, name, requalifiedFixture(t, mode, false))
				for tick := 1; tick <= 3; tick++ {
					w.tick()
					work, err := w.store.Get("gw-1")
					if err != nil {
						t.Fatalf("tick %d: %v", tick, err)
					}
					if work.Status != "in_progress" || work.Assignee != requalifiedBare {
						t.Errorf("tick %d: work %s assigned %q, want in_progress assigned %q", tick, work.Status, work.Assignee, requalifiedBare)
					}
					if !liveOpenSessionAssignmentExists(w.store, requalifiedBare) {
						t.Errorf("tick %d: no live open session owns %q", tick, requalifiedBare)
					}
					all, err := w.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
					if err != nil {
						t.Fatal(err)
					}
					if row := rowByID(t, all, "gc-1"); row.Status != "open" {
						t.Errorf("tick %d: row gc-1 is %s, want open", tick, row.Status)
					}
					for _, b := range all {
						if b.ID != "gc-1" && b.ID != "gw-1" && b.Status != "closed" {
							t.Errorf("tick %d: a second open row %s (%v)", tick, b.ID, b.Metadata)
						}
					}
					if acted := runtimeLifecycleActions(w.actions()); len(acted) > 0 {
						t.Errorf("tick %d: the runtime was started or stopped: %v", tick, acted)
					}
				}
			})
		}
	}
}

// AC-4: a bare identity two bindings share is ambiguous; neither reconciler
// selects or migrates its row, nor stops, closes, retires or unclaims it, and
// legacy logs both candidates.
func TestAmbiguousBareNamedSessionRowIsKeptUntouched(t *testing.T) {
	cfg := requalifiedCity(t, "always", true)()
	d := newAllocFixture(t, cfg).sessions(preAdoptRow("always")).alive(requalifiedBare, InventoryAttrs{}).decide()
	if e := entryOf(t, d, "gc-1"); e.InDesired {
		t.Errorf("planNamed selected gc-1 for an ambiguous bare identity: %+v", e)
	}
	for _, name := range requalifiedTickerNames {
		t.Run(name, func(t *testing.T) {
			w := newRequalifiedTicker(t, name, requalifiedFixture(t, "always", true))
			w.tick()
			row, err := w.store.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			if row.Status != "open" {
				t.Errorf("row gc-1 is %s, want open", row.Status)
			}
			for _, key := range []string{"session_name", "configured_named_identity", "alias"} {
				if got := row.Metadata[key]; got != requalifiedBare {
					t.Errorf("row %s = %q, want %q", key, got, requalifiedBare)
				}
			}
			if got := row.Metadata["retired_named_identity"]; got != "" {
				t.Errorf("row was retired as %q", got)
			}
			work, err := w.store.Get("gw-1")
			if err != nil {
				t.Fatal(err)
			}
			if work.Status != "in_progress" || work.Assignee != requalifiedBare {
				t.Errorf("work %s assigned %q, want in_progress assigned %q", work.Status, work.Assignee, requalifiedBare)
			}
			for _, acted := range runtimeLifecycleActions(w.actions()) {
				if strings.HasSuffix(acted, " "+requalifiedBare) {
					t.Errorf("the row's runtime was acted on: %s", acted)
				}
			}
			if w.log == nil {
				return
			}
			logged := slices.ContainsFunc(strings.Split(w.log.String(), "\n"), func(line string) bool {
				return strings.Contains(line, "gc-1") && strings.Contains(line, requalifiedQualified) && strings.Contains(line, "contentbuild/ops.golden-hon")
			})
			if !logged {
				t.Errorf("legacy logged no line naming gc-1's two candidates:\n%s", w.log.String())
			}
		})
	}
}

// AC-5: a row born under the qualified identity never carried the bare one,
// so bare-assigned work does not belong to it: the assignee matchers stay
// exact.
func TestBornQualifiedNamedSessionRowDoesNotOwnBareAssignee(t *testing.T) {
	row := sessionRow("gc-1",
		"template", requalifiedQualified, "state", "active", "session_name", requalifiedRuntime, "alias", requalifiedQualified,
		"configured_named_session", "true", "configured_named_identity", requalifiedQualified, "configured_named_mode", "always",
		"generation", "1")
	store := beads.NewMemStoreFrom(0, []beads.Bead{row, requalifiedWork()}, nil)

	if liveOpenSessionAssignmentExists(store, requalifiedBare) {
		t.Errorf("liveOpenSessionAssignmentExists(%q) matched the born-qualified row", requalifiedBare)
	}
	if !liveOpenSessionAssignmentExists(store, requalifiedQualified) {
		t.Errorf("liveOpenSessionAssignmentExists(%q) missed the born-qualified row", requalifiedQualified)
	}
	info := sessionInfoFromBead(row)
	if len(info.AliasHistory) != 0 {
		t.Fatalf("born-qualified row has alias_history %v, want none", info.AliasHistory)
	}
	if ids := session.AssigneeIdentities(info); slices.Contains(ids, requalifiedBare) {
		t.Errorf("AssigneeIdentities = %v, want no %q", ids, requalifiedBare)
	}
}
