package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
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
	all, err := store.List(beads.ListQuery{
		Label: sessionBeadLabel,
	})
	if err != nil {
		return nil, fmt.Errorf("listing session beads: %w", err)
	}
	var result []beads.Bead
	for _, b := range all {
		if !session.IsSessionBeadOrRepairable(b) {
			continue
		}
		if b.Status == "closed" {
			continue
		}
		result = append(result, b)
	}
	return result, nil
}

func snapshotOrLoadSessionBeads(store beads.Store, sessionBeads *sessionBeadSnapshot) ([]beads.Bead, error) {
	if sessionBeads != nil {
		return sessionBeads.Open(), nil
	}
	return loadSessionBeads(store)
}

func canRebindConfiguredNamedSession(b beads.Bead, identity, sessionName string) bool {
	if identity == "" || isNamedSessionBead(b) {
		return false
	}
	if strings.TrimSpace(b.Metadata[namedSessionIdentityMetadata]) == identity {
		return true
	}
	spec := namedSessionSpec{Identity: identity, SessionName: sessionName}
	if !namedSessionBeadMatchesSpec(b, spec) {
		return false
	}
	sn := strings.TrimSpace(b.Metadata["session_name"])
	return sn == sessionName || sn == identity
}

func findSessionBeadByTemplate(sessionBeads *sessionBeadSnapshot, template string) (beads.Bead, bool) {
	if sessionBeads == nil || strings.TrimSpace(template) == "" {
		return beads.Bead{}, false
	}
	for _, b := range sessionBeads.Open() {
		if strings.TrimSpace(b.Metadata["template"]) == template || strings.TrimSpace(b.Metadata["common_name"]) == template {
			return b, true
		}
	}
	return beads.Bead{}, false
}

func preserveConfiguredNamedSessionBead(b beads.Bead, cfg *config.City, cityName string) bool {
	if cfg == nil || !isNamedSessionBead(b) {
		return false
	}
	identity := namedSessionIdentity(b)
	if identity == "" {
		return false
	}
	spec, ok := findNamedSessionSpec(cfg, cityName, identity)
	if !ok {
		return false
	}
	return strings.TrimSpace(b.Metadata["session_name"]) == spec.SessionName
}

func reopenClosedConfiguredNamedSessionBead(
	cityPath string,
	store beads.Store,
	cfg *config.City,
	cityName string,
	identity string,
	sessionName string,
	state string,
	now time.Time,
	stderr io.Writer,
) (beads.Bead, bool) {
	if store == nil || cfg == nil {
		return beads.Bead{}, false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	bead, ok := findClosedNamedSessionBeadForSessionName(store, identity, sessionName)
	if !ok {
		return beads.Bead{}, false
	}
	if strings.TrimSpace(bead.Metadata["session_name"]) == "" {
		return beads.Bead{}, false
	}
	if strings.TrimSpace(bead.Metadata["session_name"]) != strings.TrimSpace(sessionName) {
		return beads.Bead{}, false
	}
	spec, ok := findNamedSessionSpec(cfg, cityName, identity)
	if !ok || strings.TrimSpace(spec.SessionName) != strings.TrimSpace(sessionName) {
		return beads.Bead{}, false
	}
	var reopened beads.Bead
	err := session.WithCitySessionIdentifierLocks(cityPath, []string{identity, sessionName}, func() error {
		if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, identity, bead.ID, identity); err != nil {
			fmt.Fprintf(stderr, "session beads: alias %q for %s unavailable during reopen: %v\n", identity, identity, err) //nolint:errcheck
			return nil
		}
		if err := session.EnsureSessionNameAvailableWithConfigForOwner(store, cfg, sessionName, bead.ID, identity); err != nil {
			fmt.Fprintf(stderr, "session beads: session_name %q for %s unavailable during reopen: %v\n", sessionName, identity, err) //nolint:errcheck
			return nil
		}
		open := "open"
		if err := store.Update(bead.ID, beads.UpdateOpts{Status: &open}); err != nil {
			fmt.Fprintf(stderr, "session beads: reopening configured named session %q: %v\n", identity, err) //nolint:errcheck
			return nil
		}
		bead.Status = "open"
		pendingCreateClaim := ""
		if state != "active" {
			pendingCreateClaim = "true"
		}
		batch := map[string]string{
			"state":                state,
			"close_reason":         "",
			"closed_at":            "",
			"pending_create_claim": pendingCreateClaim,
			"synced_at":            now.Format("2006-01-02T15:04:05Z07:00"),
		}
		if setMetaBatch(store, bead.ID, batch, stderr) == nil {
			if bead.Metadata == nil {
				bead.Metadata = make(map[string]string, len(batch))
			}
			for k, v := range batch {
				bead.Metadata[k] = v
			}
		}
		reopened = bead
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "session beads: locking identifiers for %q reopen: %v\n", identity, err) //nolint:errcheck
	}
	if reopened.ID == "" {
		return beads.Bead{}, false
	}
	return reopened, true
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
	cfg *config.City,
	clk clock.Clock,
	stderr io.Writer,
	skipClose bool,
) map[string]string {
	openIndex, _ := syncSessionBeadsWithSnapshot(
		"", store, desiredState, sp, configuredNames, cfg, clk, stderr, skipClose, nil,
	)
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
	sessionBeads, err := loadSessionBeadSnapshot(store)
	if err != nil {
		sessionBeads = nil
	}
	return configuredSessionNamesWithSnapshot(cfg, cityName, sessionBeads)
}

