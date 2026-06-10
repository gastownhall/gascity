package api

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Characterization fixtures for the resolveSessionTargetIDWithContext
// precedence ladder (SESSION-ID-003/004 plus the API-level steps around
// them). These pin the ordering BEFORE the target-classification extraction
// so the classifier adapter can prove parity cell by cell. Template-form
// rejection, config-orphan rejection, and path-alias state filtering are
// already pinned elsewhere (phase0 interface spec,
// session_materialization_guard_test.go, session_resolution_path_alias_test.go).

func precedenceTestServer(t *testing.T) (*Server, *fakeState) {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	return New(fs), fs
}

func createPrecedenceSessionBead(t *testing.T, store beads.Store, title string, metadata map[string]string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    title,
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("create session bead %q: %v", title, err)
	}
	return b
}

// Exact bead ID outranks every other vector: a live session whose
// session_name spells another bead's ID must not shadow direct-ID lookup.
func TestResolveSessionTargetID_ExactIDBeatsSessionNameMatch(t *testing.T) {
	srv, fs := precedenceTestServer(t)

	target := createPrecedenceSessionBead(t, fs.cityBeadStore, "target", map[string]string{
		"session_name": "s-target-1",
		"state":        "active",
	})
	createPrecedenceSessionBead(t, fs.cityBeadStore, "squatter", map[string]string{
		"session_name": target.ID,
		"state":        "active",
	})

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, target.ID)
	if err != nil {
		t.Fatalf("resolve by exact ID: %v", err)
	}
	if id != target.ID {
		t.Fatalf("resolved %q, want exact-ID match %q (session_name squatter must not win)", id, target.ID)
	}
}

// Within live resolution, exact session_name outranks exact alias.
func TestResolveSessionTargetID_SessionNameBeatsAlias(t *testing.T) {
	srv, fs := precedenceTestServer(t)

	createPrecedenceSessionBead(t, fs.cityBeadStore, "alias-holder", map[string]string{
		"session_name": "s-alias-holder",
		"alias":        "shared-token",
		"state":        "active",
	})
	nameHolder := createPrecedenceSessionBead(t, fs.cityBeadStore, "name-holder", map[string]string{
		"session_name": "shared-token",
		"state":        "active",
	})

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "shared-token")
	if err != nil {
		t.Fatalf("resolve shared token: %v", err)
	}
	if id != nameHolder.ID {
		t.Fatalf("resolved %q, want session_name match %q to beat the alias match", id, nameHolder.ID)
	}
}

// Live session_name/alias matches outrank path-alias title matches, even
// when the live match is asleep and the title match is active.
func TestResolveSessionTargetID_LiveMatchBeatsPathAliasTitle(t *testing.T) {
	srv, fs := precedenceTestServer(t)

	asleep := createPrecedenceSessionBead(t, fs.cityBeadStore, "asleep-holder", map[string]string{
		"session_name": "contested",
		"state":        "asleep",
	})
	createPrecedenceSessionBead(t, fs.cityBeadStore, "contested", map[string]string{
		"session_name": "s-title-holder",
		"state":        "active",
	})

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "contested")
	if err != nil {
		t.Fatalf("resolve contested token: %v", err)
	}
	if id != asleep.ID {
		t.Fatalf("resolved %q, want live session_name match %q to beat the path-alias title match", id, asleep.ID)
	}
}

// Open matches outrank closed matches on allow-closed surfaces: closed
// lookup only runs after every live vector misses.
func TestResolveSessionTargetID_AllowClosedPrefersOpenAlias(t *testing.T) {
	srv, fs := precedenceTestServer(t)

	closed := createPrecedenceSessionBead(t, fs.cityBeadStore, "closed-name-holder", map[string]string{
		"session_name": "reused-token",
		"state":        "active",
	})
	if err := fs.cityBeadStore.Close(closed.ID); err != nil {
		t.Fatalf("close bead: %v", err)
	}
	open := createPrecedenceSessionBead(t, fs.cityBeadStore, "open-alias-holder", map[string]string{
		"session_name": "s-open-holder",
		"alias":        "reused-token",
		"state":        "active",
	})

	id, err := srv.resolveSessionIDAllowClosedWithConfig(fs.cityBeadStore, "reused-token")
	if err != nil {
		t.Fatalf("allow-closed resolve: %v", err)
	}
	if id != open.ID {
		t.Fatalf("resolved %q, want open alias match %q to beat the closed session_name match", id, open.ID)
	}
}

// Within closed lookup, closed session_name outranks closed alias —
// mirroring the live ordering.
func TestResolveSessionTargetID_ClosedSessionNameBeatsClosedAlias(t *testing.T) {
	srv, fs := precedenceTestServer(t)

	closedAlias := createPrecedenceSessionBead(t, fs.cityBeadStore, "closed-alias-holder", map[string]string{
		"session_name": "s-closed-alias",
		"alias":        "gone-token",
		"state":        "active",
	})
	closedName := createPrecedenceSessionBead(t, fs.cityBeadStore, "closed-name-holder", map[string]string{
		"session_name": "gone-token",
		"state":        "active",
	})
	for _, b := range []beads.Bead{closedAlias, closedName} {
		if err := fs.cityBeadStore.Close(b.ID); err != nil {
			t.Fatalf("close bead %s: %v", b.ID, err)
		}
	}

	id, err := srv.resolveSessionIDAllowClosedWithConfig(fs.cityBeadStore, "gone-token")
	if err != nil {
		t.Fatalf("allow-closed resolve: %v", err)
	}
	if id != closedName.ID {
		t.Fatalf("resolved %q, want closed session_name match %q to beat the closed alias match", id, closedName.ID)
	}
}

