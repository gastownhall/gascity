package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ownLaunchCollisionOutcome classifies a start that had to launch its own
// runtime (preparedStart.ownLaunchRequired) and found the name held by a live
// runtime (#6417). Only a runtime carrying this attempt's instance token is
// this start's, and the start converges on it as any collision does. Any other
// holder never got this start's prompt: the start defers, committing nothing
// and stopping nothing, with the collision error annotated for the lifecycle
// log (unattributed). A holder stamped with this session's ID under another
// token is another incarnation of the session (a launch by a non-controller
// start); every other holder, an unreadable one included, is unattributed.
func ownLaunchCollisionOutcome(err error, info sessionpkg.Info, identity pendingCreateIdentity) (outcome TraceOutcomeCode, unattributed error) {
	switch {
	case identity.token == runtimeTokenMatch:
		return TraceOutcomeSessionExists, nil
	case identity.id == runtimeTokenMatch && identity.token == runtimeTokenMismatch:
		return TraceOutcomeDeferred, liveIncarnationStartError(err, info, identity)
	default:
		return TraceOutcomeDeferred, unattributedStartError(err, identity)
	}
}

// uncommittedStartMark is what the reconciler remembers of one live runtime
// its row never committed a start for: the incarnation it was seen under, when
// it was first seen (the age basis when the provider reports no creation
// time), and whether an unattributed holder was already reported.
type uncommittedStartMark struct {
	generation, token string
	firstSeen         time.Time
	warned            bool
}

