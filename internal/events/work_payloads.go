package events

import "time"

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
	// Provider is the seat's provider (its row's provider metadata).
	Provider   string `json:"provider,omitempty"`
	BeadID     string `json:"bead_id"`
	StoreRef   string `json:"store_ref,omitempty"`
	Generation int64  `json:"generation"`
	Kind       string `json:"kind,omitempty"`
	Rung       string `json:"rung,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	Paused     bool   `json:"paused,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// IsEventPayload marks WorkLadderPayload as an events.Payload variant.
func (WorkLadderPayload) IsEventPayload() {}

// The moves of a seat's rate-limit episode (WorkRateLimitedPayload.Transition).
const (
	WorkRateLimitedOpened    = "opened"    // refused turns answered a nudge the seat took
	WorkRateLimitedRestarted = "restarted" // the episode's one fresh restart was requested
	WorkRateLimitedHeld      = "held"      // a hold keeps the ladder from restarting the seat (Cause says which)
	WorkRateLimitedExhausted = "exhausted" // no fresh restart will help this episode (Cause says why)
	WorkRateLimitedClosed    = "closed"    // the episode ended (Cause says how)
)

// Why an episode moved (WorkRateLimitedPayload.Cause).
const (
	WorkRateLimitedCauseAttached            = "attached"             // held: a person is attached to the seat
	WorkRateLimitedCauseAwaitingHuman       = "awaiting_human"       // held: work of the seat awaits a person
	WorkRateLimitedCauseBlocked             = "blocked"              // held: another seat's fresh restart on the provider, a pending notice or release, or work the ladder could not read or list, past its bound
	WorkRateLimitedCauseFreshRefused        = "fresh_refused"        // exhausted: the fresh conversation was refused too
	WorkRateLimitedCauseRestartLost         = "restart_lost"         // exhausted: refused after the restart, in the conversation it left
	WorkRateLimitedCauseAgain               = "again"                // exhausted: refused again soon after the seat's last fresh restart
	WorkRateLimitedCauseQuota               = "quota"                // exhausted: the refusal is a spent usage quota
	WorkRateLimitedCauseServed              = "served"               // closed: the provider served the seat
	WorkRateLimitedCauseStale               = "stale"                // closed: no answered refusal for an hour
	WorkRateLimitedCauseConversationChanged = "conversation_changed" // closed: someone else changed the seat's conversation
)

// WorkRateLimitedPayload is the typed payload of work.rate_limited: one move
// of a seat's rate-limit episode (internal/workladder): its provider refusing
// its turns with a rate limit, as the agent's own transcript classifies them.
// It reports facts for a pack's provider policy and for people; the SDK draws
// no conclusion about the provider from it.
//
// A pack holds provider P's seats by writing {"provider": P, "status":
// "unhealthy", "probed_at": <now>} into .gc/cache/provider-health.json
// (atomically, refreshed at least every 60 s while the hold stands): the
// ladder then restarts none of P's seats and paces them paused, and the
// reconciler starts none. The hold lifts when the entry expires or reads
// healthy. A pack may read ErrorText (the provider's own words, such as a
// pool's scope and cooldown); the SDK never does.
type WorkRateLimitedPayload struct {
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name,omitempty"`
	Template    string `json:"template,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Generation  int64  `json:"generation"`
	// Conversation is the seat's continuation epoch at this move.
	Conversation int64 `json:"conversation"`
	// Transition is the move: one of the WorkRateLimited* constants.
	Transition string `json:"transition"`
	// Cause is why a held, exhausted or closed move happened: one of the
	// WorkRateLimitedCause* constants.
	Cause string `json:"cause,omitempty"`
	// Since is when the refusal that opened the episode ended.
	Since time.Time `json:"since"`
	// Refused is how many of the seat's newest turns in a row its provider
	// refused, at this move's reading, and RefusedAt when the newest ended.
	Refused   int        `json:"refused,omitempty"`
	RefusedAt *time.Time `json:"refused_at,omitempty"`
	// FreshAt is the episode's fresh restart, once requested.
	FreshAt *time.Time `json:"fresh_at,omitempty"`
	// Alerted reports that this move raised the episode's one alert (a
	// work.escalated beside it).
	Alerted bool `json:"alerted,omitempty"`
	// ErrorCode is the agent CLI's own class for the newest refusal (e.g.
	// rate_limit), HTTPStatus its HTTP status, and ErrorText its text,
	// bounded, with control characters as spaces.
	ErrorCode  string `json:"error_code,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	ErrorText  string `json:"error_text,omitempty"`
	// Claims are the seat's started claims.
	Claims []string `json:"claims,omitempty"`
}

// IsEventPayload marks WorkRateLimitedPayload as an events.Payload variant.
func (WorkRateLimitedPayload) IsEventPayload() {}

func init() {
	for _, t := range []string{WorkNudged, WorkNudgeFailed, WorkRecycleRequested, WorkReleased, WorkReleaseUnfinished, WorkEscalated} {
		RegisterPayload(t, WorkLadderPayload{})
	}
	RegisterPayload(WorkRateLimited, WorkRateLimitedPayload{})
}
