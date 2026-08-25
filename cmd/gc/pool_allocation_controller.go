package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

const routedWorkPoolAllocationQueueSize = 256

type routedWorkPoolAllocationHint struct {
	WorkID      string
	PoolTarget  string
	SourceStore string
	EventAt     time.Time
	EnqueuedAt  time.Time
}

type routedWorkPoolAllocationResult struct {
	Session sessionpkg.Info
	Handled bool
	Created bool
}

// routedWorkPoolStartLease binds one exact-start admission to the certified
// allocation that created or rediscovered its durable session row.
type routedWorkPoolStartLease struct {
	SessionID     string
	InstanceToken string
	// SessionRevision is the exact durable row revision that authorized an
	// active-runtime recovery. Ordinary pending-create leases leave it zero.
	SessionRevision int64
	// RecoverActive limits this lease to re-starting the exact active pool row
	// after its runtime has been observed absent. It never widens ownership of
	// ordinary active members.
	RecoverActive bool
	// RecoveryPreWakeCommitted narrows a recovery lease to the exact creating
	// revision produced by this controller's fenced pre-wake transition.
	RecoveryPreWakeCommitted bool
	ControllerGeneration     uint64
	PoolTarget               string
	WorkID                   string
	SourceStore              string
	MembershipRevision       uint64
}

// routedWorkPoolDrainAckLease binds one exact drain acknowledgement to the
// durable pool member and terminal routed-work trigger it was observed for.
// It is deliberately separate from the start lease: stop admission has a
// different effect-time proof and must never inherit start authority.
type routedWorkPoolDrainAckLease struct {
	SessionID              string
	InstanceToken          string
	RequesterSessionID     string
	RequesterInstanceToken string
	ControllerGeneration   uint64
	PoolTarget             string
	WorkID                 string
	SourceStore            string
	// MembershipRevision is the keyed membership index revision observed when
	// the lease was built, and MembershipOccupied whether this row was an
	// occupied member of that index at the same instant. Both are recorded as
	// provenance and neither gates the drain. A drain acknowledgement is always
	// about a member that ALREADY EXISTS, and the fleet-wide member shape at
	// cutover is one legacy created, so gating the drain on keyed allocation
	// lineage refuses exactly the rows the keyed family has to hold. The fence
	// is the acknowledgement stamps plus the row binding (council R1).
	MembershipRevision uint64
	MembershipOccupied bool
	// TriggerFromAck reports that WorkID/SourceStore came from the
	// acknowledgement's own trigger stamp rather than from the member row. In
	// that mode the row binding is no longer the fence — the live ack stamps
	// are (see authorizeRoutedWorkPoolDrainAck).
	TriggerFromAck bool
	// DurableAgentProvenance reports that the caller has already committed the
	// agent's acknowledgement stamps onto the row under CAS and re-proven the
	// fence over the committed revision. Once that is true the acknowledgement
	// is proven durably and must NOT be re-derived from the runtime: the ack is
	// the agent announcing it is finished, so the process it would be re-read
	// from is the one the acknowledgement is about to stop.
	DurableAgentProvenance bool
}

// drainAckRefusal names why the keyed drain-ack fence declined a row. Every
// handback carries one: a stderr-only yield is a trace lie by the program's own
// delta-8 standard, and an untyped "authorization no longer holds" cannot
// distinguish a member whose acknowledgement was never agent-stamped (the one
// shape the auto-mode legacy fallback is still for) from one whose runtime
// vanished mid-tick.
type drainAckRefusal string

const (
	drainAckRefusalNone drainAckRefusal = ""
	// drainAckRefusalNotAgentStamped: the acknowledgement carries no agent
	// provenance — an older agent CLI, or a reconciler-authored marker. This is
	// the only genuinely unprovable ack, and the only one the auto-mode legacy
	// fallback may still serve.
	drainAckRefusalNotAgentStamped drainAckRefusal = "not_agent_stamped"
	// drainAckRefusalMemberNotOccupied: keyed pool membership did not certify
	// this row. Retained as a REASON only — it is no longer a precondition.
	drainAckRefusalMemberNotOccupied drainAckRefusal = "member_not_occupied"
	// drainAckRefusalRuntimeGone: the provider could not be read for the
	// session, or the session is no longer running.
	drainAckRefusalRuntimeGone drainAckRefusal = "runtime_gone"
	// drainAckRefusalLeaseInvalid: the lease shape, the row binding, the
	// generation, the config or the pool policy no longer matches. Never
	// returned bare by the authorizer — see the sub-codes below.
	drainAckRefusalLeaseInvalid drainAckRefusal = "lease_invalid"
	// drainAckRefusalUnavailable: keyed state, the conditional writer or the
	// atomic terminal closer is unavailable, so nothing was proven either way.
	drainAckRefusalUnavailable drainAckRefusal = "unavailable"
)

// lease_invalid sub-codes. The bare code covers a dozen independent
// preconditions, so a run that reads `refusal=lease_invalid` off a handback
// learns only that ONE of them failed — which is what left ga-f7v2ft.147
// undiagnosable through two campaigns after the store-ref confounder was
// removed. Each arm names itself; the sub-code keeps `lease_invalid` as its
// prefix so the coarse family stays readable in a log or a trace query.
const (
	drainAckRefusalLeaseShape        drainAckRefusal = "lease_invalid/lease_shape"
	drainAckRefusalConfigSuperseded  drainAckRefusal = "lease_invalid/config_superseded"
	drainAckRefusalGeneration        drainAckRefusal = "lease_invalid/generation"
	drainAckRefusalSessionIdentity   drainAckRefusal = "lease_invalid/session_identity"
	drainAckRefusalRowClosed         drainAckRefusal = "lease_invalid/row_closed"
	drainAckRefusalLifecycleShape    drainAckRefusal = "lease_invalid/lifecycle_shape"
	drainAckRefusalNotPoolManaged    drainAckRefusal = "lease_invalid/not_pool_managed"
	drainAckRefusalNamedRow          drainAckRefusal = "lease_invalid/named_row"
	drainAckRefusalRequesterBinding  drainAckRefusal = "lease_invalid/requester_binding"
	drainAckRefusalInstanceToken     drainAckRefusal = "lease_invalid/instance_token"
	drainAckRefusalPoolTarget        drainAckRefusal = "lease_invalid/pool_target"
	drainAckRefusalRowBinding        drainAckRefusal = "lease_invalid/row_binding"
	drainAckRefusalSessionName       drainAckRefusal = "lease_invalid/session_name"
	drainAckRefusalAgentUnavailable  drainAckRefusal = "lease_invalid/agent_unavailable"
	drainAckRefusalPolicyUnsupported drainAckRefusal = "lease_invalid/policy_unsupported"
	drainAckRefusalWorkNotClosed     drainAckRefusal = "lease_invalid/work_not_closed"
	drainAckRefusalAssignedWork      drainAckRefusal = "lease_invalid/assigned_work"
)

func validateRoutedWorkPoolStartLease(lease routedWorkPoolStartLease) error {
	if err := validateSessionStartAdmission(lease.SessionID, sessionStartAdmissionInProcess); err != nil {
		return err
	}
	if lease.ControllerGeneration == 0 || lease.MembershipRevision == 0 {
		return fmt.Errorf("admitting pool allocation %q: generation and membership revision must be positive", lease.SessionID)
	}
	// The revision is an opaque bd token tested only for equality (against
	// persisted.Revision in authorizeRoutedWorkPoolStart), so the lease requires
	// it to be KNOWN, not positive: bd revisions are signed and a sign test
	// refused recovery on the negative half of every city's rows (ga-f7v2ft.141).
	if lease.RecoverActive && !beads.RevisionKnown(lease.SessionRevision) {
		return fmt.Errorf("admitting pool allocation %q: recovery session revision is unknown", lease.SessionID)
	}
	if lease.RecoveryPreWakeCommitted && !lease.RecoverActive {
		return fmt.Errorf("admitting pool allocation %q: committed recovery pre-wake requires active recovery", lease.SessionID)
	}
	fields := []struct {
		name  string
		value string
	}{
		{name: "instance token", value: lease.InstanceToken},
		{name: "pool target", value: lease.PoolTarget},
		{name: "work ID", value: lease.WorkID},
		{name: "source store", value: lease.SourceStore},
	}
	for _, field := range fields {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("admitting pool allocation %q: %s is not canonical", lease.SessionID, field.name)
		}
	}
	return nil
}

