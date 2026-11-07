package exec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

type readingCarrier struct {
	runtime.Carrier
	screen string
	err    error
}

func (c readingCarrier) Peek(context.Context, string, int) (string, error) { return c.screen, c.err }

func TestPaneIdleReaderReadings(t *testing.T) {
	for _, tc := range []struct {
		name, screen string
		err          error
		want         idleReading
	}{
		{"idle", idleClaudePane, nil, readingIdle},
		{"busy", busyClaudePane, nil, readingBusy},
		{"unclassifiable", "no prompt", nil, readingUnclassifiable},
		{"capture-error", "", errors.New("capture failed"), readingCaptureError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (paneIdleReader{readingCarrier{screen: tc.screen, err: tc.err}}).readIdle(context.Background(), "s", "❯ ")
			if got != tc.want {
				t.Fatalf("reading=%v, want %v", got, tc.want)
			}
			if (err != nil) != (tc.want == readingUnclassifiable || tc.want == readingCaptureError) {
				t.Errorf("reading error = %v", err)
			}
		})
	}
}

func TestOpsCarrierContextReachesSubprocess(t *testing.T) {
	pk := newOpsPack(t, `"carrier.ops"`)
	pk.put(t, "peek.body", `exec sleep 30`)
	p := NewProvider(pk.script)
	// Resolve the handshake before starting the capture's clock.
	if _, err := p.Protocol(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.carrier().Peek(ctx, "s", 120)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Peek = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("subprocess ignored context: %s", time.Since(start))
	}
	pk.noTmux(t)
}

func TestOpsCarrierInterfaceRefusesEveryFence(t *testing.T) {
	pk := newOpsPack(t, `"carrier.ops"`)
	c := opsCarrier{NewProvider(pk.script)}
	ctx := context.Background()
	f := runtime.InputFence{}
	for _, err := range []error{
		c.NudgeFenced(ctx, "s", nil, f), c.SendKeysFenced(ctx, "s", f), c.InterruptFenced(ctx, "s", f), c.PublishInputFence(ctx, "s", f),
		c.FencedKeys(ctx, "s", f, func(func(...string) error) error { t.Fatal("refused fence ran body"); return nil }),
	} {
		if !errors.Is(err, runtime.ErrInputFenceUnsupported) {
			t.Errorf("fenced method = %v", err)
		}
	}
	if pk.log(t) != "" {
		t.Error("fenced carrier invoked an op")
	}
	if err := c.SendKeys(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := c.Nudge(ctx, "s", nil); err != nil {
		t.Fatal(err)
	}
	if pk.log(t) != "" {
		t.Error("empty driving operation invoked an op")
	}
}
