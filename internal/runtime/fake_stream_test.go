package runtime

import (
	"context"
	"errors"
	"testing"
)

// TestFakeStreamOutput_Branches exercises every branch of the fake's
// OutputStreamer so the StreamCapable/StreamFrames/StreamErrors scaffolding is
// not dead logic before a consumer lands.
func TestFakeStreamOutput_Branches(t *testing.T) {
	ctx := context.Background()

	t.Run("default fake is unsupported and records the call", func(t *testing.T) {
		f := NewFake()
		ch, err := f.StreamOutput(ctx, "s", []string{"tail"})
		if !errors.Is(err, ErrStreamUnsupported) {
			t.Errorf("err = %v, want ErrStreamUnsupported", err)
		}
		if ch != nil {
			t.Error("channel should be nil when unsupported")
		}
		if got := f.CountCalls("StreamOutput", "s"); got != 1 {
			t.Errorf("recorded StreamOutput calls = %d, want 1", got)
		}
		if f.Capabilities().CanStream {
			t.Error("default fake should report CanStream=false")
		}
	})

	t.Run("capable fake replays scripted frames in order", func(t *testing.T) {
		code := 3
		f := NewFake()
		f.StreamCapable = true
		f.StreamFrames = map[string][]StreamFrame{"s": {{Stdout: []byte("x")}, {Stderr: []byte("y")}, {Exit: &code}}}
		if !f.Capabilities().CanStream {
			t.Error("StreamCapable fake should report CanStream=true")
		}
		ch, err := f.StreamOutput(ctx, "s", nil)
		if err != nil {
			t.Fatalf("StreamOutput: %v", err)
		}
		var out, errOut []byte
		var exit *int
		for fr := range ch {
			out = append(out, fr.Stdout...)
			errOut = append(errOut, fr.Stderr...)
			if fr.Exit != nil {
				exit = fr.Exit
			}
		}
		if string(out) != "x" || string(errOut) != "y" || exit == nil || *exit != 3 {
			t.Errorf("replay = out:%q err:%q exit:%v, want x/y/3", out, errOut, exit)
		}
	})

	t.Run("capable fake with no scripted frames emits a clean Exit(0)", func(t *testing.T) {
		f := NewFake()
		f.StreamCapable = true
		ch, err := f.StreamOutput(ctx, "s", nil)
		if err != nil {
			t.Fatalf("StreamOutput: %v", err)
		}
		var frames []StreamFrame
		for fr := range ch {
			frames = append(frames, fr)
		}
		if len(frames) != 1 || frames[0].Exit == nil || *frames[0].Exit != 0 {
			t.Errorf("frames = %+v, want exactly one Exit(0)", frames)
		}
	})

	t.Run("StreamErrors returns a synchronous error with no channel", func(t *testing.T) {
		boom := errors.New("spawn boom")
		f := NewFake()
		f.StreamCapable = true
		f.StreamErrors = map[string]error{"s": boom}
		ch, err := f.StreamOutput(ctx, "s", nil)
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want %v", err, boom)
		}
		if ch != nil {
			t.Error("channel should be nil when StreamErrors is configured")
		}
	})

	t.Run("broken fake reports a transport error", func(t *testing.T) {
		f := NewFailFake()
		if _, err := f.StreamOutput(ctx, "s", nil); err == nil {
			t.Error("broken fake StreamOutput returned nil err")
		}
	})
}