func validateRoutedWorkPoolDrainAckLease(lease routedWorkPoolDrainAckLease) error {
	if err := validateSessionStartAdmission(lease.SessionID, sessionStartAdmissionInProcess); err != nil {
		return err
	}
	if lease.ControllerGeneration == 0 {
		return fmt.Errorf("admitting pool drain acknowledgement %q: controller generation must be positive", lease.SessionID)
	}
	// MembershipRevision is deliberately NOT required. It is allocation
	// lineage, and a drain acknowledgement is about a member that already
	// exists — requiring it refused every legacy-created member, which is the
	// shape the whole fleet has at cutover (council R1).
	fields := []struct {
		name  string
		value string
	}{
		{name: "instance token", value: lease.InstanceToken},
		{name: "requester session ID", value: lease.RequesterSessionID},
		{name: "requester instance token", value: lease.RequesterInstanceToken},
		{name: "pool target", value: lease.PoolTarget},
		{name: "work ID", value: lease.WorkID},
		{name: "source store", value: lease.SourceStore},
	}
	for _, field := range fields {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("admitting pool drain acknowledgement %q: %s is not canonical", lease.SessionID, field.name)
		}
	}
	return nil
}

// newRoutedWorkPoolDrainAckLease reads the agent's acknowledgement stamps ONCE
// and builds the lease they prove. A false result is not an error: its caller
// must leave the legacy reconciler as the only writer, and the returned refusal
// says which of the narrow reasons that was. Any failed observation is returned
// as an error so it cannot be mistaken for a clean "no work" result.
//
// Keyed pool membership is observed but NOT required. See the lease's
// MembershipRevision comment: a drain acknowledgement is always about an
// existing member, so allocation lineage cannot be its precondition.
func (cr *CityRuntime) newRoutedWorkPoolDrainAckLease(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
) (routedWorkPoolDrainAckLease, bool, drainAckRefusal, error) {
	if cr == nil || cr.cs == nil || snapshot.Config == nil || snapshot.Provider == nil || snapshot.Store == nil {
		return routedWorkPoolDrainAckLease{}, false, drainAckRefusalUnavailable, fmt.Errorf("authorizing pool drain acknowledgement: keyed state is unavailable")
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" {
		return routedWorkPoolDrainAckLease{}, false, drainAckRefusalSessionName, nil
	}
	source, err := snapshot.Provider.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil {
		if snapshot.Provider.IsRunning(name) {
			return routedWorkPoolDrainAckLease{}, true, drainAckRefusalRuntimeGone, fmt.Errorf("authorizing pool drain acknowledgement for %q: reading acknowledgement source: %w", info.ID, err)
		}
		return routedWorkPoolDrainAckLease{}, false, drainAckRefusalRuntimeGone, nil
	}
	if source != drainAckSourceAgentValue {
		return routedWorkPoolDrainAckLease{}, false, drainAckRefusalNotAgentStamped, nil
	}
	requesterSessionID, err := snapshot.Provider.GetMeta(name, drainAckRequesterSessionIDKey)
	if err != nil {
		return routedWorkPoolDrainAckLease{}, true, drainAckRefusalRuntimeGone, fmt.Errorf("authorizing pool drain acknowledgement for %q: reading requester session ID: %w", info.ID, err)
	}
	requesterInstanceToken, err := snapshot.Provider.GetMeta(name, drainAckRequesterInstanceTokenKey)
	if err != nil {
		return routedWorkPoolDrainAckLease{}, true, drainAckRefusalRuntimeGone, fmt.Errorf("authorizing pool drain acknowledgement for %q: reading requester instance token: %w", info.ID, err)
	}
	stampedWorkID, err := snapshot.Provider.GetMeta(name, reconcilerDrainAckTriggerBeadIDKey)
	if err != nil {
		return routedWorkPoolDrainAckLease{}, true, drainAckRefusalRuntimeGone, fmt.Errorf("authorizing pool drain acknowledgement for %q: reading acknowledged trigger bead: %w", info.ID, err)
	}
	stampedStoreRef, err := snapshot.Provider.GetMeta(name, reconcilerDrainAckTriggerStoreRefKey)
	if err != nil {
		return routedWorkPoolDrainAckLease{}, true, drainAckRefusalRuntimeGone, fmt.Errorf("authorizing pool drain acknowledgement for %q: reading acknowledged trigger store ref: %w", info.ID, err)
	}
	if cr.poolMembershipShadow == nil {
		return routedWorkPoolDrainAckLease{}, true, drainAckRefusalUnavailable, fmt.Errorf("authorizing pool drain acknowledgement: keyed state is unavailable")
	}
	template := normalizedSessionTemplateInfo(info, snapshot.Config)
	observation, occupied := cr.poolMembershipShadow.observeOccupiedMember(template, info.ID)
	workID, storeRef, triggerFromAck := drainAckTriggerBindingForLease(info, stampedWorkID, stampedStoreRef)
	lease := routedWorkPoolDrainAckLease{
		SessionID:              info.ID,
		InstanceToken:          strings.TrimSpace(info.InstanceToken),
		RequesterSessionID:     strings.TrimSpace(requesterSessionID),
		RequesterInstanceToken: strings.TrimSpace(requesterInstanceToken),
		ControllerGeneration:   snapshot.Generation,
		PoolTarget:             template,
		WorkID:                 workID,
		SourceStore:            canonicalizeLegacyWorkflowStoreRef(snapshot.Config, snapshot.CityPath, storeRef),
		MembershipRevision:     observation.revision,
		MembershipOccupied:     occupied,
		TriggerFromAck:         triggerFromAck,
	}
	if err := validateRoutedWorkPoolDrainAckLease(lease); err != nil {
		return routedWorkPoolDrainAckLease{}, true, drainAckRefusalLeaseShape, nil
	}
	return lease, true, drainAckRefusalNone, nil
}

// drainAckTriggerBindingForLease resolves which trigger a rebuilt drain-ack
// lease is about, returning the raw (un-canonicalized) store ref and whether the
// answer came from the acknowledgement itself.
//
// An acknowledgement is about the unit of work the agent finished. The member
// row is not that: the legacy pool builder may legitimately re-point a member
// that is still active — the ack → stop-pending window — onto the next ready
// work item, and every lease rebuilt after that named a different, genuinely
// open trigger, so the effect boundary refused forever (ga-f7v2ft.131). So the
// ack's own stamp wins whenever it is complete.
//
// Both-or-neither: a lone key is treated as no stamp at all, which is also the
// mixed-version answer — acknowledgements from an older agent CLI carry no
// stamp and behave exactly as they did before this pair existed.
func drainAckTriggerBindingForLease(info sessionpkg.Info, stampedWorkID, stampedStoreRef string) (workID, storeRef string, fromAck bool) {
	if id, ref := strings.TrimSpace(stampedWorkID), strings.TrimSpace(stampedStoreRef); id != "" && ref != "" {
		return id, ref, true
	}
	return strings.TrimSpace(info.TriggerBeadID), info.TriggerBeadStoreRef, false
}

