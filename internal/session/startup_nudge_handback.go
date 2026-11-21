package session

import (
	"context"
	"errors"
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
// else none, and the turn is queued as a plain submit. A runtime that keeps
// no session metadata (runtime.ErrMetaUnsupported) binds none either: no
// claim could ever read its launch, so a bound turn would only be held, and
// the session's sends with it, until it expired.
func StartupTurnLaunchID(sp runtime.Provider, name, startToken string) string {
	if id, ok := runtime.ReadLaunchID(sp, name); ok {
		return id
	}
	if sp != nil && name != "" {
		if _, err := sp.GetMeta(name, runtime.InstanceTokenEnv); errors.Is(err, runtime.ErrMetaUnsupported) {
			log.Printf("session: %q: the runtime keeps no session metadata, so its startup turn is queued unbound to its launch: %v", name, err)
			return ""
		}
	}
	return runtime.LaunchID(startToken)
}

// StartupHandbackItems is what a handed-back startup nudge is queued as.
// turn is the deferred submit that carries the handed-back text whole; it
// becomes the startup turn of launchID, held until notBefore when that is
// later than its deliver-after (a runtime that cannot tell when its agent
// takes input gives its first turn an earliest send time). When that text is
// exactly the
// composition of initialMessage (SplitInitialMessage), the one-shot message
// is split off as its own plain submit, messageID, so it never rides in a
// turn a claim may drop: it follows the turn by a nanosecond with the same
// session, epoch and deliver-after, and the turn is left out when nothing
// else is in it.
func StartupHandbackItems(turn nudgequeue.Item, notBefore time.Time, launchID, initialMessage, messageID string) []nudgequeue.Item {
	turn.Reference = nudgequeue.StartupTurnReference(launchID)
	if notBefore.After(turn.DeliverAfter) {
		turn.DeliverAfter = notBefore.UTC()
	}
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

// startupTurn is the first turn a Start that succeeded handed back
// (runtime.TakeStartupNudgeHandback): under program_status = "on", one its
// program held back behind a dialog through the whole startup ladder, never
// typed (design §10.2 row A20); on an exec pack's un-welded launch, every
// first turn, which that Start never types. It is taken right after that
// Start, while
// the slot is still that launch's, and bound then to the launch it came from
// (StartupTurnLaunchID). ok is false when the Start handed nothing back.
type startupTurn struct {
	text, launchID string
	notBefore      time.Time
	ok             bool
}

// takeStartupTurn takes the first turn the Start of sessName that just
// succeeded, under startToken, handed back, bound to its launch.
func (m *Manager) takeStartupTurn(sessName, startToken string) startupTurn {
	nudge, ok := runtime.TakeStartupNudgeHandback(m.sp, sessName)
	if !ok {
		return startupTurn{}
	}
	return startupTurn{text: nudge.Text, notBefore: nudge.NotBefore, launchID: StartupTurnLaunchID(m.sp, sessName, startToken), ok: true}
}

// settleStartupTurnLocked ends a start of sessName: turn, the first turn the
// last Start that succeeded handed back, is queued (queueStartupTurnLocked)
// whatever the start returns, since its launch may stand, and a claim drops
// it as stale only once another launch runs; whatever a Start that failed
// left in the runtime's slot is dropped, since no launch of its stands.
func (m *Manager) settleStartupTurnLocked(ctx context.Context, b beads.Bead, sessName string, turn startupTurn) {
	runtime.TakeStartupNudgeHandback(m.sp, sessName)
	m.queueStartupTurnLocked(ctx, b, sessName, turn)
}

// queueStartupTurnLocked queues turn as a user's submit for b's session,
// bound to its launch, with the row's initial message split off
// (StartupHandbackItems); the queue delivers it to that launch alone, before
// the session's other items. A turn not ok queues nothing; a queueing
// failure is logged, since the start itself succeeded. A queued turn is
// recorded on ctx's startupNudgeRecorder, if it carries one.
func (m *Manager) queueStartupTurnLocked(ctx context.Context, b beads.Bead, sessName string, turn startupTurn) {
	if !turn.ok {
		return
	}
	now := time.Now().UTC()
	overrides, err := ParseTemplateOverrides(b.Metadata)
	if err != nil {
		log.Printf("session: %q: reading its initial message to split from the startup nudge: %v", sessName, err)
	}
	items := StartupHandbackItems(newDeferredSubmitItem(b, turn.text, nudgequeue.SourceSubmit, now),
		turn.notBefore, turn.launchID, overrides[InitialMessageOverrideKey], newDeferredSubmitID())
	queued, err := m.enqueueDeferredItemsLocked(b, sessName, items)
	if err != nil {
		log.Printf("session: %q: started, but queueing its handed-back startup nudge failed: %v", sessName, err)
	}
	if rec, ok := ctx.Value(startupNudgeRecorderKey{}).(*startupNudgeRecorder); ok && queued {
		rec.queued = true
	}
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
