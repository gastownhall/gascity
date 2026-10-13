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
)

// mirrorFailingProvider stores metadata like the fake, then reports that the
// runtime's own view could not be updated: the value is in force regardless.
// The error has t3bridge's shape during an outage: a mirror failure whose cause
// itself wraps runtime.ErrRuntimeUnavailable and the transport error. When
// hardFailKey is set, SetMeta of that key fails outright instead.
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
	cause := fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, errors.New("dial tcp 127.0.0.1:1: connect: connection refused"))
	return &runtime.MetadataNotMirroredError{Session: name, Key: key, Cause: cause}
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