// recoverRoutedWorkPoolDrainAckLease classifies the provenance of a durable
// stop-pending row: (lease, true, false, nil) for a rebuilt agent
// acknowledgement, (_, false, true, nil) for a confirmed legacy marker (only
// this may yield to legacy), and (_, false, false, nil) for UNRECOGNIZED
// provenance — a witness that was consulted and shows no marker, including a
// runtime that is provably absent. Unrecognized is evidence, not an error: the
// caller re-validates it against a fresh COMPLETE liveness observation under
// the per-key lock before any effect (supersede on a dead runtime, refuse on a
// live one). An error is reserved for a witness that could not be read on a
// runtime that is still present — a failed read proves nothing about absence.
func (cr *CityRuntime) recoverRoutedWorkPoolDrainAckLease(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
) (routedWorkPoolDrainAckLease, bool, bool, error) {
	lease, agentDrainAck, _, err := cr.newRoutedWorkPoolDrainAckLease(snapshot, info)
	if err != nil {
		return routedWorkPoolDrainAckLease{}, false, false, err
	}
	if agentDrainAck {
		if err := validateRoutedWorkPoolDrainAckLease(lease); err != nil {
			return routedWorkPoolDrainAckLease{}, false, false, fmt.Errorf("validating recovered drain acknowledgement lease: %w", err)
		}
		return lease, true, false, nil
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" || snapshot.Provider == nil {
		return routedWorkPoolDrainAckLease{}, false, false, errors.New("drain acknowledgement provenance is unavailable")
	}
	source, sourceErr := snapshot.Provider.GetMeta(name, reconcilerDrainAckSourceKey)
	if sourceErr != nil {
		if snapshot.Provider.IsRunning(name) {
			return routedWorkPoolDrainAckLease{}, false, false, fmt.Errorf("reading drain acknowledgement provenance: %w", sourceErr)
		}
		// The runtime itself is absent, so no marker can exist on it. This is
		// still not treated as a marker of anything — it is the unrecognized
		// disposition, decided by the caller's liveness re-validation.
		return routedWorkPoolDrainAckLease{}, false, false, nil
	}
	if source == reconcilerDrainAckSourceValue {
		return routedWorkPoolDrainAckLease{}, false, true, nil
	}
	return routedWorkPoolDrainAckLease{}, false, false, nil
}

// drainAckProviderCheck is one live provider-meta equality the drain-ack effect
// boundary re-proves. normalize maps the raw stamp into the lease's own
// spelling before comparison — the trigger store ref is stamped verbatim and
// canonicalized on read (ga-2oboq) — and a nil normalize means byte equality.
type drainAckProviderCheck struct {
	key       string
	want      string
	normalize func(string) string
}

// authorizeRoutedWorkPoolDrainAck repeats every destructive precondition at
// the effect boundary. It uses live exact durable reads rather than the legacy
// reconciler's best-effort snapshots.
//
// Durable-first (council R1). The acknowledgement stamps are read from the
// RUNTIME exactly once, before the stop-pending transition. Once that
// transition has committed them onto the row under CAS and the caller has
// re-proven its fence over the committed revision — lease.DurableAgentProvenance
// — this re-authorization is satisfied by the durable row, and the provider
// probes below are skipped. Re-deriving an acknowledgement from the runtime
// after the commit asks a process that has just announced it is finished to
// keep answering for the drain that is about to stop it, and that read losing
// its race is what handed a stamp-provable drain back to legacy.
func (cr *CityRuntime) authorizeRoutedWorkPoolDrainAck(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	lease routedWorkPoolDrainAckLease,
) (bool, drainAckRefusal, error) {
	if cr == nil || cr.cs == nil || cr.poolMembershipShadow == nil || snapshot.Config == nil || snapshot.Provider == nil || snapshot.Store == nil {
		return false, drainAckRefusalUnavailable, fmt.Errorf("authorizing pool drain acknowledgement: keyed state is unavailable")
	}
	if err := validateRoutedWorkPoolDrainAckLease(lease); err != nil {
		return false, drainAckRefusalLeaseShape, err
	}
	cr.serviceStateMu.RLock()
	configCurrent := cr.cfg == snapshot.Config
	cr.serviceStateMu.RUnlock()
	// Kept as one rung per precondition rather than one conjunction: these are
	// independent facts about the row, and which one failed is the whole
	// diagnostic value of the handback.
	switch {
	case !configCurrent:
		return false, drainAckRefusalConfigSuperseded, nil
	case snapshot.Generation != lease.ControllerGeneration:
		return false, drainAckRefusalGeneration, nil
	case info.ID != lease.SessionID:
		return false, drainAckRefusalSessionIdentity, nil
	case info.Closed:
		return false, drainAckRefusalRowClosed, nil
	case !isRoutedWorkPoolDrainAckLifecycleShape(info):
		return false, drainAckRefusalLifecycleShape, nil
	case !isPoolManagedSessionInfo(info):
		return false, drainAckRefusalNotPoolManaged, nil
	case isNamedSessionInfo(info):
		return false, drainAckRefusalNamedRow, nil
	case lease.RequesterSessionID != info.ID || lease.RequesterInstanceToken != lease.InstanceToken:
		return false, drainAckRefusalRequesterBinding, nil
	case strings.TrimSpace(info.InstanceToken) != lease.InstanceToken:
		return false, drainAckRefusalInstanceToken, nil
	case normalizedSessionTemplateInfo(info, snapshot.Config) != lease.PoolTarget:
		return false, drainAckRefusalPoolTarget, nil
	}
	// The row binding is the fence only for an UNSTAMPED acknowledgement, where
	// the row is the sole evidence of what was acknowledged. A stamped ack
	// carries its own trigger, so binding the effect to the row would re-open
	// exactly the defect the stamp closes: legacy re-points a still-active
	// member during the ack → stop-pending window and the acknowledged drain
	// can never finalize (ga-f7v2ft.131). In stamp mode the fence moves to the
	// live ack stamps in the provider-meta check table below.
	if !lease.TriggerFromAck &&
		(strings.TrimSpace(info.TriggerBeadID) != lease.WorkID ||
			canonicalizeLegacyWorkflowStoreRef(snapshot.Config, snapshot.CityPath, info.TriggerBeadStoreRef) != lease.SourceStore) {
		return false, drainAckRefusalRowBinding, nil
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" {
		return false, drainAckRefusalSessionName, nil
	}
	agent := findAgentByTemplate(snapshot.Config, lease.PoolTarget)
	if agent == nil || isAgentEffectivelySuspendedWith(snapshot.Config, snapshot.CityPath, agent, loadSuspensionStateBestEffort(snapshot.CityPath)) {
		return false, drainAckRefusalAgentUnavailable, nil
	}
	namedTemplates := make(map[string]struct{}, len(snapshot.Config.NamedSessions))
	for i := range snapshot.Config.NamedSessions {
		namedTemplates[snapshot.Config.NamedSessions[i].TemplateQualifiedName()] = struct{}{}
	}
	// This is a release site, so it asks releaseEligible, not supported.
	policy := newPoolAllocationShadowPolicy(snapshot.Config, agent, namedTemplates)
	if !policy.releaseEligible() {
		return false, drainAckRefusalPolicyUnsupported, nil
	}
	// The source-store clause, asked directly rather than through the
	// forSourceStore overlay. That overlay early-returns on !supported(), so
	// under any capacity cap it never runs — which was invisible while the gate
	// above refused every capped city outright, and would have become a silent
	// skip the moment it stopped. An unreadable trigger store has to refuse
	// here: the store resolution below would otherwise surface it as an error,
	// and a drain acknowledgement the keyed lane cannot service is legacy's to
	// handle, not an incident.
	if !poolAllocationShadowReachesSourceStore(snapshot.Config, agent, snapshot.CityPath, lease.SourceStore) {
		return false, drainAckRefusalPolicyUnsupported, nil
	}
	// The singleton exclusion, read from the agent rather than from the policy's
	// maxActiveSessions. The constructor fills that field only on the agent-cap
	// branch, so any city that returns earlier — under a min floor, a workspace
	// or rig cap, or a namepool — leaves it at -1 and the exclusion quietly stops
	// being evaluated. The agent's own cap is the fact this clause is about.
	if maximum := agent.EffectiveMaxActiveSessions(); poolAllocationShadowHasCap(maximum) && *maximum == 1 &&
		!isCanonicalPoolManagedSessionInfoForTemplate(info, lease.PoolTarget) {
		return false, drainAckRefusalPolicyUnsupported, nil
	}
	// The runtime half. It proves the acknowledgement is the agent's and that
	// the pane is not mid-interaction — both of which are questions about a
	// LIVE runtime, and both of which are already settled once the stamps are
	// committed durably. Skipping it then is the whole of the durable-first
	// fix; running it then is the defect.
	if !lease.DurableAgentProvenance {
		checks := []drainAckProviderCheck{
			{key: "GC_SESSION_ID", want: info.ID},
			{key: "GC_INSTANCE_TOKEN", want: lease.InstanceToken},
			{key: reconcilerDrainAckSourceKey, want: drainAckSourceAgentValue},
			{key: drainAckRequesterSessionIDKey, want: lease.RequesterSessionID},
			{key: drainAckRequesterInstanceTokenKey, want: lease.RequesterInstanceToken},
			{key: "GC_DRAIN_ACK", want: "1"},
		}
		if lease.TriggerFromAck {
			checks = append(checks,
				drainAckProviderCheck{key: reconcilerDrainAckTriggerBeadIDKey, want: lease.WorkID, normalize: strings.TrimSpace},
				drainAckProviderCheck{key: reconcilerDrainAckTriggerStoreRefKey, want: lease.SourceStore, normalize: func(v string) string {
					return canonicalizeLegacyWorkflowStoreRef(snapshot.Config, snapshot.CityPath, v)
				}},
			)
		}
		for _, check := range checks {
			got, err := snapshot.Provider.GetMeta(name, check.key)
			if err != nil {
				return false, drainAckRefusalRuntimeGone, fmt.Errorf("authorizing pool drain acknowledgement for %q: reading %s: %w", info.ID, check.key, err)
			}
			if check.normalize != nil {
				got = check.normalize(got)
			}
			if got != check.want {
				if check.key == reconcilerDrainAckSourceKey {
					return false, drainAckRefusalNotAgentStamped, nil
				}
				return false, drainAckRefusalRuntimeGone, nil
			}
		}
		interactionProvider, ok := snapshot.Provider.(runtime.InteractionProvider)
		if !ok {
			return false, drainAckRefusalUnavailable, fmt.Errorf("authorizing pool drain acknowledgement for %q: provider cannot prove pending-interaction state", info.ID)
		}
		pending, err := interactionProvider.Pending(name)
		if err != nil {
			return false, drainAckRefusalRuntimeGone, fmt.Errorf("authorizing pool drain acknowledgement for %q: checking pending interaction: %w", info.ID, err)
		}
		if pending != nil {
			return false, drainAckRefusalRuntimeGone, nil
		}
	}
	// Unchanged in code, corrected in meaning: with a stamped acknowledgement
	// the lease's work and store are the ACKED ones, so this proves the work
	// the agent actually finished is closed in the store it finished it in —
	// not whatever the row was last re-pointed at.
	sourceStore, ok := cr.cs.routedWorkStore(snapshot.Config, lease.SourceStore)
	if !ok || sourceStore == nil {
		return false, drainAckRefusalUnavailable, fmt.Errorf("authorizing pool drain acknowledgement for %q: source store %q is unavailable", info.ID, lease.SourceStore)
	}
	work, err := beads.HandlesFor(sourceStore).Live.Get(lease.WorkID)
	if err != nil {
		return false, drainAckRefusalUnavailable, fmt.Errorf("authorizing pool drain acknowledgement for %q: reading trigger work %q: %w", info.ID, lease.WorkID, err)
	}
	if work.ID != lease.WorkID || work.Status != "closed" {
		return false, drainAckRefusalWorkNotClosed, nil
	}
	hasAssigned, err := sessionHasAwakeAssignedWorkForReachableStore(snapshot.CityPath, snapshot.Config, snapshot.Store, cr.rigBeadStores(), info)
	if err != nil {
		return false, drainAckRefusalUnavailable, fmt.Errorf("authorizing pool drain acknowledgement for %q: checking assigned work: %w", info.ID, err)
	}
	if hasAssigned {
		return false, drainAckRefusalAssignedWork, nil
	}
	// Keyed membership is NOT a precondition — see the lease's
	// MembershipRevision comment. It stays a monotonicity fence only for a row
	// the keyed allocator actually owns, where a regressed revision means the
	// index was rebuilt under this drain; a row it never owned (every
	// legacy-created member) has nothing to regress and is fenced by its ack
	// stamps and its row binding instead.
	if lease.MembershipOccupied {
		observation, occupied := cr.poolMembershipShadow.observeOccupiedMember(lease.PoolTarget, lease.SessionID)
		if !occupied || observation.revision < lease.MembershipRevision {
			return false, drainAckRefusalMemberNotOccupied, nil
		}
	}
	return true, drainAckRefusalNone, nil
}

