package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

// TestClosedSessionIntervalSettledByUsagePass pins AC-1 of issue #6672: a
// session a close ended straight from awake carries the close's settle
// markers, and the next non-boot usage pass — whose open-snapshot feed no
// longer holds the closed row — still accounts exactly one compute fact for
// the closed interval, anchored at the close time, and clears the markers.
func TestClosedSessionIntervalSettledByUsagePass(t *testing.T) {
	cityPath := t.TempDir()
	codexRoot := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	store := beads.NewMemStore()

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	closedAt := start.Add(10 * time.Minute)
	startRaw := start.Format(time.RFC3339Nano)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex pool worker",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "drained",
			"session_name":     "worker-1",
			"awake_started_at": startRaw,
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-D",
			// No session_key: a terminal codex session without a key settles
			// its model sweep as a definitive miss, so the interval commits
			// on this pass.
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	// The close's settle markers, exactly as ClosePatch stamps them.
	if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}

	// The next reconcile tick's usage pass: the open snapshot no longer holds
	// the closed row, exactly as the tick feeds it.
	cr.emitDueComputeFacts(context.Background(), nil, false)

	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatal(err)
	}
	var computes []usage.Fact
	for _, f := range facts {
		if f.Kind == usage.KindCompute {
			computes = append(computes, f)
		}
	}
	if len(computes) != 1 {
		t.Fatalf("compute facts = %d, want 1", len(computes))
	}
	if got := computes[0].SessionID; got != b.ID {
		t.Errorf("compute fact session = %q, want %q", got, b.ID)
	}
	if got := computes[0].WallSeconds; got != 600 {
		t.Errorf("compute fact wall seconds = %v, want 600 (closed_at − awake_started_at)", got)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[usageComputeEmittedAtKey] != startRaw {
		t.Errorf("usage_compute_emitted_at = %q, want %q", got.Metadata[usageComputeEmittedAtKey], startRaw)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "" {
		t.Errorf("usage_close_pending = %q, want cleared", v)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosedAt]; v != "" {
		t.Errorf("usage_closed_at = %q, want cleared", v)
	}
}

// TestClosedSessionSettleSweepsTrailingTurnsAndAdvancesCursor pins AC-2 of
// issue #6672: a closed codex row's settle runs the end-of-interval model
// sweep, so the turns its transcript gained after the last live sweep (the
// drain-ack turn among them) each yield exactly one model fact at the raw
// sink, and the invocation cursor advances on the closed bead so a later
// sweep of the same conversation re-reads nothing.
func TestClosedSessionSettleSweepsTrailingTurnsAndAdvancesCursor(t *testing.T) {
	cityPath := t.TempDir()
	codexRoot := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	store := beads.NewMemStore()
	writeCodexRolloutForSweep(t, codexRoot, workDir, [][3]int{
		{150, 100, 50},
		{450, 200, 100},
	})

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	closedAt := start.Add(10 * time.Minute)
	startRaw := start.Format(time.RFC3339Nano)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex pool worker",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "drained",
			"session_name":     "worker-1",
			"awake_started_at": startRaw,
			"session_key":      codexSweepSessionKey,
			"work_dir":         workDir,
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-E",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}
	cr.emitDueComputeFacts(context.Background(), nil, false)

	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindCount(facts, usage.KindModel); got != 2 {
		t.Fatalf("model facts = %d, want 2 (one per trailing turn): %+v", got, facts)
	}
	if got := kindCount(facts, usage.KindCompute); got != 1 {
		t.Fatalf("compute facts = %d, want 1", got)
	}
	if got := rawSinkModelFactCount(t, sinkPath); got != 2 {
		t.Fatalf("raw model facts = %d, want 2 (each trailing turn recorded exactly once)", got)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Metadata[session.MetadataKeyInvocationUsageCursor]; v == "" {
		t.Error("invocation_usage_cursor on the closed bead is empty, want the last turn's identity")
	}
	if got.Metadata[usageModelSweptAtKey] != startRaw {
		t.Errorf("usage_model_swept_at = %q, want %q", got.Metadata[usageModelSweptAtKey], startRaw)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "" {
		t.Errorf("usage_close_pending = %q, want cleared", v)
	}
}

// TestReopenedSessionOldIntervalSettledBeforeRewake pins AC-4 of issue #6672:
// a configured named session closed straight from active (a dead-runtime
// close) and reopened by the sync phase before the usage lane runs is an OPEN
// row carrying the close markers, so neither the closed-row listing nor the
// old open-lane filters find it. The same tick's usage pass must still settle
// the old interval — one compute fact with end = usage_closed_at, markers
// cleared before the reconciler re-wakes the session.
func TestReopenedSessionOldIntervalSettledBeforeRewake(t *testing.T) {
	cityPath := t.TempDir()
	codexRoot := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	store := beads.NewMemStore()

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	closedAt := start.Add(10 * time.Minute)
	startRaw := start.Format(time.RFC3339Nano)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex named worker",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "active",
			"session_name":     "worker-1",
			"awake_started_at": startRaw,
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-F",
			// No session_key: the settle sweep resolves a definitive miss, so
			// the interval commits on this pass.
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	// The dead-runtime close's settle markers, exactly as ClosePatch stamps them.
	if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
		t.Fatal(err)
	}
	// The sync phase reopens the row for a fresh spawn before the usage lane
	// runs: reopenNamedSessionBatch's write, a stopped row with a pending-create
	// claim, closed_at cleared but the close markers and the old awake interval
	// retained — the reconciler has not re-woken the session yet.
	closed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	reopenBatch := reopenNamedSessionBatch(closed, "stopped", closedAt.Add(time.Minute))
	open := "open"
	if err := store.Tx("test: reopen closed named session", func(tx beads.Tx) error {
		return tx.Update(b.ID, beads.UpdateOpts{Status: &open, Metadata: reopenBatch})
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}

	// The same legacy tick's usage pass, fed the open snapshot as the tick
	// feeds it — the reopened row sits in it, marked.
	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatal(err)
	}
	cr.emitDueComputeFacts(context.Background(), snap.OpenInfos(), false)

	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatal(err)
	}
	var computes []usage.Fact
	for _, f := range facts {
		if f.Kind == usage.KindCompute {
			computes = append(computes, f)
		}
	}
	if len(computes) != 1 {
		t.Fatalf("compute facts = %d, want 1 (the reopened row's old interval)", len(computes))
	}
	if got := computes[0].SessionID; got != b.ID {
		t.Errorf("compute fact session = %q, want %q", got, b.ID)
	}
	// End = usage_closed_at, not the pass time: the interval ended at the
	// close, ten minutes after its start.
	if got := computes[0].WallSeconds; got != 600 {
		t.Errorf("compute fact wall seconds = %v, want 600 (usage_closed_at − awake_started_at)", got)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[usageComputeEmittedAtKey] != startRaw {
		t.Errorf("usage_compute_emitted_at = %q, want %q", got.Metadata[usageComputeEmittedAtKey], startRaw)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "" {
		t.Errorf("usage_close_pending = %q, want cleared before the re-wake", v)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosedAt]; v != "" {
		t.Errorf("usage_closed_at = %q, want cleared before the re-wake", v)
	}
}

// rawSinkComputeFactCount counts compute facts in the raw sink log, BEFORE
// ReadFacts' idempotency-key dedup collapses re-records — the "a later pass
// records nothing" assertions need the raw count.
func rawSinkComputeFactCount(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading usage sink %s: %v", path, err)
	}
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var f usage.Fact
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("malformed usage fact %q: %v", line, err)
		}
		if f.Kind == usage.KindCompute {
			n++
		}
	}
	return n
}

