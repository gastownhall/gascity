package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

type handoffModeCase struct {
	name       string
	controller reconcilerMode
	answered   bool
	cfg        *config.City
	staged     bool
	reason     string
}

func handoffModeCases() []handoffModeCase {
	legacy := func() *config.City {
		return &config.City{Daemon: config.DaemonConfig{SessionReconciler: "legacy"}}
	}
	v2 := func() *config.City {
		return &config.City{Storage: bornSplitStorageConfig(), Daemon: config.DaemonConfig{SessionReconciler: "v2"}}
	}
	refused := v2()
	refused.Agents = []config.Agent{{Name: "worker", DependsOn: []string{"db"}}}
	return []handoffModeCase{
		{name: "controller legacy overrides v2 config", controller: reconcilerLegacy, answered: true, cfg: v2(), staged: true},
		{name: "controller v2 overrides legacy config", controller: reconcilerV2, answered: true, cfg: legacy(), reason: "v2"},
		{name: "offline legacy config", cfg: legacy(), staged: true},
		{name: "offline v2 config", cfg: v2(), reason: "v2"},
		{name: "offline refused latch", cfg: refused, reason: "undetermined"},
		{name: "offline nil config", reason: "undetermined"},
	}
}

// Each case owns its store and restores both mutable mode seams. No controller,
// model, timer or subprocess is needed for the mode-selection matrix.
func runHandoffModeCase(t *testing.T, tc handoffModeCase) (beads.Store, string, string, int) {
	t.Helper()
	previousController, previousEnv := controllerLatchedReconcilerMode, reconcilerModeLookupEnv
	t.Cleanup(func() {
		controllerLatchedReconcilerMode, reconcilerModeLookupEnv = previousController, previousEnv
	})
	probes := 0
	controllerLatchedReconcilerMode = func(string, *config.City) (reconcilerMode, bool) {
		probes++
		return tc.controller, tc.answered
	}
	reconcilerModeLookupEnv = func(string) (string, bool) { return "", false }
	store := beads.NewMemStore()
	row := seedRestartableHandoffSession(t, store, "worker", "origin-instance")
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	r := testRowFirstRestart("gc handoff", "worker", dops, &stderr)
	r.handoffConfig = tc.cfg
	r.record = func(string) error {
		return store.SetMetadataBatch(row.ID, map[string]string{"restart_requested": "true", "continuation_reset_pending": "true"})
	}
	outcome := doHandoffWithOutcome(store, store, events.Discard, dops, r, "worker", "worker", []string{"context cycle", "resume here"}, &stdout, &stderr)
	if outcome.code != 0 || !outcome.restartRequested {
		t.Fatalf("handoff = %+v; stdout=%q stderr=%q", outcome, stdout.String(), stderr.String())
	}
	return store, row.ID, stderr.String(), probes
}

func TestSelfHandoffStagesOnlyForPositivelyLegacyReconciler(t *testing.T) {
	for _, tc := range handoffModeCases() {
		t.Run(tc.name, func(t *testing.T) {
			store, rowID, stderr, _ := runHandoffModeCase(t, tc)
			message := onlyOpenHandoffMessage(t, store)
			assertHandoffModeState(t, store, rowID, message, tc.staged)
			assertHandoffModeNotice(t, stderr, tc.reason)
		})
	}
}

func TestEligibleSelfHandoffProbesControllerExactlyOnce(t *testing.T) {
	for _, tc := range handoffModeCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, probes := runHandoffModeCase(t, tc)
			if probes != 1 {
				t.Fatalf("controller probes = %d, want 1 for a restartable handoff with an instance token", probes)
			}
		})
	}
}

func TestSelfHandoffWithUnsetModeContextDeliversImmediately(t *testing.T) {
	store, rowID, stderr, _ := runHandoffModeCase(t, handoffModeCase{})
	assertHandoffModeState(t, store, rowID, onlyOpenHandoffMessage(t, store), false)
	assertHandoffModeNotice(t, stderr, "undetermined")
}