// isRoutedWorkPoolDrainAckLifecycleShape reports whether a row is in a shape a
// drain acknowledgement can be admitted from: a member that is running, or one
// already carrying the acknowledgement's own stop-pending mark.
//
// "Running" is asked of the LIFECYCLE PROJECTION, not of one literal spelling
// of the state metadata. A live member does not stay on `active`: the status
// heal rewrites it to `awake` a tick after it reaches the runtime
// (session_status_alias_heal.go), and both spellings mean the same thing —
// projectBaseState maps them to BaseStateActive. Comparing the raw string
// against `active` therefore refused every member from the heal onward, which
// is the whole population in a real run, and handed each acknowledgement to
// legacy (ga-f7v2ft.147). The projection also carries the closed-status guard,
// so a closed row can never satisfy the shape.
func isRoutedWorkPoolDrainAckLifecycleShape(info sessionpkg.Info) bool {
	if isDrainAckStopPendingInfo(info) {
		return true
	}
	return sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info)).BaseState == sessionpkg.BaseStateActive
}

// authoritativeReadyRoutedWorkByID verifies one work bead and its blocking
// dependencies through the store's live handle. It never calls List or Ready.
func authoritativeReadyRoutedWorkByID(store beads.Store, id string, now time.Time) (beads.Bead, bool, error) {
	if store == nil {
		return beads.Bead{}, false, fmt.Errorf("reading routed work %q: store is nil", id)
	}
	id = strings.TrimSpace(id)
	if id == "" || now.IsZero() {
		return beads.Bead{}, false, fmt.Errorf("reading routed work: invalid id or observation time")
	}
	live := beads.HandlesFor(store).Live
	work, err := live.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return beads.Bead{}, false, nil
	}
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("reading routed work %q: %w", id, err)
	}
	if work.ID != id {
		return beads.Bead{}, false, fmt.Errorf("reading routed work %q: store returned %q", id, work.ID)
	}
	if !beads.IsReadyCandidateForTier(work, now, beads.TierBoth) || strings.TrimSpace(work.Assignee) != "" {
		return beads.Bead{}, false, nil
	}
	if work.IsBlocked != nil && *work.IsBlocked {
		return beads.Bead{}, false, nil
	}
	deps, err := live.DepList(id, "down")
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("reading dependencies for routed work %q: %w", id, err)
	}
	seen := make(map[string]struct{}, len(deps))
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) {
			continue
		}
		dependencyID := strings.TrimSpace(dep.DependsOnID)
		if dependencyID == "" {
			return beads.Bead{}, false, fmt.Errorf("reading dependencies for routed work %q: empty blocking dependency id", id)
		}
		if _, duplicate := seen[dependencyID]; duplicate {
			continue
		}
		seen[dependencyID] = struct{}{}
		dependency, err := live.Get(dependencyID)
		if err != nil {
			return beads.Bead{}, false, fmt.Errorf("reading dependency %q for routed work %q: %w", dependencyID, id, err)
		}
		if dependency.Status != "closed" {
			return beads.Bead{}, false, nil
		}
	}
	return work, true, nil
}

