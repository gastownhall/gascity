package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// TestWorkerFactoryWithAttributionStampsConfiguredNamedSession pins the
// FactoryConfig.CityConfig wiring in workerFactoryWithAttribution
// (SESSION-ID-014, ga-m42o8w): a session created through the CLI's factory for
// a configured named session's canonical alias is stamped from the city
// config whether the caller omits the claim or sends a false one.
func TestWorkerFactoryWithAttributionStampsConfiguredNamedSession(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Template: "mayor"}},
	}
	for name, metadata := range map[string]map[string]string{
		"empty ExtraMeta": nil,
		"false claim": {
			session.NamedSessionMetadataKey:      "false",
			session.NamedSessionIdentityMetadata: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			factory, err := workerFactoryWithAttribution(t.TempDir(), store, runtime.NewFake(), cfg, nil, nil, nil)
			if err != nil {
				t.Fatalf("workerFactoryWithAttribution: %v", err)
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
			b, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("store.Get: %v", err)
			}
			if got := b.Metadata[session.NamedSessionMetadataKey]; got != "true" {
				t.Errorf("%s = %q, want true (city config must reach the session Manager)", session.NamedSessionMetadataKey, got)
			}
			if got := b.Metadata[session.NamedSessionIdentityMetadata]; got != "mayor" {
				t.Errorf("%s = %q, want mayor", session.NamedSessionIdentityMetadata, got)
			}
		})
	}
}
