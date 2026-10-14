package legacyop

import (
	"context"
	"errors"
	"fmt"
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

// countedStep is one recordOpRecord call.
type countedStep struct {
	kind, surface, stage, result, reason string
}

// captureCounts swaps recordOpRecord for the test and returns what it saw.
func captureCounts(t *testing.T) func() []countedStep {
	t.Helper()
	var mu sync.Mutex
	var steps []countedStep
	prev := recordOpRecord
	recordOpRecord = func(_ context.Context, kind, surface, stage, result, reason string) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, countedStep{kind, surface, stage, result, reason})
	}
	t.Cleanup(func() { recordOpRecord = prev })
	return func() []countedStep {
		mu.Lock()
		defer mu.Unlock()
		return append([]countedStep(nil), steps...)
	}
}

// An unrecorded request is counted once, at admit; its Finish counts nothing.
func TestUnrecordedIsCountedOnce(t *testing.T) {
	counts := captureCounts(t)
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	r.Open = func() (*opjournal.Store, error) { return nil, errors.New("transport: refused") }
	adm, err := r.Admit(msg("k1", "hello"))
	if err != nil || adm.Recorded {
		t.Fatalf("Admit = %+v, %v", adm, err)
	}
	adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)})
	r.Unrecorded(opjournal.KindMessage, "k2", ReasonNoGeneration).Finish(Result{Outcome: succeeded(ViaLegacyDelivered)})
	want := []countedStep{
		{"message", SurfaceAPI, StageAdmit, ResultUnrecorded, ReasonStoreUnavailable},
		{"message", SurfaceAPI, StageAdmit, ResultUnrecorded, ReasonNoGeneration},
	}
	if got := counts(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("counted %+v, want %+v", got, want)
	}
}

// An invalid outcome is the caller's bug, not a store failure: Finish logs it
// as an error, counts the close failed and releases the entry, but reports no
// degrade.
func TestFinishInvalidOutcomeIsNotADegrade(t *testing.T) {
	counts := captureCounts(t)
	e := newEnv(t)
	h := &recordHandler{}
	r := apiRecorder(e, gate.Auto)
	r.Logger = slog.New(h)
	var degraded []Degrade
	r.Degraded = func(d Degrade) { degraded = append(degraded, d) }
	adm, err := r.Admit(msg("k1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	f := adm.Finish(Result{Outcome: opjournal.Outcome{Arm: "bogus"}})
	if f.Closed || f.Background != nil || r.Inflight.Len() != 0 {
		t.Fatalf("Finish = %+v, registry %d", f, r.Inflight.Len())
	}
	if len(degraded) != 0 {
		t.Fatalf("degraded %+v, want none", degraded)
	}
	if h.len() != 1 || h.records[0].Level != slog.LevelError {
		t.Fatalf("logged %+v, want one error", h.records)
	}
	got := counts()
	if last := got[len(got)-1]; last != (countedStep{"message", SurfaceAPI, StageClose, ResultFailed, ""}) {
		t.Fatalf("last count %+v", last)
	}
}

// Finish closes through the store Admit opened: one Open per admission.
func TestFinishReusesAdmitsStore(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	adm, err := r.Admit(msg("k1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if f := adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)}); !f.Closed {
		t.Fatalf("Finish = %+v", f)
	}
	if n := e.opens.Load(); n != 1 {
		t.Fatalf("opened the store %d times, want 1", n)
	}
}

// The background close retry ends when the recorder's Done closes (the
// server shutting down), releasing the entry and leaving the op to the
// sweeper, instead of retrying until finish_by.
func TestBackgroundCloseEndsOnDone(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	r.sleep = nil // real waits, interrupted by Done
	done := make(chan struct{})
	r.Done = done
	adm, err := r.Admit(msg("k1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	e.fault.updateForever = true
	f := adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)})
	if f.Background == nil {
		t.Fatalf("Finish = %+v, want a background close", f)
	}
	close(done)
	select {
	case <-f.Background:
	case <-time.After(10 * time.Second):
		t.Fatal("the background close outlived Done")
	}
	if r.Inflight.Len() != 0 {
		t.Fatalf("registry %d after Done", r.Inflight.Len())
	}
	if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("op %s after Done", got.Status)
	}
}
