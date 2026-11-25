package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The calls startPreparedStartCandidate makes on the runtime and the store
// before provider.Start run on a start goroutine that holds an async start
// slot. A call that never returns holds the slot forever, so every one of them
// is held to a bound and an unanswered wait reads as "unknown", never as
// "absent" (SESSION-RUNTIME-009). These tests wedge each call in turn.

const (
	// preStartWarmBound is the longest the warm-reuse nudge leg may wait: the
	// nudge's own idle wait, which is by design, plus the bound on the store and
	// nudge legs around it.
	preStartWarmBound = warmBindNudgeIdleTimeout + postStartObservationBound
)

// preStartProvider is postStartProvider with the switches the pre-Start calls
// need: a zombie runtime (up, agent process gone) and a hung IsRunning, the
// call the handle build's provider overlay makes. A hung call ignores its
// context and returns only when release is called, like a wedged tmux
// subprocess.
type preStartProvider struct {
	*postStartProvider
	// zombie makes the runtime read running but not alive until Stop.
	zombie atomic.Bool
	// hangIsRunning hangs every IsRunning call. Liveness observations do not
	// go through it, so a scenario wedges the handle build alone.
	hangIsRunning atomic.Bool
}

func newPreStartProvider(t *testing.T) *preStartProvider {
	t.Helper()
	sp := &preStartProvider{postStartProvider: newPostStartProvider()}
	sp.city = t.TempDir()
	return sp
}

// ObserveLivenessWithError is the liveness read of the start path's first
// look at the runtime. A zombie reads running but not alive.
func (p *preStartProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	obs, err := p.postStartProvider.ObserveLivenessWithError(name, processNames)
	if err == nil && obs.Running && p.zombie.Load() {
		obs.Alive = false
	}
	return obs, err
}

func (p *preStartProvider) IsRunning(name string) bool {
	if p.hangIsRunning.Load() {
		p.hang("IsRunning")
	}
	return p.Fake.IsRunning(name)
}

// Stop ends the runtime, zombie included.
func (p *preStartProvider) Stop(name string) error {
	if err := p.postStartProvider.Stop(name); err != nil {
		return err
	}
	p.zombie.Store(false)
	return nil
}

// seed starts a runtime under the worker session's name before the attempt and
// returns how many Start calls the provider has seen, the baseline for "no
// Start ran".
func (p *preStartProvider) seed(t *testing.T) int {
	t.Helper()
	if err := p.Fake.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	return p.CountCalls("Start", "worker")
}

// callsOf lists, in order, the calls of the given methods the provider saw.
func (p *preStartProvider) callsOf(methods ...string) []string {
	var got []string
	for _, c := range p.Calls {
		for _, m := range methods {
			if c.Method == m {
				got = append(got, c.Method)
			}
		}
	}
	return got
}

// preStartWedgedStore is a store whose Get of one bead hangs the way a stalled bd
// subprocess does: it waits on the provider's release and then answers.
type preStartWedgedStore struct {
	beads.Store
	sp     *preStartProvider
	id     string
	armed  atomic.Bool
	getsOf atomic.Int64
}

func (s *preStartWedgedStore) Get(id string) (beads.Bead, error) {
	if id == s.id && s.armed.Load() {
		s.getsOf.Add(1)
		s.sp.hang("store.Get " + id)
	}
	return s.Store.Get(id)
}

