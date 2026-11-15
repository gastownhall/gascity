package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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

// requalifyAmbiguousCity is the config in which the bare leaf maps to two
// configured named sessions: the forward bare rule cannot choose between
// them, so neither direction may resolve the bare row.
func requalifyAmbiguousCity(mode string) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "contentbuild", Path: "contentbuild"}},
		Agents: []config.Agent{
			{Name: requalifyBare, Dir: "contentbuild", BindingName: "lane", StartCommand: "true"},
			{Name: requalifyBare, Dir: "contentbuild", BindingName: "ops", StartCommand: "true"},
		},
		NamedSessions: []config.NamedSession{
			{Template: requalifyBare, Dir: "contentbuild", BindingName: "lane", Mode: mode},
			{Template: requalifyBare, Dir: "contentbuild", BindingName: "ops", Mode: mode},
		},
	}
}

// requalifySecondRigCity is requalifyCity once a second rig imports the same
// pack: other/lane.golden-hon binds the leaf too, so the leaf no longer
// resolves to one entry. A rename that already happened stands.
func requalifySecondRigCity(mode string) *config.City {
	cfg := requalifyCity(mode)
	cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "other", Path: "other"})
	cfg.Agents = append(cfg.Agents, config.Agent{Name: requalifyBare, Dir: "other", BindingName: "lane", StartCommand: "true", MaxActiveSessions: intPtr(1)})
	cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: requalifyBare, Dir: "other", BindingName: "lane", Mode: mode})
	return cfg
}

// requalifyShadowedCity is requalifyCity with a top-level named session
// configured under the leaf itself, which owns the bare name outright.
func requalifyShadowedCity(mode string) *config.City {
	cfg := requalifyCity(mode)
	cfg.Agents = append(cfg.Agents, config.Agent{Name: requalifyBare, StartCommand: "true", MaxActiveSessions: intPtr(1)})
	cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: requalifyBare, Mode: mode})
	return cfg
}

// AC-4: an ambiguous leaf keeps the bare row untouched. With two bindings
// claiming the bare identity the row is neither selected nor migrated, it is
// not stopped, closed, retired or unclaimed, and the tick logs a line naming
// both candidates.
func TestLegacyAmbiguousLeafKeepsTheBareRow(t *testing.T) {
	store := beads.NewMemStore()
	cfg := requalifyAmbiguousCity("on_demand")
	row := requalifySeedRow(t, store, "on_demand")
	work := requalifySeedWork(t, store)
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), requalifyBare, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cityPath := t.TempDir()
	clk := &clock.Fake{Time: namedEffectNow}
	var stderr bytes.Buffer
	requalifyTick(t, cityPath, cfg, store, transportFake{sp}, clk, &stderr)

	got, err := store.Get(row.ID)
	if err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("the row was closed: %v", got.Metadata)
	}
	m := got.Metadata
	if m[namedSessionIdentityMetadata] != requalifyBare || m["alias"] != requalifyBare || m["session_name"] != requalifyBare {
		t.Errorf("the row was migrated: identity=%q alias=%q session_name=%q, want all three bare", m[namedSessionIdentityMetadata], m["alias"], m["session_name"])
	}
	if s := stderr.String(); !strings.Contains(s, fmt.Sprintf("named identity %q matches 2 configured named sessions", requalifyBare)) ||
		!strings.Contains(s, requalifyQualified) || !strings.Contains(s, "contentbuild/ops."+requalifyBare) {
		t.Errorf("tick stderr does not name both candidates: %s", s)
	}
	if s := stderr.String(); strings.Contains(s, "retiring") || strings.Contains(s, "unclaim") {
		t.Errorf("tick stderr retired the row: %s", s)
	}
	for _, b := range requalifyOpenRows(t, store) {
		if b.ID != row.ID {
			t.Errorf("a second open row %s exists: session_name=%q", b.ID, b.Metadata["session_name"])
		}
	}
	w, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("reading work: %v", err)
	}
	if w.Status != "in_progress" || w.Assignee != requalifyBare {
		t.Errorf("work = %s/%q, want in_progress assigned %q", w.Status, w.Assignee, requalifyBare)
	}
	if !liveOpenSessionAssignmentExists(store, requalifyBare) {
		t.Error("liveOpenSessionAssignmentExists(golden-hon) = false, want the bare row to still own its work")
	}
	if !sp.IsRunning(requalifyBare) {
		t.Error("the runtime was stopped")
	}
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

// AC-5: a born-qualified replacement row cannot see pre-adopt work. The bare
// assignee matches none of a row minted under the qualified identity — this
// pin held at the design base and must keep holding, because it is exactly
// why a second row must not be minted: only the renamed pre-adopt row, whose
// alias_history carries the bare form, owns that work.
func TestBornQualifiedRowDoesNotMatchTheBareAssignee(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Title: "hon gate", Type: "task", Assignee: requalifyBare}); err != nil {
		t.Fatalf("seeding work: %v", err)
	}
	row, err := store.Create(beads.Bead{
		Title:  requalifyBare,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "contentbuild--lane__golden-hon",
			"alias":                      requalifyQualified,
			"template":                   "golden-hon",
			"state":                      "active",
			"session_origin":             "named",
			"continuity_eligible":        "true",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: requalifyQualified,
			// alias_history deliberately empty: the row was born qualified.
		},
	})
	if err != nil {
		t.Fatalf("seeding the born-qualified row: %v", err)
	}
	if liveOpenSessionAssignmentExists(store, requalifyBare) {
		t.Errorf("liveOpenSessionAssignmentExists(%q) = true, want the bare assignee to match no live row", requalifyBare)
	}
	for _, id := range session.AssigneeIdentities(sessionInfoFromBead(row)) {
		if id == requalifyBare {
			t.Errorf("session.AssigneeIdentities contains %q, want a born-qualified row to claim no bare identity", requalifyBare)
		}
	}
}

// requalifyDaemonTick runs one legacy controller tick the way the daemon does
// under session drains: the desired-state build, the session-beads sync with
// skipClose=true (city_runtime.go's syncBeadsAndUpdateIndex), then the bead
// reconciler over the assigned work a session's wake can reach, whose starts,
// drains and closes requalifyTick never reaches.
// rigStores attaches the rig stores a rig-scoped session's claims live in
// (nil for none), and dt carries the drain state across ticks, as the
// controller's tracker does.
func requalifyDaemonTick(t *testing.T, cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, sp runtime.Provider, dt *drainTracker, clk *clock.Fake, stderr *bytes.Buffer) {
	t.Helper()
	requalifyDaemonTickWith(t, cityPath, cfg, store, rigStores, sp, dt, clk, stderr, nil)
}

