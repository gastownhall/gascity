package events

// WorkLadderPayload is the typed payload of the work ladder's facts
// (work.nudged, work.nudge_failed, work.recycle_requested, work.released,
// work.release_unfinished, work.escalated): the seat, the work, the generation it was paced at, and
// the step's detail. Attempt is the entry's counted nudges after this one;
// Paused marks a nudge sent while the provider was degraded, which counts
// toward nothing. Reason is a failed delivery's error, an alert's reason,
// why a recycle was requested, or how a release did not finish.
type WorkLadderPayload struct {
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name,omitempty"`
	Template    string `json:"template,omitempty"`
	BeadID      string `json:"bead_id"`
	StoreRef    string `json:"store_ref,omitempty"`
	Generation  int64  `json:"generation"`
	Kind        string `json:"kind,omitempty"`
	Rung        string `json:"rung,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	Paused      bool   `json:"paused,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// IsEventPayload marks WorkLadderPayload as an events.Payload variant.
func (WorkLadderPayload) IsEventPayload() {}

func init() {
	for _, t := range []string{WorkNudged, WorkNudgeFailed, WorkRecycleRequested, WorkReleased, WorkReleaseUnfinished, WorkEscalated} {
		RegisterPayload(t, WorkLadderPayload{})
	}
}
