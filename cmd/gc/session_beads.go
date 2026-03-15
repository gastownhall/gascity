package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionBeadLabel is the unified label for all session beads. Phase 2
// consolidates the old "gc:agent_session" and session manager's "gc:session"
// into a single label. New beads are created with this label; legacy beads
// are still readable via loadSessionBeads.
//
// Migration: this is a one-way upgrade. Stores written by Phase 2+ binaries
// use "gc:session"; older binaries only query "gc:agent_session" and will
// miss new beads. Mixed-version operation (old daemon + new CLI or vice
// versa) is not supported — upgrade all binaries together.
const sessionBeadLabel = "gc:session"

// legacySessionBeadLabel is the old label used by syncSessionBeads before
// Phase 2 label unification. Retained for reading legacy beads only.
// Read paths query both labels; write paths use sessionBeadLabel exclusively.
const legacySessionBeadLabel = "gc:agent_session"

// sessionBeadType is the bead type for session beads.
const sessionBeadType = "session"

// legacyStateMap maps legacy bead states to the session-first state model.
// Legacy beads (type="agent_session") used different state names.
var legacyStateMap = map[string]string{
	"stopped":  "closed",
	"orphaned": "suspended",
	// "active" and "suspended" pass through unchanged.
}

// loadSessionBeads queries both gc:agent_session and gc:session labeled beads,
// deduplicates by session_name, and returns the unified list. New-type beads
// take precedence when both exist for the same session_name.
//
// Legacy beads are normalized: their state is mapped via legacyStateMap so
// downstream reconciliation sees a uniform state vocabulary. Legacy beads
// also count against pool occupancy (creating + active + suspended + quarantined).
func loadSessionBeads(store beads.Store) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}

	// Query both labels (unified + legacy) for migration compatibility.
	legacy, err := store.ListByLabel(legacySessionBeadLabel, 0)
	if err != nil {
		return nil, fmt.Errorf("listing legacy session beads: %w", err)
	}
	newer, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		return nil, fmt.Errorf("listing session beads: %w", err)
	}

	// Index new-type beads by session_name for dedup.
	newByName := make(map[string]beads.Bead, len(newer))
	for _, b := range newer {
		if b.Status == "closed" {
			continue
		}
		if sn := b.Metadata["session_name"]; sn != "" {
			newByName[sn] = b
		}
	}

	// Build result: start with new-type beads.
	result := make([]beads.Bead, 0, len(newer)+len(legacy))
	for _, b := range newer {
		if b.Status == "closed" {
			continue
		}
		result = append(result, b)
	}

	// Add legacy beads, skipping those with a new-type counterpart.
	for _, b := range legacy {
		if b.Status == "closed" {
			continue
		}
		sn := b.Metadata["session_name"]
		if sn == "" {
			continue
		}
		if _, hasNew := newByName[sn]; hasNew {
			continue // new-type bead takes precedence
		}
		// Normalize legacy state. Clone metadata map first to avoid
		// mutating the store's cached copy (maps are reference types).
		if mapped, ok := legacyStateMap[b.Metadata["state"]]; ok {
			newMeta := make(map[string]string, len(b.Metadata))
			for k, v := range b.Metadata {
				newMeta[k] = v
			}
			newMeta["state"] = mapped
			b.Metadata = newMeta
			// Skip terminal beads after mapping (e.g., stopped → closed).
			if mapped == "closed" {
				continue
			}
		}
		result = append(result, b)
	}

	return result, nil
}

