package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"k8s.io/client-go/util/workqueue"
)

// sessionWaitDependencyCause is deliberately ordered by explicit precedence:
// dependency_commit > wait_commit > registration_recheck.
type sessionWaitDependencyCause string

const (
	sessionWaitDependencyCauseRegistration sessionWaitDependencyCause = "registration_recheck"
	sessionWaitDependencyCauseWaitCommit   sessionWaitDependencyCause = "wait_commit"
	sessionWaitDependencyCauseDependency   sessionWaitDependencyCause = "dependency_commit"
)

type sessionWaitDependencyPlanDisposition string

const (
	sessionWaitDependencyPlanReady   sessionWaitDependencyPlanDisposition = "ready"
	sessionWaitDependencyPlanPending sessionWaitDependencyPlanDisposition = "pending"
	sessionWaitDependencyPlanParked  sessionWaitDependencyPlanDisposition = "park"
)

type sessionWaitDependencyPlanReason string

const (
	sessionWaitDependencyReasonReady   sessionWaitDependencyPlanReason = "dependencies_ready"
	sessionWaitDependencyReasonPending sessionWaitDependencyPlanReason = "dependencies_pending"
	sessionWaitDependencyReasonReadErr sessionWaitDependencyPlanReason = "read_error"
)

type sessionWaitDependencyPlan struct {
	Target      sessionWaitDependencyTarget
	Disposition sessionWaitDependencyPlanDisposition
	Reason      sessionWaitDependencyPlanReason
	Err         error
}

func planSessionWaitDependencyTarget(dependencies waitDependencyReader, target sessionWaitDependencyTarget) sessionWaitDependencyPlan {
	plan := sessionWaitDependencyPlan{Target: cloneSessionWaitDependencyTarget(target)}
	if dependencies == nil {
		plan.Disposition, plan.Reason, plan.Err = sessionWaitDependencyPlanParked, sessionWaitDependencyReasonReadErr, fmt.Errorf("planning wait %q: dependency reader is nil", target.WaitID)
		return plan
	}
	ready, err := depsWaitReadyDetailedFrom(dependencies, sessionpkg.WaitInfo{ID: target.WaitID, SessionID: target.SessionID, Kind: "deps", Status: "open", State: waitStatePending, DepIDs: append([]string(nil), target.DepIDs...), DepMode: target.DepMode})
	if err != nil {
		plan.Disposition, plan.Reason, plan.Err = sessionWaitDependencyPlanParked, sessionWaitDependencyReasonReadErr, err
		return plan
	}
	if !ready {
		plan.Disposition, plan.Reason = sessionWaitDependencyPlanPending, sessionWaitDependencyReasonPending
		return plan
	}
	plan.Disposition, plan.Reason = sessionWaitDependencyPlanReady, sessionWaitDependencyReasonReady
	return plan
}

type sessionWaitDependencyProducerOptions struct {
	MaxDistinct    int
	TargetForWait  func(string) (sessionWaitDependencyTarget, bool)
	Dependencies   func() waitDependencyReader
	EnqueueSession func(sessionWaitDependencyPlan, sessionWaitDependencyCause) error
	AfterSuccess   func(sessionWaitDependencyPlan, sessionWaitDependencyCause)
	ReportError    func(error)
}
type sessionWaitDependencyAdmission struct {
	cause      sessionWaitDependencyCause
	generation uint64
	target     sessionWaitDependencyTarget
}

// sessionWaitDependencyProducer is a bounded, one-worker, exact-wait queue.
// It owns no persistence or provider capability; the injected sink remains
// disabled in production until the lifecycle-shadow adapter is introduced.
type sessionWaitDependencyProducer struct {
	queue                       workqueue.TypedInterface[string]
	maxDistinct                 int
	targetForWait               func(string) (sessionWaitDependencyTarget, bool)
	dependencies                func() waitDependencyReader
	enqueueSession              func(sessionWaitDependencyPlan, sessionWaitDependencyCause) error
	afterSuccess                func(sessionWaitDependencyPlan, sessionWaitDependencyCause)
	reportError                 func(error)
	mu                          sync.Mutex
	admissions                  map[string]sessionWaitDependencyAdmission
	nextGeneration              uint64
	started, accepting, stopped bool
	workerWG                    sync.WaitGroup
	stopOnce                    sync.Once
}

