package exec

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The seam masks the Exec op (no production caller type-asserts it), but a
// consumer DOES type-assert OutputStreamer — so seamBackedProvider must forward
// the read-only proc.stream op through to its raw exec provider.
func TestSeamBacked_ForwardsStreamOutput(t *testing.T) {
	streamer, ok := NewSeamBacked(writeStreamScript(t, true)).(runtime.OutputStreamer)
	if !ok {
		t.Fatal("seamBackedProvider does not implement runtime.OutputStreamer")
	}
	frames, err := streamer.StreamOutput(context.Background(), "s", []string{"sh", "-c", "printf hi"})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	stdout, _, terminal := collectStream(t, frames)
	if string(stdout) != "hi" {
		t.Errorf("stdout = %q, want %q", stdout, "hi")
	}
	if terminal.Exit == nil || *terminal.Exit != 0 {
		t.Errorf("terminal Exit = %v, want 0", terminal.Exit)
	}
}

// The proc.stream gate lives in the raw provider, not the seam: a script that
// does not declare the capability makes the forwarded StreamOutput return
// ErrStreamUnsupported.
func TestSeamBacked_StreamUnsupportedWithoutCapability(t *testing.T) {
	streamer := NewSeamBacked(writeStreamScript(t, false)).(runtime.OutputStreamer)
	_, err := streamer.StreamOutput(context.Background(), "s", []string{"echo", "hi"})
	if !errors.Is(err, runtime.ErrStreamUnsupported) {
		t.Errorf("err = %v, want runtime.ErrStreamUnsupported", err)
	}
}
