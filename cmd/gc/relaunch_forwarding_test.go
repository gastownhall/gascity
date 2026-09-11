package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// noRelaunchProvider embeds the runtime.Provider INTERFACE, so the concrete type
// does NOT satisfy runtime.RelaunchProvider (the embedded interface exposes no
// Relaunch method to promote) even when the underlying value can relaunch. It
// stands in for the conjoined providers (subprocess/acp/t3bridge) whose
// cut-over wrappers deliberately omit Relaunch.
type noRelaunchProvider struct{ runtime.Provider }

// The reconciler holds its provider as a plain runtime.Provider; if any wrapper
// in the chain failed to forward RelaunchProvider, the type-assert would mask a
// relaunch-capable backend and silently fall back to Stop+Start. These tests pin
// the forwarding for the two cmd/gc wrappers in that chain.
func TestStatusProvider_ForwardsRelaunch(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s", runtime.Config{Command: "c"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rp, ok := newBoundedStatusProvider(fake).(runtime.RelaunchProvider)
	if !ok {
		t.Fatal("statusProvider does not implement runtime.RelaunchProvider")
	}
	if err := rp.Relaunch(context.Background(), "s", runtime.Config{Command: "c2"}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if got := fake.CountCalls("Relaunch", "s"); got != 1 {
		t.Errorf("forwarded Relaunch calls = %d, want 1", got)
	}

	noRP := newBoundedStatusProvider(noRelaunchProvider{Provider: runtime.NewFake()}).(runtime.RelaunchProvider)
	if err := noRP.Relaunch(context.Background(), "s", runtime.Config{}); !errors.Is(err, runtime.ErrRelaunchUnsupported) {
		t.Errorf("Relaunch err = %v, want ErrRelaunchUnsupported", err)
	}
}

func TestAttachmentCachingProvider_ForwardsRelaunch(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s", runtime.Config{Command: "c"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p := &attachmentCachingProvider{Provider: fake, cache: map[string]bool{}}
	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "c2"}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if got := fake.CountCalls("Relaunch", "s"); got != 1 {
		t.Errorf("forwarded Relaunch calls = %d, want 1", got)
	}

	noRP := &attachmentCachingProvider{Provider: noRelaunchProvider{Provider: runtime.NewFake()}, cache: map[string]bool{}}
	if err := noRP.Relaunch(context.Background(), "s", runtime.Config{}); !errors.Is(err, runtime.ErrRelaunchUnsupported) {
		t.Errorf("Relaunch err = %v, want ErrRelaunchUnsupported", err)
	}
}
