package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestFactoryCityConfigStampsConfiguredNamedSession pins the NewFactory
// wiring of FactoryConfig.CityConfig into the session.Manager (SESSION-ID-014,
// ga-m42o8w): a session created through a factory handle for a configured
// named session's canonical alias is stamped from the city config whether the
// caller omits the claim or sends a false one. Without the wiring the Manager
// has no config and trusts the caller's ExtraMeta.
func TestFactoryCityConfigStampsConfiguredNamedSession(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Template: "mayor"}},
	}
	for name, metadata := range map[string]map[string]string{
		"empty ExtraMeta": nil,
		"false claim": {
			sessionpkg.NamedSessionMetadataKey:      "false",
			sessionpkg.NamedSessionIdentityMetadata: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			factory, err := NewFactory(FactoryConfig{
				CityPath:   t.TempDir(),
				Store:      store,
				Provider:   runtime.NewFake(),
				CityConfig: cfg,
			})
			if err != nil {
				t.Fatalf("NewFactory: %v", err)
			}
			handle, err := factory.Session(SessionSpec{
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
			info, err := handle.Create(context.Background(), CreateModeDeferred)
			if err != nil {
				t.Fatalf("Create(deferred): %v", err)
			}
			b, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("store.Get: %v", err)
			}
			if got := b.Metadata[sessionpkg.NamedSessionMetadataKey]; got != "true" {
				t.Errorf("%s = %q, want true (NewFactory must wire CityConfig into the Manager)", sessionpkg.NamedSessionMetadataKey, got)
			}
			if got := b.Metadata[sessionpkg.NamedSessionIdentityMetadata]; got != "mayor" {
				t.Errorf("%s = %q, want mayor", sessionpkg.NamedSessionIdentityMetadata, got)
			}
		})
	}
}
