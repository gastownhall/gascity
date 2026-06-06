package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// createPoolSessionBead creates a session bead that simulates a pool session
// surfaced under its path-alias (Title) without registering as a configured
// named-session.
func createPoolSessionBead(t *testing.T, store beads.Store, title, sessionName, state string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  title,
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": sessionName,
			"state":        state,
			"template":     "default",
		},
	})
	if err != nil {
		t.Fatalf("create pool session bead %q: %v", title, err)
	}
	return b
}

func TestResolveSessionTargetID_MatchesPoolSessionPathAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
	}
	srv := New(fs)

	pool := createPoolSessionBead(t, fs.cityBeadStore, "gascity-maintenance-pl", "s-gc-pool-001", "active")

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "gascity-maintenance-pl")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(path-alias): %v", err)
	}
	if id != pool.ID {
		t.Fatalf("resolved id = %q, want pool bead %q", id, pool.ID)
	}
}

func TestResolveSessionTargetID_PoolPathAliasAwakeStateMatches(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	pool := createPoolSessionBead(t, fs.cityBeadStore, "awake-pl", "s-gc-pool-awake", string(session.StateAwake))

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "awake-pl")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(awake path-alias): %v", err)
	}
	if id != pool.ID {
		t.Fatalf("resolved id = %q, want awake pool bead %q", id, pool.ID)
	}
}

// pathAliasFakeStore is a minimal beads.Store implementing only List —
// the single method resolveLiveSessionByPathAlias touches. Lets the
// tiebreaker test inject beads with explicit CreatedAt values without
// going through MemStore's time.Now() stamping (which has limited
// resolution on coarse-clock hosts and would couple the test to wall-
// clock timing).
type pathAliasFakeStore struct {
	beads.Store
	items []beads.Bead
}

func (p *pathAliasFakeStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return p.items, nil
}

// TestResolveLiveSessionByPathAlias_TiebreakerPrefersMostRecent verifies
// that when two active pool sessions share the same path-alias (Title) — a
// rare misconfiguration — the most-recently-created bead wins. Uses an
// in-test fake store with explicit CreatedAt values so the test is
// deterministic regardless of host clock resolution or load.
func TestResolveLiveSessionByPathAlias_TiebreakerPrefersMostRecent(t *testing.T) {
	base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	older := beads.Bead{
		ID:        "gc-older",
		Title:     "shared-pl",
		Type:      session.BeadType,
		Labels:    []string{session.LabelSession},
		Metadata:  map[string]string{"state": "active"},
		CreatedAt: base,
	}
	newer := beads.Bead{
		ID:        "gc-newer",
		Title:     "shared-pl",
		Type:      session.BeadType,
		Labels:    []string{session.LabelSession},
		Metadata:  map[string]string{"state": "active"},
		CreatedAt: base.Add(time.Hour),
	}

	store := &pathAliasFakeStore{items: []beads.Bead{older, newer}}
	id, ok, err := resolveLiveSessionByPathAlias(store, "shared-pl")
	if err != nil {
		t.Fatalf("resolveLiveSessionByPathAlias: %v", err)
	}
	if !ok || id != newer.ID {
		t.Fatalf("resolved id = %q ok = %v, want most-recent bead %q", id, ok, newer.ID)
	}

	// Reverse the input order to confirm CreatedAt drives the tiebreaker,
	// not the iteration order.
	store = &pathAliasFakeStore{items: []beads.Bead{newer, older}}
	id, ok, err = resolveLiveSessionByPathAlias(store, "shared-pl")
	if err != nil {
		t.Fatalf("resolveLiveSessionByPathAlias (reversed): %v", err)
	}
	if !ok || id != newer.ID {
		t.Fatalf("resolved id = %q ok = %v, want most-recent bead %q (input order should not matter)", id, ok, newer.ID)
	}
}

// TestResolveSessionTargetID_PathAliasDrainingSessionNotFound parallels
// the asleep-skip test: draining sessions are on their way out and the
// resolver intentionally treats them as not-found so new external
// messages aren't routed to them.
func TestResolveSessionTargetID_PathAliasDrainingSessionNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	createPoolSessionBead(t, fs.cityBeadStore, "draining-pl", "s-gc-pool-draining", string(session.StateDraining))

	_, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "draining-pl")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDWithConfig(draining path-alias) = %v, want ErrSessionNotFound", err)
	}
}

