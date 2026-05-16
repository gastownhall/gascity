package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestAcceptConfigDriftAcrossSessions_UpdatesStaleHash(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": "stale-hash-deadbeef",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	desired := map[string]TemplateParams{
		"worker": {
			Command:      "new-cmd",
			SessionName:  "worker",
			TemplateName: "worker",
		},
	}
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if wantHash == "stale-hash-deadbeef" {
		t.Fatalf("test setup: stale fixture coincidentally equals fresh hash %q", wantHash)
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, nil, nil, nil, &stderr)
	if got.Updated != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", got.Updated, stderr.String())
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if updated.Metadata["started_config_hash"] != wantHash {
		t.Errorf("started_config_hash = %q, want %q", updated.Metadata["started_config_hash"], wantHash)
	}
	var gotBreakdown map[string]string
	if err := json.Unmarshal([]byte(updated.Metadata["core_hash_breakdown"]), &gotBreakdown); err != nil {
		t.Fatalf("core_hash_breakdown is not valid JSON: %v", err)
	}
	wantBreakdown := runtime.CoreFingerprintBreakdown(runtime.Config{Command: "new-cmd"})
	if gotBreakdown["Command"] != wantBreakdown["Command"] {
		t.Errorf("core_hash_breakdown[Command] = %q, want %q", gotBreakdown["Command"], wantBreakdown["Command"])
	}
}

// TestAcceptConfigDriftAcrossSessions_SkipsUnstartedSessions asserts that a
// session that has never recorded a started_config_hash (still in the
// startup window) is left alone. Stamping a hash on an unstarted session
// would interfere with the reconciler's first-start path; the reconciler
// already skips drift detection while started_config_hash is empty.
func TestAcceptConfigDriftAcrossSessions_SkipsUnstartedSessions(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"template":     "worker",
			// no started_config_hash — session hasn't reached started state yet
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	desired := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, nil, nil, nil, &stderr)
	if got.Updated != 0 {
		t.Fatalf("updated = %d, want 0 for unstarted session (stderr=%s)", got.Updated, stderr.String())
	}

	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if _, present := unchanged.Metadata["started_config_hash"]; present {
		t.Errorf("started_config_hash unexpectedly written for unstarted session: %q", unchanged.Metadata["started_config_hash"])
	}
}