// requalifyDaemonTickWith is requalifyDaemonTick with the controller's
// lifecycle trackers wired in as city_runtime.go wires them: it into the bead
// reconciler, and the max-session-age and assigned-work defer trackers through
// opts (withMaxSessionAgeTracker, withAssignedWorkDeferTracker).
func requalifyDaemonTickWith(t *testing.T, cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, sp runtime.Provider, dt *drainTracker, clk *clock.Fake, stderr *bytes.Buffer, it idleTracker, opts ...startExecutionOption) {
	t.Helper()
	cityName := cfg.EffectiveCityName()
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loading the session snapshot: %v", err)
	}
	ds := buildDesiredStateWithSessionBeads(cityName, cityPath, clk.Now().UTC(), cfg, sp, store, rigStores, snapshot, nil, stderr)
	cfgNames := configuredSessionNames(cfg, cityName, store)
	syncSessionBeadsWithSnapshotAndRigStores(
		cityPath, beads.SessionStore{Store: store}, testSeatWork(cityPath, cfg, store, rigStores), ds.State, sp, cfgNames, cfg, clk, stderr, true, nil, nil,
	)
	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, ds.AssignedWorkBeads, sessionInfosFromBeads(sessions), ds.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, ds.NamedSessionDemand, cfg)
	snap := newSessionBeadSnapshot(sessions)
	// The controller hands the reconciler only the assigned work a session's
	// wake can reach, with its store-scoped readiness (city_runtime.go).
	awakeWork, awakeRefs, awakeStores := filterAssignedWorkBeadsForSessionWakeWithStores(cfg, cityPath, store, snap.OpenInfos(), ds.AssignedWorkBeads, ds.AssignedWorkStoreRefs, ds.AssignedWorkStores)
	opts = append([]startExecutionOption{
		withReadyAssignedFlags(readyAssignedFlagsForBeads(ds.ReadyAssigned, awakeWork, awakeRefs)),
		withAssignedWorkStores(awakeStores),
	}, opts...)
	var stdout bytes.Buffer
	reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), cityPath, snap.OpenForReconcile(), snap, ds.State, cfgNames, cfg, sp,
		store, testSeatWork(cityPath, cfg, store, rigStores), nil, awakeWork, rigStores, nil, dt, nil, poolDesired,
		ds.NamedSessionDemand, ds.NamedSessionRoutedDemand, ds.StoreQueryPartial, nil, cityName,
		it, clk, events.Discard, 0, 0, &stdout, stderr, opts...,
	)
}

// requalifyAssertRenamedRowHoldsWork checks the ownership invariants on the
// renamed row after a tick: it is open, the only open row, and still owns the
// work claimed under the bare form.
func requalifyAssertRenamedRowHoldsWork(t *testing.T, store beads.Store, row, work beads.Bead, tick int) {
	t.Helper()
	got, err := store.Get(row.ID)
	if err != nil {
		t.Fatalf("tick %d: reading the row: %v", tick, err)
	}
	if got.Status == "closed" {
		t.Fatalf("tick %d: the renamed row was closed (close_reason=%q) while it held work claimed under %q", tick, got.Metadata["close_reason"], requalifyBare)
	}
	if got.Metadata[namedSessionIdentityMetadata] != requalifyQualified {
		t.Errorf("tick %d: configured_named_identity = %q, want %q", tick, got.Metadata[namedSessionIdentityMetadata], requalifyQualified)
	}
	w, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("tick %d: reading work: %v", tick, err)
	}
	if w.Status != "in_progress" || w.Assignee != requalifyBare {
		t.Errorf("tick %d: work = %s/%q, want in_progress assigned %q", tick, w.Status, w.Assignee, requalifyBare)
	}
	if !liveOpenSessionAssignmentExists(store, requalifyBare) {
		t.Errorf("tick %d: liveOpenSessionAssignmentExists(%q) = false, want the renamed row to own the bare-claimed work", tick, requalifyBare)
	}
	for _, b := range requalifyOpenRows(t, store) {
		if b.ID != row.ID {
			t.Errorf("tick %d: a second open row %s exists: session_name=%q", tick, b.ID, b.Metadata["session_name"])
		}
	}
}

// The renamed row owns the work its bare form claimed. Under a
// session_template that keeps session_name off the bare identity, the
// requalified prior identity is the only thing tying that work to the row: a
// runtime stop after the rename must not close the row while the work is in
// progress. With the template kept, the always-mode seat restarts on the same
// row. With the template changed after the rename, the row is genuinely
// reconfigured, and the work still holds its close until the work is done.
func TestLegacyRequalifiedRowKeepsBareClaimedWorkAcrossARuntimeStop(t *testing.T) {
	for _, tc := range []struct {
		name          string
		afterTemplate string
		restarts      bool
	}{
		{name: "template kept", afterTemplate: "{{.City}}-{{.Agent}}", restarts: true},
		{name: "template changed after the rename", afterTemplate: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := requalifyCity("always")
			cfg.Workspace.SessionTemplate = "{{.City}}-{{.Agent}}"
			runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, requalifyBare)
			if runtimeName == requalifyBare {
				t.Fatalf("test premise broken: the session template leaves the runtime name at the bare identity %q", requalifyBare)
			}
			row := requalifySeedRow(t, store, "always")
			if err := store.SetMetadataBatch(row.ID, map[string]string{"session_name": runtimeName}); err != nil {
				t.Fatalf("stamping the templated runtime name: %v", err)
			}
			work := requalifySeedWork(t, store)
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), runtimeName, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			cityPath := t.TempDir()
			clk := &clock.Fake{Time: namedEffectNow}
			dt := newDrainTracker()
			var stderr bytes.Buffer

			requalifyDaemonTick(t, cityPath, cfg, store, nil, transportFake{sp}, dt, clk, &stderr)
			requalifyAssertRenamedRowHoldsWork(t, store, row, work, 1)

			cfg.Workspace.SessionTemplate = tc.afterTemplate
			if err := sp.Stop(runtimeName); err != nil {
				t.Fatalf("stopping the runtime: %v", err)
			}
			for tick := 2; tick <= 3; tick++ {
				clk.Advance(time.Minute)
				requalifyDaemonTick(t, cityPath, cfg, store, nil, transportFake{sp}, dt, clk, &stderr)
				requalifyAssertRenamedRowHoldsWork(t, store, row, work, tick)
			}
			if tc.restarts && !sp.IsRunning(runtimeName) {
				t.Errorf("the always-mode seat was not restarted under its own runtime name %q; stderr:\n%s", runtimeName, stderr.String())
			}
		})
	}
}