// TestResolveSessionTargetID_PathAliasEmptyStateTreatedAsActive verifies
// that legacy/upgrade beads with no persisted state metadata
// (Metadata["state"] == "" → StateNone) resolve cleanly, matching the
// convention in internal/session/manager.go:741,813 where reconciler
// paths normalize StateNone to StateActive. Without this, a path-alias
// query against a bead created before the state-metadata convention
// landed would silently fall through to "not found."
func TestResolveSessionTargetID_PathAliasEmptyStateTreatedAsActive(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	// Create a bead with no "state" entry in Metadata (legacy shape).
	pool, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "legacy-pl",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-pool-legacy",
			"template":     "default",
		},
	})
	if err != nil {
		t.Fatalf("create legacy pool bead: %v", err)
	}

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "legacy-pl")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(legacy path-alias): %v", err)
	}
	if id != pool.ID {
		t.Fatalf("resolved id = %q, want legacy pool bead %q", id, pool.ID)
	}
}

// TestResolveSessionTargetID_PathAliasCreatingSessionNotFound documents
// the intentional StateCreating exclusion: routing an inbound to a
// session whose runtime is still booting would deliver against a partial
// provider state. The function falls through to apiSessionTargetNotFound
// until the reconciler flips state=active.
func TestResolveSessionTargetID_PathAliasCreatingSessionNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	createPoolSessionBead(t, fs.cityBeadStore, "creating-pl", "s-gc-pool-creating", string(session.StateCreating))

	_, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "creating-pl")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDWithConfig(creating path-alias) = %v, want ErrSessionNotFound", err)
	}
}

func TestResolveSessionTargetID_PathAliasClosedSessionNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	pool := createPoolSessionBead(t, fs.cityBeadStore, "closed-pl", "s-gc-pool-closed", "active")
	closed := "closed"
	if err := fs.cityBeadStore.Update(pool.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close pool bead: %v", err)
	}

	_, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "closed-pl")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDWithConfig(closed path-alias) = %v, want ErrSessionNotFound", err)
	}
}

func TestResolveSessionTargetID_PathAliasAsleepSessionNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	createPoolSessionBead(t, fs.cityBeadStore, "asleep-pl", "s-gc-pool-asleep", string(session.StateAsleep))

	_, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "asleep-pl")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDWithConfig(asleep path-alias) = %v, want ErrSessionNotFound", err)
	}
}

// TestResolveSessionTargetID_ExactIDWinsOverPathAlias seeds two beads where
// one is addressable by exact ID and another shares its ID string as a
// path-alias on a different (active) session. The exact-ID branch (step 2)
// must win before the Title/path-alias branch (step 5) runs.
func TestResolveSessionTargetID_ExactIDWinsOverPathAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	target := createPoolSessionBead(t, fs.cityBeadStore, "anything", "s-gc-target", "active")
	// Second pool session whose Title masquerades as the first session's ID.
	createPoolSessionBead(t, fs.cityBeadStore, target.ID, "s-gc-decoy", "active")

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, target.ID)
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(%q): %v", target.ID, err)
	}
	if id != target.ID {
		t.Fatalf("resolved id = %q, want exact-ID bead %q", id, target.ID)
	}
}

// TestResolveSessionTargetID_ConfiguredNamedSessionWinsOverPathAlias seeds
// a configured named-session with identity "myrig/worker" alongside a pool
// session whose Title shadows that identity. The configured-named-session
// branch (step 3) must win before the Title/path-alias branch (step 5).
func TestResolveSessionTargetID_ConfiguredNamedSessionWinsOverPathAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	canonical, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "configured-canonical",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "test-city--worker",
			"alias":                     "myrig/worker",
			"configured_named_session":  "true",
			"configured_named_identity": "myrig/worker",
			"configured_named_mode":     "on_demand",
			"continuity_eligible":       "true",
			"state":                     "active",
			"template":                  "myrig/worker",
		},
	})
	if err != nil {
		t.Fatalf("create canonical named session: %v", err)
	}
	// Pool session whose Title shadows the named-session identity.
	createPoolSessionBead(t, fs.cityBeadStore, "myrig/worker", "s-gc-pool-shadow", "active")

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(myrig/worker): %v", err)
	}
	if id != canonical.ID {
		t.Fatalf("resolved id = %q, want configured named-session %q", id, canonical.ID)
	}
}

