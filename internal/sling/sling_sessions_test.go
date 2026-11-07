package sling

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// These tests pin sling's session-class reads to SlingDeps.Sessions, the
// city's sessions store (typed-stores T6 plan, OD-3c): a session bead lives
// only there, and on a split city the work store either lacks it or holds a
// frozen pre-migration copy (migrate preserves ids and never deletes).

// splitSessionsDeps is deps over a city whose sessions class lives apart from
// the work store: work holds the work beads, sessions the session beads.
func splitSessionsDeps(t *testing.T, work, sessions beads.Store) (SlingDeps, *fakeRunner) {
	t.Helper()
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test-city"}}, runtime.NewFake(), runner.run)
	deps = withStore(deps, work)
	deps.Sessions = beadstest.Sessions(sessions)
	return deps, runner
}

// routedPoolWork creates, in work, a bead routed to pool target and claimed
// by assignee, tracked by a live auto-convoy so convoy recovery does not re-run.
func routedPoolWork(t *testing.T, work beads.Store, target, assignee string) beads.Bead {
	t.Helper()
	b, err := work.Create(beads.Bead{
		Title:    "pool work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: assignee,
		Metadata: map[string]string{"gc.routed_to": target},
	})
	if err != nil {
		t.Fatalf("creating the routed work: %v", err)
	}
	convoy, err := work.Create(beads.Bead{Title: AutoConvoyRootTitle(b.ID), Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatalf("creating the auto-convoy: %v", err)
	}
	if err := work.DepAdd(convoy.ID, b.ID, "tracks"); err != nil {
		t.Fatalf("tracking the work: %v", err)
	}
	return b
}

// TestReslingOfPoolClaimedWorkReadsTheClaimantInTheSessionsStore is E3: a
// re-sling of work a pool session claimed under its session bead id (#6324)
// is a no-op only when the claimant is recognized as one of the pool's own
// sessions. That session bead lives in the sessions store; read through the
// work store it was missed on a split city, so the re-sling routed the work
// again beside the session still holding it (one unit of work, two sessions),
// or it was read from the work store's stale pre-migration copy.
func TestReslingOfPoolClaimedWorkReadsTheClaimantInTheSessionsStore(t *testing.T) {
	pool := config.Agent{Name: "smiths", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(4)}
	target := agentutil.RoutedToIdentity(&pool)

	t.Run("the claimant lives only in the sessions store", func(t *testing.T) {
		work, sessions := beads.NewMemStore(), beads.NewMemStoreFrom(100, nil, nil)
		sb := createPoolSessionBead(t, sessions, target, "open")
		b := routedPoolWork(t, work, target, sb.ID)
		deps, runner := splitSessionsDeps(t, work, sessions)

		result, err := DoSling(SlingOpts{Target: pool, BeadOrFormula: b.ID}, deps, deps.Store)
		if err != nil {
			t.Fatalf("re-sling: %v", err)
		}
		if !result.Idempotent || len(runner.calls) != 0 {
			t.Fatalf("re-sling of work its own pool session %s holds = %+v after %d routes, want an idempotent no-op", sb.ID, result, len(runner.calls))
		}
	})

	t.Run("the work store holds a stale open copy of a closed claimant", func(t *testing.T) {
		sessions := beads.NewMemStoreFrom(100, nil, nil)
		sb := createPoolSessionBead(t, sessions, target, "closed")
		stale := sb
		stale.Status = "open"
		work := beads.NewMemStoreFrom(200, []beads.Bead{stale}, nil)
		b := routedPoolWork(t, work, target, sb.ID)
		deps, _ := splitSessionsDeps(t, work, sessions)

		result := CheckBeadState(deps.Store, b.ID, pool, deps)
		if result.Idempotent {
			t.Fatalf("CheckBeadState = %+v: the claimant session %s is closed in the sessions store, so the work is not held by the pool", result, sb.ID)
		}
	})
}

// createNamedSessionBead creates an open session bead for template whose
// runtime session is sessionName, in store.
func createNamedSessionBead(t *testing.T, store beads.Store, template, sessionName string) beads.Bead {
	t.Helper()
	sb, err := store.Create(beads.Bead{
		Title:    "session",
		Type:     "session",
		Status:   "open",
		Labels:   []string{"gc.session"},
		Metadata: map[string]string{beadmeta.TemplateMetadataKey: template, "session_name": sessionName},
	})
	if err != nil {
		t.Fatalf("creating the session bead: %v", err)
	}
	return sb
}

// TestSlingTargetEnvNamesTheSessionInTheSessionsStore is E2: GC_SLING_TARGET
// names a single-session agent's runtime session, read from its session bead.
// That bead lives in the sessions store; read through the work store it was
// missed on a split city, and the env fell back to the computed name, or it
// was read from the work store's stale pre-migration copy.
func TestSlingTargetEnvNamesTheSessionInTheSessionsStore(t *testing.T) {
	a := singleSessionAgent()

	t.Run("the session bead lives only in the sessions store", func(t *testing.T) {
		work, sessions := beads.NewMemStore(), beads.NewMemStoreFrom(100, nil, nil)
		createNamedSessionBead(t, sessions, a.QualifiedName(), "s-bound-1")
		deps, _ := splitSessionsDeps(t, work, sessions)

		if got := ResolveSlingEnv(a, deps, "")["GC_SLING_TARGET"]; got != "s-bound-1" {
			t.Fatalf("GC_SLING_TARGET = %q, want the sessions store's s-bound-1", got)
		}
	})

	t.Run("the work store holds a stale copy", func(t *testing.T) {
		sessions := beads.NewMemStoreFrom(100, nil, nil)
		sb := createNamedSessionBead(t, sessions, a.QualifiedName(), "s-current")
		stale := sb
		stale.Metadata = map[string]string{beadmeta.TemplateMetadataKey: a.QualifiedName(), "session_name": "s-pre-migration"}
		work := beads.NewMemStoreFrom(200, []beads.Bead{stale}, nil)
		deps, _ := splitSessionsDeps(t, work, sessions)

		if got := ResolveSlingEnv(a, deps, "")["GC_SLING_TARGET"]; got != "s-current" {
			t.Fatalf("GC_SLING_TARGET = %q, want the sessions store's s-current", got)
		}
	})
}

// rigScopeGraphFormula writes a graph.v2 formula whose one step is assigned
// to assignee, and returns the config of a city whose rig "frontend" holds
// the sling's work and graph beads.
func rigScopeGraphFormula(t *testing.T, assignee string) *config.City {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "graph-direct.toml"), []byte(`
formula = "graph-direct"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
assignee = "`+assignee+`"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := graphV2SlingTestConfig(t, dir)
	cfg.Rigs = []config.Rig{{Name: "frontend", Path: t.TempDir(), Prefix: "fe"}}
	dispatcher := slingControlDispatcherAgent()
	dispatcher.Dir = "frontend"
	cfg.Agents = append(cfg.Agents, dispatcher, rigWorker())
	return cfg
}

// rigWorker is the single-session agent of rig "frontend" the rig-scope
// tests sling to.
func rigWorker() config.Agent {
	return config.Agent{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(1)}
}

// rigScopeDeps is deps for a rig-scope sling on a city that relocates
// nothing: the rig store is the work and the graph store, and the city's
// sessions store is the city store, which holds every session bead.
func rigScopeDeps(t *testing.T, cfg *config.City, rig, city beads.Store) SlingDeps {
	t.Helper()
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps = withStore(deps, rig)
	deps.Sessions = beadstest.Sessions(city)
	deps.StoreRef = "rig:frontend"
	return deps
}

// stepBead is the one non-root bead a graph.v2 launch minted in store.
func stepBead(t *testing.T, store beads.Store, rootID string) beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.ID != rootID && b.Metadata[beadmeta.RootBeadIDMetadataKey] == rootID && b.Title == "Do work" {
			return b
		}
	}
	t.Fatalf("no step of root %s in the store: %+v", rootID, all)
	return beads.Bead{}
}

// TestRigScopeGraphSlingBindsSessionsInTheCitySessionsStore is E3b: a
// graph.v2 sling resolves its steps' session targets through a sessions
// store. It was handed the graph store relabelled as one, which for a
// rig-scope sling on a city that relocates nothing is the rig store, and a
// rig store holds no session beads: a step assigned to a live session failed
// to bind (the API path), a configured named session was materialized in the
// rig store where the controller never reads it (the CLI path's resolver),
// and the default route's session name fell back to the computed name.
func TestRigScopeGraphSlingBindsSessionsInTheCitySessionsStore(t *testing.T) {
	worker := rigWorker()

	t.Run("a step assigned to a live session binds to it", func(t *testing.T) {
		city, rig := beads.NewMemStore(), beads.NewMemStoreFrom(500, nil, nil)
		sb := createNamedSessionBead(t, city, "reviewer", "s-reviewer")
		cfg := rigScopeGraphFormula(t, sb.ID)
		deps := rigScopeDeps(t, cfg, rig, city)

		result, err := DoSling(SlingOpts{Target: worker, BeadOrFormula: "graph-direct", IsFormula: true}, deps, nil)
		if err != nil {
			t.Fatalf("rig-scope graph.v2 sling with a step assigned to live session %s: %v", sb.ID, err)
		}
		if got := stepBead(t, rig, result.WorkflowID).Assignee; got != sb.ID {
			t.Errorf("step assignee = %q, want the live session %s", got, sb.ID)
		}
	})

	t.Run("a named session is materialized in the city sessions store", func(t *testing.T) {
		city, rig := beads.NewMemStore(), beads.NewMemStoreFrom(500, nil, nil)
		cfg := rigScopeGraphFormula(t, "mayor")
		deps := rigScopeDeps(t, cfg, rig, city)
		var handed beads.SessionStore
		deps.DirectSessionResolver = func(store beads.SessionStore, _, _ string, _ *config.City, target, _ string) (string, bool, error) {
			if target != "mayor" {
				return "", false, nil
			}
			handed = store
			sb, err := store.Create(beads.Bead{Title: "mayor", Type: "session", Labels: []string{"gc.session"}})
			return sb.ID, err == nil, err
		}

		result, err := DoSling(SlingOpts{Target: worker, BeadOrFormula: "graph-direct", IsFormula: true}, deps, nil)
		if err != nil {
			t.Fatalf("rig-scope graph.v2 sling with a step assigned to named session mayor: %v", err)
		}
		if !beads.SameStore(handed, city) {
			t.Fatalf("the direct-session resolver was handed %v, want the city sessions store: a session row materialized anywhere else is stranded", handed)
		}
		bound := stepBead(t, rig, result.WorkflowID).Assignee
		if _, err := city.Get(bound); err != nil {
			t.Errorf("the step is bound to %q, which the city sessions store does not hold: %v", bound, err)
		}
	})

	t.Run("the default route names the session the city store records", func(t *testing.T) {
		city, rig := beads.NewMemStore(), beads.NewMemStoreFrom(500, nil, nil)
		createNamedSessionBead(t, city, worker.QualifiedName(), "s-worker-bound")
		cfg := rigScopeGraphFormula(t, "")
		deps := rigScopeDeps(t, cfg, rig, city)

		result, err := DoSling(SlingOpts{Target: worker, BeadOrFormula: "graph-direct", IsFormula: true}, deps, nil)
		if err != nil {
			t.Fatalf("rig-scope graph.v2 sling: %v", err)
		}
		if got := stepBead(t, rig, result.WorkflowID).Metadata[beadmeta.SessionNameMetadataKey]; got != "s-worker-bound" {
			t.Errorf("step %s = %q, want the city store's s-worker-bound", beadmeta.SessionNameMetadataKey, got)
		}
	})
}

// TestUndeliverableHandoffWarningReadsTheClaimantInTheSessionsStore is E3's
// rule in the undeliverable-hand-off warning: on a split city a bead one of
// the target pool's own sessions holds under its session bead id is not
// stranded, because the claimant is read in the sessions store.
func TestUndeliverableHandoffWarningReadsTheClaimantInTheSessionsStore(t *testing.T) {
	pool := config.Agent{Name: "smiths", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(4)}
	work, sessions := beads.NewMemStore(), beads.NewMemStoreFrom(100, nil, nil)
	own := createPoolSessionBead(t, sessions, agentutil.RoutedToIdentity(&pool), "open")
	held, err := work.Create(beads.Bead{Title: "held", Type: "task", Status: "in_progress", Assignee: own.ID})
	if err != nil {
		t.Fatal(err)
	}
	deps, _ := splitSessionsDeps(t, work, sessions)
	molErr := &MoleculeAttachedError{Label: "molecule", AttachmentID: "mol-1"}
	if msg, warn := undeliverableHandoffWarning(work, deps, held.ID, pool, molErr); warn {
		t.Fatalf("a claim by the pool's own session %s, in the sessions store, reported undeliverable: %s", own.ID, msg)
	}
}

// TestCheckBeadStateWithOnlyAWorkStore pins the convoy-recovery and pool-claim
// reads of a SlingDeps that carries a work store and nothing else: the live
// auto-convoy in the work store makes routed work idempotent, and a claim by
// a session bead id, with no sessions store to read it in, is not the pool's.
func TestCheckBeadStateWithOnlyAWorkStore(t *testing.T) {
	pool := config.Agent{Name: "smiths", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(4)}
	target := agentutil.RoutedToIdentity(&pool)
	work, sessions := beads.NewMemStore(), beads.NewMemStoreFrom(100, nil, nil)
	deps := SlingDeps{Store: beadstest.Work(work)}

	unclaimed := routedPoolWork(t, work, target, "")
	if result := CheckBeadState(work, unclaimed.ID, pool, deps); !result.Idempotent {
		t.Errorf("CheckBeadState(routed work with a live auto-convoy) = %+v, want idempotent", result)
	}
	own := createPoolSessionBead(t, sessions, target, "open")
	held := routedPoolWork(t, work, target, own.ID)
	if result := CheckBeadState(work, held.ID, pool, deps); result.Idempotent {
		t.Errorf("CheckBeadState(work claimed by session %s, no sessions store) = %+v, want not idempotent", own.ID, result)
	}
}
