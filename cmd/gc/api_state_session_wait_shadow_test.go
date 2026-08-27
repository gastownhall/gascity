package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func sessionWaitShadowEvent(t *testing.T, bead beads.Bead) events.Event {
	t.Helper()
	if bead.ID == "" {
		bead.ID = "wait-event"
	}
	payload, err := json.Marshal(bead)
	if err != nil {
		t.Fatalf("marshal bead event: %v", err)
	}
	return events.Event{
		Type:    events.BeadUpdated,
		Subject: bead.ID,
		Payload: payload,
	}
}

func TestSessionWaitDependencyShadowAdmissionRetriesPendingRequest(t *testing.T) {
	cs := &controllerState{}
	var calls int
	if err := cs.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
		calls++
		if calls > 1 {
			return sessionWaitShadowConverged
		}
		return sessionWaitShadowRetry
	}, func(string) bool { return false }); err != nil {
		t.Fatalf("install admission: %v", err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	cs.admitSessionWaitDependencyShadowEvent(sessionWaitShadowEvent(
		t,
		sessionWaitShadowBead("session-a", "dep-a"),
	))
	cs.admitSessionWaitDependencyShadowEvent(events.Event{
		Type:    events.BeadUpdated,
		Payload: []byte(`{"malformed"`),
	})

	if calls != 2 {
		t.Fatalf("refresh calls = %d, want retry after pending failure", calls)
	}
}

func TestSessionWaitDependencyShadowAdmissionSkipsCleanUnrelatedEvents(t *testing.T) {
	cs := &controllerState{}
	var calls int
	if err := cs.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
		calls++
		return sessionWaitShadowConverged
	}, func(string) bool { return false }); err != nil {
		t.Fatalf("install admission: %v", err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	cs.admitSessionWaitDependencyShadowEvent(sessionWaitShadowEvent(t, beads.Bead{
		ID:     "task-1",
		Type:   "task",
		Status: "open",
	}))
	cs.admitSessionWaitDependencyShadowEvent(events.Event{
		Type:    events.BeadUpdated,
		Payload: []byte(`{"malformed"`),
	})
	cs.admitSessionWaitDependencyShadowEvent(events.Event{
		Type:    events.ControllerStarted,
		Payload: []byte(`{"id":"not-a-bead-event"}`),
	})

	if calls != 0 {
		t.Fatalf("refresh calls = %d, want none for a clean unrelated projection", calls)
	}
}

func TestSessionWaitDependencyShadowAdmissionRecognizesWaitIdentityRemoval(t *testing.T) {
	cs := &controllerState{}
	var calls int
	if err := cs.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
		calls++
		return sessionWaitShadowConverged
	}, func(id string) bool { return id == "wait-1" }); err != nil {
		t.Fatalf("install admission: %v", err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	cs.admitSessionWaitDependencyShadowEvent(sessionWaitShadowEvent(t, beads.Bead{
		ID:     "wait-1",
		Type:   "task",
		Status: "open",
	}))

	if calls != 1 {
		t.Fatalf("refresh calls = %d, want prior wait membership to request one census", calls)
	}
}

func TestSessionWaitDependencyShadowAdmissionOlderSuccessCannotClearNewerFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs := &controllerState{}
		firstEntered := make(chan struct{})
		releaseFirst := make(chan struct{})
		firstReturned := make(chan struct{})
		var calls atomic.Int64
		if err := cs.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
			switch calls.Add(1) {
			case 1:
				close(firstEntered)
				<-releaseFirst
				return sessionWaitShadowConverged
			default:
				return sessionWaitShadowRetry
			}
		}, func(string) bool { return false }); err != nil {
			t.Fatalf("install admission: %v", err)
		}
		t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

		go func() {
			cs.requestSessionWaitDependencyShadowRefreshForBead(beads.Bead{}, true)
			close(firstReturned)
		}()
		synctest.Wait()
		select {
		case <-firstEntered:
		default:
			t.Fatal("first refresh did not enter")
		}
		cs.requestSessionWaitDependencyShadowRefreshForBead(beads.Bead{}, true)
		close(releaseFirst)
		synctest.Wait()
		select {
		case <-firstReturned:
		default:
			t.Fatal("first refresh did not return")
		}
		cs.requestSessionWaitDependencyShadowRefreshForBead(beads.Bead{}, false)

		if got := calls.Load(); got != 3 {
			t.Fatalf("refresh calls = %d, want pending newer generation retried after older success", got)
		}
	})
}

