package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

func TestExecutePlannedStarts_WorkQueryPreflight(t *testing.T) {
	t.Run("empty ready result skips every on-demand pool role before pre-wake", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		store := beads.NewMemStore()
		clk := &clock.Fake{Time: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
		cfg := &config.City{
			Workspace: config.Workspace{Name: "preflight-city"},
			Agents:    []config.Agent{{Name: "builder"}, {Name: "reviewer"}},
		}
		builder, builderTP := preflightPoolCandidate(t, store, "builder", "builder-1")
		reviewer, reviewerTP := preflightPoolCandidate(t, store, "reviewer", "reviewer-1")
		sp := runtime.NewFake()
		queried := map[string]bool{}

		got := executePlannedStartsTraced(
			context.Background(),
			[]startCandidate{builder, reviewer},
			cfg,
			map[string]TemplateParams{"builder-1": builderTP, "reviewer-1": reviewerTP},
			sp,
			store,
			"preflight-city",
			t.TempDir(),
			clk,
			events.Discard,
			time.Minute,
			ioDiscard{},
			ioDiscard{},
			nil,
			withPoolStartWorkQueryRunner(func(_ string, _ string, env map[string]string) (string, error) {
				queried[env["GC_TEMPLATE"]] = true
				if env["GC_SESSION_ID"] == "" || env["GC_SESSION_NAME"] == "" ||
					env["GC_ALIAS"] == "" || env["GC_AGENT"] == "" {
					t.Fatalf("preflight identity env is incomplete: %#v", env)
				}
				return "[]", nil
			}),
		)
		if got != 0 {
			t.Fatalf("woken = %d, want 0", got)
		}
		if !queried["builder"] || !queried["reviewer"] {
			t.Fatalf("queried templates = %#v, want both pool roles", queried)
		}
		if starts := countProviderStarts(sp); starts != 0 {
			t.Fatalf("provider Start calls = %d, want 0", starts)
		}
		for _, candidate := range []startCandidate{builder, reviewer} {
			updated, err := store.Get(candidate.info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := updated.Metadata["last_woke_at"]; got != "" {
				t.Fatalf("%s last_woke_at = %q, want unchanged empty before preflight passes", candidate.logicalTemplate(cfg), got)
			}
		}
	})

	t.Run("ready work starts the pool session", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		store := beads.NewMemStore()
		clk := &clock.Fake{Time: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
		cfg := &config.City{Workspace: config.Workspace{Name: "preflight-city"}, Agents: []config.Agent{{Name: "builder"}}}
		candidate, tp := preflightPoolCandidate(t, store, "builder", "builder-1")
		sp := runtime.NewFake()

		got := executePlannedStartsTraced(
			context.Background(),
			[]startCandidate{candidate},
			cfg,
			map[string]TemplateParams{"builder-1": tp},
			sp,
			store,
			"preflight-city",
			t.TempDir(),
			clk,
			events.Discard,
			time.Minute,
			ioDiscard{},
			ioDiscard{},
			nil,
			withPoolStartWorkQueryRunner(func(_ string, _ string, env map[string]string) (string, error) {
				if got := env["GC_SESSION_ID"]; got != candidate.info.ID {
					t.Fatalf("GC_SESSION_ID = %q, want %q", got, candidate.info.ID)
				}
				if got := env["GC_SESSION_NAME"]; got != candidate.name() {
					t.Fatalf("GC_SESSION_NAME = %q, want %q", got, candidate.name())
				}
				if got := env["GC_ALIAS"]; got != "builder-1" {
					t.Fatalf("GC_ALIAS = %q, want builder-1", got)
				}
				if got := env["GC_AGENT"]; got != "builder-1" {
					t.Fatalf("GC_AGENT = %q, want builder-1", got)
				}
				if got := env["GC_TEMPLATE"]; got != "builder" {
					t.Fatalf("GC_TEMPLATE = %q, want builder", got)
				}
				return `[{"id":"work-1"}]`, nil
			}),
		)
		if got != 1 {
			t.Fatalf("woken = %d, want 1", got)
		}
		if starts := countProviderStarts(sp); starts != 1 {
			t.Fatalf("provider Start calls = %d, want 1", starts)
		}
	})

	t.Run("existing assignment bypasses a cold-demand query", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		store := beads.NewMemStore()
		clk := &clock.Fake{Time: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
		cfg := &config.City{Workspace: config.Workspace{Name: "preflight-city"}, Agents: []config.Agent{{Name: "builder"}}}
		candidate, tp := preflightPoolCandidate(t, store, "builder", "builder-1")
		status := "in_progress"
		if _, err := store.Create(beads.Bead{ID: "work-1", Title: "claimed work", Type: "task", Status: status, Assignee: candidate.info.ID}); err != nil {
			t.Fatal(err)
		}
		sp := runtime.NewFake()
		runs := 0

		got := executePlannedStartsTraced(
			context.Background(),
			[]startCandidate{candidate},
			cfg,
			map[string]TemplateParams{"builder-1": tp},
			sp,
			store,
			"preflight-city",
			t.TempDir(),
			clk,
			events.Discard,
			time.Minute,
			ioDiscard{},
			ioDiscard{},
			nil,
			withPoolStartWorkQueryRunner(func(string, string, map[string]string) (string, error) {
				runs++
				return "[]", nil
			}),
		)
		if runs != 0 {
			t.Fatalf("preflight runner calls = %d, want 0 for an already assigned session", runs)
		}
		if got != 1 || countProviderStarts(sp) != 1 {
			t.Fatalf("woken = %d, provider starts = %d; want 1, 1", got, countProviderStarts(sp))
		}
	})

	t.Run("query error fails closed and logs its outcome", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		store := beads.NewMemStore()
		clk := &clock.Fake{Time: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
		cfg := &config.City{Workspace: config.Workspace{Name: "preflight-city"}, Agents: []config.Agent{{Name: "builder"}}}
		candidate, tp := preflightPoolCandidate(t, store, "builder", "builder-1")
		sp := runtime.NewFake()
		var stderr bytes.Buffer

		got := executePlannedStartsTraced(
			context.Background(),
			[]startCandidate{candidate},
			cfg,
			map[string]TemplateParams{"builder-1": tp},
			sp,
			store,
			"preflight-city",
			t.TempDir(),
			clk,
			events.Discard,
			time.Minute,
			ioDiscard{},
			&stderr,
			nil,
			withPoolStartWorkQueryRunner(func(string, string, map[string]string) (string, error) {
				return "", errors.New("store unavailable")
			}),
		)
		if got != 0 {
			t.Fatalf("woken = %d, want 0", got)
		}
		if starts := countProviderStarts(sp); starts != 0 {
			t.Fatalf("provider Start calls = %d, want 0", starts)
		}
		if got := stderr.String(); !bytes.Contains([]byte(got), []byte("work_query_preflight_failed")) {
			t.Fatalf("stderr = %q, want observable work_query_preflight_failed outcome", got)
		}
	})

	t.Run("configured floor named and non-model sessions bypass the preflight", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		store := beads.NewMemStore()
		clk := &clock.Fake{Time: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
		minActive := 1
		cfg := &config.City{
			Workspace: config.Workspace{Name: "preflight-city"},
			Agents: []config.Agent{
				{Name: "floor", MinActiveSessions: &minActive},
				{Name: "persistent"},
				{Name: "daemon"},
			},
		}
		floor, floorTP := preflightPoolCandidate(t, store, "floor", "floor-1")
		elastic, elasticTP := preflightPoolCandidate(t, store, "floor", "floor-2")
		persistent, persistentTP := preflightPoolCandidate(t, store, "persistent", "persistent-1")
		daemon, daemonTP := preflightPoolCandidate(t, store, "daemon", "daemon-1")
		daemonTP.ResolvedProvider = &config.ResolvedProvider{PromptMode: "none"}
		daemon.tp = daemonTP
		persistent.info.ConfiguredNamedSession = true
		if err := store.SetMetadata(persistent.info.ID, "configured_named_session", "true"); err != nil {
			t.Fatal(err)
		}
		sp := runtime.NewFake()
		runs := 0

		got := executePlannedStartsTraced(
			context.Background(),
			[]startCandidate{floor, elastic, persistent, daemon},
			cfg,
			map[string]TemplateParams{"floor-1": floorTP, "floor-2": elasticTP, "persistent-1": persistentTP, "daemon-1": daemonTP},
			sp,
			store,
			"preflight-city",
			t.TempDir(),
			clk,
			events.Discard,
			time.Minute,
			ioDiscard{},
			ioDiscard{},
			nil,
			withPoolStartWorkQueryRunner(func(string, string, map[string]string) (string, error) {
				runs++
				return "[]", nil
			}),
		)
		if runs != 1 {
			t.Fatalf("preflight runner calls = %d, want 1 for the elastic pool session only", runs)
		}
		if got != 3 {
			t.Fatalf("woken = %d, want 3", got)
		}
		if starts := countProviderStarts(sp); starts != 3 {
			t.Fatalf("provider Start calls = %d, want 3", starts)
		}
	})
}

func preflightPoolCandidate(t *testing.T, store beads.Store, template, name string) (startCandidate, TemplateParams) {
	t.Helper()
	session, err := store.Create(beads.Bead{
		ID:     "gc-" + name,
		Title:  template,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:" + name},
		Metadata: map[string]string{
			"template":     template,
			"agent_name":   name,
			"alias":        name,
			"session_name": name,
			"state":        "start-pending",
			"pool_managed": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tp := TemplateParams{
		Command:      "agent",
		SessionName:  name,
		TemplateName: template,
		Alias:        name,
		Env: map[string]string{
			"GC_AGENT":    name,
			"GC_ALIAS":    name,
			"GC_TEMPLATE": template,
		},
	}
	return startCandidate{info: sessiontest.SeedBead(t, session), tp: tp}, tp
}

func countProviderStarts(sp *runtime.Fake) int {
	count := 0
	for _, call := range sp.Calls {
		if call.Method == "Start" {
			count++
		}
	}
	return count
}