// preStartBead creates a session bead named name. mods overrides the starting
// metadata; an empty value removes the key.
func preStartBead(t *testing.T, store beads.Store, clk clock.Clock, workDir, name string, mods map[string]string) beads.Bead {
	t.Helper()
	meta := map[string]string{
		"session_name":              name,
		"template":                  "worker",
		"state":                     "creating",
		"pending_create_claim":      "true",
		"pending_create_started_at": clk.Now().Format(time.RFC3339),
		"provider":                  "claude",
		"transport":                 "tmux",
		"command":                   "claude",
		"work_dir":                  workDir,
		"generation":                "1",
		"continuation_epoch":        "7",
		"instance_token":            "tok-" + name,
		"session_key":               "resume-key",
		"resume_flag":               "--resume",
		"resume_style":              "flag",
		"started_config_hash":       "keep-hash",
	}
	for k, v := range mods {
		if v == "" {
			delete(meta, k)
			continue
		}
		meta[k] = v
	}
	bead, err := store.Create(beads.Bead{
		ID:       "gc-" + name,
		Title:    name,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Create session %q: %v", name, err)
	}
	return bead
}

// preStartItem is the prepared start of bead, named name.
func preStartItem(t *testing.T, bead beads.Bead, name, workDir string) preparedStart {
	t.Helper()
	item := postStartItem(sessiontest.SeedBead(t, bead), workDir)
	item.candidate.tp.SessionName = name
	return item
}

// runPreStartAttempt runs one start attempt on its own goroutine and fails the
// test if it has not returned within postStartWatchdog of fake time, with the
// provider calls that hung in the message.
func runPreStartAttempt(t *testing.T, item preparedStart, sp *preStartProvider, store beads.Store, cfg *config.City, warmClaim warmClaimTriggerProbe) (startResult, time.Duration) {
	t.Helper()
	item, release := leasedStart(t, sp.city, item)
	defer release()
	done := make(chan startResult, 1)
	begin := time.Now()
	go func() {
		done <- runPreparedStartCandidate(context.Background(), item, sp.city, sp, store, cfg, postStartTimeout, immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, warmClaim)
	}()
	select {
	case result := <-done:
		return result, time.Since(begin)
	case <-time.After(postStartWatchdog):
		t.Fatalf("runPreparedStartCandidate had not returned %v into the attempt: a call before provider.Start is unbounded; hung calls: %s", postStartWatchdog, sp.hungIn())
		return startResult{}, 0
	}
}

// requirePlainDeferral fails unless the result is a deferral that touched
// nothing: no error, no rollback, no rate-limit hold, no failure annotation.
func requirePlainDeferral(t *testing.T, result startResult) {
	t.Helper()
	if result.err != nil || result.outcome != TraceOutcomeDeferred || result.rollbackPending || result.rateLimitScreen || result.unattributedErr != nil {
		t.Fatalf("result = (err=%v, outcome=%q, rollback=%v, rateLimit=%v, unattributed=%v), want a plain deferral", result.err, result.outcome, result.rollbackPending, result.rateLimitScreen, result.unattributedErr)
	}
}

// requireNoRuntimeChange fails if the attempt started or stopped the worker
// session's runtime beyond the seeded baseline.
func requireNoRuntimeChange(t *testing.T, sp *preStartProvider, startsBefore int) {
	t.Helper()
	if got := sp.CountCalls("Start", "worker"); got != startsBefore {
		t.Fatalf("Start calls = %d, want %d: a start that deferred must not reach provider.Start", got, startsBefore)
	}
	if got := sp.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0 while the runtime cannot be observed", got)
	}
}

func preStartClock() *clock.Fake {
	return &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
}

// Test 1: the first liveness read wedges.
func TestRunPreparedStartCandidateDefersWhenThePreStartLivenessReadHangs(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := preStartClock()
		sp := newPreStartProvider(t)
		sp.hangObservation.Store(1)
		defer sp.release()
		store := beads.NewMemStore()
		bead := preStartBead(t, store, clk, workDir, "worker", nil)

		result, elapsed := runPreStartAttempt(t, preStartItem(t, bead, "worker", workDir), sp, store, nil, nil)

		if got := sp.hangs.Load(); got != 1 {
			t.Fatalf("%d provider calls hung, want exactly the liveness read under test: %s", got, sp.hungIn())
		}
		if elapsed > postStartBound {
			t.Fatalf("the attempt took %v, want it back within the %v bound", elapsed, postStartBound)
		}
		requirePlainDeferral(t, result)
		requireNoRuntimeChange(t, sp, 0)
		releaseAndSettle(t, sp.postStartProvider)
	})
}

