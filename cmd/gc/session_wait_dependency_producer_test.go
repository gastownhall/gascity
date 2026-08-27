package main

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestSessionWaitDependencyPlan_ClassifiesReadyPendingAndReadError(t *testing.T) {
	readyStore := beads.NewMemStore()
	ready, err := readyStore.Create(beads.Bead{Status: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := readyStore.Close(ready.ID); err != nil {
		t.Fatal(err)
	}
	target := sessionWaitDependencyTarget{WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{ready.ID}, DepMode: "all"}
	if got := planSessionWaitDependencyTarget(readyStore, target); got.Disposition != sessionWaitDependencyPlanReady {
		t.Fatalf("ready plan = %+v, want ready", got)
	}
	pendingStore := beads.NewMemStore()
	if _, err := pendingStore.Create(beads.Bead{ID: ready.ID, Status: "open"}); err != nil {
		t.Fatal(err)
	}
	if got := planSessionWaitDependencyTarget(pendingStore, target); got.Disposition != sessionWaitDependencyPlanPending || got.Reason != sessionWaitDependencyReasonPending {
		t.Fatalf("pending plan = %+v, want pending dependencies_pending", got)
	}
	failing := waitDependencyReaderFunc(func(string) (beads.Bead, error) { return beads.Bead{}, errors.New("read failed") })
	if got := planSessionWaitDependencyTarget(failing, target); got.Disposition != sessionWaitDependencyPlanParked || got.Reason != sessionWaitDependencyReasonReadErr {
		t.Fatalf("read-error plan = %+v, want parked read_error", got)
	}
}

func TestSessionWaitDependencyProducer_ExactTargetsOnlyAndPostSuccessOrdering(t *testing.T) {
	deps := beads.NewMemStore()
	ready, err := deps.Create(beads.Bead{Status: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Close(ready.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := deps.Get(ready.ID); err != nil || got.Status != "closed" {
		t.Fatalf("ready dependency = %#v, %v", got, err)
	}
	targets := map[string]sessionWaitDependencyTarget{
		"wait-ready":   {WaitID: "wait-ready", SessionID: "session-ready", DepIDs: []string{ready.ID}, DepMode: "all"},
		"wait-pending": {WaitID: "wait-pending", SessionID: "session-pending", DepIDs: []string{"dep-pending"}, DepMode: "all"},
	}
	var mu sync.Mutex
	var enqueued, published []string
	succeeded := make(chan struct{})
	producer := mustStartSessionWaitDependencyProducer(t, sessionWaitDependencyProducerOptions{
		MaxDistinct: 8,
		TargetForWait: func(waitID string) (sessionWaitDependencyTarget, bool) {
			target, ok := targets[waitID]
			return target, ok
		},
		Dependencies: func() waitDependencyReader { return deps },
		EnqueueSession: func(plan sessionWaitDependencyPlan, _ sessionWaitDependencyCause) error {
			mu.Lock()
			enqueued = append(enqueued, plan.Target.SessionID)
			mu.Unlock()
			return nil
		},
		AfterSuccess: func(plan sessionWaitDependencyPlan, _ sessionWaitDependencyCause) {
			mu.Lock()
			published = append(published, plan.Target.WaitID)
			mu.Unlock()
			close(succeeded)
		},
	})
	if err := producer.Admit(targets["wait-ready"], sessionWaitDependencyCauseDependency); err != nil {
		t.Fatalf("admit ready: %v", err)
	}
	if err := producer.Admit(targets["wait-pending"], sessionWaitDependencyCauseDependency); err != nil {
		t.Fatalf("admit pending: %v", err)
	}
	awaitClose(t, succeeded, "ready downstream enqueue")
	producer.Stop()
	mu.Lock()
	defer mu.Unlock()
	if got, want := enqueued, []string{"session-ready"}; !slices.Equal(got, want) {
		t.Fatalf("enqueued = %v, want %v", got, want)
	}
	if got, want := published, []string{"wait-ready"}; !slices.Equal(got, want) {
		t.Fatalf("published = %v, want %v", got, want)
	}
}

func TestSessionWaitDependencyProducer_FailedEnqueueDoesNotPublish(t *testing.T) {
	deps := beads.NewMemStore()
	ready, err := deps.Create(beads.Bead{})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Close(ready.ID); err != nil {
		t.Fatal(err)
	}
	published, attempted := false, 0
	producer := mustStartSessionWaitDependencyProducer(t, sessionWaitDependencyProducerOptions{
		MaxDistinct: 8,
		TargetForWait: func(string) (sessionWaitDependencyTarget, bool) {
			return sessionWaitDependencyTarget{WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{ready.ID}, DepMode: "all"}, true
		},
		Dependencies: func() waitDependencyReader { return deps },
		EnqueueSession: func(sessionWaitDependencyPlan, sessionWaitDependencyCause) error {
			attempted++
			return errors.New("downstream stopped")
		},
		AfterSuccess: func(sessionWaitDependencyPlan, sessionWaitDependencyCause) { published = true },
	})
	if err := producer.Admit(sessionWaitDependencyTarget{WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{ready.ID}, DepMode: "all"}, sessionWaitDependencyCauseDependency); err != nil {
		t.Fatal(err)
	}
	producer.Stop()
	if published {
		t.Fatal("published success after failed enqueue")
	}
	if attempted != 1 {
		t.Fatalf("enqueue attempts=%d, want 1", attempted)
	}
}

func TestSessionWaitDependencyProducer_CoalescesCausesDeterministicallyAndJoinsStop(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	causes := make(chan sessionWaitDependencyCause, 1)
	var enteredOnce sync.Once
	producer := mustStartSessionWaitDependencyProducer(t, sessionWaitDependencyProducerOptions{
		MaxDistinct: 1,
		TargetForWait: func(string) (sessionWaitDependencyTarget, bool) {
			return sessionWaitDependencyTarget{WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dep-a"}, DepMode: "all"}, true
		},
		Dependencies: func() waitDependencyReader {
			return waitDependencyReaderFunc(func(string) (beads.Bead, error) {
				enteredOnce.Do(func() { close(entered) })
				<-release
				return beads.Bead{Status: "closed"}, nil
			})
		},
		EnqueueSession: func(_ sessionWaitDependencyPlan, cause sessionWaitDependencyCause) error {
			causes <- cause
			return nil
		},
	})
	if err := producer.Admit(sessionWaitDependencyTarget{WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dep-a"}, DepMode: "all"}, sessionWaitDependencyCauseRegistration); err != nil {
		t.Fatal(err)
	}
	awaitClose(t, entered, "producer worker")
	if err := producer.Admit(sessionWaitDependencyTarget{WaitID: "wait-b", SessionID: "session-b", DepIDs: []string{"dep-b"}, DepMode: "all"}, sessionWaitDependencyCauseDependency); err == nil {
		t.Fatal("distinct admission exceeded capacity")
	}
	if err := producer.Admit(sessionWaitDependencyTarget{WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dep-a"}, DepMode: "all"}, sessionWaitDependencyCauseDependency); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { producer.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned before worker drained")
	default:
	}
	close(release)
	awaitClose(t, stopped, "producer stop")
	if cause := <-causes; cause != sessionWaitDependencyCauseDependency {
		t.Fatalf("coalesced cause = %q, want dependency", cause)
	}
	if err := producer.Admit(sessionWaitDependencyTarget{WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dep-a"}, DepMode: "all"}, sessionWaitDependencyCauseDependency); err == nil {
		t.Fatal("admission after stop succeeded")
	}
}

func TestSessionWaitDependencyProducer_HoldsAdmissionMutexDuringEnqueue(t *testing.T) {
	locked := make(chan bool, 1)
	target := sessionWaitDependencyTarget{WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dep-a"}, DepMode: "all"}
	var producer *sessionWaitDependencyProducer
	producer = mustStartSessionWaitDependencyProducer(t, sessionWaitDependencyProducerOptions{
		MaxDistinct:   1,
		TargetForWait: func(string) (sessionWaitDependencyTarget, bool) { return target, true },
		Dependencies: func() waitDependencyReader {
			return waitDependencyReaderFunc(func(string) (beads.Bead, error) { return beads.Bead{Status: "closed"}, nil })
		},
		EnqueueSession: func(sessionWaitDependencyPlan, sessionWaitDependencyCause) error {
			if producer.mu.TryLock() {
				producer.mu.Unlock()
				locked <- false
				return nil
			}
			locked <- true
			return nil
		},
	})
	if err := producer.Admit(target, sessionWaitDependencyCauseDependency); err != nil {
		t.Fatal(err)
	}
	if held := <-locked; !held {
		t.Fatal("producer admission mutex was not held during downstream enqueue")
	}
}

func TestSessionWaitDependencyProducer_RebasesUnchangedTargetAcrossGeneration(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	enqueued := make(chan string, 2)
	current := sessionWaitDependencyTarget{
		WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dep-a"}, DepMode: "all", generation: 1,
	}
	var currentMu sync.RWMutex
	var reads atomic.Int64
	loadCurrent := func() sessionWaitDependencyTarget {
		currentMu.RLock()
		defer currentMu.RUnlock()
		return cloneSessionWaitDependencyTarget(current)
	}
	producer := mustStartSessionWaitDependencyProducer(t, sessionWaitDependencyProducerOptions{
		MaxDistinct: 1,
		TargetForWait: func(string) (sessionWaitDependencyTarget, bool) {
			return loadCurrent(), true
		},
		Dependencies: func() waitDependencyReader {
			return waitDependencyReaderFunc(func(string) (beads.Bead, error) {
				if reads.Add(1) == 1 {
					close(entered)
					<-release
				}
				return beads.Bead{Status: "closed"}, nil
			})
		},
		EnqueueSession: func(plan sessionWaitDependencyPlan, _ sessionWaitDependencyCause) error {
			if plan.Target.generation != loadCurrent().generation {
				return errSessionWaitDependencyStaleCertification
			}
			enqueued <- plan.Target.SessionID
			return nil
		},
	})

	if err := producer.Admit(loadCurrent(), sessionWaitDependencyCauseDependency); err != nil {
		t.Fatalf("admit dependency transition: %v", err)
	}
	awaitClose(t, entered, "blocked dependency read")
	currentMu.Lock()
	current.generation = 2
	currentMu.Unlock()
	close(release)

	if got := receiveString(t, enqueued, "rebased dependency admission"); got != "session-a" {
		t.Fatalf("enqueued session = %q, want session-a", got)
	}
	producer.Stop()
	select {
	case extra := <-enqueued:
		t.Fatalf("unchanged target enqueued more than once: %q", extra)
	default:
	}
}

func mustStartSessionWaitDependencyProducer(t *testing.T, opts sessionWaitDependencyProducerOptions) *sessionWaitDependencyProducer {
	t.Helper()
	producer, err := newSessionWaitDependencyProducer(opts)
	if err != nil {
		t.Fatalf("newSessionWaitDependencyProducer: %v", err)
	}
	if err := producer.Start(); err != nil {
		t.Fatalf("producer.Start: %v", err)
	}
	t.Cleanup(producer.Stop)
	return producer
}
