package session

import (
	"context"
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// StartupTurnLaunchID is the launch a startup nudge handed back by the start
// of session name binds to (nudgequeue.StartupTurnReference): the id of the
// launch the runtime reports running (runtime.ReadLaunchID, the read a claim
// judges it by), else of startToken, the instance token the start carried,
// else none, and the turn is queued as a plain submit.
func StartupTurnLaunchID(sp runtime.Provider, name, startToken string) string {
	if id, ok := runtime.ReadLaunchID(sp, name); ok {
		return id
	}
	return runtime.LaunchID(startToken)
}

// StartupHandbackItems is what a handed-back startup nudge is queued as.
// turn is the deferred submit that carries the handed-back text whole; it
// becomes the startup turn of launchID. When that text is exactly the
// composition of initialMessage (SplitInitialMessage), the one-shot message
// is split off as its own plain submit, messageID, so it never rides in a
// turn a claim may drop: it follows the turn by a nanosecond with the same
// session, epoch and deliver-after, and the turn is left out when nothing
// else is in it.
func StartupHandbackItems(turn nudgequeue.Item, launchID, initialMessage, messageID string) []nudgequeue.Item {
	turn.Reference = nudgequeue.StartupTurnReference(launchID)
	text, split := SplitInitialMessage(turn.Message, initialMessage)
	if !split {
		return []nudgequeue.Item{turn}
	}
	message := turn
	message.ID, message.Message, message.Reference = messageID, initialMessage, nil
	message.CreatedAt = turn.CreatedAt.Add(time.Nanosecond)
	if text == "" {
		return []nudgequeue.Item{message}
	}
	turn.Message = text
	return []nudgequeue.Item{turn, message}
}

// queueStartupNudgeHandbackLocked takes the startup nudge the runtime handed
// back after a start of sessName that succeeded
// (runtime.TakeStartupNudgeHandback): under program_status = "on", one its
// program held back behind a dialog through the whole startup ladder, never
// typed (design §10.2 row A20). It is queued as a user's submit for b's
// session, bound to the launch that stands (StartupTurnLaunchID, startToken
// the token the start minted), with the row's initial message split off
// (StartupHandbackItems); the queue delivers it to that launch alone, before
// the session's other items. A start takes its handback once, when it knows
// which launch stands (after the stale-key check). A start that handed
// nothing back queues nothing; a queueing failure is logged, since the start
// itself succeeded. A queued nudge is recorded on ctx's startupNudgeRecorder,
// if it carries one.
func (m *Manager) queueStartupNudgeHandbackLocked(ctx context.Context, b beads.Bead, sessName, startToken string) {
	text, ok := runtime.TakeStartupNudgeHandback(m.sp, sessName)
	if !ok {
		return
	}
	now := time.Now().UTC()
	overrides, err := ParseTemplateOverrides(b.Metadata)
	if err != nil {
		log.Printf("session: %q: reading its initial message to split from the startup nudge: %v", sessName, err)
	}
	items := StartupHandbackItems(newDeferredSubmitItem(b, text, nudgequeue.SourceSubmit, now),
		StartupTurnLaunchID(m.sp, sessName, startToken), overrides[InitialMessageOverrideKey], newDeferredSubmitID())
	queued, err := m.enqueueDeferredItemsLocked(b, sessName, items)
	if err != nil {
		log.Printf("session: %q: started, but queueing the startup nudge its program's dialog held back failed: %v", sessName, err)
	}
	if rec, ok := ctx.Value(startupNudgeRecorderKey{}).(*startupNudgeRecorder); ok && queued {
		rec.queued = true
	}
}

// dropStartupNudgeHandback takes and drops the startup nudge a start of
// sessName that ended with no launch standing may have left: no session
// takes it.
func (m *Manager) dropStartupNudgeHandback(sessName string) {
	runtime.TakeStartupNudgeHandback(m.sp, sessName)
}

// startupNudgeRecorder records that a start made on behalf of a delivery
// queued its session's startup nudge: the delivery then queues its own text
// behind it rather than type it ahead of the agent's first turn.
type startupNudgeRecorder struct {
	queued bool
}

type startupNudgeRecorderKey struct{}

// withStartupNudgeRecorder returns ctx carrying a fresh startupNudgeRecorder.
func withStartupNudgeRecorder(ctx context.Context) (context.Context, *startupNudgeRecorder) {
	rec := &startupNudgeRecorder{}
	return context.WithValue(ctx, startupNudgeRecorderKey{}, rec), rec
}
