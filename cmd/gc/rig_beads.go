package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// rigRoute pairs a bead prefix with the absolute directory it lives in.
type rigRoute struct {
	Prefix string
	AbsDir string
}

// routeEntry is a single line in routes.jsonl — maps a prefix to a relative path.
type routeEntry struct {
	Prefix string `json:"prefix"`
	Path   string `json:"path"`
}

// generateRoutesFor computes the route entries for a single rig, given all
// known rigs. Each route is a relative path from `from` to every rig
// (including itself as "."). Returns an error if any relative path cannot
// be computed.
func generateRoutesFor(from rigRoute, all []rigRoute) ([]routeEntry, error) {
	routes := make([]routeEntry, 0, len(all))
	for _, to := range all {
		rel, err := filepath.Rel(from.AbsDir, to.AbsDir)
		if err != nil {
			return nil, fmt.Errorf("computing relative path from %q to %q: %w",
				from.AbsDir, to.AbsDir, err)
		}
		routes = append(routes, routeEntry{Prefix: to.Prefix, Path: rel})
	}
	return routes, nil
}

// writeAllRoutes generates and writes routes.jsonl for every rig. Each rig
// gets a routes.jsonl in its .beads/ directory mapping all known prefixes
// to relative paths.
func writeAllRoutes(rigs []rigRoute) error {
	for _, rig := range rigs {
		routes, err := generateRoutesFor(rig, rigs)
		if err != nil {
			return fmt.Errorf("generating routes for %q: %w", rig.Prefix, err)
		}
		if err := writeRoutesFile(rig.AbsDir, routes); err != nil {
			return fmt.Errorf("writing routes for %q: %w", rig.Prefix, err)
		}
	}
	return nil
}

// writeRoutesFile atomically writes routes.jsonl to <dir>/.beads/routes.jsonl.
// Uses temp file + rename for crash safety per CLAUDE.md conventions.
func writeRoutesFile(dir string, routes []routeEntry) error {
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		return fmt.Errorf("creating .beads dir: %w", err)
	}

	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, r := range routes {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("encoding route %q: %w", r.Prefix, err)
		}
	}

	target := filepath.Join(beadsDir, "routes.jsonl")
	// Use PID + timestamp for uniqueness so concurrent gc processes don't
	// clobber each other's temp files.
	suffix := strconv.Itoa(os.Getpid()) + "." + strconv.FormatInt(time.Now().UnixNano(), 36)
	tmp := target + ".tmp." + suffix
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return err
	}
	return nil
}

// collectRigRoutes builds the list of all rig routes (HQ + configured rigs)
// for route generation. Uses EffectivePrefix for consistent prefix resolution.
func collectRigRoutes(cityPath string, cfg *config.City) []rigRoute {
	hqPrefix := config.EffectiveHQPrefix(cfg)

	rigs := []rigRoute{{Prefix: hqPrefix, AbsDir: cityPath}}
	for i := range cfg.Rigs {
		rigs = append(rigs, rigRoute{
			Prefix: cfg.Rigs[i].EffectivePrefix(),
			AbsDir: cfg.Rigs[i].Path,
		})
	}
	return rigs
}