// The in-place rename outlives the first runtime stop. The row keeps the
// runtime name its bare identity resolved to, which legacy's reconfigured
// sweep must not read as a reconfiguration: no per-tick "declining to close"
// while the seat runs, and after a stop the always-mode seat restarts on the
// same row instead of being closed and re-minted under the qualified runtime
// name, which would lose its resume.
func TestLegacyRequalifiedRowRestartsInPlaceAfterARuntimeStop(t *testing.T) {
	store := beads.NewMemStore()
	cfg := requalifyCity("always")
	row := requalifySeedRow(t, store, "always")
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), requalifyBare, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cityPath := t.TempDir()
	clk := &clock.Fake{Time: namedEffectNow}
	dt := newDrainTracker()
	var stderr bytes.Buffer

	requalifyDaemonTick(t, cityPath, cfg, store, nil, transportFake{sp}, dt, clk, &stderr)
	stderr.Reset()
	clk.Advance(time.Minute)
	requalifyDaemonTick(t, cityPath, cfg, store, nil, transportFake{sp}, dt, clk, &stderr)
	if s := stderr.String(); strings.Contains(s, "declining to close") {
		t.Errorf("a running renamed row was treated as reconfigured: %s", s)
	}

	if err := sp.Stop(requalifyBare); err != nil {
		t.Fatalf("stopping the runtime: %v", err)
	}
	for tick := 3; tick <= 4; tick++ {
		clk.Advance(time.Minute)
		requalifyDaemonTick(t, cityPath, cfg, store, nil, transportFake{sp}, dt, clk, &stderr)
		got, err := store.Get(row.ID)
		if err != nil {
			t.Fatalf("tick %d: reading the row: %v", tick, err)
		}
		if got.Status == "closed" {
			t.Fatalf("tick %d: the renamed row was closed (close_reason=%q)", tick, got.Metadata["close_reason"])
		}
		for _, b := range requalifyOpenRows(t, store) {
			if b.ID != row.ID {
				t.Errorf("tick %d: a second open row %s exists: session_name=%q", tick, b.ID, b.Metadata["session_name"])
			}
		}
	}
	if !sp.IsRunning(requalifyBare) {
		t.Errorf("the always-mode seat was not restarted in place under %q; stderr:\n%s", requalifyBare, stderr.String())
	}
}

// An on_demand seat whose only claim is blocked in-progress work under the
// bare form has the named session's own demand: the claim, resident in the
// rig store the rig-scoped session claims from, wakes the asleep renamed seat
// exactly as a blocked claim under the qualified identity does.
func TestLegacyRequalifiedOnDemandSeatWakesOnBlockedBareClaim(t *testing.T) {
	store := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cfg := requalifyCity("on_demand")
	row := requalifySeedRow(t, store, "on_demand")
	if err := store.SetMetadataBatch(row.ID, map[string]string{"state": "asleep", "sleep_reason": "idle"}); err != nil {
		t.Fatalf("parking the row asleep: %v", err)
	}
	// The claim is in_progress and blocked: the awake set's assigned-work
	// pass skips blocked work (workBeadHasAwakeDemand), so named demand is
	// the only thing that can wake the seat on it.
	blocked := true
	work, err := rigStore.Create(beads.Bead{Title: "hon gate", Type: "task", Assignee: requalifyBare, IsBlocked: &blocked})
	if err != nil {
		t.Fatalf("seeding blocked work: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("claiming blocked work: %v", err)
	}
	if w, err := rigStore.Get(work.ID); err != nil || w.Status != "in_progress" || w.IsBlocked == nil || !*w.IsBlocked {
		t.Fatalf("test premise broken: seeded work = %+v (err %v), want in_progress and blocked", w, err)
	}
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	clk := &clock.Fake{Time: namedEffectNow}
	var stderr bytes.Buffer

	requalifyDaemonTick(t, cityPath, cfg, store, map[string]beads.Store{"contentbuild": rigStore}, transportFake{sp}, newDrainTracker(), clk, &stderr)
	if !sp.IsRunning(requalifyBare) {
		t.Fatalf("the asleep on_demand seat did not wake on its blocked claim under %q; stderr:\n%s", requalifyBare, stderr.String())
	}
	got, err := store.Get(row.ID)
	if err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if got.Status == "closed" || got.Metadata[namedSessionIdentityMetadata] != requalifyQualified {
		t.Errorf("the row = %s identity %q, want it open and renamed to %q", got.Status, got.Metadata[namedSessionIdentityMetadata], requalifyQualified)
	}
	if w, err := rigStore.Get(work.ID); err != nil || w.Status != "in_progress" || w.Assignee != requalifyBare {
		t.Errorf("work = %+v (err %v), want in_progress assigned %q", w, err, requalifyBare)
	}
	for _, b := range requalifyOpenRows(t, store) {
		if b.ID != row.ID {
			t.Errorf("a second open row %s exists: session_name=%q", b.ID, b.Metadata["session_name"])
		}
	}
}

// The rename is one-way, and a binding of the same leaf added after it does
// not undo it. Once a second rig imports the same pack the bare leaf resolves
// to no single entry, but the renamed row keeps its bare form: no per-tick
// "declining to close" while it runs, and after a stop it is not closed as
// reconfigured or re-minted under the qualified runtime name. The always-mode
// seat restarts on the same row and keeps owning the work claimed under the
// bare form. The second rig's born-qualified seat claims none of it.
func TestLegacyRequalifiedRowKeepsItsBareFormAfterASecondBinding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		template string
		work     bool
	}{
		{name: "templated runtime name holding bare work", template: "{{.City}}-{{.Agent}}", work: true},
		{name: "default runtime name", template: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := requalifyCity("always")
			cfg.Workspace.SessionTemplate = tc.template
			runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, requalifyBare)
			row := requalifySeedRow(t, store, "always")
			if err := store.SetMetadataBatch(row.ID, map[string]string{"session_name": runtimeName}); err != nil {
				t.Fatalf("stamping the runtime name: %v", err)
			}
			var work beads.Bead
			if tc.work {
				work = requalifySeedWork(t, store)
			}
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), runtimeName, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			cityPath := t.TempDir()
			clk := &clock.Fake{Time: namedEffectNow}
			dt := newDrainTracker()
			var stderr bytes.Buffer

			requalifyDaemonTick(t, cityPath, cfg, store, nil, transportFake{sp}, dt, clk, &stderr)
			if got, err := store.Get(row.ID); err != nil || got.Metadata[namedSessionIdentityMetadata] != requalifyQualified {
				t.Fatalf("test premise broken: tick 1 did not rename the row (identity %q, err %v)", got.Metadata[namedSessionIdentityMetadata], err)
			}

			cfg = requalifySecondRigCity("always")
			cfg.Workspace.SessionTemplate = tc.template
			stderr.Reset()
			clk.Advance(time.Minute)
			requalifyDaemonTick(t, cityPath, cfg, store, nil, transportFake{sp}, dt, clk, &stderr)
			if s := stderr.String(); strings.Contains(s, "declining to close") {
				t.Errorf("after the second binding the running renamed row was treated as reconfigured: %s", s)
			}

			if err := sp.Stop(runtimeName); err != nil {
				t.Fatalf("stopping the runtime: %v", err)
			}
			secondRigSeat := false
			for tick := 3; tick <= 4; tick++ {
				clk.Advance(time.Minute)
				requalifyDaemonTick(t, cityPath, cfg, store, nil, transportFake{sp}, dt, clk, &stderr)
				got, err := store.Get(row.ID)
				if err != nil {
					t.Fatalf("tick %d: reading the row: %v", tick, err)
				}
				if got.Status == "closed" {
					t.Fatalf("tick %d: the renamed row was closed (close_reason=%q) after a second binding of its leaf", tick, got.Metadata["close_reason"])
				}
				if tc.work {
					w, err := store.Get(work.ID)
					if err != nil {
						t.Fatalf("tick %d: reading work: %v", tick, err)
					}
					if w.Status != "in_progress" || w.Assignee != requalifyBare {
						t.Errorf("tick %d: work = %s/%q, want in_progress assigned %q", tick, w.Status, w.Assignee, requalifyBare)
					}
					if !liveOpenSessionAssignmentExists(store, requalifyBare) {
						t.Errorf("tick %d: liveOpenSessionAssignmentExists(%q) = false, want the renamed row to own the bare-claimed work", tick, requalifyBare)
					}
				}
				for _, b := range requalifyOpenRows(t, store) {
					if b.ID == row.ID {
						continue
					}
					secondRigSeat = secondRigSeat || b.Metadata[namedSessionIdentityMetadata] == "other/lane."+requalifyBare
					if b.Metadata[namedSessionIdentityMetadata] == requalifyQualified {
						t.Errorf("tick %d: a second row %s took %q: session_name=%q", tick, b.ID, requalifyQualified, b.Metadata["session_name"])
					}
					if slices.Contains(sessionAssignmentIdentifiersForConfig(b, cfg), requalifyBare) {
						t.Errorf("tick %d: row %s (%q) claims the bare form %q", tick, b.ID, b.Metadata[namedSessionIdentityMetadata], requalifyBare)
					}
				}
			}
			if !sp.IsRunning(runtimeName) {
				t.Errorf("the always-mode seat was not restarted in place under %q; stderr:\n%s", runtimeName, stderr.String())
			}
			if !secondRigSeat {
				t.Errorf("test premise broken: the second rig's always-mode seat was never minted, so nothing showed it claims no bare work")
			}
		})
	}
}

