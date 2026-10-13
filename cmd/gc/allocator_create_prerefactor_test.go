package main

// Frozen copies of the guarded pool create that P3-6 refactored to read an
// effect-local view, taken from main at adc49b7646 with comment lines removed
// and functions renamed. They are the "before" side of
// TestCreatePoolSessionBeadWithGuardedAliasMatchesPreRefactor and go away with
// the legacy reconciler.

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

func createPoolSessionBeadWithGuardedAliasUsingLockPreRefactor(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	qualifiedInstance string,
	slot int,
	metadata map[string]string,
	withLocks poolSessionIdentifierLockFunc,
) (session.Info, error) {
	if bp == nil {
		return session.Info{}, fmt.Errorf("creating pool session for %q: build params unavailable", template)
	}
	if withLocks == nil {
		return session.Info{}, fmt.Errorf("creating pool session for %q: identifier locker unavailable", template)
	}
	if err := validateAgentSessionTransportForBuildPreRefactor(bp, cfgAgent, qualifiedInstance); err != nil {
		return session.Info{}, err
	}
	resolvedTmuxAlias, err := resolveTmuxAliasForAgentPreRefactor(bp, cfgAgent)
	if err != nil {
		return session.Info{}, err
	}
	resolvedTmuxAlias, err = validateResolvedPoolTmuxAlias(template, resolvedTmuxAlias)
	if err != nil {
		return session.Info{}, err
	}
	transientSlot := usesTransientPoolSlotIdentity(cfgAgent)
	qualifiedInstance = strings.TrimSpace(qualifiedInstance)
	identityAgentName := qualifiedInstance
	if identityAgentName == "" {
		identityAgentName = template
	}
	identity := poolSessionCreateIdentity{
		AgentName:     identityAgentName,
		Slot:          slot,
		Metadata:      metadata,
		TransientSlot: transientSlot,
	}
	alias := qualifiedInstance
	persistAlias := alias
	if transientSlot {
		persistAlias = ""
	}
	identifiers, err := derivePoolSessionIdentifiers(bp.city, template, identity, resolvedTmuxAlias)
	if err != nil {
		return session.Info{}, err
	}
	if cfgAgent != nil && cfgAgent.UsesCanonicalSingletonPoolIdentity() && bp.sp != nil && bp.sp.IsRunning(identifiers.sessionName) {
		return session.Info{}, fmt.Errorf("%w: runtime %q still occupies singleton template %q", errPoolSessionNameUnavailable, identifiers.sessionName, template)
	}
	if bp.beadStore == nil {
		return createPoolSessionBeadWithIdentifiersPreRefactor(bp.beadStore, template, bp.city, bp.sessionBeads, bp.sessionBeads, poolSessionCreateStartedAt(bp), identity, identifiers)
	}
	lockIDs := poolSessionCreateLockIdentifiers(identifiers, alias, resolvedTmuxAlias)

	var info session.Info
	lockErr := withLocks(bp.cityPath, lockIDs, func() error {
		createIdentity := identity
		availabilityInfos, availabilityErr := freshPoolAvailabilityInfosPreRefactor(bp)
		if availabilityErr != nil {
			return fmt.Errorf("checking locked pool availability for template %q: %w", template, availabilityErr)
		}
		if alias != "" {
			aliasErr := session.EnsureAliasAvailableWithConfig(bp.beadStore, bp.city, alias, "")
			if aliasErr == nil {
				aliasErr = poolAliasCollisionFromInfos(availabilityInfos, alias)
			}
			switch {
			case aliasErr == nil:
				createIdentity.Alias = persistAlias
			case errors.Is(aliasErr, session.ErrSessionAliasExists):
			default:
				return fmt.Errorf("checking pool alias %q for template %q: %w", alias, template, aliasErr)
			}
		}
		var createErr error
		availabilitySnapshot := newSessionBeadSnapshotFromInfos(availabilityInfos)
		info, createErr = createPoolSessionBeadWithIdentifiersPreRefactor(
			bp.beadStore,
			template,
			bp.city,
			availabilitySnapshot,
			bp.sessionBeads,
			poolSessionCreateStartedAt(bp),
			createIdentity,
			identifiers,
		)
		return createErr
	})
	return info, lockErr
}

func freshPoolAvailabilityInfosPreRefactor(bp *agentBuildParams) ([]session.Info, error) {
	if bp == nil {
		return nil, fmt.Errorf("refreshing pool session availability: build params unavailable")
	}
	fresh, err := collectAllOpenSessionAvailabilityInfos(
		bp.cityPath,
		bp.city,
		bp.beadStore,
		bp.sessionCensusRigStores,
		bp.sessionCensusSuspendedRigPaths,
	)
	if err != nil {
		return nil, fmt.Errorf("refreshing complete pool session availability: %w", err)
	}
	primary := bp.sessionBeads.OpenInfos()
	infos := make([]session.Info, 0, len(fresh)+len(bp.sessionOccupancyInfos)+len(primary))
	seen := make(map[string]bool, cap(infos))
	appendInfo := func(info session.Info) {
		key := strings.Join([]string{
			strings.TrimSpace(info.ID),
			strings.TrimSpace(info.SessionNameMetadata),
			strings.TrimSpace(info.Alias),
			strings.TrimSpace(info.AgentName),
		}, "\x00")
		if seen[key] {
			return
		}
		seen[key] = true
		infos = append(infos, info)
	}
	for _, info := range fresh {
		appendInfo(info)
	}
	for _, info := range bp.sessionOccupancyInfos {
		appendInfo(info)
	}
	for _, info := range primary {
		appendInfo(info)
	}
	return infos, nil
}

