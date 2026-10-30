package api

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// GET /v0/city/{cityName}/session/{id}/turn: a read-only projection of the
// worker's TurnHandle (engdocs/design/x11-turn-identity-coverage.md §5.2). It
// reports where the session's turns are read from, how far that answer can
// be trusted, and what one read of them saw now. It aims nothing at a turn: a
// turn-scoped stop or interrupt is the worker's TI-2b2 slice, not this read.

// sessionTurnReadBound bounds one turn read's runtime probes (the protocol
// history read and the idle snapshots), so a wedged runtime answers
// unavailable instead of holding the request. The session's reloads, whose
// runtime overlay takes no context either, and a transcript's file reads are
// not bounded.
const sessionTurnReadBound = 10 * time.Second

// The observation values of a session turn read.
const (
	sessionTurnObserved    = "observed"
	sessionTurnUnavailable = "unavailable"
	sessionTurnUnsupported = "unsupported"
)

// sessionTurnResponse is the body of GET /v0/city/{cityName}/session/{id}/turn.
type sessionTurnResponse struct {
	SessionID    string                  `json:"session_id" doc:"The resolved session id."`
	Source       string                  `json:"source" enum:"protocol,transcript,none" doc:"Where the session's turns are read from: protocol (the runtime observes turns itself), transcript (the provider's transcript of the session's own conversation, on this host), or none (the session's turns cannot be identified here). Fixed for the session's generation."`
	Capabilities sessionTurnCapabilities `json:"capabilities" doc:"How far the source's answer can be trusted."`
	Observation  string                  `json:"observation" enum:"observed,unavailable,unsupported" doc:"What this read saw. observed: current and recent are this read's answer, and an absent current means no turn is running. unavailable: the source exists but could not be read now (a transcript not written yet, no conversation key yet, a runtime that did not answer); the session's turns are unknown, not idle, so retry. unsupported: the session's turns cannot be identified (no source, or a transcript that records turns without ids)."`
	Detail       string                  `json:"detail,omitempty" doc:"Why the observation is not observed."`
	ObservedAt   *time.Time              `json:"observed_at,omitempty" doc:"When the read was made, on the server's clock. Present only when observed."`
	Current      *sessionTurnCurrent     `json:"current,omitempty" doc:"The turn in progress. Absent when no turn is running, or when the observation is not observed."`
	Recent       []sessionTurnEnded      `json:"recent,omitempty" doc:"Turns that ended, newest first. A transcript source reports at most 8 from a bounded window, so a turn that ends between two reads can fall out of it unless capabilities.lossless is true."`
}

// sessionTurnCapabilities is what the worker can tell about a session's turns.
type sessionTurnCapabilities struct {
	Start     string `json:"start" enum:"none,boundary,recorded" doc:"How turn starts are known: recorded means every start carries its turn id."`
	End       string `json:"end" enum:"none,boundary,inferred,recorded" doc:"How turn ends are known: recorded means every end carries its id and how it ended; inferred means some ends are inferred from the session going idle."`
	Fence     string `json:"fence" enum:"none,checked,atomic" doc:"How an interrupt could be confined to a named turn: atomic means the runtime refuses to interrupt a turn that is not current; checked means the turn is re-read just before an untargeted interrupt, so a turn starting in between can still be hit; none means an interrupt cannot be aimed at a turn."`
	Link      string `json:"link" enum:"none,correlated,exact" doc:"How a delivery is tied to the turn it started: exact means the runtime names it; correlated means it is matched by observation."`
	Lossless  bool   `json:"lossless" doc:"Every turn since the last read can be recovered from the next one."`
	Interrupt string `json:"interrupt" enum:"keystroke,protocol,unsupported" doc:"How the runtime interrupts the session's turn. unsupported forces fence none."`
}

// sessionTurnCurrent is the turn in progress.
type sessionTurnCurrent struct {
	ID        string     `json:"id" doc:"The turn's id, as the source records it. Opaque; compare for equality only."`
	StartedAt *time.Time `json:"started_at,omitempty" doc:"When the turn started, at the source's precision; absent when unknown."`
}

// sessionTurnEnded is a turn the source saw end.
type sessionTurnEnded struct {
	ID        string     `json:"id" doc:"The turn's id, as the source records it. Opaque; compare for equality only."`
	StartedAt *time.Time `json:"started_at,omitempty" doc:"When the turn started, at the source's precision; absent when unknown."`
	EndedAt   *time.Time `json:"ended_at,omitempty" doc:"When the turn ended: the source's time, or, for an inferred end, the server's when the read inferred it; absent when unknown. Each read infers an end anew, so an inferred end's time can differ between reads, and a read that cannot confirm the session idle reports that turn current again."`
	// Status carries no enum keyword: a "failed" member would collide with
	// SlingChildOutcomeOutcome's in the generated Go client and rename its
	// constants.
	Status   string `json:"status,omitempty" doc:"How the turn ended: completed, cancelled or failed (the runtime's spelling); absent when the source closed it only because a newer turn started."` //nolint:misspell // matches the runtime's ACP wire spelling
	Inferred bool   `json:"inferred" doc:"The end was inferred from the session going idle, not recorded."`
}