// syncSessionBeads ensures every desired session has a corresponding session
// bead. Accepts desiredState (sessionName → TemplateParams) instead of
// map[string]TemplateParams, and uses runtime.Provider for liveness checks.
//
// configuredNames is the set of ALL configured agent session names (including
// suspended agents). Beads for names not in this set are marked "orphaned".
// Beads for names in configuredNames but not in desiredState are marked
// "suspended" (the agent exists in config but isn't currently runnable).
//
// When skipClose is true, orphan/suspended beads are NOT closed. This is
// used when the bead-driven reconciler is active — it handles drain/stop
// for orphan sessions before closing their beads.
//
// Returns a map of session_name → bead_id for all open session beads after
// sync. Callers that don't need the index can ignore the return value.
func syncSessionBeads(
	store beads.Store,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	configuredNames map[string]bool,
	_ *config.City,
	clk clock.Clock,
	stderr io.Writer,
	skipClose bool,
) map[string]string {
	if store == nil {
		return nil
	}

	// Load existing session beads from both unified and legacy labels.
	// This ensures migration: legacy gc:agent_session beads are still found
	// and updated (or superseded by unified gc:session beads).
	unified, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		fmt.Fprintf(stderr, "session beads: listing existing: %v\n", err) //nolint:errcheck
		return nil
	}
	legacy, _ := store.ListByLabel(legacySessionBeadLabel, 0)

	// Merge: unified beads take precedence over legacy by session_name.
	existing := make([]beads.Bead, 0, len(unified)+len(legacy))
	existing = append(existing, unified...)
	unifiedNames := make(map[string]bool, len(unified))
	for _, b := range unified {
		if sn := b.Metadata["session_name"]; sn != "" {
			unifiedNames[sn] = true
		}
	}
	for _, b := range legacy {
		if sn := b.Metadata["session_name"]; sn != "" && !unifiedNames[sn] {
			existing = append(existing, b)
		}
	}

	// Index by session_name for O(1) lookup. Skip closed beads — a closed
	// bead is a completed lifecycle record, not a live session. If an agent
	// restarts after its bead was closed, we create a fresh bead.
	bySessionName := make(map[string]beads.Bead, len(existing))
	for _, b := range existing {
		if b.Status == "closed" {
			continue
		}
		if sn := b.Metadata["session_name"]; sn != "" {
			bySessionName[sn] = b
		}
	}

	// Track open bead IDs for the returned index.
	openIndex := make(map[string]string, len(desiredState))

	now := clk.Now().UTC()

	for sn, tp := range desiredState {
		agentCfg := templateParamsToConfig(tp)
		coreHash := runtime.CoreFingerprint(agentCfg)
		liveHash := runtime.LiveFingerprint(agentCfg)

		// Use provider for liveness check (includes zombie detection).
		state := "stopped"
		if sp.IsRunning(sn) && sp.ProcessAlive(sn, tp.Hints.ProcessNames) {
			state = "active"
		}

		agentName := tp.TemplateName
		// For pool instances, use the qualified instance name as the agent_name.
		if slot := resolvePoolSlot(tp.InstanceName, tp.TemplateName); slot > 0 {
			agentName = tp.InstanceName
		}

		b, exists := bySessionName[sn]
		if !exists {
			// Create a new session bead.
			meta := map[string]string{
				"session_name":   sn,
				"agent_name":     agentName,
				"config_hash":    coreHash,
				"live_hash":      liveHash,
				"generation":     "1",
				"instance_token": generateToken(),
				"state":          state,
				"synced_at":      now.Format("2006-01-02T15:04:05Z07:00"),
			}
			// Generate session_key for providers that support --session-id.
			// Without this, transcript lookup falls back to workdir-based
			// matching which is ambiguous when multiple sessions share a dir.
			if tp.ResolvedProvider != nil && tp.ResolvedProvider.SessionIDFlag != "" {
				if key, err := session.GenerateSessionKey(); err == nil {
					meta["session_key"] = key
				}
			}
			if tp.WorkDir != "" {
				meta["work_dir"] = tp.WorkDir
			}
			// Store the qualified template name so the API can derive the
			// rig from it (e.g., "tower-of-hanoi/polecat" not just "polecat").
			if tp.RigName != "" && !strings.Contains(tp.TemplateName, "/") {
				meta["template"] = tp.RigName + "/" + tp.TemplateName
			} else {
				meta["template"] = tp.TemplateName
			}
			if slot := resolvePoolSlot(tp.InstanceName, tp.TemplateName); slot > 0 {
				meta["pool_slot"] = strconv.Itoa(slot)
			}
			// Store command and resume fields so gc session attach can
			// reconstruct the resume command from bead metadata alone.
			if tp.Command != "" {
				meta["command"] = tp.Command
			}
			if tp.ResolvedProvider != nil {
				if tp.ResolvedProvider.Name != "" {
					meta["provider"] = tp.ResolvedProvider.Name
				}
				if tp.ResolvedProvider.ResumeFlag != "" {
					meta["resume_flag"] = tp.ResolvedProvider.ResumeFlag
				}
				if tp.ResolvedProvider.ResumeStyle != "" {
					meta["resume_style"] = tp.ResolvedProvider.ResumeStyle
				}
				if tp.ResolvedProvider.ResumeCommand != "" {
					meta["resume_command"] = tp.ResolvedProvider.ResumeCommand
				}
			}
			newBead, createErr := store.Create(beads.Bead{
				Title:    agentName,
				Type:     sessionBeadType,
				Labels:   []string{sessionBeadLabel, "agent:" + agentName},
				Metadata: meta,
			})
			if createErr != nil {
				fmt.Fprintf(stderr, "session beads: creating bead for %s: %v\n", agentName, createErr) //nolint:errcheck
			} else {
				openIndex[sn] = newBead.ID
			}
			continue
		}

		// Record existing open bead in index.
		openIndex[sn] = b.ID

		// Backfill template and pool_slot metadata for beads created
		// before Phase 2f. Also upgrade unqualified template names to
		// qualified form so the API can derive the rig.
		qualifiedTemplate := tp.TemplateName
		if tp.RigName != "" && !strings.Contains(tp.TemplateName, "/") {
			qualifiedTemplate = tp.RigName + "/" + tp.TemplateName
		}
		if b.Metadata["template"] == "" || (tp.RigName != "" && !strings.Contains(b.Metadata["template"], "/")) {
			if setMeta(store, b.ID, "template", qualifiedTemplate, stderr) == nil {
				b.Metadata["template"] = qualifiedTemplate
			}
		}
		if b.Metadata["pool_slot"] == "" {
			if slot := resolvePoolSlot(tp.InstanceName, tp.TemplateName); slot > 0 {
				if setMeta(store, b.ID, "pool_slot", strconv.Itoa(slot), stderr) == nil {
					b.Metadata["pool_slot"] = strconv.Itoa(slot)
				}
			}
		}
		if b.Metadata["work_dir"] == "" && tp.WorkDir != "" {
			if setMeta(store, b.ID, "work_dir", tp.WorkDir, stderr) == nil {
				b.Metadata["work_dir"] = tp.WorkDir
			}
		}
		// Backfill session_key for beads created before this fix.
		if b.Metadata["session_key"] == "" && tp.ResolvedProvider != nil && tp.ResolvedProvider.SessionIDFlag != "" {
			if key, err := session.GenerateSessionKey(); err == nil {
				if setMeta(store, b.ID, "session_key", key, stderr) == nil {
					b.Metadata["session_key"] = key
				}
			}
		}
		// Backfill command and resume fields for beads created before
		// these fields were persisted. Required for gc session attach.
		if b.Metadata["command"] == "" && tp.Command != "" {
			setMeta(store, b.ID, "command", tp.Command, stderr) //nolint:errcheck
		}
		if tp.ResolvedProvider != nil {
			if b.Metadata["provider"] == "" && tp.ResolvedProvider.Name != "" {
				setMeta(store, b.ID, "provider", tp.ResolvedProvider.Name, stderr) //nolint:errcheck
			}
			if b.Metadata["resume_flag"] == "" && tp.ResolvedProvider.ResumeFlag != "" {
				setMeta(store, b.ID, "resume_flag", tp.ResolvedProvider.ResumeFlag, stderr) //nolint:errcheck
			}
			if b.Metadata["resume_style"] == "" && tp.ResolvedProvider.ResumeStyle != "" {
				setMeta(store, b.ID, "resume_style", tp.ResolvedProvider.ResumeStyle, stderr) //nolint:errcheck
			}
			if b.Metadata["resume_command"] == "" && tp.ResolvedProvider.ResumeCommand != "" {
				setMeta(store, b.ID, "resume_command", tp.ResolvedProvider.ResumeCommand, stderr) //nolint:errcheck
			}
		}

		// Update existing bead metadata.
		// config_hash and live_hash are NOT updated here — they record
		// what config the session was STARTED with. The reconciler detects
		// drift by comparing bead config_hash against desired config.
		changed := false

		if b.Metadata["state"] != state {
			if setMeta(store, b.ID, "state", state, stderr) == nil {
				changed = true
			}
		}

		if b.Metadata["close_reason"] != "" || b.Metadata["closed_at"] != "" {
			if setMeta(store, b.ID, "close_reason", "", stderr) == nil &&
				setMeta(store, b.ID, "closed_at", "", stderr) == nil {
				changed = true
			}
		}

		if changed {
			setMeta(store, b.ID, "synced_at", now.Format("2006-01-02T15:04:05Z07:00"), stderr) //nolint:errcheck
		}
	}

	// Classify and close beads with no matching desired entry.
	if !skipClose {
		for _, b := range existing {
			sn := b.Metadata["session_name"]
			if sn == "" {
				continue
			}
			if _, hasDesired := desiredState[sn]; hasDesired {
				continue
			}
			if b.Status == "closed" {
				continue
			}
			if configuredNames[sn] {
				closeBead(store, b.ID, "suspended", now, stderr)
			} else {
				closeBead(store, b.ID, "orphaned", now, stderr)
			}
		}
	}

	return openIndex
}

