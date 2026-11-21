package main

import (
	"context"
	"fmt"
	"io"
	"strings"

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
// observation of name, is the dead pane of info's committed named
// incarnation.
func deadPaneCrashCandidate(info session.Info, obs runtime.Liveness) bool {
	if !obs.Corpse || !isNamedSessionInfo(info) {
		return false
	}
	return session.ProjectLifecycle(session.LifecycleInputFromInfo(info)).BaseState == session.BaseStateActive
}

// recordDeadPaneCrash records session.crashed for a configured named row
// whose runtime name is observed as a remain-on-exit corpse: the pane is
// gone, so without this record the restart that follows reads the same as
// an operator's wake (#6298). The pane tail rides on the event; a failed
// peek is logged and leaves it empty, and the crash is still recorded. One
// record is made per dead object (markCorpseCrash).
func recordDeadPaneCrash(info session.Info, name string, obs runtime.Liveness, tp TemplateParams, peek func(int) (string, error), dt *drainTracker, rec events.Recorder, stderr io.Writer) {
	if obs.Alive {
		dt.clearCorpseCrash(info.ID)
		return
	}
	if !deadPaneCrashCandidate(info, obs) || !dt.markCorpseCrash(info.ID, deadObjectKey(info, obs)) {
		return
	}
	output, err := peek(rateLimitPeekLines)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: reading dead pane of %s for its crash record: %v\n", name, err) //nolint:errcheck
		output = ""
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