// Test 2a: the pending-create identity read wedges.
func TestRunPreparedStartCandidateDefersWhenThePendingCreateIdentityReadHangs(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := preStartClock()
		sp := newPreStartProvider(t)
		sp.hangGetMeta.Store(true)
		defer sp.release()
		store := beads.NewMemStore()
		bead := preStartBead(t, store, clk, workDir, "worker", nil)
		startsBefore := sp.seed(t)

		result, elapsed := runPreStartAttempt(t, preStartItem(t, bead, "worker", workDir), sp, store, nil, nil)

		if got := sp.hangs.Load(); got != 1 {
			t.Fatalf("%d provider calls hung, want exactly the identity read under test: %s", got, sp.hungIn())
		}
		if elapsed > postStartBound {
			t.Fatalf("the attempt took %v, want it back within the one %v budget the identity reads share", elapsed, postStartBound)
		}
		requirePlainDeferral(t, result)
		requireNoRuntimeChange(t, sp, startsBefore)
		releaseAndSettle(t, sp.postStartProvider)
	})
}

// Test 2b: the identity read fails at once. An identity that cannot be read is
// unknown, so the start defers on it directly: no rate-limit peek and no
// second identity pass first.
func TestRunPreparedStartCandidateDefersOnAnUnreadableIdentityWithoutASecondPass(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := preStartClock()
		sp := newPreStartProvider(t)
		defer sp.release()
		boom := errors.New("show-environment failed")
		sp.GetMetaErrors = map[string]map[string]error{"worker": {
			"GC_SESSION_ID": boom, "GC_INSTANCE_TOKEN": boom, "GC_RUNTIME_EPOCH": boom,
		}}
		store := beads.NewMemStore()
		bead := preStartBead(t, store, clk, workDir, "worker", nil)
		startsBefore := sp.seed(t)

		result, _ := runPreStartAttempt(t, preStartItem(t, bead, "worker", workDir), sp, store, nil, nil)

		requirePlainDeferral(t, result)
		requireNoRuntimeChange(t, sp, startsBefore)
		if got := sp.CountCalls("Peek", "worker"); got != 0 {
			t.Fatalf("Peek calls = %d, want 0: an unattributable runtime defers before any rate-limit peek", got)
		}
		// One pass reads the id and then the token; a second pass would read both again.
		if got := sp.CountCalls("GetMeta", "worker"); got != 2 {
			t.Fatalf("GetMeta calls = %d, want 2: the identity is read once, not re-read after the start failed", got)
		}
	})
}

// Test 3: the warm-reuse store read wedges. The warm start already succeeded,
// so the nudge is skipped and the start still succeeds.
func TestRunPreparedStartCandidateSkipsTheWarmNudgeWhenTheStoreReadHangs(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := preStartClock()
		sp := newPreStartProvider(t)
		defer sp.release()
		mem := beads.NewMemStore()
		bead := preStartBead(t, mem, clk, workDir, "worker", map[string]string{
			"pending_create_claim":            "",
			"state":                           "active",
			"pool_managed":                    "true",
			beadmeta.TriggerBeadIDMetadataKey: "w-1",
		})
		store := &preStartWedgedStore{Store: mem, sp: sp, id: bead.ID}
		store.armed.Store(true)
		cfg := &config.City{Agents: []config.Agent{{Name: "worker", Nudge: warmClaimText}}}
		startsBefore := sp.seed(t)

		result, elapsed := runPreStartAttempt(t, preStartItem(t, bead, "worker", workDir), sp, store, cfg, alwaysUnclaimed)

		if got := store.getsOf.Load(); got != 1 {
			t.Fatalf("%d store reads hung, want exactly the one under test: %s", got, sp.hungIn())
		}
		if elapsed > preStartWarmBound {
			t.Fatalf("the attempt took %v, want it back within the %v bound of the warm nudge's wait", elapsed, preStartWarmBound)
		}
		if result.err != nil || result.outcome != TraceOutcomeSuccess {
			t.Fatalf("result = (err=%v, outcome=%q), want success: the warm reuse already succeeded and its nudge is best-effort", result.err, result.outcome)
		}
		if n, _ := countNudges(sp.Fake); n != 0 {
			t.Fatalf("%d nudges delivered, want none: the wait did not answer", n)
		}
		requireNoRuntimeChange(t, sp, startsBefore)
		releaseAndSettle(t, sp.postStartProvider)
	})
}

