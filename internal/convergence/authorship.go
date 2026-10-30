package convergence

import "github.com/gastownhall/gascity/internal/events"

// eventTypes lists every convergence event type. The controller's convergence
// tick is their only emitter, so a worker site may author none of them.
var eventTypes = []string{
	EventCreated, EventIteration, EventTerminated, EventWaitingManual,
	EventManualApprove, EventManualIterate, EventManualStop, EventTriggerAdvance,
}

func init() {
	for _, eventType := range eventTypes {
		events.RegisterAuthorship(eventType, events.AuthorController)
	}
}