func TestSessionWaitDependencyShadowAdmissionStopJoinsAndRejectsLaterEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs := &controllerState{}
		entered := make(chan struct{})
		release := make(chan struct{})
		requestReturned := make(chan struct{})
		stopReturned := make(chan struct{})
		var calls atomic.Int64
		if err := cs.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
			calls.Add(1)
			close(entered)
			<-release
			return sessionWaitShadowConverged
		}, func(string) bool { return false }); err != nil {
			t.Fatalf("install admission: %v", err)
		}

		go func() {
			cs.requestSessionWaitDependencyShadowRefreshForBead(beads.Bead{}, true)
			close(requestReturned)
		}()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("refresh did not enter")
		}
		go func() {
			cs.stopSessionWaitDependencyShadowAdmission()
			close(stopReturned)
		}()
		synctest.Wait()

		select {
		case <-stopReturned:
			t.Fatal("stop returned while a refresh callback was still in flight")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case <-requestReturned:
		default:
			t.Fatal("in-flight refresh did not return")
		}
		select {
		case <-stopReturned:
		default:
			t.Fatal("admission stop did not return")
		}

		cs.admitSessionWaitDependencyShadowEvent(sessionWaitShadowEvent(
			t,
			sessionWaitShadowBead("session-after-stop", "dep-after-stop"),
		))
		if got := calls.Load(); got != 1 {
			t.Fatalf("refresh calls after stop = %d, want exactly the joined in-flight callback", got)
		}
	})
}

