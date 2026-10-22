package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// SleepReasonKilled is the sleep_reason `gc session kill` records. The string
// predates this constant; defining it here lets the kill fence below match it
// without a raw literal.
const SleepReasonKilled SleepReason = "killed"

// KillPendingReason is the state_reason `gc session kill` stamps on the row
// while it tears the runtime down. The kill records its asleep intent BEFORE
// stopping the runtime, so for the length of the provider Stop the row reads
// asleep while the process is still alive. Without a marker the controller's
// advisory-state heal reads that as drift and flips the row back to awake, and
// the next tick then sees awake next to a dead runtime: exactly the
// kill-then-heal race the ordering exists to close.
const KillPendingReason = "kill-pending"

// KillPendingGrace bounds how long a kill fence is honored. The killer (`gc
// session kill`, or the API kill through KillFenced) clears the fence as soon
// as the provider Stop returns and its runtime lease is released (CONTRACT
// v5.10p), so an honored fence normally lives for one Stop. The bound only
// matters when that killer died mid-kill, or its lease release failed: past
// it the row is ordinary asleep state again and the normal drift heal and
// wake rules take over.
const KillPendingGrace = 5 * time.Minute

// KillPendingPatch is the durable kill intent: SleepPatch(now, "killed") plus
// the kill-pending state_reason. slept_at doubles as the fence timestamp, and
// together with the state_reason it identifies this particular kill, so the
// CLI clears or rolls back only its own fence. The kill supersedes any pending
// wake request (CONTRACT v5.7 D7), so the patch clears it too, and a failed
// kill's rollback restores it.
func KillPendingPatch(now time.Time) MetadataPatch {
	patch := SleepPatch(now, string(SleepReasonKilled))
	patch["state_reason"] = KillPendingReason
	for k, v := range ClearWakeRequestPatch() {
		patch[k] = v
	}
	return patch
}

// KillPendingMetadata reports whether raw session metadata carries a kill
// fence that is still within KillPendingGrace of now. A fence with an
// unparseable timestamp, or one further than the grace in the future (clock
// skew or a bogus write), is not honored: a malformed fence must never pin a
// session out of the lifecycle loop.
func KillPendingMetadata(state, stateReason, sleepReason, sleptAt string, now time.Time) bool {
	if strings.TrimSpace(state) != string(StateAsleep) ||
		strings.TrimSpace(stateReason) != KillPendingReason ||
		SleepReason(strings.TrimSpace(sleepReason)) != SleepReasonKilled {
		return false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(sleptAt))
	if err != nil {
		return false
	}
	age := now.Sub(at)
	return age < KillPendingGrace && age > -KillPendingGrace
}

// IsKillPendingInfo reports whether info carries an honored kill fence: a
// `gc session kill` is mid-teardown and owns the row until it clears the
// fence. Lifecycle passes must leave such a row alone, neither healing it back
// to awake while the runtime is still dying nor restarting or closing it once
// the runtime is gone.
func IsKillPendingInfo(info Info, now time.Time) bool {
	return KillPendingMetadata(info.MetadataState, info.StateReason, info.SleepReason, info.SleptAt, now)
}

// KillFence is the durable kill intent an operator kill (`gc session kill`,
// the API session kill) writes before it stops the runtime (see
// KillPendingPatch). It remembers the values the patch replaced so a failed
// Stop can put the row back exactly as it was, and the slept_at stamp that
// identifies this kill's fence so the killer clears or rolls back only its
// own write.
type KillFence struct {
	sessionID string
	sleptAt   string
	wrote     map[string]string // what the fence wrote
	restore   map[string]string // the row's values for those keys before it
}

// KillFenceAttempts bounds the re-read/re-decide loop around a
// revision-fenced write. A precondition failure is an observation, not a
// verdict: the revision can move for an unrelated key, and on stores that
// emulate the fence over a whole-row revision even a derived-column rewrite
// moves it. Each retry re-reads and re-checks the guard before writing again.
const KillFenceAttempts = 3

// ErrKillFenceSuperseded reports that the row no longer satisfies the
// guard of a fence write: it closed before the kill could record its intent,
// or someone else rewrote it after the fence landed.
var ErrKillFenceSuperseded = errors.New("session row changed underneath the kill")