func (cr *CityRuntime) enqueueRoutedWorkPoolAllocation(contribution readyRoutedWorkDemandContribution) bool {
	if cr == nil || cr.routedWorkPoolAllocationCh == nil {
		return false
	}
	hint := routedWorkPoolAllocationHint{
		WorkID:      strings.TrimSpace(contribution.WorkID),
		PoolTarget:  strings.TrimSpace(contribution.PoolTarget),
		SourceStore: strings.TrimSpace(contribution.SourceStore),
		EventAt:     contribution.EventAt.UTC(),
		EnqueuedAt:  contribution.DecidedAt.UTC(),
	}
	if hint.WorkID == "" || hint.PoolTarget == "" || hint.SourceStore == "" {
		return false
	}
	if hint.EnqueuedAt.IsZero() {
		hint.EnqueuedAt = time.Now().UTC()
	}
	select {
	case cr.routedWorkPoolAllocationCh <- hint:
		// The allocation-ownership seam opens HERE, at the moment the exact key
		// enters the keyed lane — not at create, where the durable claim lands.
		// That is what makes keyed the winner of the materialization instead of
		// merely the second creator to notice (ga-f7v2ft.126's cutover arm).
		keyedRoutedWorkAllocations.reserve(
			routedWorkAllocationKeyFor(hint.WorkID, hint.PoolTarget, hint.SourceStore), time.Now())
		return true
	default:
		return false
	}
}

func (cr *CityRuntime) handleRoutedWorkPoolAllocation(ctx context.Context, hint routedWorkPoolAllocationHint) {
	// Close the allocation-ownership seam on EVERY path — materialized, refused,
	// or failed. A retained reservation would fence the legacy pool builder off
	// a work item nobody is allocating; releasing here is what makes the
	// stand-down lease-triggered and leaves legacy free on its next pass.
	defer keyedRoutedWorkAllocations.release(
		routedWorkAllocationKeyFor(hint.WorkID, hint.PoolTarget, hint.SourceStore))
	result, err := cr.reconcileRoutedWorkPoolAllocation(ctx, hint)
	if err != nil || !result.Handled {
		if cr.sessionStartRolloutMode() == rollout.Require {
			if err != nil {
				fmt.Fprintf(cr.sessionStartStderr(), "%s: routed-work pool allocation for %s: %v; parked in required keyed reconciliation\n", cr.sessionStartLogPrefix(), hint.WorkID, err) //nolint:errcheck // required-mode park must remain visible
			} else {
				fmt.Fprintf(cr.sessionStartStderr(), "%s: routed-work pool allocation for %s was not handled; parked in required keyed reconciliation\n", cr.sessionStartLogPrefix(), hint.WorkID) //nolint:errcheck // required-mode park must remain visible
			}
		} else if err != nil {
			// Census-owed re-detection (Q2): an unhandled or failed allocation
			// is re-detected by the next patrol's declared routed-work view. The
			// legacy poke this used to fire is retired, so a routed key never
			// crosses back to the legacy pool builder.
			fmt.Fprintf(cr.sessionStartStderr(), "%s: routed-work pool allocation for %s: %v; re-detection owed to the next patrol census\n", cr.sessionStartLogPrefix(), hint.WorkID, err) //nolint:errcheck // failure cause must remain visible
		}
	}
	if !result.Handled {
		return
	}
	if result.Created {
		cr.recordRoutedWorkPoolAllocationMaterialized(hint, result.Session)
	}
}

