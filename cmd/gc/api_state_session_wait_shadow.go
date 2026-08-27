package main

import (
	"fmt"
	"slices"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

type sessionWaitShadowRefreshResult uint8

type sessionWaitDependencyProducerRequest struct {
	beadID     string
	waitHint   bool
	fullCensus bool
}

const (
	sessionWaitShadowRetry sessionWaitShadowRefreshResult = iota
	sessionWaitShadowConverged
	sessionWaitShadowAwaitRelevant
)

func (cs *controllerState) installSessionWaitDependencyShadowAdmission(admit func() sessionWaitShadowRefreshResult, mayContain func(string) bool) error {
	return cs.installSessionWaitDependencyShadowAdmissionWithProducer(admit, mayContain, nil)
}

func (cs *controllerState) installSessionWaitDependencyShadowAdmissionWithProducer(admit func() sessionWaitShadowRefreshResult, mayContain func(string) bool, producer func(sessionWaitDependencyProducerRequest)) error {
	if cs == nil {
		return fmt.Errorf("installing session-wait shadow admission: controller state is nil")
	}
	if admit == nil || mayContain == nil {
		return fmt.Errorf("installing session-wait shadow admission: callback is nil")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.sessionWaitShadowAdmission != nil || cs.sessionWaitShadowProducerAdmission != nil || cs.sessionWaitShadowAdmissionStopping {
		return fmt.Errorf("installing session-wait shadow admission: admission unavailable")
	}
	cs.sessionWaitShadowAdmission = admit
	cs.sessionWaitShadowMayContain = mayContain
	cs.sessionWaitShadowProducerAdmission = producer
	return nil
}

func (cs *controllerState) installSessionWaitDependencyPrePokeAdmission(admit func(events.Event)) error {
	if cs == nil {
		return fmt.Errorf("installing session-wait pre-poke admission: controller state is nil")
	}
	if admit == nil {
		return fmt.Errorf("installing session-wait pre-poke admission: callback is nil")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.sessionWaitPrePokeAdmission != nil || cs.sessionWaitShadowAdmissionStopping {
		return fmt.Errorf("installing session-wait pre-poke admission: admission unavailable")
	}
	cs.sessionWaitPrePokeAdmission = admit
	return nil
}

func (cs *controllerState) admitSessionWaitDependencyPrePokeEvent(evt events.Event) {
	if cs == nil {
		return
	}
	cs.mu.Lock()
	admit := cs.sessionWaitPrePokeAdmission
	stopping := cs.sessionWaitShadowAdmissionStopping
	if admit != nil && !stopping {
		cs.sessionWaitShadowAdmissionWG.Add(1)
	}
	cs.mu.Unlock()
	if admit == nil || stopping {
		return
	}
	defer cs.sessionWaitShadowAdmissionWG.Done()
	admit(evt)
}

func (cs *controllerState) sessionWaitDependencyPrePokeArmed() bool {
	if cs == nil {
		return false
	}
	cs.mu.RLock()
	armed := cs.sessionWaitPrePokeAdmission != nil && !cs.sessionWaitShadowAdmissionStopping
	cs.mu.RUnlock()
	return armed
}

// acquireSessionWaitDependencyEventVisibility starts the writer side only for
// dependency-close candidates while keyed pre-poke admission is armed. A
// no-op release keeps the event path flat in Off and during shutdown.
func (cs *controllerState) acquireSessionWaitDependencyEventVisibility(evt events.Event) func() {
	if cs == nil || evt.Type != events.BeadClosed || evt.Subject == "" || !cs.sessionWaitDependencyPrePokeArmed() {
		return func() {}
	}
	cs.sessionWaitDependencyVisibilityMu.Lock()
	return cs.sessionWaitDependencyVisibilityMu.Unlock
}

// acquireSessionWaitDependencyLegacyVisibility prevents legacy wait
// preparation from observing a dependency-close cache mutation before the
// same event has completed exact reservation. It is inert outside keyed
// pre-poke admission.
func (cs *controllerState) acquireSessionWaitDependencyLegacyVisibility() func() {
	if cs == nil || !cs.sessionWaitDependencyPrePokeArmed() {
		return func() {}
	}
	cs.sessionWaitDependencyVisibilityMu.RLock()
	return cs.sessionWaitDependencyVisibilityMu.RUnlock
}

func (cs *controllerState) stopSessionWaitDependencyShadowAdmission() {
	if cs == nil {
		return
	}
	cs.mu.Lock()
	cs.sessionWaitShadowAdmissionStopping = true
	cs.sessionWaitShadowAdmission = nil
	cs.sessionWaitPrePokeAdmission = nil
	cs.sessionWaitShadowMayContain = nil
	cs.sessionWaitShadowProducerAdmission = nil
	cs.mu.Unlock()
	cs.sessionWaitShadowAdmissionWG.Wait()
	cs.mu.Lock()
	cs.sessionWaitShadowPending = false
	cs.sessionWaitShadowPendingRequests = nil
	cs.sessionWaitShadowPendingOverflow = false
	cs.sessionWaitShadowAdmissionStopping = false
	cs.mu.Unlock()
}

func (cs *controllerState) admitSessionWaitDependencyShadowEvent(evt events.Event) {
	if cs == nil || !isBeadMutationEvent(evt.Type) {
		return
	}
	bead, decoded := beads.DecodeBeadEventPayload(evt.Payload)
	id := beadEventID(evt)
	bead.ID = id
	waitHint := decoded && session.IsWaitBead(bead)
	requests := cs.requestSessionWaitDependencyShadowRefreshForBead(bead, waitHint)
	cs.mu.Lock()
	producer := cs.sessionWaitShadowProducerAdmission
	stopping := cs.sessionWaitShadowAdmissionStopping
	if producer != nil && len(requests) != 0 && !stopping {
		cs.sessionWaitShadowAdmissionWG.Add(1)
	}
	cs.mu.Unlock()
	if producer != nil && len(requests) != 0 && !stopping {
		defer cs.sessionWaitShadowAdmissionWG.Done()
		for _, request := range requests {
			producer(request)
		}
	}
}

func (cs *controllerState) requestSessionWaitDependencyShadowRefreshForBead(bead beads.Bead, mayHaveChanged bool) []sessionWaitDependencyProducerRequest {
	if cs == nil {
		return nil
	}
	cs.mu.Lock()
	admit := cs.sessionWaitShadowAdmission
	mayContain := cs.sessionWaitShadowMayContain
	if admit == nil || mayContain == nil || cs.sessionWaitShadowAdmissionStopping {
		cs.mu.Unlock()
		return nil
	}
	cs.sessionWaitShadowAdmissionWG.Add(1)
	cs.mu.Unlock()
	defer cs.sessionWaitShadowAdmissionWG.Done()

	if !mayHaveChanged && bead.ID != "" {
		mayHaveChanged = mayContain(bead.ID)
	}

	cs.mu.Lock()
	if mayHaveChanged {
		cs.sessionWaitShadowPending = true
		cs.sessionWaitShadowGeneration++
	}
	if bead.ID != "" && (mayHaveChanged || cs.sessionWaitShadowPending) {
		if cs.sessionWaitShadowPendingRequests == nil {
			cs.sessionWaitShadowPendingRequests = make(map[string]sessionWaitDependencyProducerRequest)
		}
		request, known := cs.sessionWaitShadowPendingRequests[bead.ID]
		if known || len(cs.sessionWaitShadowPendingRequests) < session.SessionWaitLookupLimit {
			request.beadID = bead.ID
			request.waitHint = request.waitHint || mayHaveChanged
			cs.sessionWaitShadowPendingRequests[bead.ID] = request
		} else {
			cs.sessionWaitShadowPendingOverflow = true
		}
	}
	if !cs.sessionWaitShadowPending {
		cs.mu.Unlock()
		if bead.ID == "" {
			return nil
		}
		return []sessionWaitDependencyProducerRequest{{beadID: bead.ID, waitHint: mayHaveChanged}}
	}
	generation := cs.sessionWaitShadowGeneration
	cs.mu.Unlock()

	result := admit()
	cs.mu.Lock()
	var requests []sessionWaitDependencyProducerRequest
	if cs.sessionWaitShadowGeneration == generation {
		switch result {
		case sessionWaitShadowConverged:
			requests = sessionWaitDependencyRequestsSorted(cs.sessionWaitShadowPendingRequests, cs.sessionWaitShadowPendingOverflow)
			cs.sessionWaitShadowPendingRequests = nil
			cs.sessionWaitShadowPendingOverflow = false
			cs.sessionWaitShadowPending = false
		case sessionWaitShadowAwaitRelevant:
			cs.sessionWaitShadowPending = false
		}
	}
	cs.mu.Unlock()
	return requests
}

func sessionWaitDependencyRequestsSorted(pending map[string]sessionWaitDependencyProducerRequest, fullCensus bool) []sessionWaitDependencyProducerRequest {
	if len(pending) == 0 {
		if fullCensus {
			return []sessionWaitDependencyProducerRequest{{fullCensus: true}}
		}
		return nil
	}
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	requests := make([]sessionWaitDependencyProducerRequest, 0, len(ids))
	for _, id := range ids {
		requests = append(requests, pending[id])
	}
	if fullCensus {
		requests = append(requests, sessionWaitDependencyProducerRequest{fullCensus: true})
	}
	return requests
}
