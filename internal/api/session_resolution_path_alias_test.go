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

// TestResolveSessionTargetID_PathAliasTiebreakerPrefersMostRecent verifies
// that when two active pool sessions share the same path-alias (Title) — a
// rare misconfiguration — the most-recently-created bead wins. CreatedAt is
// the documented tiebreaker.
func TestResolveSessionTargetID_PathAliasTiebreakerPrefersMostRecent(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	srv := New(fs)

	older := createPoolSessionBead(t, fs.cityBeadStore, "shared-pl", "s-gc-pool-older", "active")
	// Force a measurable CreatedAt gap: MemStore stamps CreatedAt at Create
	// time; sleeping 5ms keeps the test fast while preserving ordering.
	time.Sleep(5 * time.Millisecond)
	newer := createPoolSessionBead(t, fs.cityBeadStore, "shared-pl", "s-gc-pool-newer", "active")

	if !newer.CreatedAt.After(older.CreatedAt) {
		t.Fatalf("setup: newer.CreatedAt (%v) is not after older.CreatedAt (%v)", newer.CreatedAt, older.CreatedAt)
	}

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "shared-pl")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(shared path-alias): %v", err)
	}
	if id != newer.ID {
		t.Fatalf("resolved id = %q, want most-recent bead %q (older was %q)", id, newer.ID, older.ID)
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
// must win before the path-alias branch (step 4) runs.
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
// branch (step 3) must win before the path-alias branch (step 4).
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
