package api

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/processenv"
)

// componentVersions holds the versions of the external binaries the
// supervisor drives. A field is empty when the corresponding binary is
// unavailable or its probe failed; callers surface empty as an absent,
// omitempty wire field rather than a guessed value.
type componentVersions struct {
	Dolt  string
	Beads string
}

// resolveComponentVersions returns the dolt engine and bd CLI versions the
// supervisor drives, probing each binary at most once per process. Binary
// versions are immutable for the life of the process that launched them, so a
// single probe is both cheaper than re-probing on the hot status path and
// semantically correct: the running supervisor keeps driving the binaries it
// resolved until it restarts. The actual subprocess execution lives in
// internal/beads (bd/dolt are confined there by architectural rule); this
// layer only caches the result and logs probe failures server-side.
func (s *Server) resolveComponentVersions() componentVersions {
	s.componentVersionsOnce.Do(func() {
		if s.componentVersionsProbe != nil {
			s.componentVersionsValue = s.componentVersionsProbe()
			return
		}
		bdBin := ""
		if s.state != nil {
			bdBin = cityPinnedBDBin(s.state.Config())
		}
		probeBD := func() (string, error) { return beads.ProbeBDVersion(bdBin) }
		s.componentVersionsValue = probeComponentVersions(beads.ProbeDoltVersion, probeBD)
	})
	return s.componentVersionsValue
}

// cityPinnedBDBin returns the city's pinned bd executable, read from the same
// sources in the same order that cmd/gc's workspacePinnedBdBinaryOptional
// (cmd/gc/bd_env.go) uses for real subprocess work, so the status surface
// reports the version of the bd actually driving this city rather than
// whichever "bd" happens to be first on the supervisor process's PATH.
//
// internal/api cannot import cmd/gc (package main), so this mirrors the
// algorithm rather than calling it: expand workspace.env (so a city.toml
// BD_BIN = "$BD_BIN"-style reference resolves the same way), prefer an
// explicit absolute workspace.env BD_BIN, else search an explicit
// workspace.env PATH for "bd", else fall back to an inherited ambient BD_BIN.
// A pin that is SET but invalid (relative, or not an executable found by
// LookPath) is a misconfiguration, not silence: it is logged as a warning
// and the probe falls through to the next source rather than disappearing.
//
// That fall-through deliberately diverges from cmd/gc on one input, an
// invalid workspace.env BD_BIN: cmd/gc returns a configuration error for it
// and runs no other bd in its place, while a status probe reports what it can
// rather than fail. For such a city the version reported here belongs to a bd
// the city is not running, and the logged warning is what flags it.
//
// Returns "" (meaning "resolve bd on PATH") when cfg is nil or no source
// resolves.
func cityPinnedBDBin(cfg *config.City) string {
	if cfg == nil {
		return ""
	}
	env := expandWorkspaceEnv(cfg.Workspace.Env)
	if raw := strings.TrimSpace(env["BD_BIN"]); raw != "" {
		if !filepath.IsAbs(raw) {
			log.Printf("status: ignoring workspace.env BD_BIN %q: must be an absolute executable path", raw)
		} else if resolved, err := exec.LookPath(raw); err == nil {
			return resolved
		} else {
			log.Printf("status: ignoring workspace.env BD_BIN %q: not executable: %v", raw, err)
		}
	}
	if pathValue, configured := env["PATH"]; configured {
		for _, dir := range filepath.SplitList(pathValue) {
			dir = strings.TrimSpace(dir)
			if !filepath.IsAbs(dir) {
				continue
			}
			if resolved, err := exec.LookPath(filepath.Join(dir, "bd")); err == nil && filepath.IsAbs(resolved) {
				return resolved
			}
		}
		// An explicit workspace PATH is authoritative, same as
		// workspacePinnedBdBinaryOptional: do not fall through to the
		// ambient BD_BIN below when that PATH does not contain bd.
		return ""
	}
	if raw := strings.TrimSpace(os.Getenv("BD_BIN")); raw != "" {
		if !filepath.IsAbs(raw) {
			log.Printf("status: ignoring ambient BD_BIN %q: not an absolute path", raw)
			return ""
		}
		resolved, err := exec.LookPath(raw)
		if err != nil {
			log.Printf("status: ignoring ambient BD_BIN %q: not executable: %v", raw, err)
			return ""
		}
		return resolved
	}
	return ""
}

// expandWorkspaceEnv expands $VAR references in a city.toml workspace.env
// block against the process environment, mirroring cmd/gc's expandEnvMap
// (cmd/gc/cmd_start.go) so a BD_BIN = "$BD_BIN" style pin resolves the same
// value here as it does for the supervisor's own subprocess environment.
func expandWorkspaceEnv(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = processenv.ExpandSessionEnvValue(v)
	}
	return out
}

// probeComponentVersions resolves both binary versions. A failed probe leaves
// that field empty and logs the cause server-side so a missing version is
// diagnosable rather than mystifying (consistent with the "don't swallow
// errors" rule). probeDolt/probeBD are injectable for tests.
func probeComponentVersions(probeDolt, probeBD func() (string, error)) componentVersions {
	var cv componentVersions
	if v, err := probeDolt(); err != nil {
		log.Printf("status: dolt version probe failed: %v", err)
	} else {
		cv.Dolt = v
	}
	if v, err := probeBD(); err != nil {
		log.Printf("status: bd version probe failed: %v", err)
	} else {
		cv.Beads = v
	}
	return cv
}
