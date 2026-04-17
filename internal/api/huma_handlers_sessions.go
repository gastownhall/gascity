package api

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// Session handler shared helpers. Handler methods now live in
// huma_handlers_sessions_query.go, _command.go, and _stream.go.


// --- Huma error helpers for session endpoints ---
//
// These helpers emit RFC 9457 Problem Details via Huma's error constructors.
// Messages are prefixed with a short `code: ` token (e.g. "pending_interaction:
// session has a pending interaction") so callers can still string-match on
// the semantic code while reading the typed Problem Details body.

// humaResolveError maps session.ResolveSessionID errors to Huma errors.
func humaResolveError(err error) error {
	switch {
	case errors.Is(err, session.ErrAmbiguous), errors.Is(err, errConfiguredNamedSessionConflict):
		return huma.Error409Conflict("ambiguous: " + err.Error())
	case errors.Is(err, session.ErrSessionNotFound):
		return huma.Error404NotFound("not_found: " + err.Error())
	default:
		return huma.Error500InternalServerError("internal: " + err.Error())
	}
}

// humaSessionManagerError maps session manager errors to Huma errors.

func humaSessionManagerError(err error) error {
	switch {
	case errors.Is(err, session.ErrInvalidSessionName):
		return huma.Error400BadRequest("invalid: " + err.Error())
	case errors.Is(err, session.ErrSessionNameExists):
		return huma.Error409Conflict("conflict: " + err.Error())
	case errors.Is(err, session.ErrInvalidSessionAlias):
		return huma.Error400BadRequest("invalid: " + err.Error())
	case errors.Is(err, session.ErrSessionAliasExists):
		return huma.Error409Conflict("conflict: " + err.Error())
	case errors.Is(err, session.ErrInteractionUnsupported):
		return huma.Error501NotImplemented("unsupported: " + err.Error())
	case errors.Is(err, session.ErrPendingInteraction):
		return huma.Error409Conflict("pending_interaction: " + err.Error())
	case errors.Is(err, session.ErrNoPendingInteraction):
		return huma.Error409Conflict("no_pending: " + err.Error())
	case errors.Is(err, session.ErrInteractionMismatch):
		return huma.Error409Conflict("invalid_interaction: " + err.Error())
	case errors.Is(err, session.ErrSessionClosed), errors.Is(err, session.ErrResumeRequired):
		return huma.Error409Conflict("conflict: " + err.Error())
	case errors.Is(err, session.ErrNotSession):
		return huma.Error400BadRequest("invalid: " + err.Error())
	case errors.Is(err, session.ErrIllegalTransition):
		return huma.Error409Conflict("illegal_transition: " + err.Error())
	default:
		return humaStoreError(err)
	}
}

// humaStoreError maps bead store errors to Huma errors.

func humaStoreError(err error) error {
	if errors.Is(err, beads.ErrNotFound) {
		return huma.Error404NotFound("not_found: " + err.Error())
	}
	return huma.Error500InternalServerError("internal: " + err.Error())
}

// --- Session List ---

// humaHandleSessionList is the Huma-typed handler for GET /v0/sessions.