// TestResolveSessionTargetID_PathAliasUnknownNotFound confirms unrelated
// identifiers still return apiSessionTargetNotFound — the new branch only
// matches active pool sessions.
func TestResolveSessionTargetID_PathAliasUnknownNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	createPoolSessionBead(t, fs.cityBeadStore, "real-pl", "s-gc-pool-real", "active")

	_, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "ghost-pl")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDWithConfig(ghost-pl) = %v, want ErrSessionNotFound", err)
	}
}

// TestResolveLiveSessionByPathAlias_SkipsConfiguredNamedSessions guards the
// invariant that the path-alias resolver does not attempt to own configured
// named-session beads — those are handled by the dedicated config-driven
// branch (and its orphan-rejection safety net).
func TestResolveLiveSessionByPathAlias_SkipsConfiguredNamedSessions(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "named-pl",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "test-city--named",
			"alias":                     "named-pl",
			"configured_named_session":  "true",
			"configured_named_identity": "named-pl",
			"state":                     "active",
		},
	}); err != nil {
		t.Fatalf("create named-session bead: %v", err)
	}

	id, ok, err := resolveLiveSessionByPathAlias(store, "named-pl")
	if err != nil {
		t.Fatalf("resolveLiveSessionByPathAlias: %v", err)
	}
	if ok || id != "" {
		t.Fatalf("resolveLiveSessionByPathAlias matched configured named-session bead (id=%q ok=%v); want skip", id, ok)
	}
}

func TestResolveLiveSessionByPathAlias_EmptyIdentifier(t *testing.T) {
	store := beads.NewMemStore()
	if _, _, err := resolveLiveSessionByPathAlias(store, "  "); err != nil {
		t.Fatalf("resolveLiveSessionByPathAlias(whitespace): %v", err)
	}
}

func TestResolveLiveSessionByPathAlias_NilStore(t *testing.T) {
	id, ok, err := resolveLiveSessionByPathAlias(nil, "anything")
	if err != nil || ok || id != "" {
		t.Fatalf("resolveLiveSessionByPathAlias(nil) = (%q, %v, %v), want (\"\", false, nil)", id, ok, err)
	}
}

// TestResolveSessionTargetID_PathAliasResolvesViaContextHelper exercises the
// context-aware entry point used by /extmsg/inbound and gc session nudge.
func TestResolveSessionTargetID_PathAliasResolvesViaContextHelper(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	pool := createPoolSessionBead(t, fs.cityBeadStore, "extmsg-pl", "s-gc-extmsg", "active")

	id, err := srv.resolveSessionTargetIDWithContext(context.Background(), fs.cityBeadStore, "extmsg-pl", apiSessionResolveOptions{})
	if err != nil {
		t.Fatalf("resolveSessionTargetIDWithContext(extmsg-pl): %v", err)
	}
	if id != pool.ID {
		t.Fatalf("resolved id = %q, want %q", id, pool.ID)
	}
}

// TestResolveSessionTargetID_SessionNameWinsOverPathAliasTitle verifies the
// resolver-chain ordering: when one bead's session_name matches the
// identifier and a different bead's Title matches the same identifier,
// session.ResolveSessionID (session_name/alias index) wins. The Title-based
// path-alias step runs after, so its match is used only when nothing more
// specific resolved. Guards against the cross-bead collision codex flagged
// during /gascity-ship review.
func TestResolveSessionTargetID_SessionNameWinsOverPathAliasTitle(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	// Bead A: session_name match (session.ResolveSessionID will catch this).
	matchByName := createPoolSessionBead(t, fs.cityBeadStore, "different-title", "shared-id", "active")

	// Bead B: Title match for the same identifier (path-alias step would
	// otherwise catch this).
	createPoolSessionBead(t, fs.cityBeadStore, "shared-id", "different-name", "active")

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "shared-id")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(shared-id): %v", err)
	}
	if id != matchByName.ID {
		t.Fatalf("resolved id = %q, want session_name match %q (path-alias should run after session.ResolveSessionID)", id, matchByName.ID)
	}
}
