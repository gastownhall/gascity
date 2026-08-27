package main

import (
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"
)

type sessionLifecycleStartShadowEvaluation struct {
	Observation     sessionLifecycleStartShadowObservation
	Comparison      sessionLifecycleStartSelectionComparison
	EnqueuedAt      time.Time
	StartedAt       time.Time
	CompletedAt     time.Time
	QueueLatency    time.Duration
	PlanningLatency time.Duration
}

type sessionLifecycleStartShadowEvaluationObserver func(sessionLifecycleStartShadowEvaluation)

type sessionLifecycleShadowProjectionEntry struct {
	observation sessionLifecycleStartShadowObservation
	enqueuedAt  time.Time
}

// sessionLifecycleShadowWorker coalesces start-selection observations by
// durable session ID. It owns scheduling and pure evaluation only; no store,
// runtime provider, or session lifecycle capability is reachable from it.
type sessionLifecycleShadowWorker struct {
	queue    workqueue.TypedInterface[string]
	workers  int
	observer sessionLifecycleStartShadowEvaluationObserver
	stderr   io.Writer
	now      func() time.Time

	mu         sync.Mutex
	projection map[string]sessionLifecycleShadowProjectionEntry
	started    bool
	accepting  bool
	stopped    bool
	workerWG   sync.WaitGroup
	stopOnce   sync.Once
	stderrMu   sync.Mutex
}

func newSessionLifecycleShadowWorker(
	workers int,
	observer sessionLifecycleStartShadowEvaluationObserver,
	stderr io.Writer,
) (*sessionLifecycleShadowWorker, error) {
	if workers < 1 {
		return nil, fmt.Errorf("creating lifecycle shadow worker: workers must be positive")
	}
	if observer == nil {
		return nil, fmt.Errorf("creating lifecycle shadow worker: observer is nil")
	}
	if stderr == nil {
		return nil, fmt.Errorf("creating lifecycle shadow worker: stderr is nil")
	}
	return &sessionLifecycleShadowWorker{
		queue:      workqueue.NewTyped[string](),
		workers:    workers,
		observer:   observer,
		stderr:     stderr,
		now:        time.Now,
		projection: make(map[string]sessionLifecycleShadowProjectionEntry),
	}, nil
}

func (w *sessionLifecycleShadowWorker) Start() error {
	if w == nil {
		return fmt.Errorf("starting lifecycle shadow worker: worker is nil")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.stopped {
		return fmt.Errorf("starting lifecycle shadow worker: worker is single-start")
	}
	w.started = true
	w.accepting = true
	w.workerWG.Add(w.workers)
	for i := 0; i < w.workers; i++ {
		go w.runWorker()
	}
	return nil
}

func (w *sessionLifecycleShadowWorker) EnqueueStart(observation sessionLifecycleStartShadowObservation) error {
	if w == nil {
		return fmt.Errorf("enqueueing lifecycle shadow observation: worker is nil")
	}
	id := observation.Input.Info.ID
	if id == "" || strings.TrimSpace(id) != id {
		return fmt.Errorf("enqueueing lifecycle shadow observation: session id %q is not canonical", id)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.accepting || w.stopped {
		return fmt.Errorf("enqueueing lifecycle shadow observation for %s: worker is stopped", id)
	}
	w.projection[id] = sessionLifecycleShadowProjectionEntry{
		observation: newAdmittedSessionLifecycleStartShadowObservation(
			observation.Input,
			observation.LegacySelected,
			observation.Admission,
		),
		enqueuedAt: w.now(),
	}
	w.queue.Add(id)
	return nil
}

func (w *sessionLifecycleShadowWorker) acceptingObservations() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.accepting && !w.stopped
}

func (w *sessionLifecycleShadowWorker) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		w.mu.Lock()
		started := w.started
		w.accepting = false
		w.stopped = true
		w.mu.Unlock()

		if started {
			w.queue.ShutDownWithDrain()
			w.workerWG.Wait()
		} else {
			w.queue.ShutDown()
		}

		w.mu.Lock()
		clear(w.projection)
		w.mu.Unlock()
	})
}

func (w *sessionLifecycleShadowWorker) runWorker() {
	defer w.workerWG.Done()
	for {
		key, shutdown := w.queue.Get()
		if shutdown {
			return
		}
		w.evaluate(key)
		w.queue.Done(key)
	}
}

func (w *sessionLifecycleShadowWorker) evaluate(key string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.stderrMu.Lock()
			defer w.stderrMu.Unlock()
			fmt.Fprintf(w.stderr, "lifecycle shadow evaluation panicked for %s: %v\n%s\n", key, recovered, debug.Stack()) //nolint:errcheck // shadow diagnostics must not affect legacy reconciliation
		}
	}()
	entry, ok := w.consumeProjection(key)
	if !ok {
		return
	}
	startedAt := w.now()
	plan := planSessionLifecycleStartSelection(entry.observation.Input)
	comparison := compareSessionLifecycleStartSelection(
		plan,
		entry.observation.LegacySelected,
	)
	completedAt := w.now()
	w.observer(sessionLifecycleStartShadowEvaluation{
		Observation: newAdmittedSessionLifecycleStartShadowObservation(
			entry.observation.Input,
			entry.observation.LegacySelected,
			entry.observation.Admission,
		),
		Comparison:      comparison,
		EnqueuedAt:      entry.enqueuedAt,
		StartedAt:       startedAt,
		CompletedAt:     completedAt,
		QueueLatency:    startedAt.Sub(entry.enqueuedAt),
		PlanningLatency: completedAt.Sub(startedAt),
	})
}

func (w *sessionLifecycleShadowWorker) consumeProjection(key string) (sessionLifecycleShadowProjectionEntry, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	entry, ok := w.projection[key]
	if !ok {
		return sessionLifecycleShadowProjectionEntry{}, false
	}
	delete(w.projection, key)
	entry.observation = newAdmittedSessionLifecycleStartShadowObservation(
		entry.observation.Input,
		entry.observation.LegacySelected,
		entry.observation.Admission,
	)
	return entry, true
}