// TestSettledMarkedRowWithNothingToAccountClearsMarkers pins AC-5 of issue
// #6672: a marked row the lane visits with nothing to account — no confirmed
// awake interval, an interval already committed, an open row whose re-wake
// replaced the marked interval, or an open row still live on the interval the
// close claims to have ended — records no compute fact and clears the
// markers so the listing and the open filter stop returning it.
func TestSettledMarkedRowWithNothingToAccountClearsMarkers(t *testing.T) {
	cityRoot := t.TempDir()
	codexRoot := t.TempDir()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	closedAt := start.Add(10 * time.Minute)
	startRaw := start.Format(time.RFC3339Nano)
	rewakeRaw := start.Add(20 * time.Minute).Format(time.RFC3339Nano)

	cases := []struct {
		name string
		// open feeds the row as the tick does (the open snapshot); closed
		// leaves it to the marked closed listing.
		open         bool
		state        string
		awakeStartAt string
		emittedAt    string
		// wantEmitted is the usage_compute_emitted_at value the pass must
		// leave on the row: unchanged where the interval's own accounting is
		// not the settle's to touch.
		wantEmitted string
	}{
		{
			name:         "no awake interval",
			state:        "drained",
			awakeStartAt: "",
			emittedAt:    "",
		},
		{
			name:         "interval already committed",
			state:        "drained",
			awakeStartAt: startRaw,
			emittedAt:    startRaw,
			wantEmitted:  startRaw,
		},
		{
			// The sync phase reopened the row and the reconciler already
			// re-woke it on a newer interval: the marked interval is
			// superseded and lost.
			name:         "reopen re-woke a newer interval",
			open:         true,
			state:        "active",
			awakeStartAt: rewakeRaw,
			wantEmitted:  "",
		},
		{
			// The close's marker raced a row that is still awake on the
			// interval the close claims to have ended: the interval is live,
			// not closed, and the ordinary lanes account it when it ends.
			name:         "still live on the marked interval's start",
			open:         true,
			state:        "active",
			awakeStartAt: startRaw,
			wantEmitted:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := filepath.Join(cityRoot, t.Name())
			sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
			store := beads.NewMemStore()
			meta := map[string]string{
				"state":            tc.state,
				"session_name":     "worker-1",
				"provider":         "codex",
				"builtin_ancestor": "codex",
				"molecule_id":      "run-G",
			}
			if tc.awakeStartAt != "" {
				meta["awake_started_at"] = tc.awakeStartAt
			}
			if tc.emittedAt != "" {
				meta[usageComputeEmittedAtKey] = tc.emittedAt
			}
			b, err := store.Create(beads.Bead{
				Type:     session.BeadType,
				Status:   "open",
				Title:    "codex worker",
				Labels:   []string{session.LabelSession},
				Metadata: meta,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.open {
				if err := store.Close(b.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
				t.Fatal(err)
			}

			cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
			cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
			cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}

			var openInfos []session.Info
			if tc.open {
				snap, err := loadSessionBeadSnapshot(store)
				if err != nil {
					t.Fatal(err)
				}
				openInfos = snap.OpenInfos()
			}
			cr.emitDueComputeFacts(context.Background(), openInfos, false)

			facts, _, err := usage.ReadFacts(sinkPath)
			if err != nil {
				t.Fatal(err)
			}
			if n := kindCount(facts, usage.KindCompute); n != 0 {
				t.Errorf("compute facts = %d, want 0 (nothing to account)", n)
			}
			got, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "" {
				t.Errorf("usage_close_pending = %q, want cleared", v)
			}
			if v := got.Metadata[session.MetadataKeyUsageClosedAt]; v != "" {
				t.Errorf("usage_closed_at = %q, want cleared", v)
			}
			if got := got.Metadata[usageComputeEmittedAtKey]; got != tc.wantEmitted {
				t.Errorf("usage_compute_emitted_at = %q, want %q (the interval's own accounting untouched)", got, tc.wantEmitted)
			}
		})
	}
}

// TestSettledRowIsNotRevisitedOnLaterPass pins AC-5 of issue #6672: a row the
// lane has settled — its markers cleared — is listed no more on a later pass
// and records nothing, so the settle is idempotent across passes.
func TestSettledRowIsNotRevisitedOnLaterPass(t *testing.T) {
	cityPath := t.TempDir()
	codexRoot := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	store := beads.NewMemStore()

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	closedAt := start.Add(10 * time.Minute)
	startRaw := start.Format(time.RFC3339Nano)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex worker",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "drained",
			"session_name":     "worker-1",
			"awake_started_at": startRaw,
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-H",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}

	cr.emitDueComputeFacts(context.Background(), nil, false)
	if got := rawSinkComputeFactCount(t, sinkPath); got != 1 {
		t.Fatalf("raw compute facts after the first pass = %d, want 1", got)
	}
	// The later pass: the markers are gone, so the listing returns the row no
	// more and nothing is recorded for it again.
	cr.emitDueComputeFacts(context.Background(), nil, false)
	cr.emitDueComputeFacts(context.Background(), nil, false)
	if got := rawSinkComputeFactCount(t, sinkPath); got != 1 {
		t.Fatalf("raw compute facts after later passes = %d, want 1 (settled row records nothing)", got)
	}
}

// closedIntervalFailSink is a usage sink whose Record always fails: the
// settle must keep every marker so the next pass retries the fact.
type closedIntervalFailSink struct{}

func (closedIntervalFailSink) Record(_ context.Context, _ usage.Fact) error {
	return errors.New("sink down")
}

// TestClosedIntervalUnsettledSweepRetriesFlooredThenGivesUp pins AC-6 of
// issue #6672: a closed row whose trailing model-usage sweep cannot settle (a
// keyed codex rollout that never appeared) records its compute fact per
// attempt without committing, keeps its markers, and retries at most once per
// liveModelSweepMinInterval per session; the first attempt at or after
// usage_closed_at + 15m commits the compute marker and clears the markers
// even though the sweep never settled.
func TestClosedIntervalUnsettledSweepRetriesFlooredThenGivesUp(t *testing.T) {
	codexRoot := t.TempDir()
	startRaw := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339Nano)

	t.Run("before the deadline: records without committing, floors the retry", func(t *testing.T) {
		cityPath := t.TempDir()
		sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
		store := beads.NewMemStore()
		// The close sits in the past but its give-up deadline in the future,
		// so every attempt in this subtest is before the deadline.
		closedAt := time.Now().UTC().Add(-5 * time.Minute)
		b, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Status: "open",
			Title:  "codex worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"state":            "drained",
				"session_name":     "worker-1",
				"awake_started_at": startRaw,
				// A keyed codex session whose rollout never appears: the
				// keyed miss stays transient, so the sweep never settles.
				"session_key":      codexSweepSessionKey,
				"provider":         "codex",
				"builtin_ancestor": "codex",
				"molecule_id":      "run-I",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
			t.Fatal(err)
		}

		cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
		cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
		cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}

		cr.emitDueComputeFacts(context.Background(), nil, false)
		if got := rawSinkComputeFactCount(t, sinkPath); got != 1 {
			t.Fatalf("raw compute facts after the first attempt = %d, want 1 (recorded without committing)", got)
		}
		got, err := store.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v := got.Metadata[usageComputeEmittedAtKey]; v != "" {
			t.Errorf("usage_compute_emitted_at = %q, want unset (not committed)", v)
		}
		if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "true" {
			t.Errorf("usage_close_pending = %q, want kept (markers survive an unsettled attempt)", v)
		}
		// The pass inside the retry floor makes no attempt: nothing more is
		// recorded and nothing changes.
		cr.emitDueComputeFacts(context.Background(), nil, false)
		if got := rawSinkComputeFactCount(t, sinkPath); got != 1 {
			t.Fatalf("raw compute facts after a floored pass = %d, want 1 (at most one attempt per 30s per session)", got)
		}
		got, err = store.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v := got.Metadata[usageComputeEmittedAtKey]; v != "" {
			t.Errorf("usage_compute_emitted_at after a floored pass = %q, want unset", v)
		}
		if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "true" {
			t.Errorf("usage_close_pending after a floored pass = %q, want kept", v)
		}
	})

	t.Run("at the deadline: an unsettled attempt commits and clears", func(t *testing.T) {
		cityPath := t.TempDir()
		sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
		store := beads.NewMemStore()
		// The close sits past the give-up deadline, so the first attempt is
		// already at it.
		closedAt := time.Now().UTC().Add(-20 * time.Minute)
		b, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Status: "open",
			Title:  "codex worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"state":            "drained",
				"session_name":     "worker-1",
				"awake_started_at": startRaw,
				"session_key":      codexSweepSessionKey,
				"provider":         "codex",
				"builtin_ancestor": "codex",
				"molecule_id":      "run-J",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
			t.Fatal(err)
		}

		cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
		cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
		cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}

		cr.emitDueComputeFacts(context.Background(), nil, false)
		if got := rawSinkComputeFactCount(t, sinkPath); got != 1 {
			t.Fatalf("raw compute facts = %d, want 1", got)
		}
		got, err := store.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata[usageComputeEmittedAtKey] != startRaw {
			t.Errorf("usage_compute_emitted_at = %q, want %q (committed past the give-up deadline)",
				got.Metadata[usageComputeEmittedAtKey], startRaw)
		}
		if v := got.Metadata[usageModelSweptAtKey]; v != "" {
			t.Errorf("usage_model_swept_at = %q, want unset (the sweep never settled)", v)
		}
		if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "" {
			t.Errorf("usage_close_pending = %q, want cleared", v)
		}
		if v := got.Metadata[session.MetadataKeyUsageClosedAt]; v != "" {
			t.Errorf("usage_closed_at = %q, want cleared", v)
		}
	})
}