// A claim under the bare leaf that no row owns is no seat's demand: config
// resolving the leaf to the requalified entry does not let the born-qualified
// seat a mint would start see the work, so the on_demand session is neither
// minted nor started for it.
func TestLegacyUnownedBareClaimMintsNoSeat(t *testing.T) {
	store := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cfg := requalifyCity("on_demand")
	work, err := rigStore.Create(beads.Bead{Title: "hon gate", Type: "task", Assignee: requalifyBare})
	if err != nil {
		t.Fatalf("seeding work: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("claiming work: %v", err)
	}
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	clk := &clock.Fake{Time: namedEffectNow}
	dt := newDrainTracker()
	var stderr bytes.Buffer
	qualifiedRuntime := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, requalifyQualified)

	for tick := 1; tick <= 2; tick++ {
		clk.Advance(time.Minute)
		requalifyDaemonTick(t, cityPath, cfg, store, map[string]beads.Store{"contentbuild": rigStore}, transportFake{sp}, dt, clk, &stderr)
		if rows := requalifyOpenRows(t, store); len(rows) != 0 {
			t.Fatalf("tick %d: a seat was minted for the unowned claim under %q: %s (session_name=%q)", tick, requalifyBare, rows[0].ID, rows[0].Metadata["session_name"])
		}
		if sp.IsRunning(qualifiedRuntime) {
			t.Fatalf("tick %d: the born-qualified seat %q was started for a claim it cannot see", tick, qualifiedRuntime)
		}
	}
	if w, err := rigStore.Get(work.ID); err != nil || w.Status != "in_progress" || w.Assignee != requalifyBare {
		t.Errorf("work = %+v (err %v), want in_progress assigned %q", w, err, requalifyBare)
	}
}

// requalifyRenamedRowMeta is the requalified row after its in-place rename
// under a session_template that keeps session_name off the bare identity:
// qualified identity and alias, the bare form in alias_history only.
func requalifyRenamedRowMeta() map[string]string {
	return map[string]string{
		"session_name":               "test-city-" + requalifyBare,
		"alias":                      requalifyQualified,
		"alias_history":              requalifyBare,
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: requalifyQualified,
	}
}

