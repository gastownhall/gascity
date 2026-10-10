package main

// Frozen copies of the pool decision code that P3-1 refactored, taken from
// main at c3d461c78e with comment lines removed and functions renamed. They
// are the "before" side of the differential tests in pool_pieces_test.go and
// go away with the legacy reconciler.

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func computeAwakeSetPreRefactor(input AwakeInput) map[string]AwakeDecision {
	agentsByName := make(map[string]AwakeAgent, len(input.Agents))
	agentsByBaseName := make(map[string]AwakeAgent, len(input.Agents))
	duplicateBaseNames := make(map[string]bool)
	for _, a := range input.Agents {
		agentsByName[a.QualifiedName] = a
		base := awakeAgentBaseName(a.QualifiedName)
		if existing, ok := agentsByBaseName[base]; ok && existing.QualifiedName != a.QualifiedName {
			duplicateBaseNames[base] = true
			continue
		}
		if !duplicateBaseNames[base] {
			agentsByBaseName[base] = a
		}
	}
	lookupAgent := func(name string) (AwakeAgent, bool) {
		if agent, ok := agentsByName[name]; ok {
			return agent, true
		}
		base := awakeAgentBaseName(name)
		if duplicateBaseNames[base] {
			return AwakeAgent{}, false
		}
		agent, ok := agentsByBaseName[base]
		return agent, ok
	}

	desired := make(map[string]string) // sessionName → reason

	for _, bead := range input.SessionBeads {
		if !bead.PendingCreate {
			continue
		}
		desired[bead.SessionName] = "pending-create"
	}
	for _, bead := range input.SessionBeads {
		if !bead.ExplicitWake || bead.State == "closed" || bead.DependencyOnly {
			continue
		}
		if agent, ok := agentsByName[bead.Template]; ok && !agent.Suspended {
			desired[bead.SessionName] = "explicit-wake"
		}
	}
	for _, ns := range input.NamedSessions {
		if agent, ok := lookupAgent(ns.Identity); ok && agent.Suspended {
			continue
		}
		switch ns.Mode {
		case "always":
			if sn := resolveNamedSessionBeadNamePreRefactor(input.SessionBeads, ns); sn != "" {
				bead := findBeadBySessionName(input.SessionBeads, sn)
				if bead != nil && !bead.DependencyOnly {
					desired[sn] = "named-always"
				}
			} else {
				desired[ns.Identity] = "named-always"
			}
		case "on_demand":
			reason := ""
			switch {
			case input.NamedSessionDemand[ns.Identity]:
				reason = "named-demand"
			case input.NamedSessionRoutedDemand[ns.Identity]:
				reason = "routed-demand"
			case input.NamedSessionWorkQ[ns.Identity]:
				reason = "work-query"
			default:
				continue
			}
			if agent, ok := agentsByName[ns.Template]; ok && agent.Suspended {
				continue
			}
			if sn := resolveNamedSessionBeadNamePreRefactor(input.SessionBeads, ns); sn != "" {
				bead := findBeadBySessionName(input.SessionBeads, sn)
				if bead != nil && bead.Drained && reason == "named-demand" && input.NamedSessionRoutedDemand[ns.Identity] {
					reason = "routed-demand"
				}
				drainedExempt := reason == "routed-demand"
				if bead != nil && !bead.DependencyOnly && (!bead.Drained || drainedExempt) && bead.State != "closed" {
					desired[sn] = reason
				}
			} else {
				desired[ns.Identity] = reason
			}
		}
	}

	for template, count := range input.ScaleCheckCounts {
		if count <= 0 {
			continue
		}
		agent, ok := lookupAgent(template)
		if !ok || agent.Suspended {
			continue
		}
		active := collectActiveBeads(input.SessionBeads, template, input.Now)
		filled := countAssignedScaleSlots(input.SessionBeads, input.WorkBeads, input.NamedSessions, template)
		for _, bead := range active {
			if filled >= count {
				break
			}
			if sessionHasAssignedWork(input.WorkBeads, input.NamedSessions, bead) {
				continue
			}
			desired[bead.SessionName] = "scaled:demand"
			filled++
		}
		creating := collectCreatingBeads(input.SessionBeads, template)
		for _, bead := range creating {
			if filled >= count {
				break
			}
			if sessionHasAssignedWork(input.WorkBeads, input.NamedSessions, bead) {
				continue
			}
			desired[bead.SessionName] = "scaled:creating"
			filled++
		}
	}

	for template, hasWork := range input.WorkSet {
		if !hasWork {
			continue
		}
		if input.ScaleCheckCounts[template] > 0 {
			continue // ScaleCheck already covers this template
		}
		agent, ok := lookupAgent(template)
		if !ok || agent.Suspended {
			continue
		}
		if isNamedSessionTemplate(input.NamedSessions, template) {
			continue // named sessions are handled in the named-session pass
		}
		if active := collectActiveBeads(input.SessionBeads, template, input.Now); len(active) > 0 {
			desired[active[0].SessionName] = "work-query"
			continue
		}
		if creating := collectCreatingBeads(input.SessionBeads, template); len(creating) > 0 {
			desired[creating[0].SessionName] = "work-query"
		}
	}

	for _, bead := range input.SessionBeads {
		if !bead.ManualSession || bead.State == "closed" || bead.Drained {
			continue
		}
		if _, ok := agentsByName[bead.Template]; ok {
			desired[bead.SessionName] = "manual"
		}
	}

	assignedAnchor := make(map[string]string) // sessionName → matched work bead ID
	for _, bead := range input.SessionBeads {
		if bead.State == "closed" {
			continue
		}
		if agent, ok := lookupAgent(bead.Template); ok && agent.Suspended {
			continue
		}
		var (
			fallback   string
			haveExact  bool
			anchorBead string
			recorded   = bead.CurrentlyProcessingBeadID
			matchedAny = false
		)
		for _, wb := range input.WorkBeads {
			assignee := strings.TrimSpace(wb.Assignee)
			if assignee == "" || !workBeadHasAwakeDemand(wb) {
				continue
			}
			if !sessionAssigneeMatches(input.NamedSessions, bead, assignee) {
				continue
			}
			matchedAny = true
			if recorded != "" && wb.ID == recorded {
				anchorBead = wb.ID
				haveExact = true
				break
			}
			if fallback == "" {
				fallback = wb.ID
			}
		}
		if !matchedAny {
			continue
		}
		if !haveExact {
			anchorBead = fallback
		}
		desired[bead.SessionName] = "assigned-work"
		assignedAnchor[bead.SessionName] = anchorBead
	}

	for _, agent := range input.Agents {
		if agent.Suspended || agent.MinActiveSessions <= 0 {
			continue
		}
		template := agent.QualifiedName
		covered := countMinActiveCoveredPreRefactor(input.SessionBeads, desired, template, input.Now)
		if covered >= agent.MinActiveSessions {
			continue
		}
		for _, bead := range cityStopPoolBeads(input.SessionBeads, template) {
			if covered >= agent.MinActiveSessions {
				break
			}
			if _, already := desired[bead.SessionName]; already {
				continue
			}
			if minActiveHardBlocked(bead, input.Now) {
				continue
			}
			desired[bead.SessionName] = "min-active"
			covered++
		}
	}

	for _, bead := range input.SessionBeads {
		if !bead.ContinuationResetPending || bead.RestartRequested || bead.WaitHold || bead.Drained {
			continue
		}
		switch desired[bead.SessionName] {
		case "pending-create", "explicit-wake":
			continue
		default:
			desired[bead.SessionName] = "reset-pending"
		}
	}

	result := make(map[string]AwakeDecision)

	for _, bead := range input.SessionBeads {
		name := bead.SessionName
		anchor, hasAssignedWork := assignedAnchor[name]
		decision := AwakeDecision{
			HasAssignedWork: hasAssignedWork,
		}
		if hasAssignedWork {
			decision.AssignedWorkBeadID = anchor
			for _, work := range input.WorkBeads {
				if work.Status != "in_progress" {
					continue
				}
				if sessionAssigneeMatches(input.NamedSessions, bead, strings.TrimSpace(work.Assignee)) {
					decision.AssignedWorkClaimed = true
					break
				}
			}
			if bead.CurrentlyProcessingBeadID != "" && anchor != bead.CurrentlyProcessingBeadID {
				decision.RequiresFreshCycle = true
			}
		}

		if reason, inDesired := desired[name]; inDesired {
			if !bead.WaitHold || bead.PendingCreate || bead.ExplicitWake {
				decision.ShouldWake = true
				decision.Reason = reason
			}
		}

		if input.AttachedSessions[name] && !bead.WaitHold {
			decision.ShouldWake = true
			decision.Reason = "attached"
		}

		if input.PendingSessions[name] && !bead.WaitHold {
			decision.ShouldWake = true
			decision.Reason = "pending"
		}

		if input.ReadyWaitSet[bead.ID] {
			decision.ShouldWake = true
			decision.Reason = "wait-ready"
		}

		if !decision.ShouldWake && !bead.Drained && !bead.WaitHold &&
			bead.SleepReason != string(session.SleepReasonIdleTimeout) {
			if input.RunningSessions[name] && isOnDemandSession(input.NamedSessions, bead) {
				decision.ShouldWake = true
				decision.Reason = "on-demand:running"
			}
		}

		pinBlockedByState := bead.State == "suspended" || bead.State == "closed" || bead.Drained
		if !decision.ShouldWake && bead.Pinned && !pinBlockedByState && !bead.DependencyOnly && !bead.WaitHold {
			if agent, ok := lookupAgent(bead.Template); ok && !agent.Suspended {
				decision.ShouldWake = true
				decision.Reason = "pin"
			}
		}

		agent, hasAgent := lookupAgent(bead.Template)
		holdsClaimedWork := hasAgent && !agent.Suspended && sessionHasClaimedInProgressWork(input.WorkBeads, input.NamedSessions, bead)
		if decision.ShouldWake && !input.AttachedSessions[name] && !input.PendingSessions[name] && !bead.Pinned && !holdsClaimedWork && !bead.IdleSince.IsZero() &&
			!isAlwaysNamedSession(input.NamedSessions, bead) &&
			desired[name] != "assigned-work" && desired[name] != "min-active" &&
			desired[name] != "reset-pending" &&
			desired[name] != "named-demand" && desired[name] != "routed-demand" &&
			desired[name] != "work-query" && desired[name] != "explicit-wake" &&
			!inManualGracePeriod(bead, input.ManualGracePeriod, input.Now) {
			var idleTimeout time.Duration
			switch {
			case bead.ManualSession && input.ChatIdleTimeout > 0:
				idleTimeout = input.ChatIdleTimeout
			case hasAgent && agent.SleepAfterIdle > 0:
				idleTimeout = agent.SleepAfterIdle
			case isOnDemandSession(input.NamedSessions, bead):
				idleTimeout = defaultOnDemandIdleTimeout
			}
			if idleTimeout > 0 && input.Now.Sub(bead.IdleSince) >= idleTimeout {
				decision.ShouldWake = false
				decision.Reason = "idle-sleep"
			}
		}

		if !bead.HeldUntil.IsZero() && input.Now.Before(bead.HeldUntil) {
			decision.ShouldWake = false
			decision.Reason = "held"
		}

		if !bead.QuarantinedUntil.IsZero() && input.Now.Before(bead.QuarantinedUntil) {
			decision.ShouldWake = false
			decision.Reason = "quarantined"
		}

		result[name] = decision
	}

	return result
}