// uncommittedStartSeen returns the mark for the row's current incarnation,
// starting one at now when the row has none or moved to another incarnation.
func (dt *drainTracker) uncommittedStartSeen(info sessionpkg.Info, now time.Time) uncommittedStartMark {
	if dt == nil {
		return uncommittedStartMark{firstSeen: now}
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if dt.uncommitted == nil {
		dt.uncommitted = make(map[string]uncommittedStartMark)
	}
	mark, ok := dt.uncommitted[info.ID]
	if !ok || mark.generation != info.Generation || mark.token != info.InstanceToken {
		mark = uncommittedStartMark{generation: info.Generation, token: info.InstanceToken, firstSeen: now}
		dt.uncommitted[info.ID] = mark
	}
	return mark
}

// markUncommittedStartWarned reports whether this is the first unattributed
// holder report for the row's current mark.
func (dt *drainTracker) markUncommittedStartWarned(id string) bool {
	if dt == nil {
		return true
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	mark, ok := dt.uncommitted[id]
	if !ok || mark.warned {
		return !ok
	}
	mark.warned = true
	dt.uncommitted[id] = mark
	return true
}

// clearUncommittedStart forgets the row's mark.
func (dt *drainTracker) clearUncommittedStart(id string) {
	if dt == nil {
		return
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	delete(dt.uncommitted, id)
}

// uncommittedStartPending reports whether info is a row that owes its runtime
// a controller start: no start committed, no create in flight, no suspend,
// sleep intent or live hold, and a prompt the start must deliver.
func uncommittedStartPending(info sessionpkg.Info, tp TemplateParams, now time.Time) bool {
	return strings.TrimSpace(info.StartedConfigHash) == "" &&
		!info.PendingCreateClaim &&
		strings.TrimSpace(info.SleepIntent) == "" &&
		!sessionpkg.DispositionOfInfo(info).KeepsSlot() &&
		!metadataTimeInFuture(info.HeldUntil, now) &&
		strings.TrimSpace(tp.Prompt) != ""
}

// uncommittedStartAgeBasis is when the live runtime's wait began: the latest
// of the runtime's creation (the mark's first sighting when the provider
// reports none) and the row's last wake or reset commit.
func uncommittedStartAgeBasis(info sessionpkg.Info, created, firstSeen time.Time) time.Time {
	basis := firstSeen
	if !created.IsZero() {
		basis = created
	}
	for _, raw := range []string{info.LastWokeAt, info.ResetCommittedAt} {
		if at, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil && at.After(basis) {
			basis = at
		}
	}
	return basis
}

// uncommittedRuntimeCreated is the live runtime's creation time as the
// provider reports it, or zero when it reports none or cannot be read.
func uncommittedRuntimeCreated(ctx context.Context, cityPath string, sp runtime.Provider, name string, processNames []string, now time.Time) time.Time {
	ctx, cancel := context.WithTimeout(ctx, startFailureProbeBudget)
	defer cancel()
	live, err := observeSessionBounded(ctx, cityPath, name, func() (runtime.Liveness, error) {
		return runtime.ObserveLivenessSince(sp, name, processNames, now)
	})
	if err != nil || strings.TrimSpace(live.ObjectCreated) == "" {
		return time.Time{}
	}
	created, err := strconv.ParseInt(live.ObjectCreated, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(created, 0)
}

// uncommittedStartActs are the store-backed acts evictUncommittedLiveStartIfDue
// makes on the row's runtime: the attach probe (an error reads as attached) and
// the stop, made against the facts that decided it (legacyAct.Stop).
type uncommittedStartActs struct {
	attached func() (bool, error)
	stop     func(sessionpkg.Decided) error
}

// evictUncommittedLiveStartIfDue stops a live runtime its row never committed
// a start for, once it has waited past the startup timeout (#6417). Such a
// runtime was launched by a start other than the controller's (a Manager start
// racing a restart handoff) and never got the prompt; the controller's start
// refuses to commit it (ownLaunchRequired), so without this the row waits on
// it forever. Only a detached runtime stamped with the row's session ID is
// stopped, under the row's current incarnation (FactsLegacyUncommittedEvict);
// a holder of unknown attribution is reported once and left alone. It reports
// whether it stopped the runtime.
func evictUncommittedLiveStartIfDue(
	cityPath string,
	sp runtime.Provider,
	acts uncommittedStartActs,
	info sessionpkg.Info,
	tp TemplateParams,
	name string,
	startupTimeout time.Duration,
	now time.Time,
	dt *drainTracker,
	stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) bool {
	if sp == nil || startupTimeout <= 0 || !uncommittedStartPending(info, tp, now) {
		dt.clearUncommittedStart(info.ID)
		return false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	mark := dt.uncommittedStartSeen(info, now)
	created := uncommittedRuntimeCreated(context.Background(), cityPath, sp, name, tp.Hints.ProcessNames, now)
	elapsed := now.Sub(uncommittedStartAgeBasis(info, created, mark.firstSeen))
	if elapsed <= startupTimeout {
		return false
	}
	if attached, err := acts.attached(); attached || err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), startFailureProbeBudget)
	identity := readPendingCreateIdentityBounded(ctx, cityPath, info, name, sp)
	cancel()
	switch identity.id {
	case runtimeTokenMatch:
	case runtimeTokenGone:
		return false
	default:
		if dt.markUncommittedStartWarned(info.ID) {
			fmt.Fprintf(stderr, "session reconciler: %s has a live runtime with no committed start, not attributed to session %s; leaving it (#6417): %v\n", //nolint:errcheck
				name, info.ID, unattributedStartError(runtime.ErrSessionExists, identity))
		}
		return false
	}
	evict := sessionpkg.Decide(info, sessionpkg.FactsLegacyUncommittedEvict)
	if err := acts.stop(evict); err != nil {
		if !uncommittedEvictDeferred(err) {
			fmt.Fprintf(stderr, "session reconciler: evicting %s's uncommitted runtime: %v\n", name, err) //nolint:errcheck
		}
		return false
	}
	elapsedSeconds := int(elapsed / time.Second)
	fmt.Fprintf(stderr, "session reconciler: stopped %s: its runtime never committed a start (elapsed_s=%d bead_id=%s); the next start launches it with its prompt\n", //nolint:errcheck
		name, elapsedSeconds, info.ID)
	if trace != nil {
		trace.RecordDecision(
			TraceSiteReconcilerUncommittedStart,
			TraceReasonUncommittedStart,
			TraceOutcomeStop,
			tp.TemplateName,
			name,
			map[string]any{
				"bead_id":           info.ID,
				"elapsed_s":         elapsedSeconds,
				"startup_timeout_s": int(startupTimeout / time.Second),
			},
		)
	}
	dt.clearUncommittedStart(info.ID)
	return true
}

// uncommittedEvictDeferred reports whether a failed eviction is a deferral a
// later tick decides again: the runtime already gone, the row moved past the
// facts that decided the kill, or the runtime lease held by a start or stop.
func uncommittedEvictDeferred(err error) bool {
	return runtime.IsSessionGone(err) ||
		errors.Is(err, sessionpkg.ErrKillPremiseMoved) ||
		errors.Is(err, sessionpkg.ErrRuntimeLeaseBusy) ||
		errors.Is(err, sessionpkg.ErrSessionStarting)
}