func createPoolSessionBeadWithIdentifiersPreRefactor(
	store beads.Store,
	template string,
	cfg *config.City,
	availabilitySnapshot *sessionBeadSnapshot,
	writebackSnapshot *sessionBeadSnapshot,
	now time.Time,
	identity poolSessionCreateIdentity,
	identifiers poolSessionIdentifiers,
) (session.Info, error) {
	if store == nil {
		return session.Info{}, fmt.Errorf("session store unavailable for pool template %q", template)
	}
	providedAgentName := strings.TrimSpace(identity.AgentName)
	agentName := providedAgentName
	if agentName == "" {
		agentName = template
	}
	identity.AgentName = agentName
	if identifiers.beadScoped {
		if err := ensurePoolIdentityNotHeldByOpenRow(nil, cfg, availabilitySnapshot, template, agentName); err != nil {
			return session.Info{}, err
		}
	}
	if err := ensurePoolSessionIdentifiersAvailable(store, cfg, availabilitySnapshot, template, identity, identifiers); err != nil {
		return session.Info{}, err
	}
	if identifiers.beadScoped {
		if err := ensurePoolIdentityNotHeldByOpenRow(store, cfg, nil, template, agentName); err != nil {
			return session.Info{}, err
		}
	}
	instanceToken := session.NewInstanceToken()
	title := targetBasename(template)
	if providedAgentName != "" {
		title = agentName
	}
	explicitID := poolSessionExplicitBeadID(store, instanceToken)
	runtimeName := identifiers.sessionName
	if identifiers.beadScoped {
		runtimeName = pendingPoolSessionName(template, instanceToken)
		if explicitID != "" {
			runtimeName = PoolSessionName(template, explicitID)
		}
	}
	meta := map[string]string{
		"template":                  template,
		"agent_name":                agentName,
		"state":                     string(session.StateStartPending),
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(now),
		"session_origin":            "ephemeral",
		"generation":                "1",
		"continuation_epoch":        "1",
		"instance_token":            instanceToken,
		"session_name":              runtimeName,
		poolManagedMetadataKey:      boolMetadata(true),
	}
	if alias := strings.TrimSpace(identity.Alias); alias != "" {
		meta["alias"] = alias
	}
	if identity.Slot > 0 {
		meta["pool_slot"] = strconv.Itoa(identity.Slot)
	}
	for key, value := range identity.Metadata {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		meta[key] = strings.TrimSpace(value)
	}
	meta[session.CanonicalInstanceNameMetadata] = agentName
	if identity.Slot > 0 {
		meta[session.CanonicalPoolSlotMetadata] = strconv.Itoa(identity.Slot)
	}
	info, err := sessionFrontDoor(store).CreateSessionInfo(session.CreateSpec{
		ID:        explicitID,
		Title:     title,
		AgentName: agentName,
		Metadata:  meta,
	})
	if err != nil {
		return session.Info{}, err
	}
	if identifiers.beadScoped {
		if want := PoolSessionName(template, info.ID); info.SessionNameMetadata != want {
			if err := sessionFrontDoor(store).SetMarker(info.ID, "session_name", want); err != nil {
				closeFailedCreateBead(sessionFrontDoor(store), info, now, io.Discard)
				return session.Info{}, err
			}
			info = info.ApplyPatch(session.MetadataPatch{"session_name": want})
		}
	}
	if writebackSnapshot != nil {
		writebackSnapshot.addInfo(info)
	}
	return info, nil
}

func validateAgentSessionTransportForBuildPreRefactor(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string) error {
	if bp == nil || cfgAgent == nil {
		return nil
	}
	if bp.lookPath == nil {
		return nil
	}
	workspace := bp.workspace
	if workspace == nil {
		workspace = &config.Workspace{}
	}
	resolved, err := config.ResolveProvider(cfgAgent, workspace, bp.providers, bp.lookPath)
	if err != nil {
		return fmt.Errorf("agent %q: %w", qualifiedName, err)
	}
	transport := config.ResolveSessionCreateTransport(cfgAgent.Session, resolved)
	if err := validateResolvedSessionTransport(resolved, transport, bp.sp); err != nil {
		return fmt.Errorf("agent %q: %w", qualifiedName, err)
	}
	return nil
}

func resolveTmuxAliasForAgentPreRefactor(p *agentBuildParams, agent *config.Agent) (string, error) {
	if p == nil || agent == nil {
		return "", nil
	}
	resolved, err := workdirutil.ResolveTmuxAlias(p.cityPath, p.cityName, *agent, p.rigs)
	if err != nil {
		return "", fmt.Errorf("resolving tmux_alias for %q: %w", agent.QualifiedName(), err)
	}
	return resolved, nil
}
