package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const (
	spawnStormMayorRecipient = "mayor"
	spawnStormMailSender     = "controller"
)

// observeSessionStoppedForSafetyNet is the wiring entry point: invoked
// after the session reconciler confirms a worker has stopped, it asks
// the active safety net to record the outcome. If the outcome causes a
// storm-episode transition, mayor is notified once.
//
// Inputs:
//
//   - session: the session bead being torn down.
//   - sessionName: the runtime identifier the worker used as $GC_SESSION_NAME.
//   - template: the resolved template name from the session bead. Empty
//     when the template can't be determined (defensive: we skip).
//   - store: the city bead store; nil-safe.
//   - rigStores: rig-attached bead stores; nil-safe.
//   - now: clock reading. Wall clock by default; tests pass a fixed time.
//   - stderr: best-effort diagnostics sink.
//
// All steps are nil/empty-safe so this can be called from any drain
// observation point without local pre-checks. The function never
// returns an error — observation is best-effort and must not affect
// the reconciler's primary teardown path.
func observeSessionStoppedForSafetyNet(
	cityPath string,
	session beads.Bead,
	sessionName, template string,
	store beads.Store,
	rigStores map[string]beads.Store,
	now time.Time,
	stderr io.Writer,
) {
	sn := safetyNetForGating()
	if sn == nil || template == "" || sessionName == "" {
		return
	}
	// Only pool-managed workers participate in the spawn-storm class
	// the safety net exists to catch. Named singletons and one-shots
	// drain on their own terms; recording their outcomes would
	// accumulate per-template state that no gate ever consults.
	if !isPoolManagedSessionBead(session) {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	claimed, err := sessionEverClaimedAnyWork(store, rigStores, session)
	if err != nil {
		fmt.Fprintf(stderr, "spawn-storm safety net: claim lookup for %s failed: %v (treating as claimed; observation skipped)\n", sessionName, err) //nolint:errcheck
		return
	}
	// Rate-condition guard: when another worker in this pool is currently
	// in_progress on a routed bead, the pool is making progress and a
	// coincident drain-without-claim is most likely race-loser noise
	// rather than the spawn-storm pathology. Skip registration so a
	// transient doesn't poison the sliding window.
	//
	// Fail-safe: the predicate treats probe errors as "no live claim"
	// (i.e. proceed with registration) so a transient store error never
	// silently disables the safety net.
	if !claimed && poolHasLiveClaimForSafetyNet(store, template) {
		fmt.Fprintf(stderr, "spawn-storm safety net: drain for %s suppressed by rate-condition guard (live claim present for %s)\n", sessionName, template) //nolint:errcheck
		return
	}
	if !sn.RecordDrainOutcome(template, sessionName, claimed, now) {
		return
	}
	// First transition into a new storm episode for this template.
	// Mail mayor exactly once per episode. Pass the full contributor
	// list so operators can see every session that fed the episode,
	// not just the threshold-crossing one.
	contributors := sn.CurrentDrainContributors(template)
	if err := notifyMayorOfSpawnStorm(cityPath, store, template, sessionName, contributors, stderr); err != nil {
		fmt.Fprintf(stderr, "spawn-storm safety net: notify mayor for %s: %v\n", template, err) //nolint:errcheck
	}
}

// notifyMayorOfSpawnStorm sends mayor a mail describing the detected
// storm and enqueues a nudge so the mail wakes mayor immediately rather
// than waiting for the next inbox check.
//
// Mail-only when no cityPath is available (test paths, exec providers);
// the nudge enqueue requires filesystem access under cityPath. A failed
// nudge does not cause the function to error — the mail bead is the
// durable signal.
func notifyMayorOfSpawnStorm(cityPath string, store beads.Store, template, sessionName string, contributors []string, stderr io.Writer) error {
	if store == nil {
		return fmt.Errorf("no city store available")
	}
	if stderr == nil {
		stderr = io.Discard
	}
	provider := beadmail.New(store)
	subject := fmt.Sprintf("[SAFETY NET] Spawn-storm detected for pool %s", template)
	body := buildSpawnStormMailBody(template, sessionName, contributors)
	if _, err := provider.Send(spawnStormMailSender, spawnStormMayorRecipient, subject, body); err != nil {
		return fmt.Errorf("send mail to mayor: %w", err)
	}
	if cityPath == "" {
		// Mail-only path. Mayor will see the message on its next inbox
		// check; the wake-immediately property is lost. Diagnostics-
		// only — not an error condition.
		fmt.Fprintf(stderr, "spawn-storm safety net: nudge skipped (no city path)\n") //nolint:errcheck
		return nil
	}
	nudgeMsg := fmt.Sprintf("Spawn-storm detected for pool %s — please investigate", template)
	item := newQueuedNudge(spawnStormMayorRecipient, nudgeMsg, "spawn-storm-safety-net", time.Now())
	if err := enqueueQueuedNudgeWithStore(cityPath, store, item); err != nil {
		// Mail bead has been created — log but don't propagate.
		fmt.Fprintf(stderr, "spawn-storm safety net: nudge enqueue failed: %v (mail bead created)\n", err) //nolint:errcheck
	}
	return nil
}

// poolHasLiveClaimForSafetyNet reports whether any non-session bead in
// the city store is currently in_progress with assignee set and routed
// to template. It is the safety net's rate-condition predicate: if the
// pool is provably making progress, a coincident drain-without-claim is
// almost certainly race-loser noise.
//
// Fail-safe: returns false on probe failure or missing store. Returning
// "no live claim" lets the caller proceed with registration, which is
// the conservative choice — a transient store error must not silently
// disable the safety net's signal collection.
func poolHasLiveClaimForSafetyNet(store beads.Store, template string) bool {
	if store == nil || template == "" {
		return false
	}
	items, err := store.List(beads.ListQuery{
		Status:   "in_progress",
		Metadata: map[string]string{"gc.routed_to": template},
	})
	if err != nil {
		return false
	}
	for _, b := range items {
		if sessionpkg.IsSessionBeadOrRepairable(b) {
			continue
		}
		if strings.TrimSpace(b.Assignee) == "" {
			continue
		}
		return true
	}
	return false
}

func buildSpawnStormMailBody(template, sessionName string, contributors []string) string {
	contributorBlock := formatStormContributors(contributors, sessionName)
	return fmt.Sprintf(`The spawn-storm safety net detected a sustained pattern of pool
workers draining without claiming any bead for template %q.
Most recent observation: session %q drained without a claim.

Sessions that contributed to this episode (in arrival order):
%s

This pattern indicates the reconciler's spawn-decision predicate
disagrees with what workers can actually claim. The safety net has now
suppressed new spawns for this template via exponential backoff;
in-flight workers are NOT disturbed and complete normally.

While the safety net's throttle decays automatically when the underlying
condition clears, the root cause should be investigated.

Useful starting points:

  bd list --status=open --metadata gc.routed_to=%s
  gc events --type session.stopped --since 10m
  gc agents peek <worker> --tail 200

If a sustained mismatch is found, file a bead capturing the predicate
divergence — that's the durable fix layer; the safety net is only the
backstop.
`, template, sessionName, contributorBlock, template)
}

// formatStormContributors renders the contributor list as an indented
// bullet block. Falls back to the most-recent sessionName when the
// detector returned an empty list (defensive: contributor tracking is
// best-effort and must not block the mail).
func formatStormContributors(contributors []string, sessionName string) string {
	if len(contributors) == 0 {
		if sessionName == "" {
			return "  (none recorded)"
		}
		return "  - " + sessionName
	}
	var b strings.Builder
	for i, name := range contributors {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("  - ")
		b.WriteString(name)
	}
	return b.String()
}

// sessionEverClaimedAnyWork reports whether any non-session-bead in any
// reachable store carries one of the session's assignment identifiers,
// regardless of bead status (open, in_progress, closed). It is the
// safety net's "did this worker do anything before draining?" probe.
//
// A worker that only enumerated `bd ready` and then drain-acked never
// recorded itself as assignee on any bead, so this returns false. A
// worker that ran `bd update <id> --claim` left assignee=<sessionName>
// on the bead, which persists across status transitions (claimed,
// in_progress, closed), so this returns true even after the worker
// closes the bead and drains.
//
// Closed beads are intentionally included so we don't confuse "claimed
// and finished cleanly" (the healthy case) with "never claimed" (the
// storm signal). Without IncludeClosed we'd flag every successful
// worker as drain-without-claim.
func sessionEverClaimedAnyWork(store beads.Store, rigStores map[string]beads.Store, session beads.Bead) (bool, error) {
	if store == nil && len(rigStores) == 0 {
		// No store visibility — be conservative and treat as claimed
		// (no observation) rather than blame the worker for a probe
		// failure on our side.
		return true, nil
	}
	identifiers := sessionAssignmentIdentifiers(session)
	if len(identifiers) == 0 {
		return true, nil
	}
	stores := workAssignmentStores(store, rigStores)
	for _, s := range stores {
		if s == nil {
			continue
		}
		for _, id := range identifiers {
			items, err := s.List(beads.ListQuery{
				Assignee:      id,
				IncludeClosed: true,
				Limit:         1,
			})
			if err != nil {
				if beads.IsPartialResult(err) && len(items) == 0 {
					continue
				}
				return false, err
			}
			for _, item := range items {
				// Don't count the session's own self-reference bead.
				if sessionpkg.IsSessionBeadOrRepairable(item) {
					continue
				}
				return true, nil
			}
		}
	}
	return false, nil
}
