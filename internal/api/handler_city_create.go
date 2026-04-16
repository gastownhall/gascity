package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

type cityCreateRequest struct {
	Dir              string `json:"dir"`
	Provider         string `json:"provider"`
	BootstrapProfile string `json:"bootstrap_profile,omitempty"`
}

type cityCreateResponse struct {
	OK   bool   `json:"ok"`
	Path string `json:"path"`
}

// handleCityCreate handles POST /v0/city — creates a new city by shelling
// out to `gc init`. This is stateless (no city context needed) so it can be
// called from both the per-city Server and the SupervisorMux. Fix 3b will
// migrate this onto Huma; until then it emits RFC 9457 Problem Details so
// the wire format matches Huma's native errors.
func handleCityCreate(w http.ResponseWriter, r *http.Request) {
	var body cityCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblemDetails(w, http.StatusBadRequest, problemDetailsTitle(http.StatusBadRequest), "invalid: "+err.Error())
		return
	}

	if body.Dir == "" {
		writeProblemDetails(w, http.StatusBadRequest, problemDetailsTitle(http.StatusBadRequest), "invalid: dir is required")
		return
	}
	if body.Provider == "" {
		writeProblemDetails(w, http.StatusBadRequest, problemDetailsTitle(http.StatusBadRequest), "invalid: provider is required")
		return
	}

	// Validate provider against builtins
	if _, ok := config.BuiltinProviders()[body.Provider]; !ok {
		writeProblemDetails(w, http.StatusBadRequest, problemDetailsTitle(http.StatusBadRequest),
			fmt.Sprintf("invalid: unknown provider %q", body.Provider))
		return
	}

	// Validate bootstrap profile if present
	if body.BootstrapProfile != "" {
		switch body.BootstrapProfile {
		case "k8s-cell", "kubernetes", "kubernetes-cell", "single-host-compat":
			// valid
		default:
			writeProblemDetails(w, http.StatusBadRequest, problemDetailsTitle(http.StatusBadRequest),
				fmt.Sprintf("invalid: unknown bootstrap profile %q", body.BootstrapProfile))
			return
		}
	}

	// Resolve absolute path. Relative dirs are resolved against $HOME,
	// not CWD, because the supervisor's CWD may already be the city
	// directory — resolving "gc" relative to /home/user/gc would
	// produce /home/user/gc/gc (double nesting).
	dir := body.Dir
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			writeProblemDetails(w, http.StatusInternalServerError, problemDetailsTitle(http.StatusInternalServerError),
				fmt.Sprintf("internal: resolving home dir: %v", err))
			return
		}
		dir = filepath.Join(home, dir)
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeProblemDetails(w, http.StatusInternalServerError, problemDetailsTitle(http.StatusInternalServerError),
			fmt.Sprintf("internal: creating directory: %v", err))
		return
	}

	// Shell out to `gc init` — the current binary is the gc binary.
	gcBin, err := os.Executable()
	if err != nil {
		writeProblemDetails(w, http.StatusInternalServerError, problemDetailsTitle(http.StatusInternalServerError),
			fmt.Sprintf("internal: finding gc binary: %v", err))
		return
	}

	args := []string{"init", dir, "--provider", body.Provider}
	if body.BootstrapProfile != "" {
		args = append(args, "--bootstrap-profile", body.BootstrapProfile)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, gcBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		// Check for "already initialized" which is a 409 conflict
		if bytes.Contains(stderr.Bytes(), []byte("already initialized")) {
			writeProblemDetails(w, http.StatusConflict, problemDetailsTitle(http.StatusConflict), "conflict: city already initialized at "+dir)
			return
		}
		writeProblemDetails(w, http.StatusInternalServerError, problemDetailsTitle(http.StatusInternalServerError), "init_failed: "+msg)
		return
	}

	writeTypedJSON(w, http.StatusOK, cityCreateResponse{OK: true, Path: dir})
}