func TestSessionWaitDependencyProducerEventAdmissionQueuesWithoutDependencyRead(t *testing.T) {
	cs := &controllerState{}
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	target := sessionWaitDependencyTarget{WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dep-a"}, DepMode: "all"}
	producer := mustStartSessionWaitDependencyProducer(t, sessionWaitDependencyProducerOptions{
		MaxDistinct: 1, TargetForWait: func(string) (sessionWaitDependencyTarget, bool) { return target, true },
		Dependencies: func() waitDependencyReader {
			return waitDependencyReaderFunc(func(string) (beads.Bead, error) {
				close(entered)
				<-release
				return beads.Bead{Status: "closed"}, nil
			})
		},
		EnqueueSession: func(sessionWaitDependencyPlan, sessionWaitDependencyCause) error { return nil },
	})
	if err := cs.installSessionWaitDependencyShadowAdmissionWithProducer(
		func() sessionWaitShadowRefreshResult { return sessionWaitShadowConverged },
		func(string) bool { return false },
		func(_ sessionWaitDependencyProducerRequest) {
			if err := producer.Admit(target, sessionWaitDependencyCauseDependency); err != nil {
				t.Error(err)
			}
		},
	); err != nil {
		t.Fatal(err)
	}
	go func() {
		cs.admitSessionWaitDependencyShadowEvent(sessionWaitShadowEvent(t, beads.Bead{ID: "dep-a", Type: "task", Status: "closed"}))
		close(returned)
	}()
	awaitClose(t, entered, "blocked dependency read")
	awaitClose(t, returned, "event admission while dependency read remained blocked")
	releaseOnce.Do(func() { close(release) })
	cs.stopSessionWaitDependencyShadowAdmission()
}

func TestSessionWaitDependencyProducerAdmissionReplaysExactRequestsAfterCertifiedRefresh(t *testing.T) {
	cs := &controllerState{}
	var refreshCalls atomic.Int64
	requests := make(chan sessionWaitDependencyProducerRequest, 2)
	if err := cs.installSessionWaitDependencyShadowAdmissionWithProducer(
		func() sessionWaitShadowRefreshResult {
			if refreshCalls.Add(1) == 1 {
				return sessionWaitShadowRetry
			}
			return sessionWaitShadowConverged
		},
		func(string) bool { return false },
		func(request sessionWaitDependencyProducerRequest) { requests <- request },
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	cs.admitSessionWaitDependencyShadowEvent(sessionWaitShadowEvent(
		t,
		sessionWaitShadowBead("session-a", "dep-a"),
	))
	select {
	case request := <-requests:
		t.Fatalf("uncertified refresh admitted producer request %+v", request)
	default:
	}

	cs.admitSessionWaitDependencyShadowEvent(sessionWaitShadowEvent(t, beads.Bead{
		ID: "dep-b", Type: "task", Status: "closed",
	}))
	awaitCond(t, func() bool { return len(requests) == 2 }, "certified exact producer recovery")
	got := []sessionWaitDependencyProducerRequest{<-requests, <-requests}
	if got[0].beadID != "dep-b" || got[0].waitHint || got[1].beadID != "wait-event" || !got[1].waitHint {
		t.Fatalf("recovery requests = %+v, want exact dep-b then wait-event", got)
	}
}

func TestSessionWaitDependencyProducerAdmissionFallsBackToOneCensusAfterExactRequestOverflow(t *testing.T) {
	cs := &controllerState{sessionWaitShadowPending: true, sessionWaitShadowPendingRequests: make(map[string]sessionWaitDependencyProducerRequest, sessionpkg.SessionWaitLookupLimit)}
	for n := 0; n < sessionpkg.SessionWaitLookupLimit; n++ {
		id := fmt.Sprintf("dep-%04d", n)
		cs.sessionWaitShadowPendingRequests[id] = sessionWaitDependencyProducerRequest{beadID: id}
	}
	requests := make(chan sessionWaitDependencyProducerRequest, sessionpkg.SessionWaitLookupLimit+1)
	if err := cs.installSessionWaitDependencyShadowAdmissionWithProducer(
		func() sessionWaitShadowRefreshResult { return sessionWaitShadowConverged },
		func(string) bool { return false },
		func(request sessionWaitDependencyProducerRequest) { requests <- request },
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	cs.admitSessionWaitDependencyShadowEvent(sessionWaitShadowEvent(t, beads.Bead{ID: "dep-overflow", Type: "task", Status: "closed"}))
	if got := len(requests); got != sessionpkg.SessionWaitLookupLimit+1 {
		t.Fatalf("requests = %d, want %d exact requests plus one census fallback", got, sessionpkg.SessionWaitLookupLimit+1)
	}
}

func TestSessionWaitDependencyProducerAdmissionUsesSubjectFirstIdentity(t *testing.T) {
	cs := &controllerState{}
	requests := make(chan sessionWaitDependencyProducerRequest, 2)
	if err := cs.installSessionWaitDependencyShadowAdmissionWithProducer(
		func() sessionWaitShadowRefreshResult { return sessionWaitShadowConverged },
		func(string) bool { return false },
		func(request sessionWaitDependencyProducerRequest) { requests <- request },
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	payload, err := json.Marshal(beads.Bead{ID: "dep-payload", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	cs.admitSessionWaitDependencyShadowEvent(events.Event{
		Type: events.BeadClosed, Subject: "dep-subject", Payload: payload,
	})
	cs.admitSessionWaitDependencyShadowEvent(events.Event{
		Type: events.BeadUpdated, Subject: "dep-malformed", Payload: []byte(`{"malformed"`),
	})

	for _, want := range []string{"dep-subject", "dep-malformed"} {
		awaitCond(t, func() bool { return len(requests) > 0 }, "subject-first producer request")
		request := <-requests
		if request.beadID != want || request.waitHint {
			t.Fatalf("request = %+v, want exact subject %q", request, want)
		}
	}
}

func TestSessionWaitDependencyShadowAdmissionValidatesCallbacks(t *testing.T) {
	var nilState *controllerState
	if err := nilState.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
		return sessionWaitShadowConverged
	}, func(string) bool { return false }); err == nil {
		t.Fatal("nil controller state install succeeded")
	}
	cs := &controllerState{}
	if err := cs.installSessionWaitDependencyShadowAdmission(nil, func(string) bool { return false }); err == nil {
		t.Fatal("nil refresh callback install succeeded")
	}
	if err := cs.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
		return sessionWaitShadowConverged
	}, nil); err == nil {
		t.Fatal("nil membership callback install succeeded")
	}
}

func TestSessionWaitDependencyShadowAdmissionRecognizesLegacyWait(t *testing.T) {
	cs := &controllerState{}
	var calls int
	if err := cs.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
		calls++
		return sessionWaitShadowConverged
	}, func(string) bool { return false }); err != nil {
		t.Fatalf("install admission: %v", err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	cs.admitSessionWaitDependencyShadowEvent(sessionWaitShadowEvent(t, beads.Bead{
		ID:     "legacy-wait",
		Type:   sessionpkg.LegacyWaitBeadType,
		Status: "open",
		Labels: []string{sessionpkg.WaitBeadLabel},
	}))
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want legacy wait event admitted", calls)
	}
}

func TestSessionWaitDependencyShadowAdmissionRunsAfterExistingEventEffects(t *testing.T) {
	previousDispatch := beadCloseAutocloseDispatch
	var autocloseDispatched bool
	beadCloseAutocloseDispatch = func(func()) {
		autocloseDispatched = true
	}
	t.Cleanup(func() { beadCloseAutocloseDispatch = previousDispatch })

	backing := beads.NewMemStore()
	wait, err := backing.Create(sessionWaitShadowBead("session-a", "dep-a"))
	if err != nil {
		t.Fatalf("Create(wait): %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if err := backing.Close(wait.ID); err != nil {
		t.Fatalf("Close(wait): %v", err)
	}
	closed, err := backing.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(closed wait): %v", err)
	}

	cs := &controllerState{
		cityBeadStore: cache,
		pokeCh:        make(chan struct{}, 1),
	}
	var refreshCalls int
	if err := cs.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
		refreshCalls++
		if !autocloseDispatched {
			t.Error("wait-shadow refresh ran before bead-close autoclose dispatch")
		}
		select {
		case <-cs.pokeCh:
		default:
			t.Error("wait-shadow refresh ran before the existing controller poke")
		}
		census, censusErr := observeSessionWaitCensus(beads.SessionStore{Store: cache})
		if censusErr != nil {
			t.Errorf("observe post-event wait census: %v", censusErr)
			return sessionWaitShadowRetry
		}
		if len(census.waits) != 0 {
			t.Errorf("post-event wait census = %#v, want closed wait removed before refresh", census.waits)
			return sessionWaitShadowRetry
		}
		return sessionWaitShadowConverged
	}, func(string) bool { return false }); err != nil {
		t.Fatalf("install admission: %v", err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	cs.applyBeadEventToStores(beadSnapshotEvent(t, events.BeadClosed, closed))
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want one post-event refresh", refreshCalls)
	}
}

