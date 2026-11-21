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
	"github.com/gastownhall/gascity/internal/fsys"
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
	if err := ensureBeadsDir(fsys.OSFS{}, beadsDir); err != nil {
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
// for route generation. Each scope's route spells the prefix its store ACTUALLY
// mints under, read from the store itself (routeIssuePrefix); the config-derived
// prefix is only the fallback for a scope whose store cannot answer. A route
// keyed by a derived prefix shorter than the store's real one selects nothing —
// every consumer of routes.jsonl (bd's lead-prefix match, the API plane's
// residency arm, gc bd's routes arm) dash-appends — so DeriveBeadsPrefix("city")
// = "ci" against IDs like city-d1i routes nothing from anywhere (city-fr3t).
// Stores gc itself initializes already carry the derived prefix, so for them the
// read answers the same prefix and registration is unchanged.
func collectRigRoutes(cityPath string, cfg *config.City) []rigRoute {
	rigs := []rigRoute{{
		Prefix: routeIssuePrefix(cityPath, cityPath, config.EffectiveHQPrefix(cfg)),
		AbsDir: cityPath,
	}}
	for i := range cfg.Rigs {
		if strings.TrimSpace(cfg.Rigs[i].Path) == "" {
			continue
		}
		rigs = append(rigs, rigRoute{
			Prefix: routeIssuePrefix(cityPath, cfg.Rigs[i].Path, cfg.Rigs[i].EffectivePrefix()),
			AbsDir: cfg.Rigs[i].Path,
		})
	}
	return rigs
}

// routeIssuePrefix is the prefix a route for the store at scopePath spells:
// the issue prefix the bd store records in its own config table
// (rigAdoptStorePrefix — bd keeps it there and leaves .beads/config.yaml a
// commented template on dolt backends), or fallback when no store answers:
// deferred init, GC_DOLT=skip, a missing store, or a bd failure. Route
// registration is best effort and never fails the caller; a scope whose store
// is cold registers under its derived prefix exactly as it did before
// city-fr3t, and the next registration while the store is warm rewrites it
// with the real one.
//
// The read never wakes a cold store: a provider-owned scope that is not ready
// (suspended, deferred) or whose proxy retired on its idle timeout is answered
// with the fallback, because a proxied `bd config get` restarts the store's
// proxy — the wake init and the reconciler deliberately never pay for a scope
// nobody is working. Suspended-and-ready and plain (non-provider-owned)
// stores read directly; neither call dials anything that is down.
// routeScopeCold reports whether scopeRoot's provider-owned store is one the
// lifecycle deliberately keeps down — not ready (suspended, deferred), or a
// proxy retired on its idle timeout. A store read must never wake such a
// scope (a proxied `bd config get` restarts the proxy), so route registration
// answers it with the derived prefix. A variable so tests can model a cold
// scope without an ownership journal.
var routeScopeCold = func(cityPath, scopeRoot string) bool {
	entry, owned, err := providerOwnedScopeState(cityPath, scopeRoot)
	if err != nil || !owned {
		return false
	}
	return entry.State != providerScopeReady || beadsScopeIdleRetired(scopeRoot)
}

func routeIssuePrefix(cityPath, scopePath, fallback string) string {
	root := resolveStoreScopeRoot(cityPath, scopePath)
	if routeScopeCold(cityPath, root) {
		return fallback
	}
	prefix, err := rigAdoptStorePrefix(cityPath, root)
	if err != nil {
		return fallback
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return fallback
	}
	return prefix
}

// routeEntriesFor reads every prefix→path entry in dir's .beads/routes.jsonl,
// the way writeRoutesFile writes them (paths relative to dir). ok is false
// when the file is absent or unreadable; malformed lines are skipped. The
// caller owns matching and path resolution policy.
func routeEntriesFor(dir string) ([]routeEntry, bool) {
	data, err := os.ReadFile(filepath.Join(dir, ".beads", "routes.jsonl"))
	if err != nil {
		return nil, false
	}
	var entries []routeEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry routeEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		entry.Prefix = strings.TrimSpace(entry.Prefix)
		entry.Path = strings.TrimSpace(entry.Path)
		if entry.Prefix == "" || entry.Path == "" {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, true
}
