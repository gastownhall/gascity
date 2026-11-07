package sling

import (
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