func TestIneligibleSelfHandoffDoesNotProbeReconciler(t *testing.T) {
	for _, name := range []string{"non-restartable named session", "session without instance token"} {
		t.Run(name, func(t *testing.T) {
			previous := controllerLatchedReconcilerMode
			t.Cleanup(func() { controllerLatchedReconcilerMode = previous })
			probes := 0
			controllerLatchedReconcilerMode = func(string, *config.City) (reconcilerMode, bool) {
				probes++
				return reconcilerV2, true
			}
			store := beads.NewMemStore()
			row := seedRestartableHandoffSession(t, store, "worker", "origin-instance")
			if name == "non-restartable named session" {
				if err := store.SetMetadataBatch(row.ID, map[string]string{namedSessionMetadataKey: "true", namedSessionModeMetadata: "on_demand"}); err != nil {
					t.Fatal(err)
				}
			} else if err := store.SetMetadata(row.ID, "instance_token", ""); err != nil {
				t.Fatal(err)
			}
			outcome, _, stderr := runSelfHandoff(store, events.Discard, nil, "ordinary brief")
			if outcome.code != 0 {
				t.Fatalf("handoff exit = %d; stderr=%q", outcome.code, stderr)
			}
			if probes != 0 {
				t.Errorf("controller probes = %d, want none for an ineligible session", probes)
			}
			assertHandoffModeState(t, store, row.ID, onlyOpenHandoffMessage(t, store), false)
			assertHandoffModeNotice(t, stderr, "")
		})
	}
}

// The state assertion includes mailbox visibility as well as both halves of
// staging's durable record, so printing an immediate-delivery notice alone
// cannot satisfy it.
func assertHandoffModeState(t *testing.T, store beads.Store, rowID string, message beads.Bead, staged bool) {
	t.Helper()
	row, err := store.Get(rowID)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := newTestBeadMail(store).Check("worker")
	if err != nil {
		t.Fatal(err)
	}
	type state struct {
		label, committed, releaseAttempted bool
		mailStaged, mailToken, rowMessage  string
		visibleCount                       int
		visibleID                          string
	}
	got := state{
		label:            hasString(message.Labels, redHandoffStagedLabel),
		committed:        row.Metadata[redHandoffStageCommittedAtKey] != "",
		releaseAttempted: row.Metadata[redHandoffReleaseAttemptedAtKey] != "",
		mailStaged:       message.Metadata[redHandoffStagedMetadataKey],
		mailToken:        message.Metadata[redHandoffStagedForTokenKey],
		rowMessage:       row.Metadata[redHandoffStagedMessageIDKey],
		visibleCount:     len(visible),
	}
	if len(visible) == 1 {
		got.visibleID = visible[0].ID
	}
	want := state{visibleCount: 1, visibleID: message.ID}
	if staged {
		want = state{label: true, committed: true, mailStaged: "true", mailToken: "origin-instance", rowMessage: message.ID}
	}
	if got != want {
		t.Errorf("handoff delivery state = %+v, want %+v", got, want)
	}
}

