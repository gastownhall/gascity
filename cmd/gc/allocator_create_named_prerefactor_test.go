package main

// Frozen copies of the legacy named-session code P3-6b refactored, taken from
// main at 28279be3f4 with comment lines removed and functions renamed (the
// named overrides were inline in buildDesiredStateWithSessionBeadsAt). They
// are the "before" side of TestSyncSessionBeadsNamedArmMatchesPreRefactor
// and TestApplyNamedTemplateOverridesMatchesPreRefactor and go away with the
// legacy reconciler.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func reopenClosedConfiguredNamedSessionBeadPreRefactor(
	cityPath string,
	store beads.Store,
	cfg *config.City,
	cityName string,
	identity string,
	sessionName string,
	state string,
	now time.Time,
	extraMeta map[string]string,
	stderr io.Writer,
) (beads.Bead, string, bool) {
	if store == nil || cfg == nil {
		return beads.Bead{}, "", false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	bead, ok, err := session.FindClosedNamedSessionBeadForSessionName(store, identity, sessionName)
	if err != nil {
		fmt.Fprintf(stderr, "session beads: finding closed configured named session %q: %v\n", identity, err) //nolint:errcheck
		return beads.Bead{}, "", false
	}
	if !ok {
		return beads.Bead{}, "", false
	}
	if strings.TrimSpace(bead.Metadata["session_name"]) == "" {
		return beads.Bead{}, "", false
	}
	if strings.TrimSpace(bead.Metadata["session_name"]) != strings.TrimSpace(sessionName) {
		return beads.Bead{}, "", false
	}
	spec, ok := findNamedSessionSpec(cfg, cityName, identity)
	if !ok || strings.TrimSpace(spec.SessionName) != strings.TrimSpace(sessionName) {
		return beads.Bead{}, "", false
	}
	var reopened beads.Bead
	err = session.WithCitySessionIdentifierLocks(cityPath, []string{identity, sessionName}, func() error {
		if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, identity, bead.ID, identity); err != nil {
			fmt.Fprintf(stderr, "session beads: alias %q for %s unavailable during reopen: %v\n", identity, identity, err) //nolint:errcheck
			return nil
		}
		if err := session.EnsureSessionNameAvailableWithConfigForOwner(store, cfg, sessionName, bead.ID, identity); err != nil {
			fmt.Fprintf(stderr, "session beads: session_name %q for %s unavailable during reopen: %v\n", sessionName, identity, err) //nolint:errcheck
			return nil
		}
		pendingCreateClaim := ""
		if state != "active" {
			pendingCreateClaim = "true"
		}
		batch := map[string]string{
			"state":                state,
			"state_reason":         "",
			"close_reason":         "",
			"closed_at":            "",
			"pending_create_claim": pendingCreateClaim,
			"synced_at":            now.Format("2006-01-02T15:04:05Z07:00"),
		}
		if pendingCreateClaim == "true" {
			batch["pending_create_started_at"] = pendingCreateStartedAtNow(now)
			batch["creation_complete_at"] = ""
			batch["last_woke_at"] = ""
			batch["started_config_hash"] = ""
			batch["started_live_hash"] = ""
			batch["live_hash"] = ""
			batch["startup_dialog_verified"] = ""
			batch[session.PrimedAtMetadataKey] = ""
			batch[session.PrimingAttemptedAtMetadataKey] = ""
			batch[session.PromptHashMetadataKey] = ""
			blockers := session.ClearWakeBlockersPatch(session.State(state), bead.Metadata["sleep_reason"], now)
			delete(blockers, "state")    // the reopen owns the target state set above.
			delete(blockers, "slept_at") // a respawn is a wake, not a sleep.
			for k, v := range blockers {
				batch[k] = v
			}
			batch["sleep_reason"] = ""
		} else {
			batch["pending_create_started_at"] = ""
		}
		for k, v := range extraMeta {
			batch[k] = v
		}
		open := "open"
		txErr := store.Tx("gc: reopen configured named session "+bead.ID, func(tx beads.Tx) error {
			return tx.Update(bead.ID, beads.UpdateOpts{Status: &open, Metadata: batch})
		})
		if txErr != nil {
			fmt.Fprintf(stderr, "session beads: reopening configured named session %q: %v\n", identity, txErr) //nolint:errcheck
			return nil
		}
		bead.Status = "open"
		if bead.Metadata == nil {
			bead.Metadata = make(map[string]string, len(batch))
		}
		for k, v := range batch {
			bead.Metadata[k] = v
		}
		reopened = bead
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "session beads: locking identifiers for %q reopen: %v\n", identity, err) //nolint:errcheck
	}
	if reopened.ID == "" {
		return beads.Bead{}, "", false
	}
	return reopened, strings.TrimSpace(reopened.Metadata["session_name"]), true
}