func configuredSessionNamesWithSnapshot(cfg *config.City, cityName string, sessionBeads *sessionBeadSnapshot) map[string]bool {
	cityName = config.EffectiveCityName(cfg, cityName)
	st := cfg.Workspace.SessionTemplate
	names := make(map[string]bool, len(cfg.Agents)+len(cfg.NamedSessions))

	singletonTemplates := make(map[string]bool)
	for _, a := range cfg.Agents {
		if isConfiguredPoolAgentForSessionNames(&a) {
			names[agent.SessionNameFor(cityName, a.QualifiedName(), st)] = true
		} else {
			if sessionBeads != nil {
				if sn := sessionBeads.FindSessionNameByTemplate(a.QualifiedName()); sn != "" {
					names[sn] = true
				}
			}
			names[agent.SessionNameFor(cityName, a.QualifiedName(), st)] = true
			singletonTemplates[a.QualifiedName()] = true
		}
	}

	if sessionBeads != nil && len(singletonTemplates) > 0 {
		for _, b := range sessionBeads.Open() {
			sn := strings.TrimSpace(b.Metadata["session_name"])
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

	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		if identity == "" {
			continue
		}
		runtimeName := config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity)
		if sessionBeads != nil {
			if spec, ok := findNamedSessionSpec(cfg, cityName, identity); ok {
				if b, ok := findCanonicalNamedSessionBead(sessionBeads, spec); ok {
					if sn := strings.TrimSpace(b.Metadata["session_name"]); sn != "" {
						names[sn] = true
					}
				}
			}
		}
		names[runtimeName] = true
	}

	return names
}

func isConfiguredPoolAgentForSessionNames(a *config.Agent) bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return true
	}
	if a.MinActiveSessions != nil && *a.MinActiveSessions > 0 {
		return true
	}
	if strings.TrimSpace(a.ScaleCheck) != "" {
		return true
	}
	if a.MaxActiveSessions != nil && *a.MaxActiveSessions != 1 {
		return true
	}
	return false
}

