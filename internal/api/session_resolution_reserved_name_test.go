package api

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// Regression tests for ga-4of1nc: a configured named-session identity with no
// canonical bead is reserved. Non-materializing resolution must short-circuit
// after the named-session path (not-found, or conflict when a live bead blocks
// the identity) instead of falling through to ordinary live-session matching,
// where a rogue session could hijack the reserved name.

func createTestSessionBead(t *testing.T, store beads.Store, metadata map[string]string, title string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Type:     session.BeadType,
		Title:    title,
		Labels:   []string{session.LabelSession},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	return b
}

func TestResolveSessionTargetID_ReservedNameNotHijackedByRogueLiveSession(t *testing.T) {
	// fakeState configures named session myrig/worker (runtime session name
	// myrig--worker); no canonical bead exists, so the identity is reserved.
	rogues := map[string]struct {
		metadata map[string]string
		title    string
		target   string
	}{
		"session_name equals identity": {
			metadata: map[string]string{
				"session_name": "myrig/worker",
				"state":        "active",
			},
			target: "myrig/worker",
		},
		"path-alias title equals identity": {
			metadata: map[string]string{
				"session_name": "s-gc-001",
				"state":        "active",
			},
			title:  "myrig/worker",
			target: "myrig/worker",
		},
		"alias equals runtime session name": {
			metadata: map[string]string{
				"session_name": "rogue-runtime",
				"alias":        "myrig--worker",
				"state":        "active",
			},
			target: "myrig--worker",
		},
	}
	for name, tc := range rogues {
		t.Run(name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			srv := New(fs)
			rogue := createTestSessionBead(t, fs.cityBeadStore, tc.metadata, tc.title)

			resolvers := map[string]func(beads.Store, string) (string, error){
				"live":        srv.resolveSessionIDWithConfig,
				"allowClosed": srv.resolveSessionIDAllowClosedWithConfig,
			}
			for mode, resolve := range resolvers {
				id, err := resolve(fs.cityBeadStore, tc.target)
				if id == rogue.ID {
					t.Fatalf("%s resolution returned rogue session %q for reserved target %q", mode, id, tc.target)
				}
				if !errors.Is(err, session.ErrSessionNotFound) {
					t.Fatalf("%s resolution = %q, %v, want ErrSessionNotFound", mode, id, err)
				}
			}
		})
	}
}

func TestResolveSessionTargetID_BareLeafTargetStillFallsThroughToOrdinaryAlias(t *testing.T) {
	// "worker" is only a bare-leaf convenience token for the configured
	// myrig/worker named session, not the reserved identity itself; ordinary
	// sessions keep owning that alias on non-materializing lookups.
	fs := newSessionFakeState(t)
	srv := New(fs)
	ordinary := createTestSessionBead(t, fs.cityBeadStore, map[string]string{
		"session_name": "test-city--worker",
		"alias":        "worker",
		"state":        "active",
	}, "")

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "worker")
	if err != nil || id != ordinary.ID {
		t.Fatalf("resolution = %q, %v, want ordinary session %q", id, err, ordinary.ID)
	}
}

func TestResolveSessionTargetID_ReservedNameRogueAliasSurfacesConflict(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	rogue := createTestSessionBead(t, fs.cityBeadStore, map[string]string{
		"session_name": "rogue-runtime",
		"alias":        "myrig/worker",
		"state":        "active",
	}, "")

	id, err := srv.resolveSessionIDWithConfig(fs.cityBeadStore, "myrig/worker")
	if id == rogue.ID {
		t.Fatalf("resolution returned rogue session %q for reserved identity myrig/worker", id)
	}
	if !errors.Is(err, errConfiguredNamedSessionConflict) {
		t.Fatalf("resolution = %q, %v, want configured named session conflict", id, err)
	}
}

func TestResolveSessionTargetID_CanonicalNamedBeadWinsOverRogue(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	canonical := createTestSessionBead(t, fs.cityBeadStore, map[string]string{
		"session_name":              "test-city--worker",
		"alias":                     "myrig/worker",
		"configured_named_session":  "true",
		"configured_named_identity": "myrig/worker",
		"configured_named_mode":     "on_demand",
		"continuity_eligible":       "true",
		"state":                     "active",
		"template":                  "myrig/worker",
	}, "")
	createTestSessionBead(t, fs.cityBeadStore, map[string]string{
		"session_name": "myrig/worker",
		"state":        "active",
	}, "")

	for mode, resolve := range map[string]func(beads.Store, string) (string, error){
		"live":        srv.resolveSessionIDWithConfig,
		"allowClosed": srv.resolveSessionIDAllowClosedWithConfig,
	} {
		id, err := resolve(fs.cityBeadStore, "myrig/worker")
		if err != nil || id != canonical.ID {
			t.Fatalf("%s resolution = %q, %v, want canonical %q", mode, id, err, canonical.ID)
		}
	}
}

func TestResolveSessionTargetID_OrdinarySessionsUnaffectedByReservedNameGuard(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	ordinary := createTestSessionBead(t, fs.cityBeadStore, map[string]string{
		"session_name": "free-agent",
		"state":        "active",
	}, "")

	for mode, resolve := range map[string]func(beads.Store, string) (string, error){
		"live":        srv.resolveSessionIDWithConfig,
		"allowClosed": srv.resolveSessionIDAllowClosedWithConfig,
	} {
		id, err := resolve(fs.cityBeadStore, "free-agent")
		if err != nil || id != ordinary.ID {
			t.Fatalf("%s resolution = %q, %v, want %q", mode, id, err, ordinary.ID)
		}
	}
}