func findNamedSessionNamePreRefactor(beads []AwakeSessionBead, identity string) string {
	for _, b := range beads {
		if b.NamedIdentity == identity {
			return b.SessionName
		}
	}
	return ""
}

func resolveNamedSessionBeadNamePreRefactor(beads []AwakeSessionBead, ns AwakeNamedSession) string {
	if sn := findNamedSessionNamePreRefactor(beads, ns.Identity); sn != "" {
		return sn
	}
	if ns.RuntimeName == "" {
		return ""
	}
	bead := findBeadBySessionName(beads, ns.RuntimeName)
	if bead == nil || !bead.ConfiguredNamedSession {
		return ""
	}
	if ns.Template != "" && bead.Template != ns.Template {
		return ""
	}
	if bead.NamedIdentity != "" && bead.NamedIdentity != ns.Identity {
		return ""
	}
	return bead.SessionName
}

func countMinActiveCoveredPreRefactor(beads []AwakeSessionBead, desired map[string]string, template string, now time.Time) int {
	n := 0
	for _, b := range beads {
		if !isMinActivePoolBead(b, template) {
			continue
		}
		if minActiveHardBlocked(b, now) {
			continue
		}
		if b.State == "asleep" {
			if _, awake := desired[b.SessionName]; awake {
				n++
			}
			continue
		}
		if b.State == "active" || b.State == "creating" {
			n++
		}
	}
	return n
}