// WriteKillFence durably records the kill intent before the runtime is
// torn down. Where the store resolves a conditional writer the write is fenced
// on the revision of the row it observed, so it cannot silently overwrite a
// transition that landed between the read and the write; otherwise it is the
// same unconditional batch write the post-kill sync always was.
func WriteKillFence(store beads.Store, sessionID string, now time.Time) (*KillFence, error) {
	patch := KillPendingPatch(now)
	patch["synced_at"] = now.UTC().Format(time.RFC3339)
	fence := &KillFence{sessionID: sessionID, sleptAt: patch["slept_at"]}
	err := ApplyKillFencePatch(store, sessionID, func(b beads.Bead) (map[string]string, bool) {
		if b.Status == "closed" {
			return nil, false
		}
		// The kill is the disposition's Kill cell: an operator suspend goes
		// whole, its held_until with it, so no heartbeat-shaped hold is left
		// behind; a wait or a heartbeat stays. The fence writes
		// KillPendingPatch's lifecycle and only the class keys the kill
		// changed, so a store without conditional writes reverts no
		// concurrent write to the others.
		killed, err := DecodeDisposition(b.Metadata).Apply(ActorOperator, DispEvent{Kind: EvKill}, State(b.Metadata["state"]), now)
		if err != nil {
			return nil, false
		}
		write := map[string]string(killed.Changed().Into(sessionID, maps.Clone(patch)))
		// Capture the pre-image from the same observation the fenced write is
		// conditioned on, so a rollback restores the row the kill replaced.
		restore := make(map[string]string, len(write))
		for key := range write {
			restore[key] = b.Metadata[key]
		}
		fence.wrote, fence.restore = write, restore
		return write, true
	})
	if err != nil {
		return nil, err
	}
	return fence, nil
}

// Owns reports whether b still carries this kill's fence.
func (f *KillFence) Owns(b beads.Bead) bool {
	return b.Status != "closed" &&
		strings.TrimSpace(b.Metadata["state"]) == string(StateAsleep) &&
		strings.TrimSpace(b.Metadata["state_reason"]) == KillPendingReason &&
		strings.TrimSpace(b.Metadata["slept_at"]) == f.sleptAt
}

// Clear lifts the fence after the runtime is confirmed gone. The row keeps the
// asleep/killed intent; only the kill-pending marker goes, handing the session
// back to the ordinary lifecycle rules (which is what restarts it).
func (f *KillFence) Clear(store beads.Store) error {
	return ApplyKillFencePatch(store, f.sessionID, func(b beads.Bead) (map[string]string, bool) {
		if !f.Owns(b) {
			return nil, false
		}
		return map[string]string{"state_reason": ""}, true
	})
}

// Rollback restores the pre-kill values after a Stop that left the runtime
// alive, so the row does not claim a runtime is gone when it is not. It only
// touches a row that still carries this kill's fence: if anything else has
// rewritten the row since, that newer state is left alone.
func (f *KillFence) Rollback(store beads.Store) error {
	return ApplyKillFencePatch(store, f.sessionID, func(b beads.Bead) (map[string]string, bool) {
		if !f.Owns(b) {
			return nil, false
		}
		// Only the keys the fence changed that still read as it wrote them:
		// a write since (a wait, a quarantine) stands. The class keys' undo
		// is the Kill cell's (UndoDisposition).
		undo := make(MetadataPatch, len(f.wrote))
		for k, v := range LifecycleOnly(f.wrote) {
			if b.Metadata[k] == v && f.restore[k] != v {
				undo[k] = f.restore[k]
			}
		}
		return UndoDisposition(f.restore, f.wrote, b.Metadata).Into(f.sessionID, undo), true
	})
}

