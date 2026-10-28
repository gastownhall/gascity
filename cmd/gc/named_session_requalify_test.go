package main

import (
	"io"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Issue #5957: gc rig add --adopt requalifies a city's top-level named
// session golden-hon to contentbuild/lane.golden-hon, and its live row still
// carries the bare identity.
const (
	requalifiedBare      = "golden-hon"
	requalifiedQualified = "contentbuild/lane.golden-hon"
	requalifiedRuntime   = "contentbuild--lane__golden-hon"
)

// requalifiedCity is the adopted config: golden-hon bound as lane in the
// contentbuild rig, in mode, and, when ambiguous, also bound as ops.
func requalifiedCity(t *testing.T, mode string, ambiguous bool) func() *config.City {
	rigPath := t.TempDir()
	return func() *config.City {
		cfg := &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs:      []config.Rig{{Name: "contentbuild", Path: rigPath}},
			Agents:    []config.Agent{{Name: requalifiedBare, Dir: "contentbuild", BindingName: "lane", StartCommand: "true"}},
			NamedSessions: []config.NamedSession{
				{Template: requalifiedBare, Dir: "contentbuild", BindingName: "lane", Mode: mode},
			},
		}
		if ambiguous {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: requalifiedBare, Dir: "contentbuild", BindingName: "ops", StartCommand: "true"})
			cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: requalifiedBare, Dir: "contentbuild", BindingName: "ops", Mode: mode})
		}
		return cfg
	}
}

// requalifiedStaleHash is the pre-adopt start's config hash, which the
// requalified template's never equals.
var requalifiedStaleHash = runtime.FingerprintVersion + ":pre-adopt"

// preAdoptRow is the running, started top-level named session's row, under
// its pre-adopt bare identity.
func preAdoptRow(mode string) beads.Bead {
	return sessionRow("gc-1",
		"template", requalifiedBare, "state", "active", "session_name", requalifiedBare, "alias", requalifiedBare,
		"configured_named_session", "true", "configured_named_identity", requalifiedBare, "configured_named_mode", mode,
		"generation", "1", "started_config_hash", requalifiedStaleHash)
}

// requalifiedWork is in-progress work assigned under the bare identity.
func requalifiedWork() beads.Bead {
	return beads.Bead{ID: "gw-1", Title: "gw-1", Type: "task", Status: "in_progress", Assignee: requalifiedBare}
}

// requalifiedFixture is the adopted city with the pre-adopt row running and
// its work in progress.
func requalifiedFixture(t *testing.T, mode string, ambiguous bool) parityFixture {
	return parityFixture{
		Name:     "requalified-" + mode,
		Rows:     []parityRow{preAdoptRow(mode)},
		Work:     []parityRow{requalifiedWork()},
		Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: rowToken("gc-1")}},
		City:     requalifiedCity(t, mode, ambiguous),
	}
}

// rowByID is the bead with id in all.
func rowByID(t *testing.T, all []beads.Bead, id string) beads.Bead {
	t.Helper()
	i := slices.IndexFunc(all, func(b beads.Bead) bool { return b.ID == id })
	if i < 0 {
		t.Fatalf("no bead %s", id)
	}
	return all[i]
}

// AC-1: one legacy tick renames the pre-adopt row in place to the qualified
// identity and rebaselines its drift hash, and mints no second row.
func TestLegacyRequalifiedNamedSessionMigratesBareRowInOneTick(t *testing.T) {
	f := requalifiedFixture(t, "always", false)
	cityPath := t.TempDir()
	w := newLegacyWorld(t, f, cityPath, f.beads())
	w.tick(t, true)

	all := w.list(t)
	row := rowByID(t, all, "gc-1")
	if row.Status != "open" {
		t.Fatalf("row status = %q, want open", row.Status)
	}
	for key, want := range map[string]string{
		"session_name":              requalifiedBare,
		"configured_named_identity": requalifiedQualified,
		"alias":                     requalifiedQualified,
	} {
		if got := row.Metadata[key]; got != want {
			t.Errorf("row %s = %q, want %q", key, got, want)
		}
	}
	if history := session.AliasHistory(row.Metadata); !slices.Contains(history, requalifiedBare) {
		t.Errorf("alias_history = %v, want it to contain %q", history, requalifiedBare)
	}
	for _, b := range all {
		if b.Metadata["session_name"] == requalifiedRuntime {
			t.Errorf("row %s (%s) was minted for the requalified session", b.ID, b.Status)
		}
	}

	snap, err := loadSessionBeadSnapshot(w.store)
	if err != nil {
		t.Fatal(err)
	}
	now := w.clk.Now()
	result := buildDesiredStateWithSessionBeadsAt(w.cfg.Workspace.Name, cityPath, now, now, w.cfg, w.sp, w.store, nil, snap, nil, io.Discard)
	tp, ok := result.State[requalifiedBare]
	if !ok {
		t.Fatalf("desired state has no %s entry: %v", requalifiedBare, mapKeys(result.State))
	}
	if tp.TemplateName != requalifiedQualified {
		t.Fatalf("desired template = %q, want %q", tp.TemplateName, requalifiedQualified)
	}
	want := runtime.CoreFingerprint(sessionCoreConfigForHashInfo(tp, sessionInfoFromBead(row)))
	if got := row.Metadata["started_config_hash"]; got != want {
		t.Errorf("started_config_hash = %q, want the requalified template's %q (stale %q)", got, want, requalifiedStaleHash)
	}
}
