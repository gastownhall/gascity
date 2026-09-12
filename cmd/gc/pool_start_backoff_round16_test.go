package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The cases codex round 16 found (evidence 07-codex-r16.md).

// TestBuildDesiredStateDeferralGateReadsTheWallClock: the production
// builder's beacon time is captured once at supervisor start; a backoff
// deadline written after it must still expire, so the gate reads the wall
// clock — pinned here through the production builder with a stale beacon.
func TestBuildDesiredStateDeferralGateReadsTheWallClock(t *testing.T) {
	cityPath := t.TempDir()
	cfg := warmCrossStoreCfg(t, cityPath)
	cfg.Agents[0].WorkQuery = "printf ''"
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"gascity": rigStore}
	beacon := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC) // the supervisor's start
	failedAt := beacon.Add(time.Minute)                    // a failure after it
	work, err := rigStore.Create(beads.Bead{Title: "rig work", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: warmWorkerTemplate, beadmeta.StartFailuresMetadataKey: "1", beadmeta.StartFailedAtMetadataKey: failedAt.Format(time.RFC3339), beadmeta.StartBackoffUntilMetadataKey: failedAt.Add(10 * time.Second).Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	inProgress, identity := "in_progress", warmWorkerTemplate
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &identity}); err != nil {
		t.Fatal(err)
	}
	seatsAt := func(now time.Time) int {
		prev := workStartDeferralNow
		workStartDeferralNow = func() time.Time { return now }
		defer func() { workStartDeferralNow = prev }()
		snapshot := newSessionBeadSnapshot(nil)
		var stderr bytes.Buffer
		buildDesiredStateWithSessionBeads("gc", cityPath, beacon, cfg, runtime.NewFake(), cityStore, rigStores, snapshot, nil, &stderr)
		seats := 0
		for _, info := range snapshot.OpenInfos() {
			if info.TriggerBeadID == work.ID {
				seats++
			}
		}
		return seats
	}
	if got := seatsAt(failedAt.Add(5 * time.Second)); got != 0 {
		t.Fatalf("inside the backoff: no seat, got %d", got)
	}
	if got := seatsAt(failedAt.Add(20 * time.Second)); got != 1 {
		t.Fatalf("the backoff expired on the WALL clock (the beacon is older than the failure): one seat, got %d", got)
	}
}

// TestControlDispatcherFallbackIgnoresDeferredWork: through the production
// builder, an open routed task of the deterministic control dispatcher is
// demand (count 1) while unparked and none once parked — the fallback that
// restores dispatcher demand is fed the same undeferred rows as the probes,
// so a parked task restores no triggerless seat.
func TestControlDispatcherFallbackIgnoresDeferredWork(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			StartCommand:      "gc convoy control --serve",
			WorkQuery:         "printf ''",
			MaxActiveSessions: intPtr(1),
			MaxStartFailures:  intPtr(1),
		}},
	}
	if !config.IsDeterministicControlDispatcher(&cfg.Agents[0]) {
		t.Fatal("fixture: not a deterministic control dispatcher")
	}
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "control task", Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: config.ControlDispatcherAgentName}})
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var stderr bytes.Buffer
		res := buildDesiredStateWithSessionBeads("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, nil, newSessionBeadSnapshot(nil), nil, &stderr)
		total := 0
		for template, n := range res.ScaleCheckCounts {
			if strings.Contains(template, config.ControlDispatcherAgentName) {
				total += n
			}
		}
		return total
	}
	if got := count(); got != 1 {
		t.Fatalf("control: an open routed dispatcher task is demand, count = %d", got)
	}
	if err := store.SetMetadataBatch(work.ID, map[string]string{beadmeta.ParkedAtMetadataKey: time.Now().UTC().Format(time.RFC3339), beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "1", beadmeta.ParkIDMetadataKey: "cafe", beadmeta.ParkMailedAtMetadataKey: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 0 {
		t.Fatalf("a parked dispatcher task restores no demand, count = %d", got)
	}
}

// TestParkMailStampRefusesAParkLiftedMeanwhile: the operator lifts the park
// with the documented three keys while the send is in flight; gc.park_id
// stays behind, so the identity alone would still match — the stamp must
// find the row parked, or write nothing.
func TestParkMailStampRefusesAParkLiftedMeanwhile(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	store := beads.NewMemStore()
	now := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	work, err := store.Create(beads.Bead{Title: "parked", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker", beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339), beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: "cafe",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore:         store,
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		notify: func(parkedWorkNotice) error {
			// The three-key unpark lands while the mail is in flight.
			return store.SetMetadataBatch(work.ID, map[string]string{beadmeta.ParkedAtMetadataKey: "", beadmeta.ParkReasonMetadataKey: "", beadmeta.ParkFailuresMetadataKey: ""})
		},
		stderr: &stderr,
	}
	policy.mailPark(store, work.ID, "worker", true)
	row, _ := store.Get(work.ID)
	if row.Metadata[beadmeta.ParkMailedAtMetadataKey] != "" {
		t.Fatalf("a park lifted meanwhile is not stamped: %v\nstderr:\n%s", row.Metadata, stderr.String())
	}
}