// ApplyKillFencePatch reads the row, asks decide for the patch to write
// against that observation, and writes it: revision-fenced when the store
// resolves a conditional writer (re-reading and re-deciding on a precondition
// failure), unconditionally otherwise. decide returning false means the row no
// longer qualifies and nothing is written (ErrKillFenceSuperseded).
func ApplyKillFencePatch(store beads.Store, sessionID string, decide func(beads.Bead) (map[string]string, bool)) error {
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < KillFenceAttempts; attempt++ {
		b, err := store.Get(sessionID)
		if err != nil {
			return err
		}
		patch, ok := decide(b)
		if !ok {
			return ErrKillFenceSuperseded
		}
		if len(patch) == 0 {
			return nil
		}
		if writer == nil {
			err := store.SetMetadataBatch(sessionID, patch)
			if err != nil {
				UnmintDisposition(sessionID, patch)
			}
			return err
		}
		lastErr = writer.UpdateIfMatch(sessionID, b.Revision, beads.UpdateOpts{Metadata: patch})
		if lastErr != nil {
			UnmintDisposition(sessionID, patch) // decide mints again on the next attempt
		}
		if lastErr == nil || !beads.IsPreconditionFailed(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("conditional write kept losing to concurrent updates: %w", lastErr)
}

// KillSleepPatch is the kill's asleep sync with no fence, on the row's fresh
// metadata: SleepPatch's lifecycle, the wake request cleared, and the Kill
// cell's class keys, as the fence writes them (no heartbeat-shaped hold left).
func KillSleepPatch(id string, meta map[string]string, now time.Time) map[string]string {
	patch := SleepPatch(now, string(SleepReasonKilled))
	patch["synced_at"] = now.Format(time.RFC3339)
	maps.Copy(patch, ClearWakeRequestPatch())
	return CellInto(id, meta, ActorOperator, DispEvent{Kind: EvKill}, now, patch)
}

// Wrote returns a copy of the keys the fence wrote, with their values.
func (f *KillFence) Wrote() map[string]string { return maps.Clone(f.wrote) }

// KillFenced is an operator kill under the kill fence, as `gc session kill`
// does it: kill (Manager.Kill, or a worker handle's Kill over it) runs with
// the runtime lease held from the fence write through the asleep sync
// (I-LEASE), the fence durable before the runtime is stopped, and afterwards
// the row left asleep/killed with the fence lifted (or restored, when the
// stop left the runtime alive). Without the fence the controller reads the
// kill as a crash: it heals the asleep row back to awake while the stop runs,
// accrues the death as a wake failure, and restarts the session in the same
// pass.
//
// A row whose runtime is already gone has no window to fence: kill runs
// alone (under the lease) and its result is returned as it is; a configured
// named session's row is synced asleep/killed after it all the same, as `gc
// session kill` does, so the kill supersedes a pending wake. A fence that
// cannot be written does not hold the kill hostage: it falls back to the stop
// and the best-effort asleep sync after it. Warnings (a write that did not
// land) go to the log; KillFencedReport takes a warning sink and reports
// what the kill found.
func (m *Manager) KillFenced(ctx context.Context, by Actor, id string, kill func(context.Context, Actor) error) error {
	_, err := m.KillFencedReport(ctx, by, id, kill, func(msg string) { log.Printf("session kill: %s", msg) })
	return err
}

// KillReport is what KillFencedReport found.
type KillReport struct {
	// RuntimeWasGone reports that the fresh read under the lease found no
	// live runtime, so the kill ran alone, unfenced.
	RuntimeWasGone bool
}

// KillFencedReport is KillFenced with a warning sink, for a caller that
// reports to its own operator (`gc session kill`'s stderr). A caller that
// holds the runtime lease already (`gc session kill --force` past a hung
// holder) lends it in by.Lease. A fenced kill that stopped the runtime ends
// the lease, a lent one too, before it lifts the fence (CONTRACT v5.10p);
// the caller's own Release is then a no-op.
func (m *Manager) KillFencedReport(ctx context.Context, by Actor, id string, kill func(context.Context, Actor) error, warn func(string)) (KillReport, error) {
	var report KillReport
	if err := m.checkActor(by); err != nil {
		return report, err
	}
	_, sessName, err := m.sessionBead(id)
	if err != nil {
		return report, err
	}
	lease, release, err := m.leaseRuntime(ctx, by, id, sessName, operatorLeaseWait)
	if err != nil && !errors.Is(err, ErrSessionStarting) && !errors.Is(err, ErrRuntimeLeaseBusy) && !by.City.IsZero() {
		// As leaseForStop: a lease failure other than busy stops under the
		// name's flock alone; a busy flock is retryable (ErrSessionStarting).
		log.Printf("runtime lease: session %q: killing %q under the name's flock alone: %v", id, sessName, err)
		lease, err = operatorRuntimeLease(TryRuntimeLease(nil, RuntimeLeaseRequest{City: by.City.Path(), Name: sessName}))
		if err == nil {
			release = lease.Release
		}
	}
	if err != nil {
		return report, err
	}
	defer release()
	if lease != nil {
		by.Lease = lease
	}
	// Decided under the lease: a start that finished while the kill waited
	// for it has left a live runtime, which takes the fenced path.
	b, _, err := m.sessionBead(id)
	if err != nil {
		return report, err
	}
	report.RuntimeWasGone = m.killRuntimeGone(ctx, b, sessName)
	if report.RuntimeWasGone {
		killErr := kill(ctx, by)
		if killErr == nil || NamedSessionIdentityInfo(m.infoFromBead(b)) != "" {
			m.syncKilledAsleep(id, warn)
		}
		return report, killErr
	}

	fence, ferr := WriteKillFence(m.store, id, m.now().UTC())
	if ferr != nil {
		warn(fmt.Sprintf("recording kill intent for session %s before stopping it: %v", id, ferr))
		fence = nil
	}
	killErr := kill(ctx, by)
	if killErr != nil && fence != nil && m.runtimeConfirmedGone(ctx, sessName) {
		// The fenced row reads asleep, so Manager.Kill checks the provider
		// and reports "not active" for a runtime already gone: the runtime
		// is down, which is what the kill is for.
		killErr = nil
	}
	if killErr != nil {
		if fence != nil {
			// The runtime survived the stop: the fence would claim a
			// teardown that did not happen.
			if err := fence.Rollback(m.store); err != nil {
				warn(fmt.Sprintf("restoring session %s after the failed kill: %v", id, err))
			}
		}
		return report, killErr
	}
	if fence != nil {
		// The fence covers the whole kill (CONTRACT v5.10p): it lifts only
		// once the lease, the kill's own or a lent one, is released, record
		// and flock, so a controller pass that reads the lift never finds
		// the name busy. A release that failed leaves the fence in place,
		// to lapse at KillPendingGrace.
		if err := lease.ReleaseChecked(); err != nil {
			warn(fmt.Sprintf("releasing the runtime lease on session %s: %v; its kill fence lapses after %v", id, err, KillPendingGrace))
			return report, nil
		}
		if err := fence.Clear(m.store); err != nil {
			warn(fmt.Sprintf("releasing kill fence on session %s: %v", id, err))
		}
		return report, nil
	}
	m.syncKilledAsleep(id, warn)
	return report, nil
}

// syncKilledAsleep is the kill's best-effort asleep sync without a fence
// (KillSleepPatch on the row's fresh metadata).
func (m *Manager) syncKilledAsleep(id string, warn func(string)) {
	now := m.now().UTC()
	if err := ApplyKillFencePatch(m.store, id, func(b beads.Bead) (map[string]string, bool) {
		return KillSleepPatch(b.ID, b.Metadata, now), true
	}); err != nil {
		warn(fmt.Sprintf("syncing session %s to asleep: %v", id, err))
	}
}

// killLivenessTimeout bounds KillFenced's fresh liveness reads, as the v2
// effects bound theirs (fenceProbeTimeout).
const killLivenessTimeout = 5 * time.Second

// runtimeConfirmedGone reports a fresh, bounded liveness read that finds no
// live pane under name. Provider.IsRunning may answer from a cache (tmux's
// state cache is up to 2s old), which can read a just-started runtime gone
// and so kill it without its fence; an incomplete or failed read is not
// gone either, so the kill keeps the fence.
//
// The read is the routed leaf's (runtime.ResolveBackend), as v2's runtime
// read takes it: a composite (auto, hybrid) routes by name, and hybrid
// deliberately offers no fresh read of its own (its read is its leaves'
// cached one), so asking it would fall back to the cache. The route comes
// from the bead (loadSessionBead registers an ACP session's route), so call
// this after reading the row. A route that is only a guess confirms nothing,
// nor does a leaf that cannot read fresh (runtime.FreshReadable: herdr, k8s,
// exec answer from IsRunning, which proves no absence), and the composite's
// own read must agree with the leaf's: auto falls through to its other
// backend, which can hold a runtime the route table missed.
func (m *Manager) runtimeConfirmedGone(ctx context.Context, name string) bool {
	if m.sp == nil || strings.TrimSpace(name) == "" {
		return false
	}
	leaf, _, known := runtime.ResolveBackend(m.sp, name)
	if !known || leaf == nil || !runtime.FreshReadable(leaf) {
		return false
	}
	gone := func(sp runtime.Provider) bool {
		live, status, err := runtime.ObserveLivenessBoundedSince(ctx, sp, name, nil, time.Now(), killLivenessTimeout)
		return status == runtime.ObservationComplete && err == nil && !live.Running
	}
	return gone(leaf) && (leaf == m.sp || gone(m.sp))
}

// killRuntimeGone is KillFencedReport's "runtime already gone" verdict on row
// b: the row does not read live and a fresh read finds no runtime.
func (m *Manager) killRuntimeGone(ctx context.Context, b beads.Bead, sessName string) bool {
	return !killRuntimeLive(State(b.Metadata["state"])) && m.runtimeConfirmedGone(ctx, sessName)
}

// KillRuntimeGone takes KillFencedReport's "runtime already gone" verdict
// now, without the lease, reading the runtime routed from the bead as the
// kill does. `gc session kill` uses it before the lease only to gate its
// live-subagent refusal; the kill decides again under the lease.
func (m *Manager) KillRuntimeGone(ctx context.Context, id string) (bool, error) {
	b, sessName, err := m.loadSessionBead(id, true)
	if err != nil {
		return false, err
	}
	return m.killRuntimeGone(ctx, b, sessName), nil
}

// killRuntimeLive reports the states Manager.Kill treats as holding a live
// runtime without asking the provider.
func killRuntimeLive(s State) bool {
	switch s {
	case StateActive, StateStartPending, StateCreating, StateDraining, StateAwake:
		return true
	}
	return false
}