func (cr *CityRuntime) reconcileRoutedWorkPoolAllocation(ctx context.Context, hint routedWorkPoolAllocationHint) (routedWorkPoolAllocationResult, error) {
	if cr == nil || cr.cs == nil || cr.poolMembershipShadow == nil {
		return routedWorkPoolAllocationResult{}, fmt.Errorf("keyed allocation state is unavailable")
	}
	if ctx == nil {
		return routedWorkPoolAllocationResult{}, fmt.Errorf("allocation context is nil")
	}
	if err := ctx.Err(); err != nil {
		return routedWorkPoolAllocationResult{}, err
	}
	if cr.sessionStartOwnershipState() != sessionStartOwnershipKeyed {
		return routedWorkPoolAllocationResult{}, nil
	}
	snapshot, release, err := cr.cs.acquireSessionStartSnapshot()
	if err != nil {
		return routedWorkPoolAllocationResult{}, err
	}
	defer release()
	cr.serviceStateMu.RLock()
	configCurrent := cr.cfg == snapshot.Config
	cr.serviceStateMu.RUnlock()
	if !configCurrent {
		return routedWorkPoolAllocationResult{}, nil
	}
	sourceStore, ok := cr.cs.routedWorkStore(snapshot.Config, hint.SourceStore)
	if !ok || sourceStore == nil {
		return routedWorkPoolAllocationResult{}, fmt.Errorf("source store %q is unavailable", hint.SourceStore)
	}
	work, ready, err := authoritativeReadyRoutedWorkByID(sourceStore, hint.WorkID, time.Now().UTC())
	if err != nil {
		return routedWorkPoolAllocationResult{}, err
	}
	if !ready {
		return routedWorkPoolAllocationResult{}, nil
	}

	agent := findAgentByTemplate(snapshot.Config, hint.PoolTarget)
	if agent == nil || isAgentEffectivelySuspendedWith(snapshot.Config, snapshot.CityPath, agent, loadSuspensionStateBestEffort(snapshot.CityPath)) {
		return routedWorkPoolAllocationResult{}, nil
	}
	namedTemplates := make(map[string]struct{}, len(snapshot.Config.NamedSessions))
	for i := range snapshot.Config.NamedSessions {
		namedTemplates[snapshot.Config.NamedSessions[i].TemplateQualifiedName()] = struct{}{}
	}
	policy := newPoolAllocationShadowPolicy(snapshot.Config, agent, namedTemplates).
		forSourceStore(snapshot.Config, agent, snapshot.CityPath, hint.SourceStore)
	if !demandServableForTemplate(snapshot.Config, work, hint.PoolTarget) {
		return routedWorkPoolAllocationResult{}, nil
	}
	if !policy.supported() {
		return routedWorkPoolAllocationResult{}, nil
	}
	existing, found, findErr := findRoutedWorkPoolSession(snapshot.Store, snapshot.Config, hint)
	if findErr != nil {
		return routedWorkPoolAllocationResult{}, findErr
	}
	if found {
		if err := cr.poolMembershipShadow.replace(snapshot.Config, existing); err != nil {
			return routedWorkPoolAllocationResult{}, fmt.Errorf("publishing existing session membership: %w", err)
		}
		lifecycle := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(existing))
		if lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) {
			lease, leaseErr := cr.newRoutedWorkPoolStartLease(snapshot, existing, hint)
			if leaseErr != nil {
				return routedWorkPoolAllocationResult{Session: existing, Handled: true}, leaseErr
			}
			if err := cr.admitRoutedWorkPoolSession(lease); err != nil {
				return routedWorkPoolAllocationResult{Session: existing, Handled: true}, err
			}
			return routedWorkPoolAllocationResult{Session: existing, Handled: true}, nil
		}
		_, occupied := cr.poolMembershipShadow.observeOccupiedMember(hint.PoolTarget, existing.ID)
		if lifecycle.BaseState == sessionpkg.BaseStateActive && !lifecycle.Terminal && occupied {
			recoveryInfo, recoveryPersisted, readErr := getAuthoritativeSessionStartPersistedRecord(snapshot.Store, existing.ID)
			if readErr != nil {
				return routedWorkPoolAllocationResult{Session: existing, Handled: true}, fmt.Errorf("reading exact pool recovery row: %w", readErr)
			}
			if recoveryInfo.ID != existing.ID {
				return routedWorkPoolAllocationResult{Session: existing, Handled: true}, fmt.Errorf("reading exact pool recovery row %q returned %q", existing.ID, recoveryInfo.ID)
			}
			if err := cr.poolMembershipShadow.replace(snapshot.Config, recoveryInfo); err != nil {
				return routedWorkPoolAllocationResult{Session: existing, Handled: true}, fmt.Errorf("publishing exact pool recovery membership: %w", err)
			}
			recoveryLifecycle := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(recoveryInfo))
			_, recoveryOccupied := cr.poolMembershipShadow.observeOccupiedMember(hint.PoolTarget, recoveryInfo.ID)
			if recoveryLifecycle.BaseState != sessionpkg.BaseStateActive || recoveryLifecycle.Terminal || !recoveryOccupied {
				return routedWorkPoolAllocationResult{}, nil
			}
			if recoveryInfo.SessionName == "" {
				if cr.sessionStartRolloutMode() == rollout.Require {
					return routedWorkPoolAllocationResult{Session: recoveryInfo, Handled: true}, nil
				}
				return routedWorkPoolAllocationResult{}, nil
			}
			liveness := runtime.ObserveFreshLiveness(snapshot.Provider, runtime.LivenessTarget{
				SessionID:            recoveryInfo.ID,
				SessionName:          recoveryInfo.SessionName,
				ProcessNames:         processHints(snapshot.Config, agent),
				IncarnationStartedAt: drainAckIncarnationStartedAt(recoveryInfo),
			})
			if liveness.Running || liveness.Alive {
				return routedWorkPoolAllocationResult{Session: recoveryInfo, Handled: true}, nil
			}
			if !liveness.Complete {
				if cr.sessionStartRolloutMode() == rollout.Require {
					return routedWorkPoolAllocationResult{Session: recoveryInfo, Handled: true}, nil
				}
				return routedWorkPoolAllocationResult{}, nil
			}
			lease, leaseErr := cr.newRoutedWorkPoolRecoveryLease(snapshot, recoveryInfo, recoveryPersisted, hint)
			if leaseErr != nil {
				return routedWorkPoolAllocationResult{Session: recoveryInfo, Handled: true}, leaseErr
			}
			if err := cr.admitRoutedWorkPoolSession(lease); err != nil {
				return routedWorkPoolAllocationResult{Session: recoveryInfo, Handled: true}, err
			}
			return routedWorkPoolAllocationResult{Session: recoveryInfo, Handled: true}, nil
		}
		return routedWorkPoolAllocationResult{}, nil
	}
	request := SessionRequest{
		Template:       hint.PoolTarget,
		BeadPriority:   beadPriority(work),
		Tier:           "new",
		WorkBeadID:     work.ID,
		WorkBeadTitle:  work.Title,
		WorkPack:       strings.TrimSpace(work.Metadata[beadmeta.PackMetadataKey]),
		WorkWorkspace:  strings.TrimSpace(work.Metadata[beadmeta.PackWorkspaceMetadataKey]),
		WorkStoreRef:   hint.SourceStore,
		BrainParentSID: strings.TrimSpace(work.Metadata[beadmeta.BrainParentSIDMetadataKey]),
	}
	bp := &agentBuildParams{
		city:      snapshot.Config,
		cityName:  snapshot.CityName,
		cityPath:  snapshot.CityPath,
		workspace: &snapshot.Config.Workspace,
		agents:    snapshot.Config.Agents,
		providers: snapshot.Config.Providers,
		lookPath:  exec.LookPath,
		sp:        snapshot.Provider,
		rigs:      snapshot.Config.Rigs,
		beadStore: snapshot.Store,
		stderr:    cr.sessionStartStderr(),
	}
	if policy.maxActiveSessions != 0 {
		reused, reuseDisposition, reuseErr := cr.reuseIdleRoutedWorkPoolMember(ctx, snapshot, agent, work, hint, bp, request)
		if reuseErr != nil {
			return routedWorkPoolAllocationResult{}, reuseErr
		}
		switch reuseDisposition {
		case routedWorkPoolReuseReusable:
			return reused, nil
		case routedWorkPoolReuseRefused:
			return routedWorkPoolAllocationResult{}, nil
		}
	}
	decision := decideRoutedWorkPoolAllocationShadow(readyRoutedWorkDemandContribution{
		WorkID:              work.ID,
		PoolTarget:          hint.PoolTarget,
		SourceStore:         hint.SourceStore,
		ContributionPresent: policy.contributionPresent,
		AllocationPolicy:    policy,
	}, cr.poolMembershipShadow.observe(hint.PoolTarget))
	if decision.action != poolAllocationShadowStartOne || decision.poolSlot <= 0 {
		return routedWorkPoolAllocationResult{}, nil
	}
	if !routedWorkPoolProviderHealthy(snapshot.CityPath, snapshot.Config, agent) {
		return routedWorkPoolAllocationResult{}, nil
	}
	_, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(agent, decision.poolSlot)
	metadata := poolTriggerMetadata(bp, agent, qualifiedInstance, request)
	info, err := createPoolSessionBeadWithGuardedAlias(bp, agent, hint.PoolTarget, qualifiedInstance, poolSlot, metadata)
	if err != nil {
		return routedWorkPoolAllocationResult{}, fmt.Errorf("creating one session for pool %q: %w", hint.PoolTarget, err)
	}
	result := routedWorkPoolAllocationResult{Session: info, Handled: true, Created: true}
	if err := cr.poolMembershipShadow.replace(snapshot.Config, info); err != nil {
		return result, fmt.Errorf("publishing created session membership: %w", err)
	}
	lease, err := cr.newRoutedWorkPoolStartLease(snapshot, info, hint)
	if err != nil {
		return result, err
	}
	if err := cr.admitRoutedWorkPoolSession(lease); err != nil {
		return result, err
	}
	return result, nil
}

func (cr *CityRuntime) newRoutedWorkPoolStartLease(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	hint routedWorkPoolAllocationHint,
) (routedWorkPoolStartLease, error) {
	observation, occupied := cr.poolMembershipShadow.observeOccupiedMember(hint.PoolTarget, info.ID)
	if !occupied {
		return routedWorkPoolStartLease{}, fmt.Errorf("certifying pool session %q: pool membership does not contain an occupied member", info.ID)
	}
	lease := routedWorkPoolStartLease{
		SessionID:            info.ID,
		InstanceToken:        strings.TrimSpace(info.InstanceToken),
		ControllerGeneration: snapshot.Generation,
		PoolTarget:           strings.TrimSpace(hint.PoolTarget),
		WorkID:               strings.TrimSpace(hint.WorkID),
		SourceStore:          strings.TrimSpace(hint.SourceStore),
		MembershipRevision:   observation.revision,
	}
	if err := validateRoutedWorkPoolStartLease(lease); err != nil {
		return routedWorkPoolStartLease{}, err
	}
	return lease, nil
}

func (cr *CityRuntime) newRoutedWorkPoolRecoveryLease(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	persisted sessionpkg.PersistedResponse,
	hint routedWorkPoolAllocationHint,
) (routedWorkPoolStartLease, error) {
	lease, err := cr.newRoutedWorkPoolStartLease(snapshot, info, hint)
	if err != nil {
		return routedWorkPoolStartLease{}, err
	}
	lease.RecoverActive = true
	lease.SessionRevision = persisted.Revision
	if err := validateRoutedWorkPoolStartLease(lease); err != nil {
		return routedWorkPoolStartLease{}, err
	}
	return lease, nil
}

