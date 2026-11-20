package opjournal

// LaneStage names a crash point of a pass: a point between two durable
// writes where a host process may die and leave the store as it stands.
type LaneStage string

// Lane stages OnStage reports.
const (
	// LaneStageTaken is right after a Take's guarded batch landed: the batch
	// is executing under this lane's lease and its effect has not started.
	LaneStageTaken LaneStage = "take"
	// LaneStageEffected is right after the effect returned, link wait
	// included: the runtime may hold the effect and its Done is not written.
	LaneStageEffected LaneStage = "effect"
)

// OnStage makes the lane call fn at each crash point of its passes: after a
// Take (content or control) and after its effect. It exists for process
// failpoints that gather crash evidence (a host that kills itself at a
// stage); fn's return ends nothing, and a pass goes on after it returns. A
// nil fn clears it. Set it before the lane's first pass: passes read it
// without a lock. fn must be safe for concurrent use, since one lane serves
// many conversations at once.
func (l *ConversationLane) OnStage(fn func(LaneStage)) {
	if fn == nil {
		l.hooks.at = nil
		return
	}
	l.hooks.at = func(s laneStage) error {
		switch s {
		case stageAfterTake:
			fn(LaneStageTaken)
		case stageAfterEffect:
			fn(LaneStageEffected)
		}
		return nil
	}
}