func assertHandoffModeNotice(t *testing.T, stderr, reason string) {
	t.Helper()
	if reason == "" {
		if stderr != "" {
			t.Errorf("stderr = %q, want no mode notice", stderr)
		}
		return
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 1 || !strings.Contains(strings.ToLower(lines[0]), "immediat") || !strings.Contains(strings.ToLower(lines[0]), reason) {
		t.Errorf("stderr = %q, want one immediate-delivery line naming %s", stderr, reason)
	}
}

// This is the production wiring proof: runHandoff loads the city's config and
// constructs its own rowFirstRestart. Hand-setting the unit seam here would
// miss a constructor that forgot to pass the reconciler context.
func TestBornSplitSuccessorCanHandoffWithoutAStagedBriefWedge(t *testing.T) {
	f, cfg := newHandoffModeCity(t, true)
	if mode, err := latchReconcilerMode(cfg, f.cityPath, func(string) (string, bool) { return "", false }); err != nil || mode != reconcilerV2 {
		t.Fatalf("born-split fixture resolves %v, %v; want v2", mode, err)
	}
	classStore := newSplitEnvClassLeaf(t)
	// A production binding open stamps its conditional-write mode. The strict
	// fixture exposes its SQLite leaf through the same capability target.
	leaf := classStore.(beads.ConditionalWritesResolveTargeter).ConditionalWritesResolveTarget()
	if err := beads.StampOpenedStore(leaf, "SQLiteStore", "auto", false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if writer, _, err := beads.ResolveConditionalWriter(classStore); err != nil || writer == nil {
		t.Fatalf("born-split fixture has no conditional writer: %v", err)
	}
	row, err := f.store.Get(f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	row.ID = ""
	created, err := classStore.Create(row)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(f.sessionID); err != nil {
		t.Fatal(err)
	}
	f.sessionID = created.ID
	t.Setenv("GC_SESSION_ID", f.sessionID)
	routes := splitEnvRoutes(classStore)
	routes.binding = initSplitBindingName
	registerResidencyRoutes(f.cityPath, routes, func() beads.Store { return f.store })
	t.Cleanup(func() { unregisterResidencyRoutes(f.cityPath, routes) })
	for i, token := range []string{explicitRestartToken, "successor-instance"} {
		t.Setenv("GC_INSTANCE_TOKEN", token)
		t.Setenv("GC_RUNTIME_EPOCH", fmt.Sprint(i+1))
		if err := classStore.SetMetadataBatch(f.sessionID, map[string]string{"instance_token": token, "generation": fmt.Sprint(i + 1), "restart_requested": "", "continuation_reset_pending": ""}); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		started := time.Now()
		code, _ := runHandoff(handoffRun{args: []string{"context cycle", "resume here"}, stdout: &stdout, jsonOut: &stdout, stderr: &stderr})
		t.Logf("incarnation %d handoff without controller: %s", i+1, time.Since(started))
		if code != 0 {
			t.Fatalf("incarnation %d handoff exit = %d; stdout=%q stderr=%q", i+1, code, stdout.String(), stderr.String())
		}
	}
	visible, err := newTestBeadMail(classStore).Check("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 2 {
		t.Errorf("visible briefs after two handoffs = %d, want 2", len(visible))
	}
	row, err = classStore.Get(f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Metadata[redHandoffStagedMessageIDKey] != "" {
		t.Errorf("successor left staged brief %q", row.Metadata[redHandoffStagedMessageIDKey])
	}
}

func TestLegacyPinnedCityKeepsSelfHandoffStaged(t *testing.T) {
	f, _ := newHandoffModeCity(t, false)
	var stdout, stderr bytes.Buffer
	code, _ := runHandoff(handoffRun{args: []string{"context cycle", "resume here"}, stdout: &stdout, jsonOut: &stdout, stderr: &stderr})
	if code != 0 {
		t.Fatalf("legacy handoff exit = %d; stderr=%q", code, stderr.String())
	}
	message := onlyOpenHandoffMessage(t, f.store)
	assertHandoffProvisionalRecord(t, message, explicitRestartToken)
	if visible, err := newTestBeadMail(f.store).Check("worker"); err != nil || len(visible) != 0 {
		t.Errorf("legacy mailbox = %v, %v; want staged brief hidden", visible, err)
	}
	if strings.Contains(strings.ToLower(stderr.String()), "immediat") {
		t.Errorf("legacy handoff printed an immediate-delivery notice: %q", stderr.String())
	}
}

// Reuse the restart fixture's identity/runtime setup, with the file-per-agent
// PackV2 layout that the production config loader accepts.
func newHandoffModeCity(t *testing.T, split bool) (explicitRestartFixture, *config.City) {
	t.Helper()
	f := newExplicitRestartFixture(t)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
	}
	if split {
		cfg.Storage = bornSplitStorageConfig()
	} else {
		cfg.Daemon.SessionReconciler = "legacy"
	}
	writeHandoffModeConfig(t, f.cityPath, cfg)
	writeCatalogFile(t, f.cityPath, "agents/worker/agent.toml", "scope = \"city\"\nstart_command = \"echo hello\"\n")
	writeCatalogFile(t, f.cityPath, "agents/worker/prompt.md", "resume the assigned work\n")
	loaded, err := loadCityConfig(f.cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return f, loaded
}

func writeHandoffModeConfig(t *testing.T, cityPath string, cfg *config.City) {
	t.Helper()
	body, err := cfg.MarshalForWrite()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}
