package session

// This file adds the typed front door for the session-circuit-breaker metadata
// cluster (RECONCILER-FRONT-DOOR-SPEC sec 5.3, Step 5). The reconciler's
// Phase-0.5 breaker restore was the last decision-path read that cracked raw
// beads.Bead.Metadata; it now speaks CircuitState instead. CircuitState is a
// distinct concern from session.Info: the breaker's timers and counters are not
// lifecycle-decision facts, so they are deliberately NOT mirrored on Info.

// The session-circuit-breaker metadata cluster. Two of the keys
// (SessionCircuitStateMetadataKey, SessionCircuitResetGenerationMetadataKey)
// already live in lifecycle_projection.go; the remaining seven are declared here
// so the whole cluster's key vocabulary is confined to the session package (the
// codec edge, per spec decision 2). The cmd/gc circuit-breaker constants alias
// these, keeping one source of truth for the string values and guarding them
// against drift.
const (
	// SessionCircuitRestartsMetadataKey holds the JSON array of RFC3339Nano
	// restart timestamps within the rolling window.
	SessionCircuitRestartsMetadataKey = "session_circuit_restarts"
	// SessionCircuitLastRestartMetadataKey holds the most recent restart time.
	SessionCircuitLastRestartMetadataKey = "session_circuit_last_restart"
	// SessionCircuitLastProgressMetadataKey holds the most recent observed
	// progress time.
	SessionCircuitLastProgressMetadataKey = "session_circuit_last_progress"
	// SessionCircuitLastObservedMetadataKey holds the most recent progress
	// signature observation time.
	SessionCircuitLastObservedMetadataKey = "session_circuit_last_observed"
	// SessionCircuitProgressSignatureMetadataKey holds the last observed
	// assigned-work status signature.
	SessionCircuitProgressSignatureMetadataKey = "session_circuit_progress_signature"
	// SessionCircuitOpenedAtMetadataKey holds the time the breaker last opened.
	SessionCircuitOpenedAtMetadataKey = "session_circuit_opened_at"
	// SessionCircuitOpenRestartCountMetadataKey holds the restart count captured
	// at the moment the breaker opened.
	SessionCircuitOpenRestartCountMetadataKey = "session_circuit_open_restart_count"
)

// CircuitState is a typed projection of the persisted session-circuit-breaker
// metadata cluster off a session bead. Each field carries its key's raw string
// value verbatim; the breaker (cmd/gc) owns all parsing — restart-history JSON,
// timestamps, restart count, state kind, reset generation. Routing a read
// through CircuitState is therefore byte-identical to the direct meta[key] reads
// it replaces: the codec moves serialization only, never a decision.
type CircuitState struct {
	State             string // SessionCircuitStateMetadataKey
	Restarts          string // SessionCircuitRestartsMetadataKey (JSON array of RFC3339Nano)
	LastRestart       string // SessionCircuitLastRestartMetadataKey
	LastProgress      string // SessionCircuitLastProgressMetadataKey
	LastObserved      string // SessionCircuitLastObservedMetadataKey
	ProgressSignature string // SessionCircuitProgressSignatureMetadataKey
	OpenedAt          string // SessionCircuitOpenedAtMetadataKey
	OpenRestartCount  string // SessionCircuitOpenRestartCountMetadataKey
	ResetGeneration   string // SessionCircuitResetGenerationMetadataKey
}

// CircuitStateFromMetadata projects a session bead's metadata map onto a typed
// CircuitState, reading only the session_circuit_* keys verbatim. It is pure and
// side-effect-free, so it is byte-identical to the direct meta[key] reads the
// circuit breaker performed before the front-door migration. A nil or empty map
// yields the zero CircuitState (every field "").
func CircuitStateFromMetadata(meta map[string]string) CircuitState {
	return CircuitState{
		State:             meta[SessionCircuitStateMetadataKey],
		Restarts:          meta[SessionCircuitRestartsMetadataKey],
		LastRestart:       meta[SessionCircuitLastRestartMetadataKey],
		LastProgress:      meta[SessionCircuitLastProgressMetadataKey],
		LastObserved:      meta[SessionCircuitLastObservedMetadataKey],
		ProgressSignature: meta[SessionCircuitProgressSignatureMetadataKey],
		OpenedAt:          meta[SessionCircuitOpenedAtMetadataKey],
		OpenRestartCount:  meta[SessionCircuitOpenRestartCountMetadataKey],
		ResetGeneration:   meta[SessionCircuitResetGenerationMetadataKey],
	}
}

// CircuitState returns the persisted session-circuit-breaker metadata cluster
// for id as a typed CircuitState (each field the raw string; "" when unset).
//
// It is the store-authoritative front door for the raw store.Get(id) + read of
// the session_circuit_* keys the circuit breaker performs during restore. Like
// CircuitResetGeneration and PersistedMarkers, the bead read and the
// metadata-key access are confined here; the caller owns parsing each value and
// its own diagnostic wrapping (the error is returned bare). It does NOT validate
// the bead as a session bead: the raw read it replaces did not either.
func (s *Store) CircuitState(id string) (CircuitState, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return CircuitState{}, err
	}
	return CircuitStateFromMetadata(b.Metadata), nil
}
