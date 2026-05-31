package supervisor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// TestRunOnce_SuccessEmitsExactlyOneDoneEvent covers the happy path:
// a single successful runOnce must record exactly one event on the
// supplied recorder, and that event must be gc.store.maintenance.done with
// a decodable StoreMaintenanceDonePayload envelope.
func TestRunOnce_SuccessEmitsExactlyOneDoneEvent(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	fake := events.NewFake()
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      cfg,
		CityPath: "/tmp/city",
		Clock:    func() time.Time { return now },
		Rand:     func() float64 { return 0.5 },
		Recorder: fake,
	})

	loop.runOnce(context.Background())

	if got := len(fake.Events); got != 1 {
		t.Fatalf("recorded %d events; want exactly 1", got)
	}
	evt := fake.Events[0]
	if evt.Type != events.StoreMaintenanceDone {
		t.Fatalf("event type = %q; want %q", evt.Type, events.StoreMaintenanceDone)
	}
	if evt.Actor != maintenanceActor {
		t.Fatalf("event actor = %q; want %q", evt.Actor, maintenanceActor)
	}
	if evt.Subject != "/tmp/city" {
		t.Fatalf("event subject = %q; want %q", evt.Subject, "/tmp/city")
	}
	var payload events.StoreMaintenanceDonePayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("decode done payload: %v", err)
	}
}

// TestEmitRunEvent_FailureEmitsExactlyOneFailedEvent covers the error
// path. Downstream beads (ga-74d, ga-zoj) produce failing MaintenanceRun
// values when a stage errors; this bead's contract is that emitRunEvent
// translates any such run into exactly one gc.store.maintenance.failed
// event whose payload faithfully reproduces the stage, message, and
// snapshot path.
func TestEmitRunEvent_FailureEmitsExactlyOneFailedEvent(t *testing.T) {
	t.Parallel()
	fake := events.NewFake()
	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	finished := started.Add(3 * time.Second)
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true},
		CityPath: "/tmp/city",
		Recorder: fake,
	})

	loop.emitRunEvent(MaintenanceRun{
		StartedAt:    started,
		FinishedAt:   finished,
		Stage:        "gc",
		Err:          "CALL DOLT_GC() failed: out of disk",
		SnapshotPath: "/tmp/city/.beads/backups/2026-04-22T12-00-00Z",
	})

	if got := len(fake.Events); got != 1 {
		t.Fatalf("recorded %d events; want exactly 1", got)
	}
	evt := fake.Events[0]
	if evt.Type != events.StoreMaintenanceFailed {
		t.Fatalf("event type = %q; want %q", evt.Type, events.StoreMaintenanceFailed)
	}
	var payload events.StoreMaintenanceFailedPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("decode failed payload: %v", err)
	}
	if payload.Stage != "gc" {
		t.Fatalf("payload.Stage = %q; want %q", payload.Stage, "gc")
	}
	if payload.ErrorMsg != "CALL DOLT_GC() failed: out of disk" {
		t.Fatalf("payload.ErrorMsg = %q; want the injected message", payload.ErrorMsg)
	}
	if payload.SnapshotPath != "/tmp/city/.beads/backups/2026-04-22T12-00-00Z" {
		t.Fatalf("payload.SnapshotPath = %q; want the injected path", payload.SnapshotPath)
	}
	if payload.DurationSeconds != 3 {
		t.Fatalf("payload.DurationSeconds = %v; want 3", payload.DurationSeconds)
	}
}

// TestEmitRunEvent_SuccessPayloadFieldsMatch verifies the done payload
// carries duration, byte deltas, and snapshot path exactly as supplied.
// The JSON tag schema is part of the bead's acceptance contract — a
// rename here would silently break dashboards and the future status
// block (bead ga-e3s).
func TestEmitRunEvent_SuccessPayloadFieldsMatch(t *testing.T) {
	t.Parallel()
	fake := events.NewFake()
	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	finished := started.Add(250 * time.Millisecond)
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true},
		CityPath: "/tmp/city",
		Recorder: fake,
	})

	loop.emitRunEvent(MaintenanceRun{
		StartedAt:    started,
		FinishedAt:   finished,
		Stage:        "done",
		BeforeBytes:  11 * 1024 * 1024 * 1024,
		AfterBytes:   512 * 1024 * 1024,
		SnapshotPath: "/tmp/city/.beads/backups/ok",
	})

	if got := len(fake.Events); got != 1 {
		t.Fatalf("recorded %d events; want exactly 1", got)
	}
	var payload events.StoreMaintenanceDonePayload
	if err := json.Unmarshal(fake.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decode done payload: %v", err)
	}
	if payload.BeforeBytes != 11*1024*1024*1024 {
		t.Fatalf("payload.BeforeBytes = %d; want 11 GiB", payload.BeforeBytes)
	}
	if payload.AfterBytes != 512*1024*1024 {
		t.Fatalf("payload.AfterBytes = %d; want 512 MiB", payload.AfterBytes)
	}
	if payload.SnapshotPath != "/tmp/city/.beads/backups/ok" {
		t.Fatalf("payload.SnapshotPath = %q; want the injected path", payload.SnapshotPath)
	}
	if payload.DurationSeconds != 0.25 {
		t.Fatalf("payload.DurationSeconds = %v; want 0.25", payload.DurationSeconds)
	}
}

// TestEmitRunEvent_NilRecorderDoesNothing ensures that when the loop
// has no recorder (Discard default), emitRunEvent is safe to call and
// does not panic — supervisor boot paths that run without the typed
// event stream still exercise runOnce during tests.
func TestEmitRunEvent_NilRecorderDoesNothing(t *testing.T) {
	t.Parallel()
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true},
		CityPath: "/tmp/city",
		// Recorder unset → defaults to events.Discard.
	})

	// Should not panic and has no observable side effect.
	loop.emitRunEvent(MaintenanceRun{
		StartedAt: time.Now(),
		Stage:     "done",
	})
}

// TestRunOnce_LeaseContentionDoesNotEmit ensures that the silent
// lease-contention path records no event (runOnce returns before doing
// work).
func TestRunOnce_LeaseContentionDoesNotEmit(t *testing.T) {
	t.Parallel()
	fake := events.NewFake()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      cfg,
		CityPath: "/tmp/city",
		Clock:    func() time.Time { return now },
		Rand:     func() float64 { return 0.5 },
		Recorder: fake,
	})

	loop.mu.Lock()
	defer loop.mu.Unlock()

	loop.runOnce(context.Background())

	if got := len(fake.Events); got != 0 {
		t.Fatalf("recorded %d events under lease contention; want 0", got)
	}
}

// TestRunOnce_CanceledCtxDoesNotEmit ensures that a runOnce called with
// a canceled context returns before emitting. The scheduler uses this
// path during shutdown; an emitted event with zero duration would be a
// misleading artifact.
func TestRunOnce_CanceledCtxDoesNotEmit(t *testing.T) {
	t.Parallel()
	fake := events.NewFake()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      cfg,
		CityPath: "/tmp/city",
		Clock:    func() time.Time { return now },
		Rand:     func() float64 { return 0.5 },
		Recorder: fake,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop.runOnce(ctx)

	if got := len(fake.Events); got != 0 {
		t.Fatalf("recorded %d events after canceled ctx; want 0", got)
	}
}
