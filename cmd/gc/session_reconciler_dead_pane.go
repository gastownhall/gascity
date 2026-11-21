package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// deadPaneCrashReason is the session.crashed reason of a configured named
// row whose runtime the reconciler observed as a remain-on-exit corpse.
const deadPaneCrashReason = "dead pane"

// deadPaneCrashCandidate reports whether obs, a completed liveness
// observation, is the dead pane of info's committed named incarnation with no
// other handler owning the row: no pending create claim, requested restart,
// tracked drain or kill fence.
func deadPaneCrashCandidate(info session.Info, obs runtime.Liveness, dt *drainTracker, now time.Time) bool {
	if !obs.Corpse || !isNamedSessionInfo(info) {
		return false
	}
	if info.PendingCreateClaim || strings.TrimSpace(info.RestartRequested) == "true" ||
		(dt != nil && dt.get(info.ID) != nil) || session.IsKillPendingInfo(info, now) {
		return false
	}
	return session.ProjectLifecycle(session.LifecycleInputFromInfo(info)).BaseState == session.BaseStateActive
}

// deadPaneOwnedElsewhere reports whether runtime name says its exit is
// another handler's: a drain ack, a requested restart, or the token of
// another incarnation. A read error is returned, and the caller records
// nothing.
func deadPaneOwnedElsewhere(info session.Info, name string, sp runtime.Provider) (bool, error) {
	for _, key := range []string{"GC_DRAIN_ACK", "GC_RESTART_REQUESTED"} {
		v, err := sp.GetMeta(name, key)
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", key, err)
		}
		if strings.TrimSpace(v) != "" {
			return true, nil
		}
	}
	foreign, err := runtimeOfAnotherIncarnation(info, name, sp)
	if err != nil {
		return false, fmt.Errorf("reading GC_INSTANCE_TOKEN: %w", err)
	}
	return foreign, nil
}

// recordDeadPaneCrash records session.crashed for a configured named row
// whose runtime name is observed as a remain-on-exit corpse: the pane is
// gone, so without this record the restart that follows reads the same as
// an operator's wake (#6298). info is the row as the pass found it, before
// any heal. The pane tail rides on the event; a failed peek is logged and
// leaves it empty, and the crash is still recorded. A pane showing a
// provider rate-limit screen is not a crash. One classification is made per
// dead object (markCorpseCrash). A failed runtime metadata read leaves the
// exit's owner unknown, so nothing is recorded and the read error is
// logged; the heal and restart later in the same pass usually leave no
// later tick to see that object, so its crash is dropped. A row whose
// template the pass did not resolve (a preserved named row whose provider
// is missing, say) is named by its own identity and template.
func recordDeadPaneCrash(info session.Info, name string, obs runtime.Liveness, tp TemplateParams, sp runtime.Provider, peek func(int) (string, error), dt *drainTracker, now time.Time, rec events.Recorder, stderr io.Writer) {
	if obs.Alive {
		dt.clearCorpseCrash(info.ID)
		return
	}
	if !deadPaneCrashCandidate(info, obs, dt, now) {
		return
	}
	owned, err := deadPaneOwnedElsewhere(info, name, sp)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: classifying dead pane of %s: %v\n", name, err) //nolint:errcheck
		return
	}
	if owned || !dt.markCorpseCrash(info.ID, deadObjectKey(info, obs)) {
		return
	}
	output, err := peek(rateLimitPeekLines)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: reading dead pane of %s for its crash record: %v\n", name, err) //nolint:errcheck
		output = ""
	}
	if runtime.ContainsProviderRateLimitScreen(output) {
		return
	}
	if strings.TrimSpace(tp.TemplateName) == "" {
		tp.TemplateName, tp.InstanceName = strings.TrimSpace(info.Template), namedSessionIdentityInfo(info)
	}
	rec.Record(sessionCrashedEvent(info.ID, tp, output, deadPaneCrashReason))
	telemetry.RecordAgentCrash(context.Background(), tp.DisplayName(), output)
}

// deadObjectKey names the dead runtime object obs observed: the provider's
// object id pinned by its creation time, or, from a provider without object
// ids, the incarnation token the row records.
func deadObjectKey(info session.Info, obs runtime.Liveness) string {
	if obs.ObjectID != "" {
		return obs.ObjectID + "@" + obs.ObjectCreated
	}
	return "token:" + strings.TrimSpace(info.InstanceToken)
}

// sessionCrashedEvent is the legacy reconciler's session.crashed for session
// bead id of tp, carrying the truncated pane output and the crash reason.
func sessionCrashedEvent(id string, tp TemplateParams, output, reason string) events.Event {
	return events.Event{
		Type:    events.SessionCrashed,
		Actor:   "gc",
		Subject: tp.DisplayName(),
		Message: truncateCrashPaneOutput(output, crashEventPaneOutputMaxLines),
		Payload: api.SessionLifecyclePayloadJSON(id, tp.TemplateName, reason),
	}
}
