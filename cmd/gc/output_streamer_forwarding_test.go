package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// noStreamProvider embeds the runtime.Provider INTERFACE, so the concrete type
// does NOT satisfy runtime.OutputStreamer (the embedded interface exposes no
// StreamOutput to promote). It stands in for a backend without the proc.stream op.
type noStreamProvider struct{ runtime.Provider }

// These cmd/gc wrappers sit on one-shot CLI paths, not the daemon SSE path the
// streaming consumer reads (cs.sp from newSessionProviderForCityByName is never
// status/caching-wrapped). Forwarding is still kept consistent with the Relaunch
// precedent — which forwards through these same two wrappers — so a future
// streaming consumer behind them is not silently masked. These tests pin it.
func TestStatusProvider_ForwardsStreamOutput(t *testing.T) {
	fake := runtime.NewFake()
	fake.StreamCapable = true
	fake.StreamFrames = map[string][]runtime.StreamFrame{"s": {{Stdout: []byte("SENTINEL")}}}
	sp, ok := newBoundedStatusProvider(fake).(runtime.OutputStreamer)
	if !ok {
		t.Fatal("statusProvider does not implement runtime.OutputStreamer")
	}
	frames, err := sp.StreamOutput(context.Background(), "s", []string{"tail"})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	if got := drainStdout(frames); got != "SENTINEL" {
		t.Errorf("streamed stdout = %q, want %q (channel must be the wrapped provider's)", got, "SENTINEL")
	}
	if got := fake.CountCalls("StreamOutput", "s"); got != 1 {
		t.Errorf("forwarded StreamOutput calls = %d, want 1", got)
	}

	noStream := newBoundedStatusProvider(noStreamProvider{Provider: runtime.NewFake()}).(runtime.OutputStreamer)
	if _, err := noStream.StreamOutput(context.Background(), "s", nil); !errors.Is(err, runtime.ErrStreamUnsupported) {
		t.Errorf("StreamOutput err = %v, want ErrStreamUnsupported", err)
	}
}

func TestAttachmentCachingProvider_ForwardsStreamOutput(t *testing.T) {
	fake := runtime.NewFake()
	fake.StreamCapable = true
	fake.StreamFrames = map[string][]runtime.StreamFrame{"s": {{Stdout: []byte("SENTINEL")}}}
	p := &attachmentCachingProvider{Provider: fake, cache: map[string]bool{}}
	frames, err := p.StreamOutput(context.Background(), "s", []string{"tail"})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	if got := drainStdout(frames); got != "SENTINEL" {
		t.Errorf("streamed stdout = %q, want %q (channel must be the wrapped provider's)", got, "SENTINEL")
	}
	if got := fake.CountCalls("StreamOutput", "s"); got != 1 {
		t.Errorf("forwarded StreamOutput calls = %d, want 1", got)
	}

	noStream := &attachmentCachingProvider{Provider: noStreamProvider{Provider: runtime.NewFake()}, cache: map[string]bool{}}
	if _, err := noStream.StreamOutput(context.Background(), "s", nil); !errors.Is(err, runtime.ErrStreamUnsupported) {
		t.Errorf("StreamOutput err = %v, want ErrStreamUnsupported", err)
	}
}

// drainStdout reads a stream channel to close, returning the concatenated stdout.
func drainStdout(frames <-chan runtime.StreamFrame) string {
	var out []byte
	for f := range frames {
		out = append(out, f.Stdout...)
	}
	return string(out)
}