// TestAcceptConfigDriftAcrossSessions_SkipsOrphanedSessions asserts that a
// session whose name has no entry in the freshly-built desired state
// (orphaned by the config edit — e.g. an agent was removed) is left
// untouched. The orphan/suspended branch of the reconciler handles such
// sessions on the next tick; soft-reload only updates sessions still
// mapped to a configured agent.
func TestAcceptConfigDriftAcrossSessions_SkipsOrphanedSessions(t *testing.T) {
	store := beads.NewMemStore()
	const staleHash = "stale-orphan-hash"
	sessionBead, err := store.Create(beads.Bead{
		Title:  "removed-agent",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "removed-agent",
			"template":            "removed-agent",
			"started_config_hash": staleHash,
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	// Desired state has a different agent — the original session is orphaned.
	desired := map[string]TemplateParams{
		"surviving-agent": {Command: "cmd", SessionName: "surviving-agent", TemplateName: "surviving-agent"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, nil, nil, nil, &stderr)
	if got.Updated != 0 {
		t.Fatalf("updated = %d, want 0 for orphaned session (stderr=%s)", got.Updated, stderr.String())
	}

	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != staleHash {
		t.Errorf("started_config_hash changed for orphan = %q, want %q", unchanged.Metadata["started_config_hash"], staleHash)
	}
}

// TestAcceptConfigDriftAcrossSessions_LeavesNonDriftingSessionsAlone asserts
// that a session whose stored hash already matches the recomputed current
// hash is not rewritten — the function returns 0 and metadata is
// untouched. This keeps soft-reload a no-op for the common no-drift case.
func TestAcceptConfigDriftAcrossSessions_LeavesNonDriftingSessionsAlone(t *testing.T) {
	store := beads.NewMemStore()
	matchingHash := runtime.CoreFingerprint(runtime.Config{Command: "cmd"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": matchingHash,
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	desired := map[string]TemplateParams{
		"worker": {Command: "cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, nil, nil, nil, &stderr)
	if got.Updated != 0 {
		t.Fatalf("updated = %d, want 0 (no drift) — stderr=%s", got.Updated, stderr.String())
	}

	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != matchingHash {
		t.Errorf("started_config_hash rewritten unexpectedly = %q, want %q", unchanged.Metadata["started_config_hash"], matchingHash)
	}
}

func TestAcceptConfigDriftAcrossSessions_CancelsExistingConfigDriftDrain(t *testing.T) {
	store := beads.NewMemStore()
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": oldHash,
			"generation":          "7",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	sp := runtime.NewFake()
	dt := newDrainTracker()
	clk := &clock.Fake{Time: time.Unix(100, 0)}
	if !beginSessionDrain(sessionBead, sp, dt, "config-drift", clk, time.Minute) {
		t.Fatal("beginSessionDrain returned false")
	}

	desired := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}
	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, nil, sp, dt, &stderr)
	if got.Updated != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", got.Updated, stderr.String())
	}
	if got.CanceledDrains != 1 {
		t.Fatalf("canceled drains = %d, want 1", got.CanceledDrains)
	}
	if drain := dt.get(sessionBead.ID); drain != nil {
		t.Fatalf("config-drift drain still queued after soft acceptance: %+v", drain)
	}
}

func TestAcceptConfigDriftAcrossSessions_FailsAckedDrainWithoutProviderBeforeMetadataWrite(t *testing.T) {
	store := beads.NewMemStore()
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": oldHash,
			"generation":          "7",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	dt := newDrainTracker()
	dt.set(sessionBead.ID, &drainState{
		reason:     "config-drift",
		generation: 7,
		ackSet:     true,
	})
	desired := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, nil, nil, dt, &stderr)
	if got.Updated != 0 || got.Failed != 1 {
		t.Fatalf("result = %+v, want updated=0 failed=1 (stderr=%s)", got, stderr.String())
	}
	if drain := dt.get(sessionBead.ID); drain == nil {
		t.Fatal("config-drift drain was removed even though provider cancellation was unavailable")
	}
	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != oldHash {
		t.Fatalf("started_config_hash = %q, want unchanged %q", unchanged.Metadata["started_config_hash"], oldHash)
	}
	warnings := strings.Join(got.warnings(), "\n")
	if !strings.Contains(warnings, "worker") {
		t.Fatalf("warnings = %q, want failed session name", warnings)
	}
}

func TestAcceptConfigDriftAcrossSessions_FailsWhenAckMetadataClearFails(t *testing.T) {
	store := beads.NewMemStore()
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": oldHash,
			"generation":          "7",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	sp := &removeMetaErrorProvider{Fake: runtime.NewFake(), err: errors.New("injected remove metadata failure")}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "old-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	if err := setReconcilerDrainAckMetadata(sp.Fake, "worker", &drainState{reason: "config-drift", generation: 7}); err != nil {
		t.Fatalf("set drain ack metadata: %v", err)
	}
	dt := newDrainTracker()
	dt.set(sessionBead.ID, &drainState{
		reason:     "config-drift",
		generation: 7,
		ackSet:     true,
	})
	desired := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, nil, sp, dt, &stderr)
	if got.Updated != 0 || got.Failed != 1 || got.CanceledDrains != 0 {
		t.Fatalf("result = %+v, want updated=0 failed=1 canceled=0 (stderr=%s)", got, stderr.String())
	}
	if drain := dt.get(sessionBead.ID); drain == nil || !drain.ackSet {
		t.Fatalf("config-drift drain = %+v, want acked drain retained", drain)
	}
	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != oldHash {
		t.Fatalf("started_config_hash = %q, want unchanged %q", unchanged.Metadata["started_config_hash"], oldHash)
	}
	if gotAck, err := sp.GetMeta("worker", "GC_DRAIN_ACK"); err != nil || gotAck != "1" {
		t.Fatalf("GC_DRAIN_ACK = %q, %v; want still set after injected remove failure", gotAck, err)
	}
	warnings := strings.Join(got.warnings(), "\n")
	if !strings.Contains(warnings, "worker") {
		t.Fatalf("warnings = %q, want failed session name", warnings)
	}
}

func TestAcceptConfigDriftAcrossSessions_AppliesTemplateOverridesToHash(t *testing.T) {
	store := beads.NewMemStore()
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "agent --effort low"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": oldHash,
			"template_overrides":  `{"effort":"high"}`,
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	provider := &config.ResolvedProvider{
		OptionsSchema: []config.ProviderOption{{
			Key:  "effort",
			Type: "select",
			Choices: []config.OptionChoice{
				{Value: "low", FlagArgs: []string{"--effort", "low"}},
				{Value: "high", FlagArgs: []string{"--effort", "high"}},
			},
		}},
		EffectiveDefaults: map[string]string{"effort": "low"},
	}
	desired := map[string]TemplateParams{
		"worker": {
			Command:          "agent --effort low",
			SessionName:      "worker",
			TemplateName:     "worker",
			ResolvedProvider: provider,
		},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, nil, nil, nil, &stderr)
	if got.Updated != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", got.Updated, stderr.String())
	}
	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "agent --effort high"})
	if updated.Metadata["started_config_hash"] != wantHash {
		t.Fatalf("started_config_hash = %q, want override hash %q", updated.Metadata["started_config_hash"], wantHash)
	}
}