// The copies below are of code P3-1 moved verbatim (buildDemandTargets,
// computeNamedSessionDemand, classifyOverlaySession). They pin the move, not
// the behavior: when main changes the moved code, delete these copies and
// their differential tests. Don't update them.

func buildDemandTargetsPreRefactor(
	cityName, cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	allOpenSessionInfos []session.Info,
	stderr io.Writer,
) demandTargets {
	var pendingPools []poolEvalWork
	var defaultScaleTargets []defaultScaleCheckTarget
	var defaultNamedScaleTargets []defaultScaleCheckTarget
	coldWakeTemplates := map[string]bool{}
	namedOnDemandTemplates := map[string]bool{}
	type activeStore struct {
		store beads.Store
		ref   string
	}
	activeStores := []activeStore{{store: store, ref: "city"}}
	for _, rig := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		if s, ok := rigStores[rig.Name]; ok {
			activeStores = append(activeStores, activeStore{store: s, ref: rig.Name})
		}
	}
	controlBinding := convergedRoutedWorkBinding(cityPath, cfg, store, rigStores, suspendedRigPaths)

	for i := range cfg.Agents {
		if cfg.Agents[i].Suspended {
			continue
		}
		namedSessionMode := ""
		for j := range cfg.NamedSessions {
			if cfg.NamedSessions[j].TemplateQualifiedName() == cfg.Agents[i].QualifiedName() {
				namedSessionMode = cfg.NamedSessions[j].ModeOrDefault()
				break
			}
		}
		backsNamedSession := namedSessionMode != ""

		sp := scaleParamsForTopology(&cfg.Agents[i], config.QueryTopology{Beads: cfg.Beads})
		sp.Check = expandAgentCommandTemplate(cityPath, cityName, &cfg.Agents[i], cfg.Rigs, "scale_check", sp.Check, stderr)

		if !cfg.Agents[i].SupportsGenericEphemeralSessions() {
			continue
		}

		hasCustomScaleCheck := strings.TrimSpace(cfg.Agents[i].ScaleCheck) != ""
		template := cfg.Agents[i].QualifiedName()
		storeScopedControlDispatcher := cfg.Agents[i].Name == config.ControlDispatcherAgentName
		runningSessions := 0
		for _, si := range allOpenSessionInfos {
			if isPoolManagedSessionInfo(si) && poolSessionIsLiveInfo(si) {
				if agentTemplateIdentitiesEquivalent(cfg, si.Template, template) {
					runningSessions++
				}
			}
		}

		isCold := runningSessions == 0 && cfg.Agents[i].EffectiveMinActiveSessions() == 0

		if backsNamedSession {
			rigName := configuredRigName(cityPath, &cfg.Agents[i], cfg.Rigs)
			if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
				continue
			}
			poolDir := agentCommandDir(cityPath, &cfg.Agents[i], cfg.Rigs)
			if store != nil && !hasCustomScaleCheck {
				ownTarget := ownScaleCheckTarget(cityPath, cfg, &cfg.Agents[i], store, rigStores, controlBinding, storeScopedControlDispatcher)
				if namedSessionMode != "always" {
					defaultScaleTargets = append(defaultScaleTargets, ownTarget)
					namedOnDemandTemplates[template] = true
				}
				defaultNamedScaleTargets = append(defaultNamedScaleTargets, ownTarget)
				if !storeScopedControlDispatcher && ownTarget.storeKey != "city" && ownTarget.store != nil && ownTarget.err == nil && ownTarget.store != store {
					cityTarget := defaultScaleCheckTarget{template: template, store: store, storeKey: "city"}
					if namedSessionMode != "always" {
						defaultScaleTargets = append(defaultScaleTargets, cityTarget)
					}
					defaultNamedScaleTargets = append(defaultNamedScaleTargets, cityTarget)
				}
				continue
			}
			if store != nil && isCold && !storeScopedControlDispatcher {
				for _, source := range activeStores {
					target := defaultScaleCheckTarget{template: template, store: source.store, storeKey: source.ref}
					if namedSessionMode != "always" {
						defaultScaleTargets = append(defaultScaleTargets, target)
					}
					defaultNamedScaleTargets = append(defaultNamedScaleTargets, target)
				}
				if namedSessionMode != "always" {
					coldWakeTemplates[template] = true
				}
			}
			pendingPools = append(pendingPools, poolEvalWork{agentIdx: i, sp: sp, poolDir: poolDir, newDemand: store != nil})
			continue
		}

		rigName := configuredRigName(cityPath, &cfg.Agents[i], cfg.Rigs)
		if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
			continue
		}
		poolDir := agentCommandDir(cityPath, &cfg.Agents[i], cfg.Rigs)
		if store != nil && !hasCustomScaleCheck {
			ownTarget := ownScaleCheckTarget(cityPath, cfg, &cfg.Agents[i], store, rigStores, controlBinding, storeScopedControlDispatcher)
			defaultScaleTargets = append(defaultScaleTargets, ownTarget)
			if !storeScopedControlDispatcher && ownTarget.storeKey != "city" && ownTarget.store != nil && ownTarget.err == nil && ownTarget.store != store {
				defaultScaleTargets = append(defaultScaleTargets, defaultScaleCheckTarget{template: template, store: store, storeKey: "city"})
			}
			continue
		}
		if store != nil && isCold && !storeScopedControlDispatcher {
			for _, source := range activeStores {
				defaultScaleTargets = append(defaultScaleTargets, defaultScaleCheckTarget{template: template, store: source.store, storeKey: source.ref})
			}
			coldWakeTemplates[template] = true
		}
		env, err := controllerQueryRuntimeEnv(cityPath, cfg, &cfg.Agents[i])
		if err != nil {
			fmt.Fprintf(stderr, "scaleCheck: building env for %s: %v\n", cfg.Agents[i].QualifiedName(), err) //nolint:errcheck
			continue
		}
		pendingPools = append(pendingPools, poolEvalWork{agentIdx: i, sp: sp, poolDir: poolDir, env: env, newDemand: store != nil})
	}
	return demandTargets{
		pendingPools:             pendingPools,
		defaultScaleTargets:      defaultScaleTargets,
		defaultNamedScaleTargets: defaultNamedScaleTargets,
		coldWakeTemplates:        coldWakeTemplates,
		namedOnDemandTemplates:   namedOnDemandTemplates,
	}
}