// configuredSessionNames builds the set of ALL configured agent session names
// from the config, including suspended agents. Used to distinguish "orphaned"
// (removed from config) from "suspended" (still in config, not runnable).
//
// For non-pool agents, a bead-derived session name is used (falling back to
// the legacy SessionNameFor). For pool agents, the base template name is
// included — individual pool instances are NOT in this set, so scale-down
// excess instances are correctly classified as "orphaned".
//
// Additionally, for non-pool agents, all open session beads matching the
// template are included. This ensures forked singleton sessions (created
// via "gc session new" from a singleton template) are classified as
// "configured" rather than "orphaned" if they leave the desired set.
func configuredSessionNames(cfg *config.City, cityName string, store beads.Store) map[string]bool {
	st := cfg.Workspace.SessionTemplate
	names := make(map[string]bool, len(cfg.Agents))

	// Build a set of non-pool template names for fork detection.
	singletonTemplates := make(map[string]bool)
	for _, a := range cfg.Agents {
		if a.IsPool() {
			// Pool agents: use legacy SessionNameFor for the tmux-sanitized
			// base template name (e.g., "my-rig/worker" → "my-rig--worker").
			// We intentionally skip bead lookup because findSessionNameByTemplate
			// would return a pool INSTANCE name (e.g., "worker-1"), which would
			// prevent scale-down orphan detection.
			names[agent.SessionNameFor(cityName, a.QualifiedName(), st)] = true
		} else {
			names[lookupSessionNameOrLegacy(store, cityName, a.QualifiedName(), st)] = true
			singletonTemplates[a.QualifiedName()] = true
		}
	}

	// Include fork session names: open session beads whose template matches
	// a non-pool agent but whose session_name was not already added above.
	// This prevents forked singletons from being classified as "orphaned".
	if store != nil && len(singletonTemplates) > 0 {
		all, err := store.ListByLabel(sessionBeadLabel, 0)
		if err == nil {
			for _, b := range all {
				if b.Status == "closed" {
					continue
				}
				sn := b.Metadata["session_name"]
				if sn == "" || names[sn] {
					continue
				}
				template := b.Metadata["template"]
				if template == "" {
					template = b.Metadata["common_name"]
				}
				if singletonTemplates[template] {
					names[sn] = true
				}
			}
		}
	}

	return names
}