func syncSessionBeadsWithSnapshot(
	cityPath string,
	store beads.Store,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	configuredNames map[string]bool,
	cfg *config.City,
	clk clock.Clock,
	stderr io.Writer,
	skipClose bool,
	sessionBeads *sessionBeadSnapshot,
) (map[string]string, *sessionBeadSnapshot) {
	if store == nil {
		return nil, nil
	}
	if stderr == nil {
		stderr = io.Discard
	}

	existing, err := snapshotOrLoadSessionBeads(store, sessionBeads)
	if err != nil {
		fmt.Fprintf(stderr, "session beads: listing existing: %v\n", err) //nolint:errcheck
		return nil, sessionBeads
	}

	for i, b := range existing {
		if b.Type != "" || b.Status == "closed" {
			continue
		}
		t := sessionBeadType
		if err := store.Update(b.ID, beads.UpdateOpts{Type: &t}); err != nil {
			fmt.Fprintf(stderr, "session beads: repairing type for %s: %v\n", b.ID, err) //nolint:errcheck
		} else {
			existing[i].Type = sessionBeadType
		}
	}

	bySessionName := make(map[string]beads.Bead, len(existing))
	indexBySessionName := make(map[string]int, len(existing))
	openBeads := make([]beads.Bead, len(existing))
	copy(openBeads, existing)
	for _, b := range existing {
		if b.Status == "closed" {
			continue
		}
		if sn := b.Metadata["session_name"]; sn != "" {
			bySessionName[sn] = b
		}
	}
	for i, b := range openBeads {
		if b.Status == "closed" {
			continue
		}
		if sn := b.Metadata["session_name"]; sn != "" {
			indexBySessionName[sn] = i
		}
	}

	for i, b := range openBeads {
		if b.Status == "closed" {
			continue
		}
		sn := b.Metadata["session_name"]
		if sn == "" {
			continue
		}
		canonical, ok := bySessionName[sn]
		if ok && canonical.ID != b.ID {
			if closeBead(store, b.ID, "duplicate", clk.Now().UTC(), stderr) {
				openBeads[i].Status = "closed"
			}
		}
	}

	openIndex := make(map[string]string, len(desiredState))
	downgraded := make(map[string]bool)
	now := clk.Now().UTC()
	cityName := config.EffectiveCityName(cfg, filepath.Base(cityPath))

	if cfg != nil {
		for i, b := range openBeads {
			if b.Status == "closed" || !isNamedSessionBead(b) {
				continue
			}
			identity := namedSessionIdentity(b)
			spec, ok := findNamedSessionSpec(cfg, cityName, identity)
			if !ok {
				continue
			}
			if strings.TrimSpace(b.Metadata["session_name"]) == spec.SessionName {
				continue
			}
			if closeBead(store, b.ID, "reconfigured", now, stderr) {
				if sn := strings.TrimSpace(b.Metadata["session_name"]); sn != "" && sp.IsRunning(sn) {
					if err := sp.Stop(sn); err != nil {
						fmt.Fprintf(stderr, "session beads: stopping drifted named session %q: %v\n", sn, err) //nolint:errcheck
					}
				}
				existing[i].Status = "closed"
				openBeads[i].Status = "closed"
			}
		}
	}

	for sn, tp := range desiredState {
		agentCfg := templateParamsToConfig(tp)
		coreHash := runtime.CoreFingerprint(agentCfg)
		liveHash := runtime.LiveFingerprint(agentCfg)
		managedAlias := strings.TrimSpace(tp.Alias)
		isConfiguredNamed := strings.TrimSpace(tp.ConfiguredNamedIdentity) != ""

		state := "stopped"
		if sp.IsRunning(sn) && sp.ProcessAlive(sn, tp.Hints.ProcessNames) {
			state = "active"
		}

		agentName := tp.TemplateName
		if slot := resolvePoolSlot(tp.InstanceName, tp.TemplateName); slot > 0 {
			agentName = tp.InstanceName
		}
		cfgAgent := findAgentByTemplate(cfg, tp.TemplateName)
		isManagedPool := cfgAgent != nil && isMultiSessionCfgAgent(cfgAgent) && !tp.ManualSession

		b, exists := bySessionName[sn]
		if !exists && isConfiguredNamed {
			if reopened, ok := reopenClosedConfiguredNamedSessionBead(cityPath, store, cfg, cityName, tp.ConfiguredNamedIdentity, sn, state, now, stderr); ok {
				b = reopened
				exists = true
				bySessionName[sn] = reopened
				openBeads = append(openBeads, reopened)
				indexBySessionName[sn] = len(openBeads) - 1
			}
		}
		if !exists {
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
			if state != "active" {
				meta["pending_create_claim"] = "true"
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
			if tp.WakeMode != "" && tp.WakeMode != "resume" {
				meta["wake_mode"] = tp.WakeMode
			}
			if isConfiguredNamed {
				meta[namedSessionMetadataKey] = boolMetadata(true)
				meta[namedSessionIdentityMetadata] = tp.ConfiguredNamedIdentity
				meta[namedSessionModeMetadata] = tp.ConfiguredNamedMode
			}
			if tp.RigName != "" && !strings.Contains(tp.TemplateName, "/") {
				meta["template"] = tp.RigName + "/" + tp.TemplateName
			} else {
				meta["template"] = tp.TemplateName
			}
			if tp.PoolSlot > 0 {
				meta["pool_slot"] = strconv.Itoa(tp.PoolSlot)
			} else if slot := resolvePoolSlot(tp.InstanceName, tp.TemplateName); slot > 0 {
				meta["pool_slot"] = strconv.Itoa(slot)
			}
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
				blocked   bool
			)
			if managedAlias != "" {
				lockFn := func() error {
					if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, managedAlias, "", managedAlias); err != nil {
						fmt.Fprintf(stderr, "session beads: alias %q for %s unavailable: %v\n", managedAlias, agentName, err) //nolint:errcheck
						if isConfiguredNamed {
							createErr = err
							blocked = true
							return nil
						}
					} else {
						meta["alias"] = managedAlias
					}
					if isConfiguredNamed {
						if err := session.EnsureSessionNameAvailableWithConfigForOwner(store, cfg, sn, "", managedAlias); err != nil {
							fmt.Fprintf(stderr, "session beads: session_name %q for %s unavailable: %v\n", sn, agentName, err) //nolint:errcheck
							createErr = err
							blocked = true
							return nil
						}
					}
					newBead, createErr = createBead()
					created = true
					return nil
				}
				var lockErr error
				if isConfiguredNamed {
					lockErr = session.WithCitySessionIdentifierLocks(cityPath, []string{managedAlias, sn}, lockFn)
				} else {
					lockErr = session.WithCitySessionAliasLock(cityPath, managedAlias, lockFn)
				}
				if lockErr != nil {
					fmt.Fprintf(stderr, "session beads: locking alias %q for %s: %v\n", managedAlias, agentName, lockErr) //nolint:errcheck
				}
			}
			if !created && !blocked {
				newBead, createErr = createBead()
			}
			if createErr != nil {
				fmt.Fprintf(stderr, "session beads: creating bead for %s: %v\n", agentName, createErr) //nolint:errcheck
			} else {
				openIndex[sn] = newBead.ID
				openBeads = append(openBeads, newBead)
				indexBySessionName[sn] = len(openBeads) - 1
				if liveAlias := strings.TrimSpace(meta["alias"]); liveAlias != "" && state == "active" {
					if err := session.SyncRuntimeAlias(sp, sn, liveAlias); err != nil {
						fmt.Fprintf(stderr, "session beads: syncing runtime alias %q for %s: %v\n", liveAlias, agentName, err) //nolint:errcheck
					}
				}
			}
			continue
		}

		if isConfiguredNamed && (!isNamedSessionBead(b) || namedSessionIdentity(b) != tp.ConfiguredNamedIdentity) && !canRebindConfiguredNamedSession(b, tp.ConfiguredNamedIdentity, sn) {
			fmt.Fprintf(stderr, "session beads: configured named session %q conflicts with live bead %s\n", tp.ConfiguredNamedIdentity, b.ID) //nolint:errcheck
			continue
		}

		openIndex[sn] = b.ID
		batch := map[string]string{}
		queueMeta := func(key, value string) { batch[key] = value }

		qualifiedTemplate := tp.TemplateName
		if tp.RigName != "" && !strings.Contains(tp.TemplateName, "/") {
			qualifiedTemplate = tp.RigName + "/" + tp.TemplateName
		}
		if b.Metadata["template"] == "" || (tp.RigName != "" && !strings.Contains(b.Metadata["template"], "/")) {
			queueMeta("template", qualifiedTemplate)
		}
		if isManagedPool && b.Metadata[poolManagedMetadataKey] != boolMetadata(true) {
			queueMeta(poolManagedMetadataKey, boolMetadata(true))
		}
		if b.Metadata["pool_slot"] == "" {
			if tp.PoolSlot > 0 {
				queueMeta("pool_slot", strconv.Itoa(tp.PoolSlot))
			} else if slot := resolvePoolSlot(tp.InstanceName, tp.TemplateName); slot > 0 {
				queueMeta("pool_slot", strconv.Itoa(slot))
			}
		}
		if b.Metadata["work_dir"] == "" && tp.WorkDir != "" {
			queueMeta("work_dir", tp.WorkDir)
		}
		if b.Metadata["dependency_only"] != boolMetadata(tp.DependencyOnly) {
			queueMeta("dependency_only", boolMetadata(tp.DependencyOnly))
		}
		if isConfiguredNamed {
			if b.Metadata[namedSessionMetadataKey] != boolMetadata(true) {
				queueMeta(namedSessionMetadataKey, boolMetadata(true))
			}
			if b.Metadata[namedSessionIdentityMetadata] != tp.ConfiguredNamedIdentity {
				queueMeta(namedSessionIdentityMetadata, tp.ConfiguredNamedIdentity)
			}
			if b.Metadata[namedSessionModeMetadata] != tp.ConfiguredNamedMode {
				queueMeta(namedSessionModeMetadata, tp.ConfiguredNamedMode)
			}
		} else {
			if b.Metadata[namedSessionMetadataKey] != "" {
				queueMeta(namedSessionMetadataKey, "")
			}
			if b.Metadata[namedSessionIdentityMetadata] != "" {
				queueMeta(namedSessionIdentityMetadata, "")
			}
			if b.Metadata[namedSessionModeMetadata] != "" {
				queueMeta(namedSessionModeMetadata, "")
			}
		}
		needsAliasSync := b.Metadata["alias"] != managedAlias
		if b.Metadata["wake_mode"] != tp.WakeMode {
			queueMeta("wake_mode", tp.WakeMode)
		}
		if b.Metadata["session_key"] == "" &&
			tp.ResolvedProvider != nil && tp.ResolvedProvider.SessionIDFlag != "" {
			if key, err := session.GenerateSessionKey(); err == nil {
				queueMeta("session_key", key)
			}
		}
		if b.Metadata["continuation_epoch"] == "" {
			queueMeta("continuation_epoch", strconv.Itoa(session.DefaultContinuationEpoch))
		}
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

		changed := false
		if b.Metadata["close_reason"] != "" || b.Metadata["closed_at"] != "" {
			queueMeta("close_reason", "")
			queueMeta("closed_at", "")
			changed = true
		}

		applyBatch := func() {
			if len(batch) > 0 {
				batch["synced_at"] = now.Format("2006-01-02T15:04:05Z07:00")
				if setMetaBatch(store, b.ID, batch, stderr) == nil {
					if b.Metadata == nil {
						b.Metadata = make(map[string]string, len(batch))
					}
					for k, v := range batch {
						b.Metadata[k] = v
					}
					if idx, ok := indexBySessionName[sn]; ok {
						openBeads[idx] = b
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
				var err error
				if isConfiguredNamed {
					err = session.EnsureAliasAvailableWithConfigForOwner(store, cfg, managedAlias, b.ID, tp.ConfiguredNamedIdentity)
				} else {
					err = session.EnsureAliasAvailableWithConfig(store, cfg, managedAlias, b.ID)
				}
				if err != nil {
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

	openBeads = syncDesiredPoolSlots(store, desiredState, openBeads, indexBySessionName, cfg, now, stderr)

	for i, b := range openBeads {
		if b.Status == "closed" || !isNamedSessionBead(b) {
			continue
		}
		identity := namedSessionIdentity(b)
		if identity == "" || (cfg != nil && config.FindNamedSession(cfg, identity) != nil) {
			continue
		}
		batch := map[string]string{
			namedSessionMetadataKey:  "",
			namedSessionModeMetadata: "",
			"synced_at":              now.Format("2006-01-02T15:04:05Z07:00"),
		}
		if setMetaBatch(store, b.ID, batch, stderr) != nil {
			continue
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, len(batch))
		}
		for k, v := range batch {
			b.Metadata[k] = v
		}
		openBeads[i] = b
		if sn := strings.TrimSpace(b.Metadata["session_name"]); sn != "" {
			downgraded[sn] = true
		}
	}

	if !skipClose {
		for _, b := range existing {
			sn := b.Metadata["session_name"]
			if sn == "" {
				continue
			}
			if _, hasDesired := desiredState[sn]; hasDesired {
				continue
			}
			if downgraded[sn] {
				continue
			}
			if b.Status == "closed" {
				continue
			}
			if preserveConfiguredNamedSessionBead(b, cfg, cityName) {
				continue
			}
			if spec, conflict, err := findConflictingNamedSessionSpecForBead(cfg, cityName, b); err != nil {
				fmt.Fprintf(stderr, "session beads: checking named-session conflict for %s: %v\n", b.ID, err) //nolint:errcheck
			} else if conflict {
				fmt.Fprintf(stderr, "session beads: live bead %s blocks configured named session %q; leaving it open\n", b.ID, spec.Identity) //nolint:errcheck
				continue
			}
			if configuredNames[sn] {
				if closeBead(store, b.ID, "suspended", now, stderr) {
					if idx, ok := indexBySessionName[sn]; ok {
						openBeads[idx].Status = "closed"
					}
				}
			} else {
				if cfg != nil {
					template := strings.TrimSpace(b.Metadata["template"])
					if template != "" {
						if agentCfg := config.FindAgent(cfg, template); agentCfg != nil && !isMultiSessionCfgAgent(agentCfg) && config.FindNamedSession(cfg, template) == nil {
							fmt.Fprintf(stderr, "session beads: plain template session %s (%s) is no longer controller-managed; declare [[named_session]] to keep a canonical alias-backed session\n", b.ID, template) //nolint:errcheck
						}
					}
				}
				if closeBead(store, b.ID, "orphaned", now, stderr) {
					if idx, ok := indexBySessionName[sn]; ok {
						openBeads[idx].Status = "closed"
					}
				}
			}
		}
	}

	return openIndex, newSessionBeadSnapshot(openBeads)
}

// setBeadRestartRequested finds the open session bead for the given
// session name and sets its restart_requested metadata field. This
// persists the restart flag in the dolt database so it survives tmux
// session death.
func setBeadRestartRequested(store beads.Store, sessionName string) error {
	if store == nil {
		return fmt.Errorf("no store available")
	}
	all, err := store.List(beads.ListQuery{
		Label: sessionBeadLabel,
	})
	if err != nil {
		return fmt.Errorf("listing session beads: %w", err)
	}
	for _, b := range all {
		if !session.IsSessionBeadOrRepairable(b) {
			continue
		}
		if b.Status == "closed" {
			continue
		}
		if b.Metadata["session_name"] == sessionName {
			return store.SetMetadata(b.ID, "restart_requested", "true")
		}
	}
	return fmt.Errorf("no open session bead for %q", sessionName)
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

func syncDesiredPoolSlots(
	store beads.Store,
	desiredState map[string]TemplateParams,
	openBeads []beads.Bead,
	indexBySessionName map[string]int,
	cfg *config.City,
	now time.Time,
	stderr io.Writer,
) []beads.Bead {
	if store == nil || cfg == nil {
		return openBeads
	}

	desiredByTemplate := make(map[string][]string)
	for sn, tp := range desiredState {
		if tp.ManualSession {
			continue
		}
		agentCfg := findAgentByTemplate(cfg, tp.TemplateName)
		if agentCfg == nil || !isMultiSessionCfgAgent(agentCfg) {
			continue
		}
		desiredByTemplate[tp.TemplateName] = append(desiredByTemplate[tp.TemplateName], sn)
	}

	for _, names := range desiredByTemplate {
		sort.Strings(names)
		usedSlots := make(map[int]string)
		slotByName := make(map[string]int, len(names))
		for _, sn := range names {
			idx, ok := indexBySessionName[sn]
			if !ok {
				continue
			}
			slot, _ := strconv.Atoi(openBeads[idx].Metadata["pool_slot"])
			if slot <= 0 || slot > len(names) || usedSlots[slot] != "" {
				continue
			}
			usedSlots[slot] = sn
			slotByName[sn] = slot
		}

		nextSlot := 1
		for _, sn := range names {
			if slotByName[sn] != 0 {
				continue
			}
			for usedSlots[nextSlot] != "" {
				nextSlot++
			}
			usedSlots[nextSlot] = sn
			slotByName[sn] = nextSlot
		}

		for _, sn := range names {
			idx, ok := indexBySessionName[sn]
			if !ok {
				continue
			}
			bead := openBeads[idx]
			wantSlot := strconv.Itoa(slotByName[sn])
			batch := map[string]string{}
			if bead.Metadata[poolManagedMetadataKey] != boolMetadata(true) {
				batch[poolManagedMetadataKey] = boolMetadata(true)
			}
			if bead.Metadata["pool_slot"] != wantSlot {
				batch["pool_slot"] = wantSlot
			}
			if len(batch) == 0 {
				continue
			}
			batch["synced_at"] = now.Format("2006-01-02T15:04:05Z07:00")
			if setMetaBatch(store, bead.ID, batch, stderr) != nil {
				continue
			}
			if bead.Metadata == nil {
				bead.Metadata = make(map[string]string, len(batch))
			}
			for key, value := range batch {
				bead.Metadata[key] = value
			}
			openBeads[idx] = bead
		}
	}

	return openBeads
}

// closeBead sets final metadata on a session bead and closes it.
// This completes the bead's lifecycle record. The close_reason distinguishes
// why the bead was closed (e.g., "orphaned", "suspended").
//
// Follows the commit-signal pattern: metadata is written first, and Close
// is only called if all writes succeed. If any write fails, the bead stays
// open so the next tick retries the entire sequence.
func closeBead(store beads.Store, id, reason string, now time.Time, stderr io.Writer) bool {
	ts := now.Format("2006-01-02T15:04:05Z07:00")
	if setMetaBatch(store, id, map[string]string{
		"state":        reason,
		"close_reason": reason,
		"closed_at":    ts,
		"synced_at":    ts,
	}, stderr) != nil {
		return false
	}
	if err := store.Close(id); err != nil {
		fmt.Fprintf(stderr, "session beads: closing %s: %v\n", id, err) //nolint:errcheck
		return false
	}
	return true
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
		if isMultiSessionCfgAgent(&a) && strings.HasPrefix(agentName, qn+"-") {
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
	if slot, err := strconv.Atoi(suffix); err == nil {
		return slot
	}
	if strings.HasPrefix(suffix, "gc-") {
		slot, _ := strconv.Atoi(suffix[3:])
		return slot
	}
	return 0
}