func (cr *CityRuntime) authorizeRoutedWorkPoolStart(
	ctx context.Context,
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	lease routedWorkPoolStartLease,
) (bool, error) {
	if cr == nil || cr.cs == nil || cr.poolMembershipShadow == nil || snapshot.Config == nil {
		return false, fmt.Errorf("authorizing pool allocation start: keyed state is unavailable")
	}
	if ctx == nil {
		return false, fmt.Errorf("authorizing pool allocation start: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validateRoutedWorkPoolStartLease(lease); err != nil {
		return false, err
	}
	if lease.RecoverActive {
		current, persisted, readErr := getAuthoritativeSessionStartPersistedRecord(snapshot.Store, lease.SessionID)
		if readErr != nil {
			return false, readErr
		}
		if persisted.Revision != lease.SessionRevision {
			return false, nil
		}
		info = current
	}
	if snapshot.Generation != lease.ControllerGeneration || info.ID != lease.SessionID ||
		strings.TrimSpace(info.InstanceToken) != lease.InstanceToken || info.Closed ||
		!isPoolManagedSessionInfo(info) || isNamedSessionInfo(info) {
		return false, nil
	}
	cr.serviceStateMu.RLock()
	configCurrent := cr.cfg == snapshot.Config
	cr.serviceStateMu.RUnlock()
	if !configCurrent {
		return false, nil
	}

	lifecycleInput := sessionpkg.LifecycleInputFromInfo(info)
	lifecycleInput.Now = time.Now().UTC()
	lifecycleInput.CreatedAt = info.CreatedAt
	lifecycleInput.StaleCreatingAfter = staleCreatingStateTimeout
	lifecycle := sessionpkg.ProjectLifecycle(lifecycleInput)
	if lifecycle.Terminal {
		return false, nil
	}
	if lease.RecoverActive {
		if lease.RecoveryPreWakeCommitted {
			if lifecycle.BaseState != sessionpkg.BaseStateCreating || info.PendingCreateClaim || info.LastWokeAt == "" || info.PendingCreateStartedAt == "" ||
				lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit) || info.SessionName == "" {
				return false, nil
			}
		} else if lifecycle.BaseState != sessionpkg.BaseStateActive || lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) ||
			lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit) || info.SessionName == "" {
			return false, nil
		}
	} else if !lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) {
		return false, nil
	}
	agent := findAgentByTemplate(snapshot.Config, lease.PoolTarget)
	if agent == nil || normalizedSessionTemplateInfo(info, snapshot.Config) != lease.PoolTarget ||
		isAgentEffectivelySuspendedWith(snapshot.Config, snapshot.CityPath, agent, loadSuspensionStateBestEffort(snapshot.CityPath)) {
		return false, nil
	}
	namedTemplates := make(map[string]struct{}, len(snapshot.Config.NamedSessions))
	for i := range snapshot.Config.NamedSessions {
		namedTemplates[snapshot.Config.NamedSessions[i].TemplateQualifiedName()] = struct{}{}
	}
	policy := newPoolAllocationShadowPolicy(snapshot.Config, agent, namedTemplates).
		forSourceStore(snapshot.Config, agent, snapshot.CityPath, lease.SourceStore)
	if !policy.supported() ||
		(policy.maxActiveSessions == 1 && !isCanonicalPoolManagedSessionInfoForTemplate(info, lease.PoolTarget)) ||
		!isEphemeralSessionInfoForAgent(info, agent) || isManualSessionInfoForAgent(info, agent) || info.DependencyOnly ||
		strings.TrimSpace(info.TriggerBeadID) != lease.WorkID ||
		canonicalizeLegacyWorkflowStoreRef(snapshot.Config, snapshot.CityPath, info.TriggerBeadStoreRef) != lease.SourceStore {
		return false, nil
	}
	if lease.RecoverActive {
		liveness := runtime.ObserveFreshLiveness(snapshot.Provider, runtime.LivenessTarget{
			SessionID:            info.ID,
			SessionName:          info.SessionName,
			ProcessNames:         processHints(snapshot.Config, agent),
			IncarnationStartedAt: drainAckIncarnationStartedAt(info),
		})
		if !liveness.Complete || liveness.Running || liveness.Alive {
			return false, nil
		}
	}
	sourceStore, ok := cr.cs.routedWorkStore(snapshot.Config, lease.SourceStore)
	if !ok || sourceStore == nil {
		return false, fmt.Errorf("authorizing pool allocation start: source store %q is unavailable", lease.SourceStore)
	}
	work, ready, err := authoritativeReadyRoutedWorkByID(sourceStore, lease.WorkID, time.Now().UTC())
	if err != nil {
		return false, err
	}
	if !ready || !demandServableForTemplate(snapshot.Config, work, lease.PoolTarget) {
		return false, nil
	}
	observation, occupied := cr.poolMembershipShadow.observeOccupiedMember(lease.PoolTarget, lease.SessionID)
	if !occupied || observation.revision < lease.MembershipRevision ||
		(policy.maxActiveSessions > 0 && observation.occupied > policy.maxActiveSessions) ||
		!routedWorkPoolProviderHealthy(snapshot.CityPath, snapshot.Config, agent) {
		return false, nil
	}
	return true, nil
}

// canonicalizeLegacyWorkflowStoreRef translates the legacy demand collector's
// bare store vocabulary into the canonical workflow store refs every keyed seam
// compares against. The collector names the HQ store "city" and a rig store by
// its bare rig name (build_desired_state.go's activeStores/storeKey), and stamps
// that spelling verbatim into member rows; the keyed seams rebuild their leases
// FROM those rows, then hand the ref to agentutil.AgentReachesWorkflowStore and
// routedWorkStore, both of which speak only "city:<name>"/"rig:<name>". Under
// first-creator-wins, legacy-created members are the norm, so without this
// translation the keyed seams refuse the normal population forever (ga-2oboq).
//
// The mapping is DEFINITE, not a wildcard: "city" is the collector's own name
// for the HQ store and a bare rig name matches a configured rig, so both resolve
// to exactly one store. Anything else is returned unchanged so the downstream
// validation still refuses it rather than guessing. This compatibility is
// deliberately seam-local — storeref.ScopeRigContext and
// agentutil.AgentReachesWorkflowStore are shared vocabulary whose refusal of
// bare refs is a deliberate semantic.
func canonicalizeLegacyWorkflowStoreRef(cfg *config.City, cityPath, storeRef string) string {
	storeRef = strings.TrimSpace(storeRef)
	if storeRef == "" || cfg == nil {
		return storeRef
	}
	if _, scoped := storeref.ScopeRigContext(storeRef); scoped {
		return storeRef
	}
	if storeRef == "city" {
		if canonical := workflowStoreRefForDir(cityPath, cityPath, loadedCityName(cfg, cityPath), cfg); canonical != "" {
			return canonical
		}
		return storeRef
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == storeRef {
			return "rig:" + storeRef
		}
	}
	return storeRef
}

// routedWorkStore resolves a routed-work key's source store ref to the store it
// names. It speaks the canonical vocabulary only; canonicalWorkflowStoreEntries
// is its producer-side mirror and walks the same loop.
func (cs *controllerState) routedWorkStore(cfg *config.City, sourceStore string) (beads.Store, bool) {
	if cs == nil || cfg == nil {
		return nil, false
	}
	sourceStore = strings.TrimSpace(sourceStore)
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.cfg != cfg {
		return nil, false
	}
	cityName := loadedCityName(cfg, cs.cityPath)
	if workflowStoreRefForDir(cs.cityPath, cs.cityPath, cityName, cfg) == sourceStore {
		return cs.cityBeadStore, cs.cityBeadStore != nil
	}
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		rigPath := rig.Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cs.cityPath, rigPath)
		}
		if workflowStoreRefForDir(rigPath, cs.cityPath, cityName, cfg) == sourceStore {
			store := cs.beadStores[rig.Name]
			return store, store != nil
		}
	}
	return nil, false
}

