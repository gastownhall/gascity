package api

import (
	"errors"
	"net/http"

	"github.com/gastownhall/gascity/internal/beads"
)

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, beads.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}
