package main

import "strconv"

// sessionIdentityInputs are the durable facts that determine a session bead's
// canonical identity metadata. It is deliberately a flat value so the identity
// contract can be derived purely and table-tested, independent of the tmux /
// provider boot paths that historically stamped identity inline.
type sessionIdentityInputs struct {
	// AgentName is the qualified agent (or instance) name. When empty, the
	// caller stamps agent_name itself on a later path (the adoption barrier
	// resolves it after pool-base matching).
	AgentName string
	// SessionName is the canonical runtime session name. When empty, the
	// canonical name is assigned later (pending pool instances derive it from
	// the instance token at create time).
	SessionName string
	// State is the lifecycle state metadata value (e.g. "active" or the
	// start-pending sentinel).
	State string
	// Generation and ContinuationEpoch are the monotonic identity counters.
	Generation        int
	ContinuationEpoch int
	// InstanceToken uniquely identifies this incarnation of the session.
	InstanceToken string
	// PoolSlot is the pool instance slot; 0 for singleton / non-pool sessions.
	PoolSlot int
}

// desiredSessionIdentity states the canonical session-identity contract once,
// as a pure derivation from durable facts. Every arrival path (fresh create,
// adoption) previously hand-rolled its own subset of these keys; centralizing
// the derivation is the S19 seed that makes "identity key omitted because you
// arrived via the wrong path" harder to reintroduce.
//
// Keys are emitted only when meaningful: agent_name/session_name/pool_slot are
// omitted when their inputs are zero so callers that assign those later (the
// adoption barrier, pending pool creates) stay byte-identical.
func desiredSessionIdentity(in sessionIdentityInputs) map[string]string {
	meta := map[string]string{
		"state":              in.State,
		"generation":         strconv.Itoa(in.Generation),
		"continuation_epoch": strconv.Itoa(in.ContinuationEpoch),
		"instance_token":     in.InstanceToken,
	}
	if in.AgentName != "" {
		meta["agent_name"] = in.AgentName
	}
	if in.SessionName != "" {
		meta["session_name"] = in.SessionName
	}
	if in.PoolSlot > 0 {
		meta["pool_slot"] = strconv.Itoa(in.PoolSlot)
	}
	return meta
}
