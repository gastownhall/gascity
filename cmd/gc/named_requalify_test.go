package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// Requalification of a top-level named session (gastownhall/gascity#5957):
// `gc rig add --adopt` binds the pre-adopt `golden-hon` to
// contentbuild/lane.golden-hon, and the live row the pre-adopt config
// minted still carries the bare identity in session_name,
// configured_named_identity and alias. These tests pin the rename-in-place
// contract: the first tick finds that row for the qualified spec and
// renames identity and alias on it — no second row, no drain, no release
// of the work still assigned under the bare form.

const (
	requalifyQualified = "contentbuild/lane.golden-hon"
	requalifyBare      = "golden-hon"
)

// requalifyCity is the post-adopt config: only the bound, rig-scoped entry.
func requalifyCity(mode string) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "contentbuild", Path: "contentbuild"}},
		Agents: []config.Agent{
			{Name: "golden-hon", Dir: "contentbuild", BindingName: "lane", StartCommand: "true", MaxActiveSessions: intPtr(1)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane", Mode: mode},
		},
	}
}

// requalifySeedRow seeds the live row exactly as the pre-adopt config left
// it: bare identity everywhere, started, with the baseline a config-drift
// compare would trip over once the template requalifies.
func requalifySeedRow(t *testing.T, store beads.Store, mode string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  requalifyBare,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               requalifyBare,
			"alias":                      requalifyBare,
			"template":                   requalifyBare,
			"agent_name":                 requalifyBare,
			"state":                      "active",
			"session_origin":             "named",
			"wake_mode":                  "fresh",
			"command":                    "true",
			"continuity_eligible":        "true",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: requalifyBare,
			namedSessionModeMetadata:     mode,
			"started_config_hash":        "v6:stale-core",
			"started_live_hash":          "v6:stale-live",
			"live_hash":                  "v6:stale-live",
			"started_provision_hash":     "v6:stale-prov",
			"started_launch_hash":        "v6:stale-launch",
			"core_hash_breakdown":        "{}",
		},
	})
	if err != nil {
		t.Fatalf("seeding the pre-adopt row: %v", err)
	}
	return b
}

// requalifySeedWork claims open work under the bare assignee, the form the
// lane's gate work was stamped with.
func requalifySeedWork(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	w, err := store.Create(beads.Bead{Title: "hon gate", Type: "task", Assignee: requalifyBare})
	if err != nil {
		t.Fatalf("seeding work: %v", err)
	}
	status := "in_progress"
	if err := store.Update(w.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("claiming work: %v", err)
	}
	return w
}

// requalifyTick runs one legacy reconciler tick — the desired-state build
// followed by the session-beads sync — and returns the desired state it
// produced plus the tick's stderr.
func requalifyTick(t *testing.T, cityPath string, cfg *config.City, store beads.Store, sp runtime.Provider, clk *clock.Fake, stderr *bytes.Buffer) map[string]TemplateParams {
	t.Helper()
	ds := buildDesiredState("test-city", cityPath, clk.Now(), cfg, sp, store, stderr)
	syncSessionBeads(cityPath, store, ds.State, sp, allConfiguredDS(ds.State), cfg, clk, stderr, false)
	return ds.State
}

// requalifyOpenRows lists the store's live session rows.
func requalifyOpenRows(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Label: sessionBeadLabel, Live: true})
	if err != nil {
		t.Fatalf("listing session rows: %v", err)
	}
	return rows
}

// AC-1: one legacy tick renames the requalified named session in place. The
// same bead keeps its session_name, gains the qualified identity and alias,
// records the bare form in alias_history, rebaselines its drift baseline to
// the requalified template's core hash, and no second row is minted.
func TestLegacyTickRenamesRequalifiedNamedSessionInPlace(t *testing.T) {
	store := beads.NewMemStore()
	cfg := requalifyCity("on_demand")
	row := requalifySeedRow(t, store, "on_demand")
	work := requalifySeedWork(t, store)
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), requalifyBare, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cityPath := t.TempDir()
	clk := &clock.Fake{Time: namedEffectNow}
	var stderr bytes.Buffer
	desired := requalifyTick(t, cityPath, cfg, store, transportFake{sp}, clk, &stderr)

	got, err := store.Get(row.ID)
	if err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("the row was closed: %v", got.Metadata)
	}
	m := got.Metadata
	if m["session_name"] != requalifyBare {
		t.Errorf("session_name = %q, want the row to keep %q", m["session_name"], requalifyBare)
	}
	if m[namedSessionIdentityMetadata] != requalifyQualified {
		t.Errorf("configured_named_identity = %q, want %q", m[namedSessionIdentityMetadata], requalifyQualified)
	}
	if m["alias"] != requalifyQualified {
		t.Errorf("alias = %q, want %q", m["alias"], requalifyQualified)
	}
	if !strings.Contains(m["alias_history"], requalifyBare) {
		t.Errorf("alias_history = %q, want it to contain %q", m["alias_history"], requalifyBare)
	}
	// The drift baseline moved onto the requalified template: every
	// rebaseline key left its stale seed behind, and the stored core hash
	// is the row's own resolution's, so the next tick's drift compare sees
	// no drift.
	for _, k := range []string{"started_config_hash", "started_live_hash", "live_hash", "started_provision_hash", "started_launch_hash"} {
		if !strings.HasPrefix(m[k], runtime.FingerprintVersion+":") || strings.Contains(m[k], "stale") {
			t.Errorf("%s = %q, want a fresh %s-prefixed hash", k, m[k], runtime.FingerprintVersion)
		}
	}
	tp := desired[m["session_name"]]
	if tp.SessionName == "" {
		t.Fatalf("no desired entry for the row's session_name %q", m["session_name"])
	}
	want := runtime.CoreFingerprint(sessionCoreConfigForHashInfo(tp, sessiontest.SeedBead(t, got)))
	if m["started_config_hash"] != want {
		t.Errorf("started_config_hash = %q, want the requalified resolution's core hash %q", m["started_config_hash"], want)
	}
	// No contentbuild--lane__golden-hon row exists: the rename happened in
	// place, on the original bead.
	spec, ok := findNamedSessionSpec(cfg, "test-city", requalifyQualified)
	if !ok {
		t.Fatal("no spec for the requalified identity")
	}
	for _, b := range requalifyOpenRows(t, store) {
		if b.ID != row.ID {
			t.Errorf("a second open row %s exists: session_name=%q", b.ID, b.Metadata["session_name"])
		}
		if strings.TrimSpace(b.Metadata["session_name"]) == spec.SessionName {
			t.Errorf("row %s took the canonical runtime name %q", b.ID, spec.SessionName)
		}
	}
	// The work is untouched: still in_progress, still assigned to the bare
	// form, still owned by the same live row.
	w, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("reading work: %v", err)
	}
	if w.Status != "in_progress" || w.Assignee != requalifyBare {
		t.Errorf("work = %s/%q, want in_progress assigned %q", w.Status, w.Assignee, requalifyBare)
	}
	if !liveOpenSessionAssignmentExists(store, requalifyBare) {
		t.Error("liveOpenSessionAssignmentExists(golden-hon) = false, want the renamed row to still own the bare-assigned work")
	}
	if s := stderr.String(); strings.Contains(s, "retiring") || strings.Contains(s, "closing") {
		t.Errorf("tick stderr mentions retiring/closing: %s", s)
	}
}