// Test 4: the zombie's pane read wedges. The runtime that cannot answer a peek
// is not stopped blind: the start defers with one Peek and no Stop.
func TestRunPreparedStartCandidateDefersAZombieWhoseRateLimitPeekHangs(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := preStartClock()
		sp := newPreStartProvider(t)
		sp.hangPeek.Store(true)
		defer sp.release()
		store := beads.NewMemStore()
		bead := preStartBead(t, store, clk, workDir, "worker", nil)
		startsBefore := sp.seed(t)
		sp.zombie.Store(true)

		result, elapsed := runPreStartAttempt(t, preStartItem(t, bead, "worker", workDir), sp, store, nil, nil)

		if got := sp.hangs.Load(); got != 1 {
			t.Fatalf("%d provider calls hung, want exactly the peek under test: %s", got, sp.hungIn())
		}
		if elapsed > postStartBound {
			t.Fatalf("the attempt took %v, want it back within the %v bound", elapsed, postStartBound)
		}
		requirePlainDeferral(t, result)
		requireNoRuntimeChange(t, sp, startsBefore)
		if got := sp.CountCalls("Peek", "worker"); got != 1 {
			t.Fatalf("Peek calls = %d, want 1", got)
		}
		releaseAndSettle(t, sp.postStartProvider)
	})
}

// Test 5: the zombie's Stop wedges. An abandoned Stop is not a success: the
// zombie is still there, so the start defers and never reaches provider.Start.
func TestStartPreparedStartCandidateDefersAZombieWhoseStopHangs(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := preStartClock()
		sp := newPreStartProvider(t)
		sp.hangStop.Store(true)
		defer sp.release()
		store := beads.NewMemStore()
		bead := preStartBead(t, store, clk, workDir, "worker", nil)
		startsBefore := sp.seed(t)
		sp.zombie.Store(true)
		item, release := leasedStart(t, sp.city, preStartItem(t, bead, "worker", workDir))
		defer release()

		type outcome struct {
			started bool
			err     error
		}
		done := make(chan outcome, 1)
		begin := time.Now()
		go func() {
			started, err := startPreparedStartCandidate(context.Background(), item, sp.city, store, sp, nil, &startPhaseTimings{}, immediateSessionStaleKeyDetectionWaiter, nil)
			done <- outcome{started, err}
		}()
		var got outcome
		select {
		case got = <-done:
		case <-time.After(postStartWatchdog):
			t.Fatalf("startPreparedStartCandidate had not returned %v in: a Stop before provider.Start is unbounded; hung calls: %s", postStartWatchdog, sp.hungIn())
		}

		if elapsed := time.Since(begin); elapsed > 2*postStartBound {
			t.Fatalf("the attempt took %v, want the peek and the stop each within the %v bound", elapsed, postStartBound)
		}
		if !errors.Is(got.err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable so the start defers", got.err)
		}
		if runtime.IsSessionGone(got.err) {
			t.Fatalf("err = %v reads as a gone session: an abandoned Stop would carry the start on past a zombie that is still there", got.err)
		}
		if got.started {
			t.Fatal("startedFresh = true for a start that never reached provider.Start")
		}
		if n := sp.CountCalls("Start", "worker"); n != startsBefore {
			t.Fatalf("Start calls = %d, want %d", n, startsBefore)
		}
		if n := sp.CountCalls("Stop", "worker"); n != 1 {
			t.Fatalf("Stop calls = %d, want the one that hung", n)
		}
		releaseAndSettle(t, sp.postStartProvider)
	})
}