// The narrow release scope carries a renamed row's requalified prior
// identity: the leaf of the row's own config entry, which the row's alias
// history records. A binding of the same leaf added after the rename, in the
// row's rig or another, leaves the row its prior identity. A born-qualified
// row, a row whose history never held the leaf, and a row whose leaf a
// top-level entry is now configured under get none, and the raw-bead and Info
// forms agree.
func TestSessionAssignmentIdentifiersCarryTheRequalifiedPriorIdentity(t *testing.T) {
	bornQualified := requalifyRenamedRowMeta()
	bornQualified["session_name"] = "test-city-contentbuild--lane__golden-hon"
	delete(bornQualified, "alias_history")
	foreignHistory := requalifyRenamedRowMeta()
	foreignHistory["alias_history"] = "someone-else"
	for _, tc := range []struct {
		name string
		cfg  *config.City
		meta map[string]string
		want bool
	}{
		{"renamed row", requalifyCity("always"), requalifyRenamedRowMeta(), true},
		{"born-qualified row", requalifyCity("always"), bornQualified, false},
		{"history without the leaf", requalifyCity("always"), foreignHistory, false},
		{"leaf bound again in the row's rig", requalifyAmbiguousCity("always"), requalifyRenamedRowMeta(), true},
		{"leaf bound again by a second rig", requalifySecondRigCity("always"), requalifyRenamedRowMeta(), true},
		{"leaf shadowed by a top-level entry", requalifyShadowedCity("always"), requalifyRenamedRowMeta(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := beads.Bead{ID: "gc-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: tc.meta}
			info := sessionInfoFromBead(b)
			raw := sessionAssignmentIdentifiersForConfig(b, tc.cfg)
			fromInfo := sessionAssignmentIdentifiersForConfigInfo(info, tc.cfg)
			if !slices.Equal(raw, fromInfo) {
				t.Fatalf("raw identifiers %v differ from the Info form %v", raw, fromInfo)
			}
			if got := slices.Contains(raw, requalifyBare); got != tc.want {
				t.Errorf("identifiers %v contain %q = %v, want %v", raw, requalifyBare, got, tc.want)
			}
			// The awake set matches assignees against the same form.
			awake := newAwakeInputFromSnapshot(tc.cfg, func(*config.Agent) bool { return false }, []session.Info{info}, nil, nil, nil, nil, nil, nil, nil, namedEffectNow)
			if len(awake.SessionBeads) != 1 {
				t.Fatalf("awake input rows = %+v, want the one session row", awake.SessionBeads)
			}
			if got := awake.SessionBeads[0].RequalifiedFrom == requalifyBare; got != tc.want {
				t.Errorf("AwakeSessionBead.RequalifiedFrom = %q, want it to name %q: %v", awake.SessionBeads[0].RequalifiedFrom, requalifyBare, tc.want)
			}
		})
	}
}

// requalifyNamedDemand is the named-session demand of one in_progress claim
// under the bare form, with rows as the city's open session rows.
func requalifyNamedDemand(t *testing.T, cfg *config.City, rows ...beads.Bead) map[string]bool {
	t.Helper()
	work := []beads.Bead{{ID: "w-1", Title: "hon gate", Type: "task", Status: "in_progress", Assignee: requalifyBare}}
	var stderr bytes.Buffer
	return computeNamedSessionDemand(cfg.EffectiveCityName(), t.TempDir(), cfg, nil, newSessionBeadSnapshot(rows),
		nil, nil, work, nil, nil, nil, &stderr).workReady
}

// A claim under the bare leaf is named demand only through the row that owns
// the leaf: the row still stamped with it, awaiting its rename, or the row
// renamed from it, also once a later binding shares the leaf. With no row, a
// born-qualified one, or two bare rows of which neither is canonical, it is
// no seat's demand, since no seat it would wake or mint can see it; and a
// top-level entry configured under the leaf owns the bare name itself.
func TestNamedDemandCountsABareClaimOnlyThroughTheRowThatOwnsIt(t *testing.T) {
	row := func(meta map[string]string) []beads.Bead {
		return []beads.Bead{{ID: "gc-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta}}
	}
	twoRows := func(meta map[string]string) []beads.Bead {
		rows := row(meta)
		return append(rows, beads.Bead{ID: "gc-2", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	}
	awaitingRename := map[string]string{
		"session_name":               requalifyBare,
		"alias":                      requalifyBare,
		"template":                   requalifyBare,
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: requalifyBare,
	}
	bornQualified := requalifyRenamedRowMeta()
	delete(bornQualified, "alias_history")
	for _, tc := range []struct {
		name string
		cfg  *config.City
		rows []beads.Bead
		want string // the identity the claim is demand for; "" for none
	}{
		{"row awaiting its rename", requalifyCity("on_demand"), row(awaitingRename), requalifyQualified},
		{"two rows awaiting the rename", requalifyCity("on_demand"), twoRows(awaitingRename), ""},
		{"renamed row", requalifyCity("on_demand"), row(requalifyRenamedRowMeta()), requalifyQualified},
		{"renamed row, leaf bound again in its rig", requalifyAmbiguousCity("on_demand"), row(requalifyRenamedRowMeta()), requalifyQualified},
		{"renamed row, leaf bound again by a second rig", requalifySecondRigCity("on_demand"), row(requalifyRenamedRowMeta()), requalifyQualified},
		{"born-qualified row", requalifyCity("on_demand"), row(bornQualified), ""},
		{"no row", requalifyCity("on_demand"), nil, ""},
		{"leaf shadowed by a top-level entry", requalifyShadowedCity("on_demand"), row(requalifyRenamedRowMeta()), requalifyBare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			demand := requalifyNamedDemand(t, tc.cfg, tc.rows...)
			for identity, ready := range demand {
				if ready && identity != tc.want {
					t.Errorf("the claim under %q is demand for %q, want it only for %q", requalifyBare, identity, tc.want)
				}
			}
			if tc.want != "" && !demand[tc.want] {
				t.Errorf("the claim under %q is not demand for %q: %v", requalifyBare, tc.want, demand)
			}
		})
	}
}

// The reconfigured sweep's exemption holds only for the runtime name the bare
// identity resolves to under the current session_template: a born-qualified
// row is not exempt, and a template change after the rename moves that name,
// so the renamed row is swept like any other reconfigured session. A later
// binding of the leaf keeps the exemption; a top-level entry configured under
// the leaf, which now owns the bare name, ends it.
func TestKeepsRequalifiedRuntimeNameFollowsTheSessionTemplate(t *testing.T) {
	templated := requalifyCity("always")
	templated.Workspace.SessionTemplate = "{{.City}}-{{.Agent}}"
	renamedDefault := requalifyRenamedRowMeta()
	renamedDefault["session_name"] = requalifyBare
	bornQualified := requalifyRenamedRowMeta()
	bornQualified["session_name"] = "test-city-contentbuild--lane__golden-hon"
	delete(bornQualified, "alias_history")
	for _, tc := range []struct {
		name string
		cfg  *config.City
		meta map[string]string
		want bool
	}{
		{"renamed row under its template", templated, requalifyRenamedRowMeta(), true},
		{"renamed row under the default template", requalifyCity("always"), renamedDefault, true},
		{"session_template changed after the rename", requalifyCity("always"), requalifyRenamedRowMeta(), false},
		{"born-qualified row", templated, bornQualified, false},
		{"leaf bound again in the row's rig", requalifyAmbiguousCity("always"), renamedDefault, true},
		{"leaf bound again by a second rig", requalifySecondRigCity("always"), renamedDefault, true},
		{"leaf shadowed by a top-level entry", requalifyShadowedCity("always"), renamedDefault, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := beads.Bead{ID: "gc-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: tc.meta}
			if got := keepsRequalifiedRuntimeName(b, tc.cfg, tc.cfg.EffectiveCityName()); got != tc.want {
				t.Errorf("keepsRequalifiedRuntimeName = %v, want %v", got, tc.want)
			}
		})
	}
}

// requalifyQualifiedRuntime is the runtime name the requalified named session
// resolves to under the default session_template: the name a born-qualified
// seat runs under, and the claim form namedSessionAssigneeMatchesSpec counts
// beside the qualified identity.
const requalifyQualifiedRuntime = "contentbuild--lane__golden-hon"

// requalifyLifecycleSeat runs tick 1 of a case under the always-mode config,
// which produces a running seat one of two ways: renamed seeds the pre-adopt
// bare row with its runtime running and lets the tick rename it in place, and
// otherwise the tick mints and starts a born-qualified row. It returns the
// seat's row and the runtime name the seat runs under.
func requalifyLifecycleSeat(t *testing.T, renamed bool, cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, sp *runtime.Fake, dt *drainTracker, clk *clock.Fake) (beads.Bead, string) {
	t.Helper()
	cityName := cfg.EffectiveCityName()
	runtimeName := config.NamedSessionRuntimeName(cityName, cfg.Workspace, requalifyQualified)
	if renamed {
		runtimeName = config.NamedSessionRuntimeName(cityName, cfg.Workspace, requalifyBare)
		row := requalifySeedRow(t, store, "always")
		if err := store.SetMetadataBatch(row.ID, map[string]string{
			"session_name":         runtimeName,
			"creation_complete_at": clk.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("stamping the seeded row's runtime: %v", err)
		}
		if err := sp.Start(context.Background(), runtimeName, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	var stderr bytes.Buffer
	requalifyDaemonTick(t, cityPath, cfg, store, rigStores, transportFake{sp}, dt, clk, &stderr)
	rows := requalifyOpenRows(t, store)
	if len(rows) != 1 || rows[0].Metadata[namedSessionIdentityMetadata] != requalifyQualified ||
		rows[0].Metadata["session_name"] != runtimeName || !sp.IsRunning(runtimeName) {
		t.Fatalf("test premise broken: tick 1 left rows %+v, want one running seat %q named %q; stderr:\n%s", rows, runtimeName, requalifyQualified, stderr.String())
	}
	return rows[0], runtimeName
}

// requalifySeatKind names the two ways requalifyLifecycleSeat produces a seat.
func requalifySeatKind(renamed bool) string {
	if renamed {
		return "renamed row"
	}
	return "born-qualified row"
}

// requalifyStops counts the Stop calls on name among calls.
func requalifyStops(calls []runtime.Call, name string) int {
	n := 0
	for _, c := range calls {
		if c.Method == "Stop" && c.Name == name {
			n++
		}
	}
	return n
}

// A row renamed in place keeps the bare form's runtime name, and it is still
// its named session for every lifecycle setting. The named session's
// idle_timeout, max_session_age and assigned_work_defer_limit, and a
// mode="always" session's exemption from them, are registered under the named
// session's own runtime name (buildIdleTracker, buildMaxSessionAgeTracker,
// buildAssignedWorkDeferTracker), which is not the renamed row's
// session_name. Each case produces its seat both ways on tick 1, the bare row
// renamed in place and a born-qualified row, then runs three daemon ticks with
// the trackers wired in. Both seats must be stopped alike, as the case
// expects: a singleton on_demand seat idle-sleeps, is recycled at its max age
// and is forced down at its own defer limit, and an always seat on an
// instance-expanding agent is neither idle-stopped nor max-age restarted.
func TestLegacyRenamedRowGetsItsNamedSessionLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        string // the named session's mode from tick 2 on
		expand      bool   // the backing agent expands into instances
		set         func(*config.Agent)
		idle        bool // the runtime has been inactive for an hour
		work        bool // the seat holds in_progress work under its qualified identity
		wantStopped bool
	}{
		{name: "singleton on_demand idle_timeout", mode: "on_demand", set: func(a *config.Agent) { a.IdleTimeout = "10m" }, idle: true, wantStopped: true},
		{name: "singleton on_demand max_session_age", mode: "on_demand", set: func(a *config.Agent) { a.MaxSessionAge = "30m" }, wantStopped: true},
		{name: "singleton on_demand assigned_work_defer_limit", mode: "on_demand", set: func(a *config.Agent) {
			a.IdleTimeout = "10m"
			a.AssignedWorkDeferLimit = intPtr(1)
		}, idle: true, work: true, wantStopped: true},
		{name: "always on an instance-expanding agent", mode: "always", expand: true, set: func(a *config.Agent) {
			a.IdleTimeout = "10m"
			a.MaxSessionAge = "30m"
		}, idle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type outcome struct {
				stops       int
				state       string
				sleepReason string
			}
			run := func(renamed bool) outcome {
				store := beads.NewMemStore()
				rigStore := beads.NewMemStore()
				rigStores := map[string]beads.Store{"contentbuild": rigStore}
				cfg := requalifyCity("always")
				if tc.expand {
					cfg.Agents[0].MaxActiveSessions = nil
				}
				sp := runtime.NewFake()
				cityPath := t.TempDir()
				clk := &clock.Fake{Time: namedEffectNow}
				dt := newDrainTracker()
				row, runtimeName := requalifyLifecycleSeat(t, renamed, cityPath, cfg, store, rigStores, sp, dt, clk)
				if tc.work {
					w, err := rigStore.Create(beads.Bead{Title: "lane work", Type: "task", Assignee: requalifyQualified})
					if err != nil {
						t.Fatalf("seeding work: %v", err)
					}
					inProgress := "in_progress"
					if err := rigStore.Update(w.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
						t.Fatalf("claiming work: %v", err)
					}
				}
				cfg.NamedSessions[0].Mode = tc.mode
				tc.set(&cfg.Agents[0])
				cityName := cfg.EffectiveCityName()
				it := buildIdleTracker(cfg, cityName, cityPath, sp)
				opts := []startExecutionOption{
					withMaxSessionAgeTracker(buildMaxSessionAgeTracker(cfg, cityName, sp)),
					withAssignedWorkDeferTracker(buildAssignedWorkDeferTracker(cfg, cityName, sp)),
				}
				before := len(sp.SnapshotCalls())
				var stderr bytes.Buffer
				clk.Advance(time.Hour)
				for tick := 2; tick <= 4; tick++ {
					if tc.idle {
						sp.SetActivity(runtimeName, clk.Now().Add(-time.Hour))
					} else {
						sp.SetActivity(runtimeName, clk.Now())
					}
					requalifyDaemonTickWith(t, cityPath, cfg, store, rigStores, transportFake{sp}, dt, clk, &stderr, it, opts...)
					clk.Advance(time.Minute)
				}
				got, err := store.Get(row.ID)
				if err != nil {
					t.Fatalf("%s: reading the seat: %v", requalifySeatKind(renamed), err)
				}
				if got.Status == "closed" {
					t.Fatalf("%s: the seat was closed (close_reason=%q); stderr:\n%s", requalifySeatKind(renamed), got.Metadata["close_reason"], stderr.String())
				}
				return outcome{
					stops:       requalifyStops(sp.SnapshotCalls()[before:], runtimeName),
					state:       got.Metadata["state"],
					sleepReason: got.Metadata["sleep_reason"],
				}
			}
			born, renamed := run(false), run(true)
			if stopped := born.stops > 0; stopped != tc.wantStopped {
				t.Fatalf("test premise broken: the born-qualified seat = %+v, want stopped=%v", born, tc.wantStopped)
			}
			if renamed != born {
				t.Errorf("the renamed seat = %+v, want the born-qualified seat's lifecycle %+v", renamed, born)
			}
		})
	}
}

// requalifyClaimSeat is a running seat of the requalified on_demand named
// session, produced as requalifyLifecycleSeat does, with work in the rig store
// assigned under the qualified runtime name: the claim form a born-qualified
// seat answers to by its session_name, and that the renamed seat, which keeps
// the bare form's runtime name, answers to as its named session. status is the
// work's: in_progress, or open and ready. It returns the seat's row, its
// runtime name and the work.
func requalifyClaimSeat(t *testing.T, renamed bool, status, cityPath string, cfg *config.City, store, rigStore beads.Store, sp *runtime.Fake, dt *drainTracker, clk *clock.Fake) (beads.Bead, string, beads.Bead) {
	t.Helper()
	row, runtimeName := requalifyLifecycleSeat(t, renamed, cityPath, cfg, store, map[string]beads.Store{"contentbuild": rigStore}, sp, dt, clk)
	cfg.NamedSessions[0].Mode = "on_demand"
	work, err := rigStore.Create(beads.Bead{
		Title: "lane work", Type: "task", Assignee: requalifyQualifiedRuntime,
		Metadata: map[string]string{"gc.routed_to": requalifyQualified},
	})
	if err != nil {
		t.Fatalf("seeding work: %v", err)
	}
	if status != "open" {
		if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("claiming work: %v", err)
		}
	}
	return row, runtimeName, work
}

// A claim under the qualified runtime name is the renamed seat's own, as it is
// a born-qualified seat's. Each case runs on both seats: the on_demand seat
// wakes on an in_progress claim once drained and on a ready open claim once
// idle-slept, a work-gated close of it is refused, and the controller's orphan
// release keeps the claim where it is.
func TestLegacyRenamedRowOwnsAClaimUnderItsNamedRuntimeName(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(requalifySeatKind(renamed), func(t *testing.T) {
			// The assigned-work pass wakes a drained seat on its in_progress
			// claim; a ready open claim is the asleep seat's named demand once
			// the ready probe has asked for the claim's assignee.
			for _, c := range []struct{ status, sleepReason string }{{"in_progress", "drained"}, {"open", "idle"}} {
				t.Run(fmt.Sprintf("a seat asleep for %s wakes on the %s claim", c.sleepReason, c.status), func(t *testing.T) {
					store, rigStore := beads.NewMemStore(), beads.NewMemStore()
					cfg := requalifyCity("always")
					sp := runtime.NewFake()
					cityPath := t.TempDir()
					clk := &clock.Fake{Time: namedEffectNow}
					dt := newDrainTracker()
					row, runtimeName, _ := requalifyClaimSeat(t, renamed, c.status, cityPath, cfg, store, rigStore, sp, dt, clk)
					if err := sp.Stop(runtimeName); err != nil {
						t.Fatalf("stopping the runtime: %v", err)
					}
					if err := store.SetMetadataBatch(row.ID, map[string]string{"state": "asleep", "sleep_reason": c.sleepReason}); err != nil {
						t.Fatalf("parking the seat asleep: %v", err)
					}
					var stderr bytes.Buffer
					clk.Advance(time.Minute)
					requalifyDaemonTick(t, cityPath, cfg, store, map[string]beads.Store{"contentbuild": rigStore}, transportFake{sp}, dt, clk, &stderr)
					if !sp.IsRunning(runtimeName) {
						t.Errorf("the on_demand seat asleep for %s did not wake on its %s claim under %q; stderr:\n%s", c.sleepReason, c.status, requalifyQualifiedRuntime, stderr.String())
					}
				})
			}
			t.Run("a work-gated close is refused", func(t *testing.T) {
				store, rigStore := beads.NewMemStore(), beads.NewMemStore()
				cfg := requalifyCity("always")
				sp := runtime.NewFake()
				cityPath := t.TempDir()
				clk := &clock.Fake{Time: namedEffectNow}
				row, _, _ := requalifyClaimSeat(t, renamed, "in_progress", cityPath, cfg, store, rigStore, sp, newDrainTracker(), clk)
				got, err := store.Get(row.ID)
				if err != nil {
					t.Fatalf("reading the seat: %v", err)
				}
				sw := testSeatWork(cityPath, cfg, store, map[string]beads.Store{"contentbuild": rigStore})
				if cause, err := closeAllowed(sw, sessionInfoFromBead(got), closeWhy{reason: "test", gate: closeGated}); cause != causeHasWork {
					t.Errorf("closeAllowed = %q (err %v), want %q: the seat holds work claimed under %q", cause, err, causeHasWork, requalifyQualifiedRuntime)
				}
			})
			t.Run("the orphan release keeps the claim", func(t *testing.T) {
				store, rigStore := beads.NewMemStore(), beads.NewMemStore()
				rigStores := map[string]beads.Store{"contentbuild": rigStore}
				cfg := requalifyCity("always")
				sp := runtime.NewFake()
				cityPath := t.TempDir()
				clk := &clock.Fake{Time: namedEffectNow}
				_, _, work := requalifyClaimSeat(t, renamed, "in_progress", cityPath, cfg, store, rigStore, sp, newDrainTracker(), clk)
				// The claim on a city-store step: a rig-scoped seat also holds
				// work on the leading work arm, which is not its agent's store.
				city, err := store.Create(beads.Bead{
					Title: "graph step", Type: "task", Assignee: requalifyQualifiedRuntime,
					Metadata: map[string]string{"gc.routed_to": requalifyQualified},
				})
				if err != nil {
					t.Fatalf("seeding city work: %v", err)
				}
				inProgress := "in_progress"
				if err := store.Update(city.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
					t.Fatalf("claiming city work: %v", err)
				}
				snapshot, err := loadSessionBeadSnapshot(store)
				if err != nil {
					t.Fatalf("loading the session snapshot: %v", err)
				}
				ds := buildDesiredStateWithSessionBeads(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, transportFake{sp}, store, rigStores, snapshot, nil, io.Discard)
				// No wake-candidate protection: the seat's ownership of its
				// claims must keep them on its own, as it does a born-qualified
				// seat's.
				if released := releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(store, beads.SessionStore{Store: store}, cfg, cityPath, snapshot.OpenInfos(), ds, rigStores, nil, nil, nil); len(released) != 0 {
					t.Errorf("released %+v, want the seat's claims under %q kept", released, requalifyQualifiedRuntime)
				}
				for _, w := range []struct {
					store beads.Store
					id    string
				}{{rigStore, work.ID}, {store, city.ID}} {
					got, err := w.store.Get(w.id)
					if err != nil {
						t.Fatalf("reading work %s: %v", w.id, err)
					}
					if got.Status != "in_progress" || got.Assignee != requalifyQualifiedRuntime {
						t.Errorf("work %s = %s/%q, want in_progress assigned %q", w.id, got.Status, got.Assignee, requalifyQualifiedRuntime)
					}
				}
			})
		})
	}
}

// The renamed row's named runtime name joins its narrow release scope and its
// awake-set assignee matching only while the row keeps the bare form's
// runtime name, and the raw-bead and Info forms agree. A born-qualified row
// already carries that name as its session_name, so it gets no second form, and
// a session_template change after the rename, which moves the row's runtime
// name off the bare form's, or a top-level entry configured under the leaf,
// ends it.
func TestSessionAssignmentIdentifiersCarryTheRequalifiedRuntimeName(t *testing.T) {
	templated := requalifyCity("always")
	templated.Workspace.SessionTemplate = "{{.City}}-{{.Agent}}"
	renamedDefault := requalifyRenamedRowMeta()
	renamedDefault["session_name"] = requalifyBare
	bornQualified := requalifyRenamedRowMeta()
	bornQualified["session_name"] = requalifyQualifiedRuntime
	delete(bornQualified, "alias_history")
	for _, tc := range []struct {
		name string
		cfg  *config.City
		meta map[string]string
		want string // the runtime name the row also answers to; "" for none
	}{
		{"renamed row under its template", templated, requalifyRenamedRowMeta(), "test-city-" + requalifyQualifiedRuntime},
		{"renamed row under the default template", requalifyCity("always"), renamedDefault, requalifyQualifiedRuntime},
		{"leaf bound again by a second rig", requalifySecondRigCity("always"), renamedDefault, requalifyQualifiedRuntime},
		{"born-qualified row", requalifyCity("always"), bornQualified, ""},
		{"session_template changed after the rename", requalifyCity("always"), requalifyRenamedRowMeta(), ""},
		{"leaf shadowed by a top-level entry", requalifyShadowedCity("always"), renamedDefault, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := beads.Bead{ID: "gc-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: tc.meta}
			info := sessionInfoFromBead(b)
			cityName := tc.cfg.EffectiveCityName()
			if got, fromInfo := requalifiedSpecRuntimeName(b, tc.cfg, cityName), requalifiedSpecRuntimeNameInfo(info, tc.cfg, cityName); got != tc.want || fromInfo != tc.want {
				t.Fatalf("requalifiedSpecRuntimeName = %q, Info form %q, want %q", got, fromInfo, tc.want)
			}
			raw := sessionAssignmentIdentifiersForConfig(b, tc.cfg)
			if fromInfo := sessionAssignmentIdentifiersForConfigInfo(info, tc.cfg); !slices.Equal(raw, fromInfo) {
				t.Fatalf("raw identifiers %v differ from the Info form %v", raw, fromInfo)
			}
			if tc.want != "" && !slices.Contains(raw, tc.want) {
				t.Errorf("identifiers %v lack the named runtime name %q", raw, tc.want)
			}
			awake := newAwakeInputFromSnapshot(tc.cfg, func(*config.Agent) bool { return false }, []session.Info{info}, nil, nil, nil, nil, nil, nil, nil, namedEffectNow)
			if len(awake.SessionBeads) != 1 {
				t.Fatalf("awake input rows = %+v, want the one session row", awake.SessionBeads)
			}
			if got := awake.SessionBeads[0].RequalifiedRuntimeName; got != tc.want {
				t.Errorf("AwakeSessionBead.RequalifiedRuntimeName = %q, want %q", got, tc.want)
			}
			if tc.want != "" && !sessionAssigneeMatches(awake.NamedSessions, awake.SessionBeads[0], tc.want) {
				t.Errorf("the awake set does not match a claim under %q to the row", tc.want)
			}
			wantLifecycle := tc.want
			if wantLifecycle == "" {
				wantLifecycle = tc.meta["session_name"]
			}
			if got := lifecycleSessionNameInfo(info, tc.cfg, cityName, tc.meta["session_name"]); got != wantLifecycle {
				t.Errorf("lifecycleSessionNameInfo = %q, want %q", got, wantLifecycle)
			}
		})
	}
}