// humaHandleSessionTurn is the Huma-typed handler for
// GET /v0/city/{cityName}/session/{id}/turn.
func (s *Server) humaHandleSessionTurn(ctx context.Context, input *SessionIDInput) (*IndexOutput[sessionTurnResponse], error) {
	index := s.latestIndex()
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	id, err := s.resolveSessionIDWithConfig(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}
	handle, err := s.workerHandleForSession(store.Store, id)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, sessionTurnReadBound)
	defer cancel()

	caps, err := handle.TurnCapabilities(ctx)
	if err != nil {
		return nil, sessionTurnReadError(id, err)
	}
	resp := sessionTurnResponse{
		SessionID: id,
		Source:    string(caps.Source),
		Capabilities: sessionTurnCapabilities{
			Start:     string(caps.Start),
			End:       string(caps.End),
			Fence:     string(caps.Fence),
			Link:      string(caps.Link),
			Lossless:  caps.Lossless,
			Interrupt: string(caps.Interrupt),
		},
	}
	if caps.Source == worker.TurnSourceNone {
		resp.Observation = sessionTurnUnsupported
		resp.Detail = "the session has no turn source: its runtime does not observe turns, and no transcript of its own conversation can be read for turns on this host"
		return &IndexOutput[sessionTurnResponse]{Index: index, Body: resp}, nil
	}

	hist, err := handle.CurrentTurn(ctx)
	switch {
	case err == nil:
		resp.Observation = sessionTurnObserved
		resp.observe(hist)
	case worker.TurnSessionGone(err):
		return nil, sessionTurnReadError(id, err)
	case errors.Is(err, runtime.ErrSessionNotFound) && !errors.Is(err, runtime.ErrRuntimeUnavailable):
		// No runtime is bound to the open session (DES-T3I D2): no turn runs,
		// as /stop reads it.
		resp.Observation = sessionTurnObserved
		resp.observe(runtime.TurnHistory{ObservedAt: time.Now()})
	case errors.Is(err, runtime.ErrTurnIdentityUnsupported):
		resp.Observation = sessionTurnUnsupported
		resp.Detail = "the session's turn source records turns without ids"
	case errors.Is(err, runtime.ErrRuntimeUnavailable), errors.Is(err, runtime.ErrSessionInitializing),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// The worker's error can name the transcript's path or the
		// conversation's key, which the API does not expose, so the detail
		// is fixed and the cause is logged.
		log.Printf("api: the turns of session %s could not be read now: %v", id, err)
		resp.Observation = sessionTurnUnavailable
		resp.Detail = "the session's turn source could not be read now (a transcript not written yet, no conversation key yet, or a runtime that did not answer); its turns are unknown, not idle"
	default:
		log.Printf("api: reading the turns of session %s: %v", id, err)
		return nil, apierr.Internal.Msg("reading the turns of session " + id + " failed")
	}
	return &IndexOutput[sessionTurnResponse]{Index: index, Body: resp}, nil
}

// sessionTurnReadError maps a failed capability composition or turn read: a
// session whose row is gone, closed or not a session is 404, anything else as
// session errors map.
func sessionTurnReadError(id string, err error) error {
	if worker.TurnSessionGone(err) {
		return apierr.SessionNotFound.Msg("not_found: session " + id + " is closed or gone")
	}
	return humaSessionManagerError(err)
}

// observe fills the response's turns from one read of the session's history.
func (r *sessionTurnResponse) observe(hist runtime.TurnHistory) {
	if !hist.ObservedAt.IsZero() {
		at := hist.ObservedAt.UTC()
		r.ObservedAt = &at
	}
	if hist.Current.Active() {
		r.Current = &sessionTurnCurrent{ID: hist.Current.TurnID, StartedAt: optionalTurnTime(hist.CurrentStart)}
	}
	for _, t := range hist.Recent {
		r.Recent = append(r.Recent, sessionTurnEnded{
			ID:        t.TurnID,
			StartedAt: optionalTurnTime(t.StartedAt),
			EndedAt:   optionalTurnTime(t.EndedAt),
			Status:    string(t.Status),
			Inferred:  t.Inferred,
		})
	}
}

func optionalTurnTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}