// Test 6: a provider call inside the handle build wedges (the live overlay's
// IsRunning). The handle is the result of a bounded call and provider.Start is
// never reached.
func TestRunPreparedStartCandidateDefersWhenTheHandleBuildHangs(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := preStartClock()
		sp := newPreStartProvider(t)
		sp.hangIsRunning.Store(true)
		defer sp.release()
		store := beads.NewMemStore()
		bead := preStartBead(t, store, clk, workDir, "worker", nil)

		result, elapsed := runPreStartAttempt(t, preStartItem(t, bead, "worker", workDir), sp, store, nil, nil)

		if got := sp.hangs.Load(); got != 1 {
			t.Fatalf("%d provider calls hung, want exactly the one in the handle build: %s", got, sp.hungIn())
		}
		if elapsed > postStartBound {
			t.Fatalf("the attempt took %v, want it back within the %v bound", elapsed, postStartBound)
		}
		requirePlainDeferral(t, result)
		requireNoRuntimeChange(t, sp, 0)
		if result.providerStartCalled {
			t.Fatal("providerStartCalled = true for a start that never reached provider.Start")
		}
		releaseAndSettle(t, sp.postStartProvider)
	})
}

// Test 7: with nothing wedged each row behaves as it did.
func TestRunPreparedStartCandidateWithoutAWedgeBehavesAsBefore(t *testing.T) {
	workDir := t.TempDir()
	t.Run("fresh start", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			clk := preStartClock()
			sp := newPreStartProvider(t)
			defer sp.release()
			store := beads.NewMemStore()
			bead := preStartBead(t, store, clk, workDir, "worker", nil)

			result, _ := runPreStartAttempt(t, preStartItem(t, bead, "worker", workDir), sp, store, nil, nil)

			if result.err != nil || result.outcome != TraceOutcomeSuccess || !result.providerStartCalled {
				t.Fatalf("result = (err=%v, outcome=%q, providerStartCalled=%v), want a successful fresh start", result.err, result.outcome, result.providerStartCalled)
			}
			if got := sp.CountCalls("Start", "worker"); got != 1 {
				t.Fatalf("Start calls = %d, want 1", got)
			}
		})
	})
	t.Run("zombie recycle", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			clk := preStartClock()
			sp := newPreStartProvider(t)
			defer sp.release()
			store := beads.NewMemStore()
			bead := preStartBead(t, store, clk, workDir, "worker", nil)
			sp.seed(t)
			sp.zombie.Store(true)
			sp.Calls = nil

			result, _ := runPreStartAttempt(t, preStartItem(t, bead, "worker", workDir), sp, store, nil, nil)

			if result.err != nil || result.outcome != TraceOutcomeSuccess {
				t.Fatalf("result = (err=%v, outcome=%q), want the zombie recycled and the start to succeed", result.err, result.outcome)
			}
			if got, want := strings.Join(sp.callsOf("Peek", "Stop", "Start"), ","), "Peek,Stop,Start"; got != want {
				t.Fatalf("provider calls = %s, want %s: read the pane, stop the zombie, start fresh", got, want)
			}
		})
	})
	t.Run("warm reuse", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			clk := preStartClock()
			sp := newPreStartProvider(t)
			defer sp.release()
			store := beads.NewMemStore()
			bead := preStartBead(t, store, clk, workDir, "worker", map[string]string{
				"pending_create_claim":            "",
				"state":                           "active",
				"pool_managed":                    "true",
				beadmeta.TriggerBeadIDMetadataKey: "w-1",
			})
			cfg := &config.City{Agents: []config.Agent{{Name: "worker", Nudge: warmClaimText}}}
			startsBefore := sp.seed(t)

			result, _ := runPreStartAttempt(t, preStartItem(t, bead, "worker", workDir), sp, store, cfg, alwaysUnclaimed)

			if result.err != nil || result.outcome != TraceOutcomeSuccess || result.providerStartCalled {
				t.Fatalf("result = (err=%v, outcome=%q, providerStartCalled=%v), want a warm reuse that never calls provider.Start", result.err, result.outcome, result.providerStartCalled)
			}
			if n, last := countNudges(sp.Fake); n != 1 || last != warmClaimText {
				t.Fatalf("nudges = %d (last %q), want the claim nudge delivered once", n, last)
			}
			requireNoRuntimeChange(t, sp, startsBefore)
		})
	})
}