func computeNamedSessionDemandPreRefactor(
	cityName, cityPath string,
	cfg *config.City,
	store beads.Store,
	suspendedRigPaths map[string]bool,
	namedDefaultDemand map[string]bool,
	assignedWorkBeads []beads.Bead,
	assignedWorkStoreRefs []string,
	readyAssigned map[storeScopedBeadKey]bool,
	scaleCheckCounts map[string]int,
	stderr io.Writer,
) namedSessionDemand {
	namedSpecs := make(map[string]namedSessionSpec)
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if spec.Agent.Suspended || agentInSuspendedRig(cityPath, spec.Agent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		namedSpecs[identity] = spec
	}
	namedWorkReady := make(map[string]bool, len(namedSpecs))
	namedWorkBeadID := make(map[string]string, len(namedSpecs))
	for identity := range namedDefaultDemand {
		if _, ok := namedSpecs[identity]; ok {
			namedWorkReady[identity] = true
		}
	}
	var namedClaimRefs []string
	if len(assignedWorkBeads) > 0 && len(namedSpecs) > 0 {
		namedClaimRefs = assignedWorkRelocatedClaimRefs(cityPath, cfg, store)
	}
	for identity, spec := range namedSpecs {
		for i, wb := range assignedWorkBeads {
			switch wb.Status {
			case "in_progress":
			case "open":
				ref := ""
				if i < len(assignedWorkStoreRefs) {
					ref = assignedWorkStoreRefs[i]
				}
				if !readyAssigned[storeScopedBeadKey{StoreRef: ref, ID: wb.ID}] {
					continue
				}
			default:
				continue
			}
			assignee := strings.TrimSpace(wb.Assignee)
			if !namedSessionAssigneeMatchesSpec(spec, identity, assignee) {
				continue
			}
			if !assignedWorkIndexReachableFromAgentOnClaimRefs(cityPath, cfg, spec.Agent, assignedWorkStoreRefs, i, namedClaimRefs) {
				continue
			}
			fmt.Fprintf(stderr, "namedWorkReady: %s matched by bead %s (assignee=%s status=%s)\n", identity, wb.ID, assignee, wb.Status) //nolint:errcheck
			namedWorkReady[identity] = true
			namedWorkBeadID[identity] = wb.ID
			break
		}
	}
	if len(assignedWorkBeads) > 0 {
		fmt.Fprintf(stderr, "namedWorkReady: %d assigned beads, %d named specs, ready=%v\n", len(assignedWorkBeads), len(namedSpecs), namedWorkReady) //nolint:errcheck
	}
	namedRoutedDemand := make(map[string]bool, len(namedSpecs))
	for identity, spec := range namedSpecs {
		if !spec.Agent.UsesCanonicalSingletonPoolIdentity() {
			continue
		}
		if scaleCheckCounts[namedSessionBackingTemplate(spec)] > 0 {
			namedRoutedDemand[identity] = true
		}
	}
	return namedSessionDemand{
		specs:        namedSpecs,
		workReady:    namedWorkReady,
		workBeadID:   namedWorkBeadID,
		routedDemand: namedRoutedDemand,
	}
}

