package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
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
		return apierr.SessionConflict.Msg("ambiguous: " + err.Error())
	case errors.Is(err, session.ErrSessionNotFound):
		return apierr.SessionNotFound.Msg("not_found: " + err.Error())
	default:
		return apierr.Internal.Msg("internal: " + err.Error())
	}
}

// humaSessionManagerError maps session manager errors to Huma errors.

func humaSessionManagerError(err error) error {
	switch {
	case errors.Is(err, session.ErrInvalidSessionName):
		return apierr.InvalidRequest.Msg("invalid: " + err.Error())
	case errors.Is(err, session.ErrSessionNameExists):
		return apierr.SessionConflict.Msg("conflict: " + err.Error())
	case errors.Is(err, session.ErrInvalidSessionAlias):
		return apierr.InvalidRequest.Msg("invalid: " + err.Error())
	case errors.Is(err, session.ErrSessionAliasExists):
		return apierr.SessionConflict.Msg("conflict: " + err.Error())
	case errors.Is(err, session.ErrInteractionUnsupported):
		return apierr.NotImplemented.Msg("unsupported: " + err.Error())
	case errors.Is(err, session.ErrPendingInteraction):
		return apierr.SessionConflict.Msg("pending_interaction: " + err.Error())
	case errors.Is(err, session.ErrNoPendingInteraction):
		return apierr.SessionConflict.Msg("no_pending: " + err.Error())
	case errors.Is(err, session.ErrInteractionMismatch):
		return apierr.SessionConflict.Msg("invalid_interaction: " + err.Error())
	case errors.Is(err, session.ErrSessionClosed), errors.Is(err, session.ErrResumeRequired):
		return apierr.SessionConflict.Msg("conflict: " + err.Error())
	case errors.Is(err, session.ErrSessionActive):
		return apierr.SessionConflict.Msg("conflict: " + err.Error())
	case errors.Is(err, session.ErrNotSession):
		return apierr.InvalidRequest.Msg("invalid: " + err.Error())
	case errors.Is(err, session.ErrIllegalTransition):
		return apierr.SessionConflict.Msg("illegal_transition: " + err.Error())
	default:
		return humaStoreError(err)
	}
}

// humaStoreError maps bead store errors to Huma errors.

func humaStoreError(err error) error {
	if errors.Is(err, beads.ErrNotFound) {
		return apierr.SessionNotFound.Msg("not_found: " + err.Error())
	}
	return apierr.Internal.Msg("internal: " + err.Error())
}

func writeHumaStatusError(w http.ResponseWriter, err error) {
	var statusErr huma.StatusError
	if errors.As(err, &statusErr) {
		code := "error"
		msg := statusErr.Error()
		if before, after, ok := strings.Cut(msg, ": "); ok {
			code = before
			msg = after
		}
		writeError(w, statusErr.GetStatus(), code, msg)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

// --- Session List ---

// humaHandleSessionList is the Huma-typed handler for GET /v0/sessions.
