package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// mirrorFailingProvider stores metadata like the fake, then, for the drain
// writes t3bridge reflects onto the thread (GC_DRAIN, and GC_DRAIN_ACK set to
// 1), reports that the runtime's own view could not be updated: the value is in
// force regardless. The error has t3bridge's shape during an outage: a mirror
// failure whose cause itself wraps runtime.ErrRuntimeUnavailable and the
// transport error. When hardFailKey is set, SetMeta of that key fails outright
// instead.
type mirrorFailingProvider struct {
	*runtime.Fake
	hardFailKey string
}

func (p mirrorFailingProvider) SetMeta(name, key, value string) error {
	if key == p.hardFailKey {
		return fmt.Errorf("writing %s for %q: disk full", key, name)
	}
	if err := p.Fake.SetMeta(name, key, value); err != nil {
		return err
	}
	if key != "GC_DRAIN" && (key != "GC_DRAIN_ACK" || value != "1") {
		return nil
	}
	return t3OutageMirrorFailure(name, key)
}

// RemoveMeta removes like the fake, then reports t3bridge's mirror failure for
// the drain keys it reflects onto the thread.
func (p mirrorFailingProvider) RemoveMeta(name, key string) error {
	if err := p.Fake.RemoveMeta(name, key); err != nil {
		return err
	}
	if key != "GC_DRAIN" && key != "GC_DRAIN_ACK" {
		return nil
	}
	return t3OutageMirrorFailure(name, key)
}

func t3OutageMirrorFailure(name, key string) error {
	cause := fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, errors.New("dial tcp 127.0.0.1:1: connect: connection refused"))
	return &runtime.MetadataNotMirroredError{Session: name, Key: key, Cause: fmt.Errorf("reflecting onto its t3 thread: %w", cause)}
}

