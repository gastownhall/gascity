package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pendingwatch"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// pendingInteractionInterval is the pending-interaction lane's cadence. A pass
// reads each active session's last-activity time from the runtime's fleet
// snapshot (no fork per session) and captures a screen only for a session
// whose output moved since its last capture, so an idle city costs no
// captures and a busy session at most one per pass. Five seconds bounds how
// late a new approval prompt is announced; an answered prompt (POST
// .../respond) pokes the lane, so its clear does not wait for the cadence.
const pendingInteractionInterval = 5 * time.Second

// pendingInteractionActor stamps the session.pending / session.pending_cleared
// events the controller records.
const pendingInteractionActor = "controller"

// startPendingInteractionLane starts the controller's pending-interaction
// detection (internal/pendingwatch) and returns the function that stops it and
// waits for it. It publishes session.pending / session.pending_cleared on the
// city event log with only the controller running, whether or not any client
// watches the stream. The announced set is rebuilt from the event log, so a
// restart re-announces nothing and clears what was answered while it was down.
// It does nothing when the runtime has no event recorder.
func (cr *CityRuntime) startPendingInteractionLane(ctx context.Context) func() {
	if cr.rec == nil || cr.rec == events.Discard {
		return func() {}
	}
	published := map[string]pendingwatch.Published{}
	if ep := cr.pendingInteractionEventProvider(); ep != nil {
		rebuilt, err := pendingwatch.RebuildFrom(ep)
		if err != nil {
			fmt.Fprintf(cr.stderr, "%s: pending interactions: reading the event log: %v (starting with none announced)\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		} else {
			published = rebuilt
		}
	}
	w := pendingwatch.New(cr.pendingInteractionDeps(), published)
	if cr.cs != nil {
		cr.cs.setPendingInteractionPoke(w.Poke)
	}
	laneCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cr.safeTick(func() { w.Run(laneCtx, pendingInteractionInterval) }, "pending-interactions")
	}()
	return func() {
		if cr.cs != nil {
			cr.cs.setPendingInteractionPoke(nil)
		}
		cancel()
		<-done
	}
}

// pendingInteractionEventProvider is the readable event log the lane rebuilds
// its announced set from.
func (cr *CityRuntime) pendingInteractionEventProvider() events.Provider {
	if ep := cr.routeRecoveryEventProvider(); ep != nil {
		return ep
	}
	if ep, ok := cr.rec.(events.Provider); ok {
		return ep
	}
	return nil
}

// pendingInteractionDeps wires the watcher to this city's sessions store,
// runtime provider and event recorder.
func (cr *CityRuntime) pendingInteractionDeps() pendingwatch.Deps {
	return pendingwatch.Deps{
		List: func() (pendingwatch.Listing, error) {
			return pendingInteractionListing(cr.v2SessionsStore())
		},
		Activity: func(name string) (time.Time, bool) {
			_, sp := cr.serviceProviderSnapshot()
			return pendingInteractionActivity(sp, name)
		},
		Probe: func(ctx context.Context, name string) pendingwatch.Observation {
			_, sp := cr.serviceProviderSnapshot()
			return pendingInteractionObserve(ctx, sp, name)
		},
		Publish: func(tr pendingwatch.Transition) error {
			return publishPendingTransition(cr.rec, tr)
		},
		Logf: func(format string, args ...any) {
			fmt.Fprintf(cr.stderr, "%s: "+format+"\n", append([]any{cr.logPrefix}, args...)...) //nolint:errcheck // best-effort stderr
		},
	}
}

// pendingInteractionListing is the probe set: open sessions in the active
// state, plus legacy empty-state rows, which predate the state field and can
// still hold a live runtime. Asleep, draining, creating and closed sessions
// have no live runtime that could be blocked on a prompt.
func pendingInteractionListing(store beads.Store) (pendingwatch.Listing, error) {
	if store == nil {
		// No sessions store (yet): nothing to probe, and nothing proven gone.
		return pendingwatch.Listing{Partial: true}, nil
	}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		return pendingwatch.Listing{}, err
	}
	var out pendingwatch.Listing
	for _, info := range snapshot.OpenInfos() {
		if info.Closed || (info.State != sessionpkg.StateActive && info.State != sessionpkg.StateNone) {
			continue
		}
		name := strings.TrimSpace(info.SessionName)
		if name == "" {
			continue
		}
		out.Sessions = append(out.Sessions, pendingwatch.Session{ID: info.ID, Name: name, Template: info.Template, Alias: info.Alias})
	}
	return out, nil
}

