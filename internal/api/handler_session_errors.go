package api

import (
	"errors"
	"net/http"

	"github.com/gastownhall/gascity/internal/session"
)

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
	case errors.Is(err, session.ErrInteractionUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", err.Error())
	case errors.Is(err, session.ErrPendingInteraction):
		writeError(w, http.StatusConflict, "pending_interaction", err.Error())
	case errors.Is(err, session.ErrNoPendingInteraction):
		writeError(w, http.StatusConflict, "no_pending", err.Error())
	case errors.Is(err, session.ErrInteractionMismatch):
		writeError(w, http.StatusConflict, "invalid_interaction", err.Error())
	case errors.Is(err, session.ErrSessionClosed), errors.Is(err, session.ErrResumeRequired):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, session.ErrNotSession):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	default:
		writeStoreError(w, err)
	}
}