func discoverSessionBeadsWithRootsPreRefactor(
	bp *agentBuildParams,
	cfg *config.City,
	desired map[string]TemplateParams,
	suspendedRigPaths map[string]bool,
	poolScaleCheckPartialTemplates map[string]bool,
	namedScaleCheckPartialTemplates map[string]bool,
	stderr io.Writer,
) map[string]bool {
	sessionBeads := bp.sessionBeads
	if sessionBeads == nil && bp.beadStore != nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(bp.beadStore)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: listing session beads: %v\n", err) //nolint:errcheck
			return nil
		}
	}
	if sessionBeads == nil {
		return nil
	}
	roots := make(map[string]bool)
	for _, info := range sessionBeads.OpenInfos() {
		if info.Closed {
			continue
		}
		sn := info.SessionNameMetadata
		if sn == "" {
			continue
		}
		if isFailedCreateSessionInfo(info) {
			continue
		}
		_, sessionAlreadyDesired := desired[sn]
		template := resolvedSessionTemplateInfo(info, cfg)
		if template == "" {
			continue
		}
		poolScaleCheckPartial := poolScaleCheckPartialTemplates[template]
		namedScaleCheckPartial := namedScaleCheckPartialTemplates[template] && isNamedSessionInfo(info)
		scaleCheckPartial := scaleCheckPartialSessionPreservableInfo(info) && (poolScaleCheckPartial || namedScaleCheckPartial)
		cfgAgent := findAgentByTemplate(cfg, template)
		if cfgAgent == nil {
			continue
		}
		if agentInSuspendedRig(bp.cityPath, cfgAgent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		roots[template] = true
		if !sessionAlreadyDesired && !isManualSessionInfoForAgent(info, cfgAgent) && !isNamedSessionInfo(info) &&
			desiredHasCanonicalNonExpandingPoolSession(desired, template, cfgAgent) && staleNonExpandingPoolSessionBeadInfo(cfgAgent, info) {
			continue
		}
		if !isManualSessionInfo(info) && !isNamedSessionInfo(info) && !isPoolManagedSessionInfo(info) && desiredHasConfiguredNamedTemplate(desired, template) {
			continue
		}
		if isEphemeralSessionInfoForAgent(info, cfgAgent) {
			manualSession := isManualSessionInfoForAgent(info, cfgAgent)
			creating := info.MetadataState == "creating" || info.MetadataState == string(session.StateStartPending)
			pendingCreate := isPendingPoolCreateInfo(info)
			templateDesired := desiredHasTemplate(desired, template)
			controllerManagedPool := info.PoolManaged ||
				strings.TrimSpace(info.PoolSlot) != "" || pendingCreate
			if controllerManagedPool && isDrainedSessionInfo(info) {
				continue
			}
			if controllerManagedPool && !manualSession && !isNamedSessionInfo(info) &&
				!sessionAlreadyDesired && cfgAgent.UsesCanonicalSingletonPoolIdentity() &&
				desiredHasCanonicalNonExpandingPoolSession(desired, template, cfgAgent) {
				continue
			}
			poolPartialAlive := (poolScaleCheckPartial || namedScaleCheckPartial) &&
				(isPendingPoolCreateInfo(info) || (!creating && scaleCheckPartialSessionPreservableInfo(info)))
			if controllerManagedPool && !manualSession && !isNamedSessionInfo(info) &&
				!sessionAlreadyDesired && !templateDesired && !poolPartialAlive {
				continue
			}
			if !manualSession && (!creating || isStaleCreatingInfo(info)) && !templateDesired && !pendingCreate && !scaleCheckPartial {
				continue
			}
		}
		if sessionAlreadyDesired {
			continue
		}
		bInfo, ok := sessionBeads.FindInfoByID(info.ID)
		if !ok {
			continue
		}
		var (
			resolveAgent         *config.Agent
			sessionQualifiedName string
		)
		if isManualSessionInfoForAgent(info, cfgAgent) {
			sessionQualifiedName = sessionBeadQualifiedNameInfo(bp.cityPath, cfgAgent, bp.rigs, bInfo)
			resolveAgent = sessionBeadConfigAgent(cfgAgent, sessionQualifiedName)
		} else {
			resolveAgent, sessionQualifiedName = canonicalSessionIdentityWithConfigInfo(cfg, cfgAgent, bInfo)
		}
		fpExtra := buildFingerprintExtra(resolveAgent)
		tp, err := resolveTemplateForSessionBeadInfo(bp, resolveAgent, sessionQualifiedName, fpExtra, bInfo)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: bead %s template %q: %v (skipping)\n", info.ID, template, err) //nolint:errcheck
			continue
		}
		tp.ManualSession = isManualSessionInfoForAgent(info, cfgAgent)
		if tp.ManualSession {
			if manualAlias := strings.TrimSpace(info.Alias); manualAlias != "" {
				tp.Alias = manualAlias
			}
		}
		if isEphemeralSessionInfoForAgent(info, cfgAgent) {
			if !tp.ManualSession || strings.TrimSpace(info.Alias) == "" {
				tp.Alias = ""
			}
			if tp.ManualSession && sessionQualifiedName != "" {
				tp.InstanceName = sessionQualifiedName
			} else {
				tp.InstanceName = sn
			}
		}
		if isNamedSessionInfo(info) {
			tp.ConfiguredNamedIdentity = info.ConfiguredNamedIdentity
			tp.ConfiguredNamedMode = info.ConfiguredNamedMode
			if tp.Env == nil {
				tp.Env = make(map[string]string)
			}
			tp.Env["GC_SESSION_ORIGIN"] = "named"
		}
		installAgentSideEffects(bp, cfgAgent, tp, stderr)
		desired[sn] = tp
	}
	return roots
}
