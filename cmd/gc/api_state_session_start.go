package main

import (
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type controllerSessionStartSnapshot struct {
	Generation uint64
	CityPath   string
	CityName   string
	Config     *config.City
	Provider   runtime.Provider
	Store      beads.Store
	NudgeStore beads.Store
	Recorder   events.Recorder
}

func (cs *controllerState) sessionStartSnapshot() (controllerSessionStartSnapshot, error) {
	if cs == nil {
		return controllerSessionStartSnapshot{}, fmt.Errorf("capturing session-start state: controller state is nil")
	}
	if cs.configMutationPending.Load() {
		return controllerSessionStartSnapshot{}, fmt.Errorf("capturing session-start state: runtime config application is pending")
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	snapshot := controllerSessionStartSnapshot{
		Generation: cs.sessionStartGeneration,
		CityPath:   cs.cityPath,
		CityName:   cs.cityName,
		Config:     cs.cfg,
		Provider:   cs.sp,
		Store:      resolveSessionStore(cs.storageRoutes, cs.cityBeadStore, cs.cfg, cs.cityPath, cs.eventProv),
		NudgeStore: resolveNudgesStore(cs.storageRoutes, cs.cityBeadStore, cs.cfg, cs.cityPath, cs.eventProv),
		Recorder:   cs.eventProv,
	}
	switch {
	case snapshot.Generation == 0:
		return controllerSessionStartSnapshot{}, fmt.Errorf("capturing session-start state: runtime generation is unavailable")
	case cs.sessionStartStoreGeneration != snapshot.Generation:
		return controllerSessionStartSnapshot{}, fmt.Errorf(
			"capturing session-start state: session store generation %d does not match runtime generation %d",
			cs.sessionStartStoreGeneration, snapshot.Generation,
		)
	case snapshot.Config == nil:
		return controllerSessionStartSnapshot{}, fmt.Errorf("capturing session-start state: config is unavailable")
	case snapshot.Provider == nil:
		return controllerSessionStartSnapshot{}, fmt.Errorf("capturing session-start state: runtime provider is unavailable")
	case snapshot.Store == nil:
		return controllerSessionStartSnapshot{}, fmt.Errorf("capturing session-start state: session store is unavailable")
	}
	if snapshot.Recorder == nil {
		snapshot.Recorder = events.Discard
	}
	return snapshot, nil
}

func (cs *controllerState) acquireSessionStartSnapshot() (controllerSessionStartSnapshot, func(), error) {
	if cs == nil {
		return controllerSessionStartSnapshot{}, nil, fmt.Errorf("acquiring session-start state: controller state is nil")
	}
	cs.sessionStartLeaseMu.Lock()
	if cs.sessionStartSwapPending {
		cs.sessionStartLeaseMu.Unlock()
		return controllerSessionStartSnapshot{}, nil, fmt.Errorf("acquiring session-start state: runtime generation is changing")
	}
	cs.sessionStartLeases++
	cs.sessionStartLeaseMu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(cs.releaseSessionStartSnapshot)
	}
	snapshot, err := cs.sessionStartSnapshot()
	if err != nil {
		release()
		return controllerSessionStartSnapshot{}, nil, err
	}
	return snapshot, release, nil
}

func (cs *controllerState) releaseSessionStartSnapshot() {
	cs.sessionStartLeaseMu.Lock()
	defer cs.sessionStartLeaseMu.Unlock()
	if cs.sessionStartLeases == 0 {
		return
	}
	cs.sessionStartLeases--
	if cs.sessionStartLeases == 0 && cs.sessionStartSwapPending && cs.sessionStartLeasesDrained != nil {
		close(cs.sessionStartLeasesDrained)
		cs.sessionStartLeasesDrained = nil
	}
}

func (cs *controllerState) beginSessionStartGenerationSwap() func() {
	if cs == nil {
		return func() {}
	}
	cs.sessionStartLeaseMu.Lock()
	for cs.sessionStartSwapPending {
		done := cs.sessionStartSwapDone
		cs.sessionStartLeaseMu.Unlock()
		<-done
		cs.sessionStartLeaseMu.Lock()
	}
	cs.sessionStartSwapPending = true
	cs.sessionStartSwapDone = make(chan struct{})
	if cs.sessionStartLeases == 0 {
		cs.sessionStartLeaseMu.Unlock()
		return cs.endSessionStartGenerationSwap
	}
	cs.sessionStartLeasesDrained = make(chan struct{})
	drained := cs.sessionStartLeasesDrained
	cs.sessionStartLeaseMu.Unlock()
	<-drained
	return cs.endSessionStartGenerationSwap
}

func (cs *controllerState) endSessionStartGenerationSwap() {
	cs.sessionStartLeaseMu.Lock()
	defer cs.sessionStartLeaseMu.Unlock()
	if !cs.sessionStartSwapPending {
		return
	}
	cs.sessionStartSwapPending = false
	close(cs.sessionStartSwapDone)
	cs.sessionStartSwapDone = nil
}

func (cs *controllerState) advanceSessionStartGenerationLocked() {
	cs.sessionStartGeneration++
	if cs.sessionStartGeneration == 0 {
		// Zero is permanently invalid, so an impossible uint64 wrap fails closed
		// rather than making a future snapshot look like the initial generation.
		cs.sessionStartStoreGeneration = 0
	}
}

func (cs *controllerState) installSessionStartEventAdmission(admit func(string)) error {
	if cs == nil {
		return fmt.Errorf("installing session-start event admission: controller state is nil")
	}
	if admit == nil {
		return fmt.Errorf("installing session-start event admission: callback is nil")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.sessionStartEventAdmissionStopping {
		return fmt.Errorf("installing session-start event admission: admission is stopping")
	}
	if cs.sessionStartEventAdmission != nil {
		return fmt.Errorf("installing session-start event admission: callback is already installed")
	}
	cs.sessionStartEventAdmission = admit
	return nil
}

func (cs *controllerState) stopSessionStartEventAdmission() {
	if cs == nil {
		return
	}
	cs.mu.Lock()
	cs.sessionStartEventAdmissionStopping = true
	cs.sessionStartEventAdmission = nil
	cs.mu.Unlock()
	cs.sessionStartEventAdmissionWG.Wait()
	cs.mu.Lock()
	cs.sessionStartEventAdmissionStopping = false
	cs.mu.Unlock()
}

func (cs *controllerState) admitSessionStartEvent(evt events.Event) {
	if cs == nil || !isBeadMutationEvent(evt.Type) {
		return
	}
	bead, ok := beads.DecodeBeadEventPayload(evt.Payload)
	if !ok || !session.IsSessionBeadOrRepairable(bead) {
		return
	}
	if err := validateSessionStartAdmission(bead.ID, sessionStartAdmissionInProcess); err != nil {
		return
	}

	cs.mu.Lock()
	admit := cs.sessionStartEventAdmission
	if admit != nil && !cs.sessionStartEventAdmissionStopping {
		cs.sessionStartEventAdmissionWG.Add(1)
	} else {
		admit = nil
	}
	cs.mu.Unlock()
	if admit == nil {
		return
	}
	defer cs.sessionStartEventAdmissionWG.Done()
	admit(bead.ID)
}

func isBeadMutationEvent(eventType string) bool {
	switch eventType {
	case events.BeadCreated, events.BeadUpdated, events.BeadClosed, events.BeadDeleted:
		return true
	default:
		return false
	}
}
