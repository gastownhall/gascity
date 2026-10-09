package session

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ResumePendingAtKey is the resume fence (CONTRACT v5.9 D8 rule 2): when an
// operator's resume began starting the runtime of a dormant row. Legacy
// ignores it.
const ResumePendingAtKey = "resume_pending_at"

// ResumeFenceTTL bounds an unexpired fence: the default startup timeout
// plus D8's 30s margin. A fence older than it is left by a caller that died.
const ResumeFenceTTL = 90 * time.Second

var (
	// ErrResumeSuperseded reports that an operator write (a kill, a suspend,
	// a new incarnation, a close) landed under a resume, which wrote nothing
	// and stopped any runtime it launched.
	ErrResumeSuperseded = errors.New("session changed during resume")
	// ErrResumeInProgress reports another caller's unexpired resume fence, or
	// a CAS lost on every attempt. Nothing was written or stopped; retry.
	ErrResumeInProgress = errors.New("session resume in progress; retry")
	// ErrResumeRuntimeUnknown reports a live runtime whose identity could
	// not be read, so the resume neither adopted nor stopped it.
	ErrResumeRuntimeUnknown = errors.New("session runtime identity unknown")
)

// resumeFactKeys are the operator-intent facts the fence and the consume
// require unchanged; not state, which legacy's heal flips from suspended to
// awake on a live runtime.
var resumeFactKeys = []string{
	"held_until", "quarantined_until", "sleep_intent", "wait_hold", "suspended_at",
	"sleep_reason", "state_reason", "wake_request", "wake_requested_at",
	"generation", "instance_token",
}

// resumeCall is one resume: the facts it read, the fence it wrote ("" when
// the runtime was already live), and the session object it launched.
type resumeCall struct {
	facts  map[string]string
	stamp  string
	object *runtime.Liveness
}

func newResumeCall(meta map[string]string) *resumeCall {
	facts := make(map[string]string, len(resumeFactKeys))
	for _, key := range resumeFactKeys {
		facts[key] = meta[key]
	}
	return &resumeCall{facts: facts}
}

// fenceExpired reports whether meta's fence is absent or past ResumeFenceTTL.
func fenceExpired(meta map[string]string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(meta[ResumePendingAtKey]))
	return err != nil || !now.Before(at.Add(ResumeFenceTTL))
}

// ResumeFenceLive reports whether an operator's resume is starting meta's
// row: its fence is present and unexpired.
func ResumeFenceLive(meta map[string]string, now time.Time) bool {
	return !fenceExpired(meta, now)
}

// holds reports whether meta still carries the call's facts and the call's
// fence; a call that wrote none requires no live foreign fence.
func (rc *resumeCall) holds(meta map[string]string, now time.Time) error {
	for key, want := range rc.facts {
		if meta[key] != want {
			return ErrResumeSuperseded
		}
	}
	if rc.stamp != "" {
		if meta[ResumePendingAtKey] != rc.stamp {
			return ErrResumeSuperseded
		}
	} else if !fenceExpired(meta, now) {
		return ErrResumeInProgress
	}
	return nil
}

// fence writes D8 rule 2's fence, and only it.
func (m *Manager) fence(id string, rc *resumeCall) error {
	now := m.now()
	err := m.resumeCAS(id, func(meta map[string]string) (MetadataPatch, error) {
		return MetadataPatch{ResumePendingAtKey: now.UTC().Format(time.RFC3339Nano)}, rc.holds(meta, now)
	})
	if err == nil {
		rc.stamp = now.UTC().Format(time.RFC3339Nano)
	}
	return err
}

