package api

import (
	"errors"
	"net/http"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// turnInterruptUnsupportedCode is the error code for
// session.ErrTurnInterruptUnsupported on every wire path: the 501 from /stop
// and the async request.failed result of an interrupt_now submit.
const turnInterruptUnsupportedCode = "turn_interrupt_unsupported"

// sessionSubmitFailureCode is the request.failed error code for an async
// submit that failed with err.
func sessionSubmitFailureCode(err error) string {
	if errors.Is(err, session.ErrTurnInterruptUnsupported) {
		return turnInterruptUnsupportedCode
	}
	return "submit_failed"
}

func writeSessionManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrInvalidSessionName):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, session.ErrSessionNameExists):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, session.ErrInvalidSessionAlias):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, session.ErrSessionAliasExists):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, session.ErrInvalidSessionTitle):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, session.ErrInteractionUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", err.Error())
	case errors.Is(err, session.ErrTurnInterruptUnsupported):
		writeError(w, http.StatusNotImplemented, turnInterruptUnsupportedCode, err.Error())
	case errors.Is(err, session.ErrPendingInteraction):
		writeError(w, http.StatusConflict, "pending_interaction", err.Error())
	case errors.Is(err, session.ErrNoPendingInteraction):
		writeError(w, http.StatusConflict, "no_pending", err.Error())
	case errors.Is(err, session.ErrInteractionMismatch):
		writeError(w, http.StatusConflict, "invalid_interaction", err.Error())
	case errors.Is(err, session.ErrConversationRetained):
		// Not retryable as is: the kept conversation's resume failed, and the
		// message names the explicit reset that starts the session fresh.
		writeError(w, http.StatusConflict, "conversation_retained", err.Error())
	case errors.As(err, new(*worker.LiveSubagentsError)):
		writeError(w, http.StatusConflict, "live_subagents", err.Error())
	case errors.Is(err, session.ErrSessionClosed), errors.Is(err, session.ErrResumeRequired), errors.Is(err, session.ErrSessionKillPending):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, session.ErrSessionInactive):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, session.ErrSessionStarting):
		// Retryable: another holder (usually the controller) has the runtime lease.
		writeError(w, http.StatusConflict, "session_starting", err.Error())
	case errors.Is(err, session.ErrSessionStopping):
		// Retryable: the controller is stopping the runtime (drain-ack).
		writeError(w, http.StatusConflict, "session_stopping", err.Error())
	case errors.Is(err, session.ErrNotSession):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		// Retryable: the runtime could not be observed, so nothing was done.
		writeError(w, http.StatusServiceUnavailable, "runtime_unavailable", err.Error())
	case errors.Is(err, runtime.ErrNudgeRejected):
		// The runtime refused the turn in its current state: nothing was delivered.
		writeError(w, http.StatusConflict, "nudge_rejected", err.Error())
	default:
		writeStoreError(w, err)
	}
}