// routedWorkPoolSessionClaims returns every live, open, pool-managed member of
// hint.PoolTarget that already claims hint.WorkID through the durable trigger
// provenance stamped at member creation. The read is deliberately LIVE: a claim
// the other start family wrote moments ago must be visible within the same tick,
// which is exactly what a cached per-tick snapshot cannot promise.
//
// The work item is the identity; the store ref is a SCOPE, matched by
// routedWorkClaimStoreScopeMatches rather than by exact metadata equality. The
// two builders reach this provenance by different routes and stamp different
// spellings of the same city scope ("city" vs "city:<name>"), so an equality
// filter makes each side blind to the other's claim and both materialize a
// member for one work item.
func routedWorkPoolSessionClaims(store beads.Store, cfg *config.City, hint routedWorkPoolAllocationHint) ([]sessionpkg.Info, error) {
	rows, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Metadata: map[string]string{beadmeta.TriggerBeadIDMetadataKey: hint.WorkID},
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("finding existing routed-work pool session: %w", err)
	}
	var found []sessionpkg.Info
	for _, row := range rows {
		if row.Status == "closed" || !isPoolManagedSessionBead(row) || normalizedSessionTemplate(row, cfg) != hint.PoolTarget {
			continue
		}
		if !routedWorkClaimStoreScopeMatches(row.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey], hint.SourceStore) {
			continue
		}
		info, err := sessionFrontDoor(store).Get(row.ID)
		if err != nil {
			return nil, fmt.Errorf("projecting existing routed-work pool session %q: %w", row.ID, err)
		}
		found = append(found, info)
	}
	return found, nil
}

// routedWorkClaimStoreScopeMatches reports whether a claim stamped with
// rowStoreRef can be the claim on work in hintStoreRef. The rule is symmetric:
// an unscoped ref on EITHER side is a wildcard, and two canonical refs match
// when their rig contexts agree — city scope against city scope, or the same
// named rig. Only a canonical disagreement excludes a row.
//
// The symmetry is deliberately unlike rootStoreRefMatchesCandidate, which
// wildcards an unscoped ROOT but rejects an unscoped CANDIDATE against a scoped
// root. There the candidate is a physical store the caller enumerated and can
// always name canonically, so an unscoped candidate really is a different store
// and the strict arm is what collapses duplicate views of one physical row.
//
// Here both sides are provenance stamps written at different moments by callers
// with different vocabularies: poolTriggerMetadata canonicalizes through
// canonicalTriggerWorkStoreRef, while the re-point stamp and the demand
// contribution can carry the collector's bare storeKey ("city", a bare rig
// name) that ScopeRigContext reads as unscoped. An unscoped value here means
// "scope not recorded", not "the legacy unscoped store", so refusing on it
// would blind each side to the other's claim.
func routedWorkClaimStoreScopeMatches(rowStoreRef, hintStoreRef string) bool {
	rowRig, rowScoped := storeref.ScopeRigContext(rowStoreRef)
	hintRig, hintScoped := storeref.ScopeRigContext(hintStoreRef)
	if !rowScoped || !hintScoped {
		return true
	}
	return rowRig == hintRig
}

func findRoutedWorkPoolSession(store beads.Store, cfg *config.City, hint routedWorkPoolAllocationHint) (sessionpkg.Info, bool, error) {
	found, err := routedWorkPoolSessionClaims(store, cfg, hint)
	if err != nil {
		return sessionpkg.Info{}, false, err
	}
	if len(found) > 1 {
		return sessionpkg.Info{}, false, fmt.Errorf("ambiguous routed-work pool sessions %q and %q", found[0].ID, found[1].ID)
	}
	if len(found) == 0 {
		return sessionpkg.Info{}, false, nil
	}
	return found[0], true, nil
}

func routedWorkPoolProviderHealthy(cityPath string, cfg *config.City, agent *config.Agent) bool {
	providerName := strings.TrimSpace(agent.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(agent.InheritedProvider)
	}
	if providerName == "" && cfg != nil {
		providerName = strings.TrimSpace(cfg.Workspace.Provider)
	}
	healthy, present := loadProviderHealthSnapshot(cityPath).check(providerName)
	return !present || healthy
}

func (cr *CityRuntime) admitRoutedWorkPoolSession(lease routedWorkPoolStartLease) error {
	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	cr.sessionStartMu.Unlock()
	if !owned || controller == nil {
		return fmt.Errorf("exact-start controller is unavailable for pool session admission")
	}
	outcome, err := controller.AdmitPoolAllocation(lease)
	if err != nil {
		controller.RequestAudit()
		return fmt.Errorf("admitting pool session %q: %w", lease.SessionID, err)
	}
	if outcome == sessionStartAdmissionOverflow {
		controller.RequestAudit()
		return fmt.Errorf("admitting pool session %q: exact-start queue overflow", lease.SessionID)
	}
	return nil
}

// recordRoutedWorkPoolAllocationOverflow traces a routed-work key the
// pool-allocation hint channel dropped. Q2's resolution retires the legacy
// fallback poke this used to fire: the detector sweep's declared routed-work
// view re-detects an unallocated key on the next patrol, so an overflow costs
// discovery latency and never work. The sweep must not block, retry-loop, or
// act itself (DETECTOR.md §2, degradation rules).
func (cr *CityRuntime) recordRoutedWorkPoolAllocationOverflow(contribution readyRoutedWorkDemandContribution) {
	if cr == nil || cr.trace == nil {
		return
	}
	workID := strings.TrimSpace(contribution.WorkID)
	if workID == "" {
		return
	}
	cr.serviceStateMu.RLock()
	cfg := cr.cfg
	cr.serviceStateMu.RUnlock()
	now := time.Now().UTC()
	cycle := cr.trace.BeginCycle(TraceTickTriggerControl, "pool_allocation.overflow", now, cfg)
	if cycle == nil {
		return
	}
	cycle.RecordControllerOperation(
		TraceSitePoolAllocationMaterialize,
		TraceReasonAdmissionOverflow,
		TraceOutcomeSkipped,
		"pool_allocation.overflow",
		0,
		map[string]any{
			"effect_applied": false,
			"effect_owner":   detectorKeyedEffectOwner,
			"work_id":        workID,
			"pool_target":    strings.TrimSpace(contribution.PoolTarget),
			"source_store":   strings.TrimSpace(contribution.SourceStore),
			"census_owed":    true,
		})
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil {
		fmt.Fprintf(shadowWorkerStderr(cr.stderr), "%s: routed-work pool allocation overflow trace: %v\n", cr.logPrefix, err) //nolint:errcheck // tracing must not affect reconciliation
	}
}

func (cr *CityRuntime) sessionStartRolloutMode() rollout.Mode {
	if cr == nil {
		return rollout.Auto
	}
	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()
	return cr.sessionStartMode
}

func (cr *CityRuntime) recordRoutedWorkPoolAllocationMaterialized(hint routedWorkPoolAllocationHint, info sessionpkg.Info) {
	if cr == nil || cr.trace == nil || info.ID == "" {
		return
	}
	now := time.Now().UTC()
	startedAt := hint.EnqueuedAt
	if startedAt.IsZero() || startedAt.After(now) {
		startedAt = now
	}
	cr.serviceStateMu.RLock()
	cfg := cr.cfg
	cr.serviceStateMu.RUnlock()
	cycle := cr.trace.BeginCycle(TraceTickTriggerControl, "pool_allocation.materialize", startedAt, cfg)
	if cycle == nil {
		return
	}
	eventTimestampValid := !hint.EventAt.IsZero() && !hint.EventAt.After(now)
	eventLatency := int64(0)
	if eventTimestampValid {
		eventLatency = now.Sub(hint.EventAt).Nanoseconds()
	}
	cycle.RecordControllerOperation(
		TraceSitePoolAllocationMaterialize,
		TraceReasonRetained,
		TraceOutcomeApplied,
		"pool_allocation.materialize",
		now.Sub(startedAt),
		map[string]any{
			"work_id":                     hint.WorkID,
			"pool_target":                 hint.PoolTarget,
			"source_store":                hint.SourceStore,
			"session_id":                  info.ID,
			"event_timestamp_valid":       eventTimestampValid,
			"event_to_materialization_ns": eventLatency,
			"queue_to_materialization_ns": now.Sub(startedAt).Nanoseconds(),
			"effect_owner":                "keyed",
			"effect_applied":              true,
		},
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: routed-work pool allocation trace: %v\n", cr.sessionStartLogPrefix(), err) //nolint:errcheck // tracing cannot affect allocation
	}
}
