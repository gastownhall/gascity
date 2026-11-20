package legacyop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

// A request the caller refuses for a cause it knows cannot be recorded is
// counted as a require refusal under its reason; in auto, where the caller
// runs it unrecorded instead, and in off, nothing is counted.
func TestRefusedIsCountedInRequireOnly(t *testing.T) {
	counts := captureCounts(t)
	e := newEnv(t)
	cliRecorder(e, gate.Require).Refused(opjournal.KindKill, ReasonNoGeneration)
	cliRecorder(e, gate.Auto).Refused(opjournal.KindKill, ReasonNoGeneration)
	cliRecorder(e, gate.Off).Refused(opjournal.KindKill, ReasonNoGeneration)
	want := []countedStep{{"kill", SurfaceCLI, StageAdmit, ResultRefused, ReasonNoGeneration}}
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

// The background close retry ends when the recorder's Done closes (its
// owner shutting down), instead of retrying until finish_by. The
// entry stays held until finish_by: the request's API did not die, so a
// same-key retry in this process must not adopt the op and deliver it a
// second time (DES-O0API I3, §12). The entry is released just past
// finish_by, never at it, so a same-key retry then closes the op as expired
// rather than adopting it.
func TestBackgroundCloseEndsOnDone(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	r.sleep = nil // real waits, interrupted by Done
	var holdFor time.Duration
	var releaseHold func()
	r.after = func(d time.Duration, f func()) { holdFor, releaseHold = d, f }
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
	if r.Inflight.Len() != 1 {
		t.Fatalf("registry %d after Done, want the entry held until finish_by", r.Inflight.Len())
	}
	if _, err := r.Admit(msg("k1", "hello")); !errors.Is(err, ErrBusy) {
		t.Fatalf("same key after Done: %v, want ErrBusy (no adoption)", err)
	}
	if want := adm.Op.FinishBy.Sub(e.clk.Now()) + time.Nanosecond; releaseHold == nil || holdFor != want {
		t.Fatalf("hold = %s (set %v), want %s, until just past finish_by", holdFor, releaseHold != nil, want)
	}
	// The hold ends at finish_by itself: the entry stays held a moment longer.
	e.clk.Advance(holdFor - time.Nanosecond)
	releaseHold()
	if r.Inflight.Len() != 1 || holdFor != time.Nanosecond {
		t.Fatalf("registry %d, re-held for %s at finish_by; want the entry held 1ns more", r.Inflight.Len(), holdFor)
	}
	if _, err := r.Admit(msg("k1", "hello")); !errors.Is(err, ErrBusy) {
		t.Fatalf("same key at finish_by: %v, want ErrBusy (no adoption)", err)
	}
	e.clk.Advance(holdFor)
	releaseHold()
	if r.Inflight.Len() != 0 {
		t.Fatalf("registry %d past finish_by", r.Inflight.Len())
	}
	if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("op %s after Done", got.Status)
	}
	// The store recovers before the sweeper runs: a same-key retry closes the
	// op as expired rather than adopting it.
	e.fault.updateForever = false
	again, err := r.Admit(msg("k1", "hello"))
	if err != nil || again.Decision != Replay || again.Adopted || again.Op.Outcome != failed(opjournal.ReasonDeadlineInFlight) {
		t.Fatalf("same key past finish_by = %+v, %v; want a replay of deadline_in_flight", again, err)
	}
}