// syncCreateMetadataPreRefactor is the sync create arm's inline metadata
// block, wrapped in the loop variables it read (sn is the desired key).
func syncCreateMetadataPreRefactor(tp TemplateParams, sn, agentName, liveHash, createState, instanceToken string, poolSlot int, now time.Time) map[string]string {
	origin := templateParamsSessionOrigin(tp)
	isConfiguredNamed := strings.TrimSpace(tp.ConfiguredNamedIdentity) != ""
	isManagedPool := origin == "ephemeral"
	isPoolInstance := poolSlot > 0
	meta := desiredSessionIdentity(sessionIdentityInputs{
		AgentName:         agentName,
		State:             createState,
		Generation:        session.DefaultGeneration,
		ContinuationEpoch: session.DefaultContinuationEpoch,
		InstanceToken:     instanceToken,
		PoolSlot:          poolSlot,
		ConfigResolved:    true,
	})
	meta["live_hash"] = liveHash
	meta["session_origin"] = origin
	meta["synced_at"] = now.Format("2006-01-02T15:04:05Z07:00")
	if !isPoolInstance {
		meta["session_name"] = sn
	}
	if createState != "active" {
		meta["pending_create_claim"] = "true"
		meta["pending_create_started_at"] = pendingCreateStartedAtNow(now)
	}
	if tp.DependencyOnly {
		meta["dependency_only"] = boolMetadata(true)
	}
	if isManagedPool {
		meta[poolManagedMetadataKey] = boolMetadata(true)
	}
	if tp.ResolvedProvider != nil && tp.ResolvedProvider.SessionIDFlag != "" {
		if key, err := session.GenerateSessionKey(); err == nil {
			meta["session_key"] = key
		}
	}
	if tp.WorkDir != "" {
		meta["work_dir"] = tp.WorkDir
	}
	if tp.WakeMode != "" {
		meta["wake_mode"] = tp.WakeMode
	}
	if isConfiguredNamed {
		meta[namedSessionMetadataKey] = boolMetadata(true)
		meta[namedSessionIdentityMetadata] = tp.ConfiguredNamedIdentity
		meta[namedSessionModeMetadata] = tp.ConfiguredNamedMode
		if tp.BoundStepID != "" {
			meta[beadmeta.BoundStepIDMetadataKey] = tp.BoundStepID
			meta[startupKickoffStateKey] = startupKickoffStatePending
			meta[startupKickoffStartedAtKey] = now.UTC().Format(time.RFC3339)
			meta[startupKickoffAttemptsKey] = "0"
		}
	}
	qualifiedTemplate := tp.TemplateName
	if tp.RigName != "" && !strings.Contains(tp.TemplateName, "/") {
		qualifiedTemplate = tp.RigName + "/" + tp.TemplateName
	}
	meta["template"] = qualifiedTemplate
	if poolSlot > 0 {
		meta["session_name"] = pendingPoolSessionName(qualifiedTemplate, instanceToken)
	}
	if tp.Command != "" {
		meta["command"] = tp.Command
	}
	if tp.ResolvedProvider != nil {
		stampResolvedProviderSessionMetadata(meta, tp.ResolvedProvider)
		if tp.ResolvedProvider.ResumeFlag != "" {
			meta["resume_flag"] = tp.ResolvedProvider.ResumeFlag
		}
		if tp.ResolvedProvider.ResumeStyle != "" {
			meta["resume_style"] = tp.ResolvedProvider.ResumeStyle
		}
		if tp.ResolvedProvider.ResumeCommand != "" {
			meta["resume_command"] = tp.ResolvedProvider.ResumeCommand
		}
		if tp.ResolvedProvider.SessionIDFlag != "" {
			meta["session_id_flag"] = tp.ResolvedProvider.SessionIDFlag
		}
	}
	return meta
}

func applyNamedTemplateOverridesPreRefactor(tp *TemplateParams, spec namedSessionSpec, identity, boundStepID string) {
	tp.Alias = identity
	tp.TemplateName = namedSessionBackingTemplate(spec)
	tp.InstanceName = identity
	tp.ConfiguredNamedIdentity = identity
	tp.ConfiguredNamedMode = spec.Mode
	tp.BoundStepID = boundStepID
	if tp.Env == nil {
		tp.Env = make(map[string]string)
	}
	tp.Env["GC_TEMPLATE"] = namedSessionBackingTemplate(spec)
	tp.Env["GC_ALIAS"] = identity
	tp.Env["GC_AGENT"] = identity
	tp.Env["GC_SESSION_ORIGIN"] = "named"
}
