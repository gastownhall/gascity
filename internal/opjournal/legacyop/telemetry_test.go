package legacyop

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// recordHandler keeps every record logged through it.
type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordHandler) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// A store outage logs one degrade line per (stage, reason) per
// DegradeLogInterval, not one per request; the Degraded hook still hears
// every degrade.
func TestDegradeLogIsRateLimited(t *testing.T) {
	e := newEnv(t)
	h := &recordHandler{}
	r := apiRecorder(e, gate.Auto)
	r.Logger = slog.New(h)
	hooked := 0
	r.Degraded = func(Degrade) { hooked++ }
	outage := errors.New("transport: refused")
	r.Open = func() (*opjournal.Store, error) { return nil, outage }
	for range 5 {
		if _, err := r.Admit(msg("", "hello")); err != nil {
			t.Fatal(err)
		}
	}
	if h.len() != 1 || hooked != 5 {
		t.Fatalf("logged %d lines, hook heard %d; want 1 and 5", h.len(), hooked)
	}
	r.degrade(opjournal.KindMessage, StagePropose, DegradeStoreError, outage)
	if h.len() != 2 {
		t.Fatalf("another stage logged %d lines in all, want 2", h.len())
	}
	e.clk.Advance(DegradeLogInterval - time.Second)
	r.degrade(opjournal.KindMessage, StageOpen, DegradeStoreError, outage)
	if h.len() != 2 {
		t.Fatalf("within the interval: %d lines, want 2", h.len())
	}
	e.clk.Advance(time.Second)
	r.degrade(opjournal.KindMessage, StageOpen, DegradeStoreError, outage)
	if h.len() != 3 {
		t.Fatalf("after the interval: %d lines, want 3", h.len())
	}
}