// A background close that Done ends after finish_by has passed releases the
// entry at once: there is nothing left to hold for.
func TestBackgroundCloseEndsOnDonePastFinishBy(t *testing.T) {
	e := newEnv(t)
	r := apiRecorder(e, gate.Auto)
	r.after = func(time.Duration, func()) { t.Fatal("held an entry past finish_by") }
	done := make(chan struct{})
	r.Done = done
	adm, err := r.Admit(msg("k1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	e.fault.updateForever = true
	close(done)
	e.clk.Advance(ExecutionBound + time.Minute)
	f := adm.Finish(Result{Outcome: succeeded(ViaLegacyDelivered)})
	<-f.Background
	if r.Inflight.Len() != 0 {
		t.Fatalf("registry %d past finish_by", r.Inflight.Len())
	}
}

// Healthy is told of each close the store answers, the op closed or found
// already closed, before the entry is released: in Finish, in the background
// retry after Finish has returned, and in Admit's expiry rule (DES-O0API
// §5.5). A close the store fails, an expiry rule that writes nothing because
// the op was adopted since, a background retry that finish_by or Done ends
// and an invalid outcome are not health.
func TestHealthyIsToldOfEachAnsweredClose(t *testing.T) {
	out := succeeded(ViaLegacyDelivered)
	// watch records the stages r tells Healthy, failing a call made once the
	// request's entry is released.
	watch := func(t *testing.T, r *Recorder) *[]string {
		var stages []string
		r.Healthy = func(stage string) {
			if r.Inflight != nil && r.Inflight.Len() == 0 {
				t.Errorf("Healthy(%s) after the entry was released", stage)
			}
			stages = append(stages, stage)
		}
		return &stages
	}
	check := func(t *testing.T, got *[]string, want ...string) {
		t.Helper()
		if !slices.Equal(*got, want) {
			t.Fatalf("Healthy told %q, want %q", *got, want)
		}
	}
	sweep := func(t *testing.T, e *env) {
		e.clk.Advance(ExecutionBound + time.Second)
		if stats, err := Sweep(e.st, e.clk.Now(), 10, "controller:op-sweep"); err != nil || stats.Closed != 1 {
			t.Errorf("Sweep = %+v, %v", stats, err)
		}
	}
	t.Run("a foreground close", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		healthy := watch(t, r)
		adm, _ := r.Admit(msg("k1", "hello"))
		if f := adm.Finish(Result{Outcome: out}); !f.Closed {
			t.Fatalf("Finish = %+v", f)
		}
		check(t, healthy, StageClose)
	})
	t.Run("a foreground close that finds the op closed", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		healthy := watch(t, r)
		adm, _ := r.Admit(msg("k1", "hello"))
		sweep(t, e)
		if f := adm.Finish(Result{Outcome: out}); !f.Late {
			t.Fatalf("Finish = %+v, want late", f)
		}
		check(t, healthy, StageClose)
	})
	t.Run("a background close", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		healthy := watch(t, r)
		adm, _ := r.Admit(msg("k1", "hello"))
		e.fault.updateFails.Store(closeRetries)
		f := adm.Finish(Result{Outcome: out})
		if f.Background == nil {
			t.Fatalf("Finish = %+v, want a background close", f)
		}
		<-f.Background
		if got, _ := e.st.Get(adm.Op.ID); got.Status != opjournal.StatusClosed {
			t.Fatalf("op %s after the background close", got.Status)
		}
		check(t, healthy, StageClose)
	})
	t.Run("a background close that finds the op closed", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		healthy := watch(t, r)
		var waits atomic.Int32
		r.sleep = func(time.Duration) {
			if waits.Add(1) == closeRetries { // the first background wait
				sweep(t, e)
			}
		}
		adm, _ := r.Admit(msg("k1", "hello"))
		e.fault.updateFails.Store(closeRetries)
		f := adm.Finish(Result{Outcome: out})
		if f.Background == nil {
			t.Fatalf("Finish = %+v, want a background close", f)
		}
		<-f.Background
		if got, _ := e.st.Get(adm.Op.ID); got.Outcome != failed(opjournal.ReasonDeadlineInFlight) {
			t.Fatalf("stored %+v, want the sweeper's outcome", got.Outcome)
		}
		check(t, healthy, StageClose)
	})
	t.Run("the CLI's expiry rule", func(t *testing.T) {
		e := newEnv(t)
		r := cliRecorder(e, gate.Auto)
		healthy := watch(t, r)
		e.seed(t, msg("k1", "hello"), ExecutorCLI)
		e.clk.Advance(ExecutionBound + time.Second)
		if adm, err := r.Admit(msg("k1", "hello")); err != nil || adm.Decision != Replay {
			t.Fatalf("Admit = %+v, %v; want a replay of the expired op", adm, err)
		}
		check(t, healthy, StageClose)
	})
	t.Run("the API's expiry rule", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		healthy := watch(t, r)
		e.seed(t, msg("k1", "hello"), ExecutorAPI)
		e.clk.Advance(ExecutionBound + time.Second)
		if adm, err := r.Admit(msg("k1", "hello")); err != nil || adm.Decision != Replay {
			t.Fatalf("Admit = %+v, %v; want a replay of the expired op", adm, err)
		}
		check(t, healthy, StageClose)
	})
	t.Run("an expiry rule that finds the op adopted", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		var healthy []string
		r.Healthy = func(stage string) { healthy = append(healthy, stage) }
		op := e.seed(t, msg("k1", "hello"), ExecutorAPI)
		e.clk.Advance(ExecutionBound + time.Second)
		// Another API process adopts the op after the rule read it expired.
		if _, err := e.st.Adopt(op.ID, op.Attempt, string(ExecutorAPI), e.clk.Now().Add(ExecutionBound)); err != nil {
			t.Fatal(err)
		}
		if adm, err := r.closeExpired(e.st, "k1", false, op, e.clk.Now()); !errors.Is(err, ErrBusy) {
			t.Fatalf("closeExpired = %+v, %v; want busy", adm, err)
		}
		check(t, &healthy)
	})
	t.Run("a background retry finish_by ends", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		healthy := watch(t, r)
		r.sleep = func(time.Duration) { e.clk.Advance(time.Minute) }
		adm, _ := r.Admit(msg("k1", "hello"))
		e.fault.updateForever = true
		<-adm.Finish(Result{Outcome: out}).Background
		check(t, healthy)
	})
	t.Run("a background retry Done ends", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		healthy := watch(t, r)
		r.after = func(time.Duration, func()) {}
		done := make(chan struct{})
		r.Done = done
		adm, _ := r.Admit(msg("k1", "hello"))
		e.fault.updateForever = true
		close(done)
		<-adm.Finish(Result{Outcome: out}).Background
		check(t, healthy)
	})
	t.Run("an invalid outcome", func(t *testing.T) {
		e := newEnv(t)
		r := apiRecorder(e, gate.Auto)
		healthy := watch(t, r)
		adm, _ := r.Admit(msg("k1", "hello"))
		adm.Finish(Result{Outcome: opjournal.Outcome{Arm: "bogus"}})
		check(t, healthy)
	})
}