// Closed lookup is not reachable for configured named-session identities:
// the named-spec rejection runs first, so a closed bead for a reserved
// identity stays not-found on allow-closed query surfaces.
func TestResolveSessionTargetID_AllowClosedRejectsConfiguredNamedTargets(t *testing.T) {
	srv, fs := precedenceTestServer(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "myrig",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "myrig",
		}},
		Rigs: []config.Rig{{Name: "myrig", Path: "/tmp/myrig"}},
	}

	if config.FindNamedSession(fs.cfg, "myrig/worker") == nil {
		t.Fatal("expected configured named session myrig/worker")
	}
	runtimeName := config.NamedSessionRuntimeName(fs.cfg.EffectiveCityName(), fs.cfg.Workspace, "myrig/worker")
	closed := createPrecedenceSessionBead(t, fs.cityBeadStore, "retired", map[string]string{
		"session_name": runtimeName,
		"state":        "active",
	})
	if err := fs.cityBeadStore.Close(closed.ID); err != nil {
		t.Fatalf("close bead: %v", err)
	}

	for _, target := range []string{"myrig/worker", runtimeName} {
		_, err := srv.resolveSessionIDAllowClosedWithConfig(fs.cityBeadStore, target)
		if !errors.Is(err, session.ErrSessionNotFound) {
			t.Fatalf("allow-closed resolve(%q) = %v, want ErrSessionNotFound (named-spec rejection precedes closed lookup)", target, err)
		}
	}
}

// Closed lookup is reachable only on allow-closed surfaces: the same closed
// bead resolves through the allow-closed resolver and stays not-found on
// the live-only resolver. Pins the opts-to-facts wiring, not just the
// classifier-side gate.
func TestResolveSessionTargetID_ClosedLookupRequiresAllowClosedSurface(t *testing.T) {
	srv, fs := precedenceTestServer(t)

	closed := createPrecedenceSessionBead(t, fs.cityBeadStore, "departed", map[string]string{
		"session_name": "gone-token",
		"state":        "active",
	})
	if err := fs.cityBeadStore.Close(closed.ID); err != nil {
		t.Fatalf("close bead: %v", err)
	}

	if id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "gone-token"); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("live-only resolve(gone-token) = %q, %v; want ErrSessionNotFound", id, err)
	}
	id, err := srv.resolveSessionIDAllowClosedWithConfig(fs.cityBeadStore, "gone-token")
	if err != nil {
		t.Fatalf("allow-closed resolve(gone-token): %v", err)
	}
	if id != closed.ID {
		t.Fatalf("allow-closed resolved %q, want closed bead %q", id, closed.ID)
	}
}

// A configured-name conflict is terminal for the whole ladder: it must
// surface as the conflict error instead of falling through to a live
// ordinary session that spells the same bare token.
func TestResolveSessionTargetID_ConfiguredNameConflictBlocksLiveFallthrough(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil || !ok {
		t.Fatalf("findNamedSessionSpecForTarget(worker) = %v, %v; want spec", ok, err)
	}
	createPrecedenceSessionBead(t, fs.cityBeadStore, "squatter", map[string]string{
		"session_name": spec.SessionName,
		"template":     "other/worker",
		"agent_name":   "other/worker",
		"state":        "asleep",
	})
	live := createPrecedenceSessionBead(t, fs.cityBeadStore, "live-worker", map[string]string{
		"session_name": "worker",
		"state":        "active",
	})

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "worker")
	if !errors.Is(err, errConfiguredNamedSessionConflict) {
		t.Fatalf("resolve(worker) = %q, %v; want errConfiguredNamedSessionConflict, not fallthrough to %q", id, err, live.ID)
	}
}

// Template-form tokens are rejected before any lookup runs: a live session
// whose session_name spells the template token must stay unresolvable.
func TestResolveSessionTargetID_TemplateFormBeatsSessionNameSquatter(t *testing.T) {
	srv, fs := precedenceTestServer(t)

	createPrecedenceSessionBead(t, fs.cityBeadStore, "template-squatter", map[string]string{
		"session_name": "template:worker",
		"state":        "active",
	})

	if id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "template:worker"); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolve(template:worker) = %q, %v; want ErrSessionNotFound", id, err)
	}
}

// Config-orphan rejection holds on allow-closed surfaces too, and carries
// the rejected-by-config marker at the resolver level: the live named bead
// for a deconfigured identity is rejected rather than selected or deferred
// to path-alias/closed lookup.
func TestResolveSessionTargetID_AllowClosedStillRejectsConfigOrphan(t *testing.T) {
	srv, fs := precedenceTestServer(t)

	createPrecedenceSessionBead(t, fs.cityBeadStore, "orphan", map[string]string{
		"session_name":             "ghost-runtime",
		"state":                    "active",
		apiNamedSessionMetadataKey: "true",
		apiNamedSessionIdentityKey: "ghost/rig",
	})

	for _, resolve := range []struct {
		name string
		fn   func(target string) (string, error)
	}{
		{"live-only", func(target string) (string, error) {
			return srv.resolveSessionIDWithConfig(fs.cityBeadStore, target)
		}},
		{"allow-closed", func(target string) (string, error) {
			return srv.resolveSessionIDAllowClosedWithConfig(fs.cityBeadStore, target)
		}},
	} {
		id, err := resolve.fn("ghost-runtime")
		if !errors.Is(err, errSessionTargetRejectedByConfig) {
			t.Fatalf("%s resolve(ghost-runtime) = %q, %v; want rejected-by-config marker", resolve.name, id, err)
		}
	}
}