// TestClosedIntervalSinkFailureKeepsMarkers pins AC-6 of issue #6672: a sink
// Record failure never clears the close markers — the next pass retries the
// fact idempotently — and never commits the interval.
func TestClosedIntervalSinkFailureKeepsMarkers(t *testing.T) {
	cityPath := t.TempDir()
	codexRoot := t.TempDir()
	store := beads.NewMemStore()
	start := time.Now().UTC().Add(-30 * time.Minute)
	closedAt := time.Now().UTC().Add(-20 * time.Minute)
	startRaw := start.Format(time.RFC3339Nano)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex worker",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "drained",
			"session_name":     "worker-1",
			"awake_started_at": startRaw,
			// Keyless: the sweep settles a definitive miss, so only the sink
			// stands between the attempt and the commit.
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-K",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: closedIntervalFailSink{}, cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}

	cr.emitDueComputeFacts(context.Background(), nil, false)
	cr.emitDueComputeFacts(context.Background(), nil, false)
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Metadata[usageComputeEmittedAtKey]; v != "" {
		t.Errorf("usage_compute_emitted_at = %q, want unset (a failed record never commits)", v)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "true" {
		t.Errorf("usage_close_pending = %q, want kept (a failed record never clears the markers)", v)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosedAt]; v == "" {
		t.Error("usage_closed_at cleared, want kept (a failed record never clears the markers)")
	}
}