// TestDegradeStoreCannotRecord pins which degrades name a store that cannot
// record ops at all: an open that failed for want of conditional writes or a
// namespace. A store fault, contention, an unregistered type, or a
// capability gap at a later stage (a store that opened and then lost it) is
// not one.
func TestDegradeStoreCannotRecord(t *testing.T) {
	cases := []struct {
		stage, reason string
		want          bool
	}{
		{StageOpen, DegradeConditionalWritesUnsupported, true},
		{StageOpen, DegradeNoNamespace, true},
		{StageOpen, DegradeNotRecordedByDefault, true},
		{StageOpen, DegradeStoreError, false},
		{StageOpen, DegradeContention, false},
		{StageOpen, DegradeTypeUnregistered, false},
		{StagePropose, DegradeTypeUnregistered, false},
		{StageClose, DegradeConditionalWritesUnsupported, false},
		{StageSweep, DegradeNoNamespace, false},
	}
	for _, c := range cases {
		d := Degrade{Stage: c.stage, Reason: c.reason}
		if got := d.StoreCannotRecord(); got != c.want {
			t.Errorf("Degrade{%s, %s}.StoreCannotRecord() = %t, want %t", c.stage, c.reason, got, c.want)
		}
	}
}

// A recorder with QuietCannotRecord, as a gate that is auto only by its
// builtin default builds, treats a store that cannot record ops at all as
// off: that open degrade logs at Debug and never reaches the Degraded hook. A
// store fault still logs at Warn and reaches it, as does a store that cannot
// record under a recorder without the flag. Every request runs unrecorded.
func TestQuietCannotRecordDegrade(t *testing.T) {
	unsupported := fmt.Errorf("opening op store: %w", beads.ErrConditionalWriteUnsupported)
	for _, c := range []struct {
		name          string
		err           error
		quietRecorder bool
		quiet         bool
	}{
		{"no conditional writes", unsupported, true, true},
		{"no namespace", fmt.Errorf("opening op store: %w", opjournal.ErrNoNamespace), true, true},
		{"not recorded by default", &opjournal.NotRecordedByDefaultError{Backend: beads.ConditionalWritesBackend{Kind: "NativeDoltStore", DoltServed: true}}, true, true},
		{"store fault", errors.New("transport: refused"), true, false},
		{"no conditional writes without the flag", unsupported, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			h := &recordHandler{}
			r := apiRecorder(e, gate.Auto)
			r.Logger = slog.New(h)
			r.QuietCannotRecord = c.quietRecorder
			hooked := 0
			r.Degraded = func(Degrade) { hooked++ }
			r.Open = func() (*opjournal.Store, error) { return nil, c.err }
			adm, err := r.Admit(msg("", "hello"))
			if err != nil || adm.Decision != Execute {
				t.Fatalf("Admit = %+v, %v; want an unrecorded Execute", adm, err)
			}
			wantHooked, wantLevel := 1, slog.LevelWarn
			if c.quiet {
				wantHooked, wantLevel = 0, slog.LevelDebug
			}
			h.mu.Lock()
			var levels []slog.Level
			for _, rec := range h.records {
				levels = append(levels, rec.Level)
			}
			h.mu.Unlock()
			if hooked != wantHooked || !slices.Equal(levels, []slog.Level{wantLevel}) {
				t.Fatalf("hook heard %d, log levels %v; want %d and [%v]", hooked, levels, wantHooked, wantLevel)
			}
		})
	}
}

// A NotRecordedByDefaultError classifies as its own degrade reason, which
// CannotRecord treats as a store the default records nothing on, and counts
// its unrecorded request under its own reason.
func TestNotRecordedByDefaultClassifies(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &opjournal.NotRecordedByDefaultError{Backend: beads.ConditionalWritesBackend{Kind: "BdStore", DoltServed: true}})
	if got := DegradeReason(err); got != DegradeNotRecordedByDefault {
		t.Fatalf("DegradeReason = %q, want %q", got, DegradeNotRecordedByDefault)
	}
	if !CannotRecord(DegradeNotRecordedByDefault) {
		t.Fatal("CannotRecord(not_recorded_by_default) = false")
	}
	if got := openReason(err); got != ReasonNotRecordedByDefault {
		t.Fatalf("openReason = %q, want %q", got, ReasonNotRecordedByDefault)
	}
}