func newSessionWaitDependencyProducer(opts sessionWaitDependencyProducerOptions) (*sessionWaitDependencyProducer, error) {
	switch {
	case opts.MaxDistinct <= 0:
		return nil, fmt.Errorf("creating wait dependency producer: max distinct must be positive")
	case opts.TargetForWait == nil:
		return nil, fmt.Errorf("creating wait dependency producer: target lookup is nil")
	case opts.Dependencies == nil:
		return nil, fmt.Errorf("creating wait dependency producer: dependency reader is nil")
	case opts.EnqueueSession == nil:
		return nil, fmt.Errorf("creating wait dependency producer: downstream enqueue is nil")
	}
	return &sessionWaitDependencyProducer{queue: workqueue.NewTyped[string](), maxDistinct: opts.MaxDistinct, targetForWait: opts.TargetForWait, dependencies: opts.Dependencies, enqueueSession: opts.EnqueueSession, afterSuccess: opts.AfterSuccess, reportError: opts.ReportError, admissions: make(map[string]sessionWaitDependencyAdmission, opts.MaxDistinct)}, nil
}

func (p *sessionWaitDependencyProducer) Start() error {
	if p == nil {
		return fmt.Errorf("starting wait dependency producer: producer is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.stopped {
		return fmt.Errorf("starting wait dependency producer: producer is single-start")
	}
	p.started, p.accepting = true, true
	p.workerWG.Add(1)
	go p.run()
	return nil
}

func (p *sessionWaitDependencyProducer) Admit(target sessionWaitDependencyTarget, cause sessionWaitDependencyCause) error {
	if p == nil {
		return fmt.Errorf("admitting wait dependency target: producer is nil")
	}
	if err := validateSessionWaitDependencyTarget(target); err != nil {
		return err
	}
	if causePrecedence(cause) == 0 {
		return fmt.Errorf("admitting wait dependency target %q: unknown cause %q", target.WaitID, cause)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.accepting || p.stopped {
		return fmt.Errorf("admitting wait dependency target %q: producer is stopped", target.WaitID)
	}
	entry, exists := p.admissions[target.WaitID]
	if !exists && len(p.admissions) >= p.maxDistinct {
		return fmt.Errorf("admitting wait dependency target %q: producer is full", target.WaitID)
	}
	if !exists || target.generation >= entry.target.generation {
		p.nextGeneration++
		entry.generation = p.nextGeneration
		if !exists || target.generation > entry.target.generation {
			entry.cause = cause
		} else {
			entry.cause = mergeSessionWaitDependencyCause(entry.cause, cause)
		}
		entry.target = cloneSessionWaitDependencyTarget(target)
	}
	p.admissions[target.WaitID] = entry
	p.queue.Add(target.WaitID)
	return nil
}

func validateSessionWaitDependencyTarget(target sessionWaitDependencyTarget) error {
	for _, field := range []struct{ name, value string }{{"wait ID", target.WaitID}, {"session ID", target.SessionID}} {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("admitting wait dependency target: %s is not canonical", field.name)
		}
	}
	if len(target.DepIDs) == 0 || (target.DepMode != "all" && target.DepMode != "any") {
		return fmt.Errorf("admitting wait dependency target %q: invalid dependency target", target.WaitID)
	}
	return nil
}

func mergeSessionWaitDependencyCause(a, b sessionWaitDependencyCause) sessionWaitDependencyCause {
	if causePrecedence(b) > causePrecedence(a) {
		return b
	}
	return a
}

func causePrecedence(c sessionWaitDependencyCause) int {
	switch c {
	case sessionWaitDependencyCauseDependency:
		return 3
	case sessionWaitDependencyCauseWaitCommit:
		return 2
	case sessionWaitDependencyCauseRegistration:
		return 1
	default:
		return 0
	}
}

func (p *sessionWaitDependencyProducer) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.accepting, p.stopped = false, true
		started := p.started
		p.mu.Unlock()
		if started {
			p.queue.ShutDownWithDrain()
			p.workerWG.Wait()
		} else {
			p.queue.ShutDown()
		}
		p.mu.Lock()
		clear(p.admissions)
		p.mu.Unlock()
	})
}

