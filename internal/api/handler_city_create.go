package api

import (
	"bytes"
	"context"
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
// called from both the per-city Server and the SupervisorMux.
func handleCityCreate(w http.ResponseWriter, r *http.Request) {
	var body cityCreateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	if body.Dir == "" {
		writeError(w, http.StatusBadRequest, "invalid", "dir is required")
		return
	}
	if body.Provider == "" {
		writeError(w, http.StatusBadRequest, "invalid", "provider is required")
		return
	}

	// Validate provider against builtins
	if _, ok := config.BuiltinProviders()[body.Provider]; !ok {
		writeError(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("unknown provider %q", body.Provider))
		return
	}

	// Validate bootstrap profile if present
	if body.BootstrapProfile != "" {
		switch body.BootstrapProfile {
		case "k8s-cell", "kubernetes", "kubernetes-cell", "single-host-compat":
			// valid
		default:
			writeError(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("unknown bootstrap profile %q", body.BootstrapProfile))
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
			writeError(w, http.StatusInternalServerError, "internal",
				fmt.Sprintf("resolving home dir: %v", err))
			return
		}
		dir = filepath.Join(home, dir)
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			fmt.Sprintf("creating directory: %v", err))
		return
	}

	// Shell out to `gc init` — the current binary is the gc binary.
	gcBin, err := os.Executable()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			fmt.Sprintf("finding gc binary: %v", err))
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
			writeError(w, http.StatusConflict, "conflict", "city already initialized at "+dir)
			return
		}
		writeError(w, http.StatusInternalServerError, "init_failed", msg)
		return
	}

	writeJSON(w, http.StatusOK, cityCreateResponse{OK: true, Path: dir})
}