// pendingInteractionProvider strips the attachment cache, which answers no
// interaction reads of its own.
func pendingInteractionProvider(sp runtime.Provider) runtime.Provider {
	if cached, ok := sp.(*attachmentCachingProvider); ok && cached.Provider != nil {
		return cached.Provider
	}
	return sp
}

func pendingInteractionActivity(sp runtime.Provider, name string) (time.Time, bool) {
	sp = pendingInteractionProvider(sp)
	if sp == nil {
		return time.Time{}, false
	}
	at, err := sp.GetLastActivity(name)
	if err != nil || at.IsZero() {
		return time.Time{}, false
	}
	return at, true
}

// pendingInteractionObserve reads one session's pending interaction through
// the worker boundary, bounded like the reconciler's own probes.
func pendingInteractionObserve(ctx context.Context, sp runtime.Provider, name string) pendingwatch.Observation {
	sp = pendingInteractionProvider(sp)
	if sp == nil {
		return pendingwatch.Observation{}
	}
	handle, err := worker.NewRuntimeHandle(worker.RuntimeHandleConfig{Provider: sp, SessionName: name})
	if err != nil {
		return pendingwatch.Observation{Supported: true, Err: err}
	}
	obs, ok := boundedProbe(ctx, func() pendingwatch.Observation {
		pending, supported, err := handle.PendingStatus(ctx)
		if err != nil || pending == nil {
			return pendingwatch.Observation{Supported: supported, Err: err}
		}
		return pendingwatch.Observation{Supported: true, Pending: &runtime.PendingInteraction{
			RequestID: pending.RequestID,
			Kind:      pending.Kind,
			Prompt:    pending.Prompt,
			Options:   pending.Options,
			Metadata:  pending.Metadata,
		}}
	})
	if !ok {
		return pendingwatch.Observation{Supported: true, Err: fmt.Errorf("pending probe for %q timed out", name)}
	}
	return obs
}

// publishPendingTransition records one transition with its registered typed
// payload, and reports an error unless the log acknowledged it (a recorder
// without acknowledgements is trusted, as every other emitter trusts Record).
func publishPendingTransition(rec events.Recorder, tr pendingwatch.Transition) error {
	var payload events.Payload
	switch tr.Type {
	case events.SessionPending:
		payload = api.SessionPendingPayload{
			SessionID: tr.Session.ID,
			Template:  tr.Session.Template,
			Alias:     tr.Session.Alias,
			RequestID: tr.Pending.RequestID,
			Kind:      tr.Pending.Kind,
			Prompt:    tr.Pending.Prompt,
			Options:   tr.Pending.Options,
			Metadata:  tr.Pending.Metadata,
		}
	case events.SessionPendingCleared:
		payload = api.SessionPendingClearedPayload{
			SessionID: tr.Session.ID,
			RequestID: tr.Previous.RequestID,
			Kind:      tr.Previous.Kind,
			Reason:    tr.Reason,
		}
	default:
		return fmt.Errorf("unknown pending transition %q", tr.Type)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", tr.Type, err)
	}
	e := events.Event{
		Type:      tr.Type,
		Actor:     pendingInteractionActor,
		Subject:   tr.Session.ID,
		SessionID: tr.Session.ID,
		Payload:   raw,
	}
	if ack, ok := rec.(events.AckRecorder); ok {
		return ack.RecordAck(e)
	}
	rec.Record(e)
	return nil
}

// controllerState is the State the API pokes after a respond.
var _ api.PendingInteractionPoker = (*controllerState)(nil)

// setPendingInteractionPoke installs the running lane's poke; nil removes it.
func (cs *controllerState) setPendingInteractionPoke(poke func(sessionID string)) {
	if poke == nil {
		cs.pendingPoke.Store(nil)
		return
	}
	cs.pendingPoke.Store(&poke)
}

// PokePendingInteractions asks the controller to re-probe sessionID now, so an
// answered interaction's session.pending_cleared does not wait for the lane's
// cadence. It does nothing while no lane runs.
func (cs *controllerState) PokePendingInteractions(sessionID string) {
	if cs == nil {
		return
	}
	if poke := cs.pendingPoke.Load(); poke != nil {
		(*poke)(sessionID)
	}
}