func TestSessionWaitDependencyPrePokeAdmissionDoesNotReorderShadowRefresh(t *testing.T) {
	cs := &controllerState{
		cityBeadStore: beads.NewMemStore(),
		pokeCh:        make(chan struct{}, 1),
	}
	previousDispatch := beadCloseAutocloseDispatch
	var order []string
	beadCloseAutocloseDispatch = func(func()) {
		select {
		case <-cs.pokeCh:
			order = append(order, "autoclose")
		default:
			t.Error("bead-close autoclose ran before the existing controller poke")
		}
	}
	t.Cleanup(func() { beadCloseAutocloseDispatch = previousDispatch })

	if err := cs.installSessionWaitDependencyShadowAdmission(func() sessionWaitShadowRefreshResult {
		order = append(order, "refresh")
		return sessionWaitShadowConverged
	}, func(string) bool { return true }); err != nil {
		t.Fatalf("install shadow admission: %v", err)
	}
	if err := cs.installSessionWaitDependencyPrePokeAdmission(func(events.Event) {
		select {
		case <-cs.pokeCh:
			t.Error("pre-poke admission ran after the controller poke")
		default:
		}
		order = append(order, "reserve")
	}); err != nil {
		t.Fatalf("install pre-poke admission: %v", err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	cs.applyBeadEventToStores(beadSnapshotEvent(t, events.BeadClosed, beads.Bead{
		ID:     "dependency-a",
		Type:   "task",
		Status: "closed",
	}))
	if want := []string{"reserve", "autoclose", "refresh"}; !slices.Equal(order, want) {
		t.Fatalf("event effect order = %v, want %v", order, want)
	}
}

func TestSessionWaitDependencyVisibilityFenceIsInertOutsideDependencyCloseAdmission(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		cs := &controllerState{}
		releaseLegacy := cs.acquireSessionWaitDependencyLegacyVisibility()
		defer releaseLegacy()
		releaseEvent := cs.acquireSessionWaitDependencyEventVisibility(events.Event{
			Type:    events.BeadClosed,
			Subject: "dependency-a",
		})
		defer releaseEvent()
		if !cs.sessionWaitDependencyVisibilityMu.TryLock() {
			t.Fatal("disabled dependency-wait admission retained a visibility fence")
		}
		cs.sessionWaitDependencyVisibilityMu.Unlock()
	})

	t.Run("unrelated-mutation", func(t *testing.T) {
		cs := &controllerState{}
		if err := cs.installSessionWaitDependencyPrePokeAdmission(func(events.Event) {}); err != nil {
			t.Fatalf("install pre-poke admission: %v", err)
		}
		defer cs.stopSessionWaitDependencyShadowAdmission()
		releaseEvent := cs.acquireSessionWaitDependencyEventVisibility(events.Event{
			Type:    events.BeadUpdated,
			Subject: "dependency-a",
		})
		defer releaseEvent()
		if !cs.sessionWaitDependencyVisibilityMu.TryLock() {
			t.Fatal("non-close bead mutation retained a dependency-wait visibility fence")
		}
		cs.sessionWaitDependencyVisibilityMu.Unlock()
	})
}
