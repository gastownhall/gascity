package main

import (
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

// sessionBeadLabel is the label for all session beads.
const sessionBeadLabel = "gc:session"

// sessionBeadType is the bead type for session beads.
const sessionBeadType = "session"

// loadSessionBeads returns all open session beads from the store.
func loadSessionBeads(store beads.Store) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		return nil, fmt.Errorf("listing session beads: %w", err)
	}
	var result []beads.Bead
	for _, b := range all {
		if b.Status == "closed" {
			continue
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
	cityPath string,
	store beads.Store,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	configuredNames map[string]bool,
	cfg *config.City,
	clk clock.Clock,
	stderr io.Writer,
	skipClose bool,
) map[string]string {
	if store == nil {
		return nil
	}

	existing, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		fmt.Fprintf(stderr, "session beads: listing existing: %v\n", err) //nolint:errcheck
		return nil
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
		managedAlias := strings.TrimSpace(tp.Alias)

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
				"session_name":       sn,
				"agent_name":         agentName,
				"config_hash":        coreHash,
				"live_hash":          liveHash,
				"generation":         strconv.Itoa(session.DefaultGeneration),
				"continuation_epoch": strconv.Itoa(session.DefaultContinuationEpoch),
				"instance_token":     session.NewInstanceToken(),
				"state":              state,
				"synced_at":          now.Format("2006-01-02T15:04:05Z07:00"),
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
			if tp.WakeMode != "" && tp.WakeMode != "resume" {
				meta["wake_mode"] = tp.WakeMode
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
			createBead := func() (beads.Bead, error) {
				return store.Create(beads.Bead{
					Title:    agentName,
					Type:     sessionBeadType,
					Labels:   []string{sessionBeadLabel, "agent:" + agentName},
					Metadata: meta,
				})
			}
			var (
				newBead   beads.Bead
				createErr error
				created   bool
			)
			if managedAlias != "" {
				lockErr := session.WithCitySessionAliasLock(cityPath, managedAlias, func() error {
					if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, managedAlias, "", managedAlias); err != nil {
						fmt.Fprintf(stderr, "session beads: alias %q for %s unavailable: %v\n", managedAlias, agentName, err) //nolint:errcheck
					} else {
						meta["alias"] = managedAlias
					}
					newBead, createErr = createBead()
					created = true
					return nil
				})
				if lockErr != nil {
					fmt.Fprintf(stderr, "session beads: locking alias %q for %s: %v\n", managedAlias, agentName, lockErr) //nolint:errcheck
				}
			}
			if !created {
				newBead, createErr = createBead()
			}
			if createErr != nil {
				fmt.Fprintf(stderr, "session beads: creating bead for %s: %v\n", agentName, createErr) //nolint:errcheck
			} else {
				openIndex[sn] = newBead.ID
				if liveAlias := strings.TrimSpace(meta["alias"]); liveAlias != "" && state == "active" {
					if err := session.SyncRuntimeAlias(sp, sn, liveAlias); err != nil {
						fmt.Fprintf(stderr, "session beads: syncing runtime alias %q for %s: %v\n", liveAlias, agentName, err) //nolint:errcheck
					}
				}
			}
			continue
		}

		// Record existing open bead in index.
		openIndex[sn] = b.ID

		// Backfill/update metadata in a single batch. On Dolt-backed stores,
		// per-key writes are expensive enough to stall unrelated reconciler
		// work during city startup.
		batch := map[string]string{}
		queueMeta := func(key, value string) {
			batch[key] = value
		}

		// Backfill template and pool_slot metadata for beads created
		// before Phase 2f. Also upgrade unqualified template names to
		// qualified form so the API can derive the rig.
		qualifiedTemplate := tp.TemplateName
		if tp.RigName != "" && !strings.Contains(tp.TemplateName, "/") {
			qualifiedTemplate = tp.RigName + "/" + tp.TemplateName
		}
		if b.Metadata["template"] == "" || (tp.RigName != "" && !strings.Contains(b.Metadata["template"], "/")) {
			queueMeta("template", qualifiedTemplate)
		}
		if b.Metadata["pool_slot"] == "" {
			if slot := resolvePoolSlot(tp.InstanceName, tp.TemplateName); slot > 0 {
				queueMeta("pool_slot", strconv.Itoa(slot))
			}
		}
		if b.Metadata["work_dir"] == "" && tp.WorkDir != "" {
			queueMeta("work_dir", tp.WorkDir)
		}
		needsAliasSync := b.Metadata["alias"] != managedAlias
		if b.Metadata["wake_mode"] != tp.WakeMode {
			queueMeta("wake_mode", tp.WakeMode)
		}
		// Backfill session_key for beads created before this fix.
		if b.Metadata["session_key"] == "" && tp.ResolvedProvider != nil && tp.ResolvedProvider.SessionIDFlag != "" {
			if key, err := session.GenerateSessionKey(); err == nil {
				queueMeta("session_key", key)
			}
		}
		if b.Metadata["continuation_epoch"] == "" {
			queueMeta("continuation_epoch", strconv.Itoa(session.DefaultContinuationEpoch))
		}
		// Backfill command and resume fields for beads created before
		// these fields were persisted. Required for gc session attach.
		if b.Metadata["command"] == "" && tp.Command != "" {
			queueMeta("command", tp.Command)
		}
		if tp.ResolvedProvider != nil {
			if b.Metadata["provider"] == "" && tp.ResolvedProvider.Name != "" {
				queueMeta("provider", tp.ResolvedProvider.Name)
			}
			if b.Metadata["resume_flag"] == "" && tp.ResolvedProvider.ResumeFlag != "" {
				queueMeta("resume_flag", tp.ResolvedProvider.ResumeFlag)
			}
			if b.Metadata["resume_style"] == "" && tp.ResolvedProvider.ResumeStyle != "" {
				queueMeta("resume_style", tp.ResolvedProvider.ResumeStyle)
			}
			if b.Metadata["resume_command"] == "" && tp.ResolvedProvider.ResumeCommand != "" {
				queueMeta("resume_command", tp.ResolvedProvider.ResumeCommand)
			}
		}

		// Update existing bead metadata.
		// config_hash and live_hash are NOT updated here — they record
		// what config the session was STARTED with. The reconciler detects
		// drift by comparing bead config_hash against desired config.
		changed := false

		if b.Metadata["state"] != state {
			queueMeta("state", state)
			changed = true
		}

		if b.Metadata["close_reason"] != "" || b.Metadata["closed_at"] != "" {
			queueMeta("close_reason", "")
			queueMeta("closed_at", "")
			changed = true
		}

		applyBatch := func() {
			if len(batch) > 0 {
				batch["synced_at"] = now.Format("2006-01-02T15:04:05Z07:00")
				if setMetaBatch(store, b.ID, batch, stderr) == nil {
					for k, v := range batch {
						b.Metadata[k] = v
					}
					if aliasValue, ok := batch["alias"]; ok && state == "active" {
						if err := session.SyncRuntimeAlias(sp, sn, aliasValue); err != nil {
							fmt.Fprintf(stderr, "session beads: syncing runtime alias %q for %s: %v\n", aliasValue, agentName, err) //nolint:errcheck
						}
					}
				}
				return
			}
			if changed {
				// Defensive fallback; current callers should always have queued at
				// least one metadata write when changed=true.
				setMeta(store, b.ID, "synced_at", now.Format("2006-01-02T15:04:05Z07:00"), stderr) //nolint:errcheck
			}
		}
		if needsAliasSync {
			lockAlias := managedAlias
			if lockAlias == "" {
				lockAlias = strings.TrimSpace(b.Metadata["alias"])
			}
			appliedWithLock := false
			lockErr := session.WithCitySessionAliasLock(cityPath, lockAlias, func() error {
				if err := session.EnsureAliasAvailableWithConfig(store, cfg, managedAlias, b.ID); err != nil {
					fmt.Fprintf(stderr, "session beads: alias %q for %s unavailable: %v\n", managedAlias, agentName, err) //nolint:errcheck
				} else {
					for key, value := range session.UpdatedAliasMetadata(b.Metadata, managedAlias) {
						queueMeta(key, value)
					}
				}
				applyBatch()
				appliedWithLock = true
				return nil
			})
			if lockErr != nil {
				fmt.Fprintf(stderr, "session beads: locking alias %q for %s: %v\n", lockAlias, agentName, lockErr) //nolint:errcheck
			}
			if appliedWithLock {
				continue
			}
		}
		applyBatch()
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

func setMetaBatch(store beads.Store, id string, batch map[string]string, stderr io.Writer) error {
	if len(batch) == 0 {
		return nil
	}
	if err := store.SetMetadataBatch(id, batch); err != nil {
		fmt.Fprintf(stderr, "session beads: setting metadata on %s: %v\n", id, err) //nolint:errcheck
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
	if setMetaBatch(store, id, map[string]string{
		"state":        reason,
		"close_reason": reason,
		"closed_at":    ts,
		"synced_at":    ts,
	}, stderr) != nil {
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