// Test 8: the headline. An async start wedged before provider.Start gives its
// slot back within the bound, so the next candidate's start runs instead of
// deferring on the limiter.
func TestEnqueuePreparedStartWaveReleasesItsSlotWhenAPreStartLivenessReadHangs(t *testing.T) {
	testCity := t.TempDir()
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := preStartClock()
		store := beads.NewMemStore()
		first := preStartBead(t, store, clk, workDir, "worker", nil)
		second := preStartBead(t, store, clk, workDir, "worker2", nil)
		sp := newPostStartProvider()
		// Observation 1 is the first candidate's liveness read before Start.
		sp.hangObservation.Store(1)
		defer sp.release()
		limiter := newAsyncStartLimiter(1)
		rec := events.NewFake()

		enqueue := func(bead beads.Bead, name string) (finished chan struct{}, released *atomic.Int64) {
			t.Helper()
			release, ok, reason := reserveAsyncStartSlot(context.Background(), limiter)
			if !ok {
				t.Fatalf("reserve async start slot for %s: %s", name, reason)
			}
			item, releaseLease := leasedStart(t, testCity, preStartItem(t, bead, name, workDir))
			t.Cleanup(releaseLease)
			finished = make(chan struct{})
			released = &atomic.Int64{}
			enqueuePreparedStartWaveForCity(
				context.Background(),
				[]asyncPreparedStart{{
					item: item,
					release: func() {
						released.Add(1)
						release()
					},
					done: func() { close(finished) },
				}},
				testCity, sp, store, nil, clk, rec, postStartTimeout, 1, ioDiscard{}, ioDiscard{}, nil, nil,
				immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil,
			)
			return finished, released
		}
		begin := time.Now()
		firstDone, firstReleased := enqueue(first, "worker")

		// The slot is held while the first start is wedged.
		if _, ok, _ := reserveAsyncStartSlot(context.Background(), limiter); ok {
			t.Fatal("a second slot was granted at limit 1: the scenario does not saturate the limiter")
		}
		select {
		case <-firstDone:
		case <-time.After(postStartWatchdog):
			t.Fatalf("the first async start had not finished %v in and still holds its slot: a call before provider.Start is unbounded (the bound is %v); hung calls: %s", postStartWatchdog, postStartBound, sp.hungIn())
		}
		if elapsed := time.Since(begin); elapsed > postStartBound {
			t.Fatalf("the first async start took %v, want its slot back within the %v bound", elapsed, postStartBound)
		}
		if got := firstReleased.Load(); got != 1 {
			t.Fatalf("slot released %d times, want 1", got)
		}

		secondDone, _ := enqueue(second, "worker2")
		select {
		case <-secondDone:
		case <-time.After(postStartWatchdog):
			t.Fatalf("the second async start had not finished: %s", sp.hungIn())
		}
		if got := sp.CountCalls("Start", "worker2"); got != 1 {
			t.Fatalf("Start calls for the second candidate = %d, want 1: it must run once the first gives its slot back", got)
		}
		if got := sp.CountCalls("Start", "worker"); got != 0 {
			t.Fatalf("Start calls for the wedged candidate = %d, want 0: it deferred", got)
		}
		releaseAndSettle(t, sp)
	})
}