func TestAcceptConfigDriftAcrossSessions_MetadataFailureReportsAndContinues(t *testing.T) {
	base := beads.NewMemStore()
	failing, err := base.Create(beads.Bead{
		Title:  "failing",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "failing",
			"template":            "failing",
			"started_config_hash": "stale-failing",
		},
	})
	if err != nil {
		t.Fatalf("Create(failing): %v", err)
	}
	succeeding, err := base.Create(beads.Bead{
		Title:  "succeeding",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "succeeding",
			"template":            "succeeding",
			"started_config_hash": "stale-succeeding",
		},
	})
	if err != nil {
		t.Fatalf("Create(succeeding): %v", err)
	}

	store := failingSetMetadataBatchStore{Store: base, failID: failing.ID}
	desired := map[string]TemplateParams{
		"failing":    {Command: "new-failing", SessionName: "failing", TemplateName: "failing"},
		"succeeding": {Command: "new-succeeding", SessionName: "succeeding", TemplateName: "succeeding"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, nil, nil, nil, &stderr)
	if got.Updated != 1 || got.Failed != 1 {
		t.Fatalf("result = %+v, want updated=1 failed=1 (stderr=%s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "updating config hash metadata for failing") {
		t.Fatalf("stderr = %q, want failing update error", stderr.String())
	}
	warnings := strings.Join(got.warnings(), "\n")
	if !strings.Contains(warnings, "failing") {
		t.Fatalf("warnings = %q, want failing session name", warnings)
	}
	updated, err := base.Get(succeeding.ID)
	if err != nil {
		t.Fatalf("Get(succeeding): %v", err)
	}
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "new-succeeding"})
	if updated.Metadata["started_config_hash"] != wantHash {
		t.Fatalf("succeeding started_config_hash = %q, want %q", updated.Metadata["started_config_hash"], wantHash)
	}
}

func TestAcceptConfigDriftAcrossSessions_EmptyDesiredReportsOpenSessions(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": "stale-hash",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, map[string]TemplateParams{}, nil, nil, nil, &stderr)
	if !got.DesiredEmpty || got.OpenSessions != 1 {
		t.Fatalf("result = %+v, want DesiredEmpty with one open session", got)
	}
}

type failingSetMetadataBatchStore struct {
	beads.Store
	failID string
}

func (s failingSetMetadataBatchStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.failID {
		return errors.New("injected metadata failure")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}