func (p *sessionWaitDependencyProducer) run() {
	defer p.workerWG.Done()
	for {
		id, shutdown := p.queue.Get()
		if shutdown {
			return
		}
		func() {
			defer p.queue.Done(id)
			p.process(id)
		}()
	}
}

func (p *sessionWaitDependencyProducer) process(id string) {
	p.mu.Lock()
	entry, ok := p.admissions[id]
	p.mu.Unlock()
	if !ok {
		return
	}
	target, ok := p.targetForWait(id)
	if !ok {
		p.forget(id, entry.generation)
		return
	}
	if target.generation != entry.target.generation ||
		!sameSessionWaitDependencyTarget(target, entry.target) {
		p.forgetOrRebase(entry)
		return
	}
	// This is only an untrusted readiness filter that keeps ordinary pending
	// events off the serialized runtime loop. Certification and every effect
	// remain exclusively in CityRuntime.run.
	plan := planSessionWaitDependencyTarget(p.dependencies(), target)
	if plan.Disposition != sessionWaitDependencyPlanReady {
		if plan.Err != nil {
			p.report(fmt.Errorf("planning wait dependency target %q: %w", id, plan.Err))
		}
		p.forget(id, entry.generation)
		return
	}
	p.mu.Lock()
	current, currentOK := p.admissions[id]
	if !currentOK || current.generation != entry.generation {
		p.mu.Unlock()
		return
	}
	err := p.enqueueSession(plan, current.cause)
	p.mu.Unlock()
	if err != nil {
		if errors.Is(err, errSessionWaitDependencyStaleCertification) {
			p.forgetOrRebase(entry)
			return
		}
		p.report(fmt.Errorf("enqueueing wait dependency target %q: %w", id, err))
		p.forget(id, entry.generation)
		return
	}
	if p.afterSuccess != nil {
		p.afterSuccess(plan, entry.cause)
	}
	p.forget(id, entry.generation)
}

func (p *sessionWaitDependencyProducer) forgetOrRebase(entry sessionWaitDependencyAdmission) {
	latest, ok := p.targetForWait(entry.target.WaitID)
	if ok &&
		latest.generation > entry.target.generation &&
		sameSessionWaitDependencyTarget(latest, entry.target) {
		if p.Admit(latest, entry.cause) == nil {
			return
		}
	}
	p.forget(entry.target.WaitID, entry.generation)
}

func sameSessionWaitDependencyTarget(a, b sessionWaitDependencyTarget) bool {
	return a.WaitID == b.WaitID &&
		a.SessionID == b.SessionID &&
		a.DepMode == b.DepMode &&
		slices.Equal(a.DepIDs, b.DepIDs)
}

func (p *sessionWaitDependencyProducer) forget(id string, generation uint64) {
	p.mu.Lock()
	if entry, ok := p.admissions[id]; ok && entry.generation == generation {
		delete(p.admissions, id)
	}
	p.mu.Unlock()
}

func (p *sessionWaitDependencyProducer) report(err error) {
	if p.reportError != nil {
		p.reportError(err)
	}
}

func cloneSessionWaitDependencyTarget(target sessionWaitDependencyTarget) sessionWaitDependencyTarget {
	target.DepIDs = append([]string(nil), target.DepIDs...)
	return target
}