// consume is D8 rule 4's CAS: active, with every blocker, dormant marker,
// request and the fence cleared, and a new awake interval only when this
// call launched the runtime (#3513). Generation, token and baseline stay.
func (m *Manager) consume(id string, rc *resumeCall) error {
	now := m.now()
	return m.resumeCAS(id, func(meta map[string]string) (MetadataPatch, error) {
		patch := ClearWakeBlockersPatch(State(strings.TrimSpace(meta["state"])), meta["sleep_reason"], now)
		for _, clear := range []MetadataPatch{ClearWakeRequestPatch(), ClearStopRequestPatch()} {
			for k, v := range clear {
				patch[k] = v
			}
		}
		for _, key := range []string{"sleep_reason", "slept_at", "suspended_at", ResumePendingAtKey, "pending_create_claim", "pending_create_started_at"} {
			patch[key] = ""
		}
		patch["state"], patch["state_reason"] = string(StateActive), "creation_complete"
		if rc.object != nil {
			patch["last_woke_at"] = now.UTC().Format(time.RFC3339)
			patch["awake_started_at"] = awakeIntervalStartedAt(now)
		}
		return patch, rc.holds(meta, now)
	})
}

// confirmStarted confirms a row that was not dormant, at the incarnation it
// read: a starting row becomes active (one gone dormant since stays so) and
// a pending-create claim is dropped. Nothing to confirm writes nothing.
func (m *Manager) confirmStarted(id string, read map[string]string) error {
	patch := func(meta map[string]string) MetadataPatch {
		patch := MetadataPatch{}
		switch State(meta["state"]) {
		case "", StateStartPending, StateCreating:
			patch["state"], patch["state_reason"] = string(StateActive), "creation_complete"
		}
		if strings.TrimSpace(meta["pending_create_claim"]) != "" {
			patch["pending_create_claim"], patch["pending_create_started_at"] = "", ""
		}
		return patch
	}
	if len(patch(read)) == 0 {
		return nil
	}
	return m.resumeCAS(id, func(meta map[string]string) (MetadataPatch, error) {
		if meta["generation"] != read["generation"] || meta["instance_token"] != read["instance_token"] {
			return nil, ErrResumeSuperseded
		}
		return patch(meta), nil
	})
}

// resumeBackoff waits before the next resume CAS attempt; tests replace it.
var resumeBackoff = func(attempt int) { time.Sleep(time.Duration(10<<attempt) * time.Millisecond) }

// resumeCAS writes decide's patch by one fenced Update per attempt while the
// row is open, holds no kill fence and passes decide's premise. A failed
// premise returns decide's error (ErrResumeSuperseded or ErrResumeInProgress)
// and writes nothing; a CAS lost on every attempt, with backoff between,
// returns ErrResumeInProgress. Callers hold the session mutation lock.
func (m *Manager) resumeCAS(id string, decide func(meta map[string]string) (MetadataPatch, error)) error {
	store := NewStore(beads.SessionStore{Store: m.store})
	for attempt := 0; attempt < 5; attempt++ {
		var refused error
		wanted := false
		now := m.now()
		ok, err := store.UpdateMetadataFenced(id, 1, func(cur Info, persisted PersistedResponse) MetadataPatch {
			patch, premise := decide(persisted.Metadata)
			switch {
			case cur.Closed || IsKillPendingInfo(cur, now):
				refused = ErrResumeSuperseded
			case premise != nil:
				refused = premise
			}
			wanted = refused == nil && len(patch) > 0
			if !wanted {
				return nil
			}
			return patch
		})
		switch {
		case err != nil:
			return fmt.Errorf("%w: updating session state: %w", ErrStateSync, err)
		case refused != nil:
			return fmt.Errorf("%w: %s", refused, id)
		case ok || !wanted:
			return nil
		}
		resumeBackoff(attempt)
	}
	return fmt.Errorf("%w: %s: the row kept changing", ErrResumeInProgress, id)
}

