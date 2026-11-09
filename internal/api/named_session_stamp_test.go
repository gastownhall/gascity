package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// namedStampClaims are the two caller claims every wiring test sends:
// nothing at all, and a false claim for the configured session.
func namedStampClaims() map[string]map[string]string {
	return map[string]map[string]string{
		"empty ExtraMeta": nil,
		"false claim": {
			session.NamedSessionMetadataKey:      "false",
			session.NamedSessionIdentityMetadata: "",
		},
	}
}

func withConfiguredMayor(cfg *config.City) {
	cfg.Agents = append(cfg.Agents, config.Agent{Name: "mayor"})
	cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: "mayor"})
}

func assertMayorStamped(t *testing.T, fs *fakeState, id string) {
	t.Helper()
	b, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", id, err)
	}
	if got := b.Metadata[session.NamedSessionMetadataKey]; got != "true" {
		t.Errorf("%s = %q, want true (city config must reach the session Manager)", session.NamedSessionMetadataKey, got)
	}
	if got := b.Metadata[session.NamedSessionIdentityMetadata]; got != "mayor" {
		t.Errorf("%s = %q, want mayor", session.NamedSessionIdentityMetadata, got)
	}
}

// TestSessionManagerStampsConfiguredNamedSession pins the
// session.WithCityConfig wiring in Server.sessionManager's cfg != nil branch
// (SESSION-ID-014, ga-m42o8w).
func TestSessionManagerStampsConfiguredNamedSession(t *testing.T) {
	for name, extra := range namedStampClaims() {
		t.Run(name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			withConfiguredMayor(fs.cfg)
			srv := New(fs)
			mgr := srv.sessionManager(beadstest.Sessions(fs.cityBeadStore))
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
				BeadOnly:     true,
				Alias:        "mayor",
				ExplicitName: "test-city--mayor",
				Template:     "mayor",
				Title:        "Mayor",
				Command:      "claude",
				WorkDir:      t.TempDir(),
				Provider:     "claude",
				ExtraMeta:    extra,
			})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			assertMayorStamped(t, fs, info.ID)
		})
	}
}

// TestWorkerFactoryStampsConfiguredNamedSession pins the
// FactoryConfig.CityConfig wiring in Server.workerFactory (SESSION-ID-014).
func TestWorkerFactoryStampsConfiguredNamedSession(t *testing.T) {
	for name, metadata := range namedStampClaims() {
		t.Run(name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			withConfiguredMayor(fs.cfg)
			srv := New(fs)
			factory, err := srv.workerFactory(beadstest.Sessions(fs.cityBeadStore))
			if err != nil {
				t.Fatalf("workerFactory: %v", err)
			}
			handle, err := factory.Session(worker.SessionSpec{
				Alias:        "mayor",
				ExplicitName: "test-city--mayor",
				Template:     "mayor",
				Title:        "Mayor",
				Command:      "claude",
				WorkDir:      t.TempDir(),
				Provider:     "claude",
				Metadata:     metadata,
			})
			if err != nil {
				t.Fatalf("factory.Session: %v", err)
			}
			info, err := handle.Create(context.Background(), worker.CreateModeDeferred)
			if err != nil {
				t.Fatalf("Create(deferred): %v", err)
			}
			assertMayorStamped(t, fs, info.ID)
		})
	}
}