// A drain whose flag landed but whose runtime mirror failed is a drain: it
// exits 0, records the draining event, and warns.
func TestDoRuntimeDrainWarnsWhenOnlyTheMirrorFailed(t *testing.T) {
	sp := mirrorFailingProvider{Fake: runtime.NewFake()}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrain(&providerDrainOps{sp: sp}, sp, rec, "worker", "worker", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got, _ := sp.GetMeta("worker", "GC_DRAIN"); got == "" {
		t.Fatal("GC_DRAIN not stored")
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionDraining {
		t.Fatalf("events = %v, want one SessionDraining event", rec.Events)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("stderr = %q, want a warning about the mirror", stderr.String())
	}
}

// Likewise an ack whose flag landed pokes the controller and exits 0.
func TestFinishRuntimeDrainAckWarnsWhenOnlyTheMirrorFailed(t *testing.T) {
	poked := false
	old := drainAckPokeController
	drainAckPokeController = func(string, reconcilekey.Key) error { poked = true; return nil }
	t.Cleanup(func() { drainAckPokeController = old })

	sp := mirrorFailingProvider{Fake: runtime.NewFake()}
	var stdout, stderr bytes.Buffer
	code := finishRuntimeDrainAck(&providerDrainOps{sp: sp}, t.TempDir(), "worker", "worker", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got, _ := sp.GetMeta("worker", "GC_DRAIN_ACK"); got != "1" {
		t.Fatalf("GC_DRAIN_ACK = %q, want 1", got)
	}
	if !poked {
		t.Fatal("controller not poked after the ack landed")
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("stderr = %q, want a warning about the mirror", stderr.String())
	}
}

// An ack whose mirror failed but one of whose other writes failed outright is
// a failed ack: the join carries a real failure, so it exits 1 and leaves the
// controller alone.
func TestFinishRuntimeDrainAckFailsWhenAnotherAckWriteFailed(t *testing.T) {
	poked := false
	old := drainAckPokeController
	drainAckPokeController = func(string, reconcilekey.Key) error { poked = true; return nil }
	t.Cleanup(func() { drainAckPokeController = old })

	sp := mirrorFailingProvider{Fake: runtime.NewFake(), hardFailKey: reconcilerDrainAckSourceKey}
	var stdout, stderr bytes.Buffer
	code := finishRuntimeDrainAck(&providerDrainOps{sp: sp}, t.TempDir(), "worker", "worker", "", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if poked {
		t.Fatal("controller poked although the ack failed")
	}
}

// An undrain whose keys were removed but whose runtime mirror failed is an
// undrain: it exits 0, records the undrained event, and warns.
func TestDoRuntimeUndrainWarnsWhenOnlyTheMirrorFailed(t *testing.T) {
	sp := mirrorFailingProvider{Fake: runtime.NewFake()}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	_ = sp.Fake.SetMeta("worker", "GC_DRAIN", "123")
	_ = sp.Fake.SetMeta("worker", "GC_DRAIN_ACK", "1")
	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	code := doRuntimeUndrain(&providerDrainOps{sp: sp}, sp, rec, "worker", "worker", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, key := range []string{"GC_DRAIN", "GC_DRAIN_ACK"} {
		if got, _ := sp.GetMeta("worker", key); got != "" {
			t.Fatalf("%s = %q, want it removed", key, got)
		}
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionUndrained {
		t.Fatalf("events = %v, want one SessionUndrained event", rec.Events)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Fatalf("stderr = %q, want a warning about the mirror", stderr.String())
	}
}

// The min-floor cancel keeps its seat when the ack was cleared and only the
// runtime's view of the clear lags: falling through to the ordinary stop would
// retire a floor session whose ack is already gone.
func TestCancelSelfInitiatedDrainAckAtMinFloorKeepsSeatWhenOnlyTheMirrorFailed(t *testing.T) {
	infoByID, fake, _ := minFloorAckFixture()
	sp := mirrorFailingProvider{Fake: fake}
	var stderr bytes.Buffer
	if !cancelSelfInitiatedDrainAckAtMinFloor(infoByID, floorCfg(1), &providerDrainOps{sp: sp}, sp, newDrainTracker(), "worker", "worker-1", "s-a", &stderr) {
		t.Fatalf("cancel = false, want the floor seat kept; stderr: %s", stderr.String())
	}
	if got, _ := sp.GetMeta("worker-1", "GC_DRAIN_ACK"); got != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want it cleared", got)
	}
}

// A config-drift ack whose keys were cleared counts as cleared even when the
// runtime's view of the clear lags, so the tracker stops claiming the ack.
func TestClearSoftReloadConfigDriftDrainAckToleratesMirrorFailure(t *testing.T) {
	sp := mirrorFailingProvider{Fake: runtime.NewFake()}
	_ = sp.Fake.SetMeta("worker", "GC_DRAIN_ACK", "1")
	dt := newDrainTracker()
	dt.set("s-a", &drainState{reason: "config-drift", ackSet: true})
	info := sessionpkg.Info{ID: "s-a", SessionNameMetadata: "worker"}
	if err := clearSoftReloadConfigDriftDrainAckInfo(info, sp, dt); err != nil {
		t.Fatalf("clearSoftReloadConfigDriftDrainAckInfo = %v, want nil", err)
	}
	if dt.get("s-a").ackSet {
		t.Fatal("ackSet still true after the ack was cleared")
	}
}

// The reconciler's deferred ack is an ack once GC_DRAIN_ACK landed, even when
// the runtime's view of it lags: rolling it back would flap the ack, and record
// a failure, on every tick of a T3 outage.
func TestSetReconcilerDrainAckMetadataToleratesMirrorFailure(t *testing.T) {
	sp := mirrorFailingProvider{Fake: runtime.NewFake()}
	if err := setReconcilerDrainAckMetadata(sp, "worker", &drainState{reason: "orphaned", generation: 3}); err != nil {
		t.Fatalf("setReconcilerDrainAckMetadata = %v, want nil", err)
	}
	for key, want := range map[string]string{
		"GC_DRAIN_ACK":                  "1",
		reconcilerDrainAckSourceKey:     reconcilerDrainAckSourceValue,
		reconcilerDrainAckReasonKey:     "orphaned",
		reconcilerDrainAckGenerationKey: "3",
	} {
		if got, _ := sp.GetMeta("worker", key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// An ack write that failed outright is no ack: the setter rolls back the keys
// it wrote and reports the failure.
func TestSetReconcilerDrainAckMetadataRollsBackWhenTheAckWriteFailed(t *testing.T) {
	sp := mirrorFailingProvider{Fake: runtime.NewFake(), hardFailKey: "GC_DRAIN_ACK"}
	if err := setReconcilerDrainAckMetadata(sp, "worker", &drainState{reason: "orphaned", generation: 3}); err == nil || runtime.OnlyMetadataNotMirrored(err) {
		t.Fatalf("setReconcilerDrainAckMetadata = %v, want the write failure", err)
	}
	for _, key := range []string{"GC_DRAIN_ACK", reconcilerDrainAckSourceKey, reconcilerDrainAckReasonKey, reconcilerDrainAckGenerationKey} {
		if got, _ := sp.GetMeta("worker", key); got != "" {
			t.Errorf("%s = %q, want it rolled back", key, got)
		}
	}
}