// liveRuntimeOwned reports whether the live runtime sessName carries the
// row's token. A failed or empty read is unknown; a foreign runtime names
// its remedy.
func (m *Manager) liveRuntimeOwned(id, sessName, token string) error {
	got, err := m.sp.GetMeta(sessName, "GC_INSTANCE_TOKEN")
	switch {
	case err != nil:
		return fmt.Errorf("%w: %s: reading its live runtime's token: %w", ErrResumeRuntimeUnknown, id, err)
	case strings.TrimSpace(got) == "":
		return fmt.Errorf("%w: %s: its live runtime carries no instance token", ErrResumeRuntimeUnknown, id)
	case strings.TrimSpace(got) != strings.TrimSpace(token):
		return fmt.Errorf("%w: %s: its live runtime %q is not this session's; stop it with `gc session kill %s`", ErrResumeSuperseded, id, sessName, id)
	}
	return nil
}

// captureObject records the session object this call's Start launched, for
// a refusal's exact-object kill. A provider without exact-object kills, or a
// read that names no object, captures only that a launch happened.
func (m *Manager) captureObject(rc *resumeCall, sessName string) {
	rc.object = &runtime.Liveness{}
	if _, ok := m.sp.(runtime.SessionObjectKiller); !ok {
		return
	}
	if live, err := runtime.ObserveLivenessSince(m.sp, sessName, nil, m.now()); err == nil {
		rc.object = &live
	}
}

// stopOwnRuntime stops the runtime a refused resume launched (D8 rule 5;
// F2's own-runtime exception): the exact object captured after the Start,
// where the provider can kill by object, honoring the kill's verdict;
// otherwise a Stop while the runtime still carries the launch token.
func (m *Manager) stopOwnRuntime(rc *resumeCall, sessName, token string) error {
	killer, ok := m.sp.(runtime.SessionObjectKiller)
	if !ok {
		if err := m.liveRuntimeOwned("", sessName, token); err != nil {
			return err
		}
		return m.sp.Stop(sessName)
	}
	if rc.object.ObjectID == "" {
		return fmt.Errorf("not stopping session %q: its launched object was not captured", sessName)
	}
	live, err := runtime.ObserveLivenessSince(m.sp, sessName, nil, m.now())
	if err != nil {
		return err
	}
	var result runtime.SessionObjectKillResult
	if live.Corpse {
		result, err = killer.KillCorpseObject(sessName, rc.object.ObjectID, rc.object.ObjectCreated)
	} else {
		result, err = killer.KillZombieObject(sessName, rc.object.ObjectID, rc.object.ObjectCreated, live.PanePID)
	}
	switch {
	case err != nil:
		return err
	case result != runtime.SessionObjectKilled && result != runtime.SessionObjectGone:
		return fmt.Errorf("not stopping session %q: its launched object changed (kill verdict %d)", sessName, result)
	}
	return nil
}

// resumeConverged reports whether a refused resume found the row already
// consumed by another resume at the same incarnation: active, unheld, the
// generation and token unchanged. Its runtime is then the session's.
func (m *Manager) resumeConverged(id string, rc *resumeCall) bool {
	_, cur, err := NewStore(beads.SessionStore{Store: m.store}).GetPersistedResponse(id)
	if err != nil {
		return false
	}
	meta := cur.Metadata
	return State(meta["state"]) == StateActive && !IsHeldForResume(meta, m.now()) &&
		meta["generation"] == rc.facts["generation"] && meta["instance_token"] == rc.facts["instance_token"]
}

// voidReconcilerDrainAck clears a drain ack legacy's reconciler minted on the
// resumed runtime: a stop it decided while the row was held (or before the
// resume's runtime existed) is stale once the resume consumed the hold. An
// agent's own ack stays.
func (m *Manager) voidReconcilerDrainAck(sessName string) {
	if src, err := m.sp.GetMeta(sessName, "GC_DRAIN_ACK_SOURCE"); err != nil || src != "reconciler" {
		return
	}
	for _, key := range []string{"GC_DRAIN_ACK", "GC_DRAIN_ACK_SOURCE", "GC_DRAIN_ACK_REQUESTER_INSTANCE_TOKEN", "GC_DRAIN_REASON", "GC_DRAIN_GENERATION"} {
		_ = m.sp.RemoveMeta(sessName, key)
	}
}