// setMeta wraps store.SetMetadata with error logging. Returns the error
// so callers can abort dependent writes (e.g., skip config_hash on failure).
func setMeta(store beads.Store, id, key, value string, stderr io.Writer) error {
	if err := store.SetMetadata(id, key, value); err != nil {
		fmt.Fprintf(stderr, "session beads: setting %s on %s: %v\n", key, id, err) //nolint:errcheck
		return err
	}
	return nil
}

// closeBead sets final metadata on a session bead and closes it.
// This completes the bead's lifecycle record. The close_reason distinguishes
// why the bead was closed (e.g., "orphaned", "suspended").
//
// Follows the commit-signal pattern: metadata is written first, and Close
// is only called if all writes succeed. If any write fails, the bead stays
// open so the next tick retries the entire sequence.
func closeBead(store beads.Store, id, reason string, now time.Time, stderr io.Writer) {
	ts := now.Format("2006-01-02T15:04:05Z07:00")
	if setMeta(store, id, "state", reason, stderr) != nil {
		return
	}
	if setMeta(store, id, "close_reason", reason, stderr) != nil {
		return
	}
	if setMeta(store, id, "closed_at", ts, stderr) != nil {
		return
	}
	if setMeta(store, id, "synced_at", ts, stderr) != nil {
		return
	}
	if err := store.Close(id); err != nil {
		fmt.Fprintf(stderr, "session beads: closing %s: %v\n", id, err) //nolint:errcheck
	}
}

// resolveAgentTemplate returns the config agent template name for a given
// agent name. For non-pool agents, this is the agent's QualifiedName.
// For pool instances like "worker-3", this is the template "worker".
func resolveAgentTemplate(agentName string, cfg *config.City) string {
	if cfg == nil {
		return agentName
	}
	// Direct match: non-pool or singleton pool agent.
	for _, a := range cfg.Agents {
		if a.QualifiedName() == agentName {
			return a.QualifiedName()
		}
	}
	// Pool instance: name matches "{template}-{slot}".
	for _, a := range cfg.Agents {
		qn := a.QualifiedName()
		if a.IsPool() && strings.HasPrefix(agentName, qn+"-") {
			suffix := agentName[len(qn)+1:]
			if _, err := strconv.Atoi(suffix); err == nil {
				return qn
			}
		}
	}
	return agentName // fallback: treat agent name as template
}

// resolvePoolSlot extracts the pool slot number from a pool instance name.
// Returns 0 for non-pool agents or if template doesn't match.
func resolvePoolSlot(agentName, template string) int {
	if !strings.HasPrefix(agentName, template+"-") {
		return 0
	}
	suffix := agentName[len(template)+1:]
	slot, _ := strconv.Atoi(suffix)
	return slot
}

// generateToken returns a cryptographically random hex token.
// Panics on crypto/rand failure (standard Go pattern — indicates broken system).
func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("session beads: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
