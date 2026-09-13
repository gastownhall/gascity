package productmetrics

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"
)

const (
	configFileName           = "config.toml"
	currentStateSchema       = uint64(1)
	maximumConfigBytes       = 16 * 1024
	maximumStateCounter      = uint64(math.MaxInt64 - 1)
	initialCounterNamespace  = uint64(1)
	terminalCounterNamespace = maximumStateCounter
)

var (
	errStateSchemaNewer = errors.New("productmetrics: state schema is newer than this binary")
	errStateInvalid     = errors.New("productmetrics: invalid state config")
)

type preference string

const (
	preferenceUnset    preference = "unset"
	preferenceEnabled  preference = "enabled"
	preferenceDisabled preference = "disabled"
)

type cleanupKind string

const (
	cleanupNone    cleanupKind = "none"
	cleanupDisable cleanupKind = "disable"
	cleanupPause   cleanupKind = "pause"
)

// persistedState is the complete atomic consent, identity, and generation
// record. There is intentionally no separately authoritative identity file.
type persistedState struct {
	StateSchema               uint64      `toml:"state_schema"`
	CounterNamespace          uint64      `toml:"counter_namespace"`
	StateGeneration           uint64      `toml:"state_generation"`
	Preference                preference  `toml:"preference"`
	RequiredNoticeVersion     uint64      `toml:"required_notice_version"`
	AcceptedNoticeVersion     uint64      `toml:"accepted_notice_version"`
	InstallationID            string      `toml:"installation_id,omitempty"`
	SpoolGeneration           string      `toml:"spool_generation,omitempty"`
	CleanupKind               cleanupKind `toml:"cleanup_kind"`
	CleanupEpoch              uint64      `toml:"cleanup_epoch"`
	PausedThroughMetricsEpoch uint64      `toml:"paused_through_metrics_epoch"`
}

type stateWire persistedState

type counterNamespaceRecoveryWire struct {
	StateSchema      uint64 `toml:"state_schema"`
	CounterNamespace uint64 `toml:"counter_namespace"`
}

var requiredStateKeys = map[string]struct{}{
	"state_schema":                 {},
	"counter_namespace":            {},
	"state_generation":             {},
	"preference":                   {},
	"required_notice_version":      {},
	"accepted_notice_version":      {},
	"cleanup_kind":                 {},
	"cleanup_epoch":                {},
	"paused_through_metrics_epoch": {},
}

var optionalStateKeys = map[string]struct{}{
	"installation_id":  {},
	"spool_generation": {},
}

func encodePersistedState(state persistedState) ([]byte, error) {
	if err := validatePersistedState(state); err != nil {
		return nil, err
	}
	return encodeStateWire(state)
}

func encodeStateWire(state persistedState) ([]byte, error) {
	var output bytes.Buffer
	if err := toml.NewEncoder(&output).Encode(stateWire(state)); err != nil {
		return nil, fmt.Errorf("productmetrics: encode state config: %w", err)
	}
	if output.Len() > maximumConfigBytes {
		return nil, fmt.Errorf("productmetrics: encoded state config exceeds %d bytes", maximumConfigBytes)
	}
	return output.Bytes(), nil
}

func decodePersistedState(data []byte) (persistedState, error) {
	if len(data) == 0 {
		return persistedState{}, fmt.Errorf("%w: empty config", errStateInvalid)
	}
	if len(data) > maximumConfigBytes {
		return persistedState{}, fmt.Errorf("%w: config exceeds %d bytes", errStateInvalid, maximumConfigBytes)
	}
	var wire stateWire
	metadata, err := toml.Decode(string(data), &wire)
	if err != nil {
		return persistedState{}, fmt.Errorf("%w: decode TOML: %w", errStateInvalid, err)
	}
	seen := make(map[string]struct{}, len(metadata.Keys()))
	for _, key := range metadata.Keys() {
		parts := []string(key)
		if len(parts) != 1 {
			return persistedState{}, fmt.Errorf("%w: nested key %q is not allowed", errStateInvalid, key.String())
		}
		name := parts[0]
		if _, required := requiredStateKeys[name]; !required {
			if _, optional := optionalStateKeys[name]; !optional {
				return persistedState{}, fmt.Errorf("%w: unknown or non-canonical field %q", errStateInvalid, name)
			}
		}
		seen[name] = struct{}{}
	}
	for key := range requiredStateKeys {
		if _, ok := seen[key]; !ok {
			return persistedState{}, fmt.Errorf("%w: required field %q is absent", errStateInvalid, key)
		}
	}
	if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
		return persistedState{}, fmt.Errorf("%w: unrecognized field %q", errStateInvalid, undecoded[0].String())
	}
	state := persistedState(wire)
	if state.StateSchema > currentStateSchema {
		return persistedState{}, fmt.Errorf("%w: got %d, maximum %d", errStateSchemaNewer, state.StateSchema, currentStateSchema)
	}
	if err := validatePersistedState(state); err != nil {
		return persistedState{}, err
	}
	return state, nil
}

// recoveryCounterNamespace returns a namespace that cannot match authority
// captured from a valid record before it became corrupt. When the two fields
// cannot be decoded under the current schema, the permanent terminal
// namespace is the only entropy-free choice that cannot reuse an older value.
func recoveryCounterNamespace(data []byte) uint64 {
	var wire counterNamespaceRecoveryWire
	metadata, err := toml.Decode(string(data), &wire)
	if err != nil || !metadata.IsDefined("state_schema") || !metadata.IsDefined("counter_namespace") ||
		wire.StateSchema != currentStateSchema || wire.CounterNamespace == 0 || wire.CounterNamespace >= terminalCounterNamespace {
		return terminalCounterNamespace
	}
	return wire.CounterNamespace + 1
}

func validatePersistedState(state persistedState) error {
	invalid := func(format string, values ...any) error {
		return fmt.Errorf("%w: %s", errStateInvalid, fmt.Sprintf(format, values...))
	}
	if state.StateSchema != currentStateSchema {
		return invalid("state_schema is %d, want %d", state.StateSchema, currentStateSchema)
	}
	if state.CounterNamespace == 0 || state.CounterNamespace > terminalCounterNamespace {
		return invalid("counter_namespace is outside the valid range")
	}
	if state.StateGeneration == 0 || state.StateGeneration >= maximumStateCounter {
		return invalid("state_generation is outside the mutable range")
	}
	if state.CleanupEpoch >= maximumStateCounter {
		return invalid("cleanup_epoch is outside the mutable range")
	}
	if state.AcceptedNoticeVersion > state.RequiredNoticeVersion {
		return invalid("accepted_notice_version exceeds required_notice_version")
	}
	if state.Preference != preferenceUnset && state.Preference != preferenceEnabled && state.Preference != preferenceDisabled {
		return invalid("unknown preference %q", state.Preference)
	}
	if state.CleanupKind != cleanupNone && state.CleanupKind != cleanupDisable && state.CleanupKind != cleanupPause {
		return invalid("unknown cleanup_kind %q", state.CleanupKind)
	}
	if state.InstallationID != "" {
		if err := validateCanonicalUUIDv4(state.InstallationID); err != nil {
			return invalid("installation_id: %v", err)
		}
	}
	if state.SpoolGeneration != "" {
		if err := validateCanonicalUUIDv4(state.SpoolGeneration); err != nil {
			return invalid("spool_generation: %v", err)
		}
	}

	switch state.Preference {
	case preferenceUnset:
		if state.InstallationID != "" || state.SpoolGeneration != "" || state.CleanupKind != cleanupNone || state.PausedThroughMetricsEpoch != 0 {
			return invalid("unset preference contains active or cleanup state")
		}
	case preferenceDisabled:
		if state.InstallationID != "" || state.SpoolGeneration != "" || state.PausedThroughMetricsEpoch != 0 {
			return invalid("disabled preference contains identity, spool, or pause state")
		}
		if state.CleanupKind == cleanupPause {
			return invalid("disabled preference cannot own pause cleanup")
		}
	case preferenceEnabled:
		if state.RequiredNoticeVersion == 0 || state.InstallationID == "" || state.AcceptedNoticeVersion == 0 {
			return invalid("enabled preference requires accepted notice and installation ID")
		}
		if state.CleanupKind == cleanupDisable {
			return invalid("enabled preference cannot own disable cleanup")
		}
		if state.CleanupKind == cleanupPause && state.PausedThroughMetricsEpoch == 0 {
			return invalid("pause cleanup requires a covered metrics epoch")
		}
		inactive := state.AcceptedNoticeVersion < state.RequiredNoticeVersion || state.CleanupKind != cleanupNone
		if inactive && state.SpoolGeneration != "" {
			return invalid("inactive enabled state contains a spool generation")
		}
		if !inactive && state.PausedThroughMetricsEpoch == 0 && state.SpoolGeneration == "" {
			return invalid("active enabled state lacks a spool generation")
		}
	}
	if state.CleanupKind != cleanupNone && state.CleanupEpoch == 0 {
		return invalid("active cleanup requires a positive cleanup_epoch")
	}
	if state.CounterNamespace == terminalCounterNamespace && state.SpoolGeneration != "" {
		return invalid("terminal counter namespace contains an active spool generation")
	}
	return nil
}

func validateCanonicalUUIDv4(value string) error {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return errors.New("not a UUID")
	}
	if parsed.String() != value || strings.ToLower(value) != value {
		return errors.New("UUID is not canonical lowercase text")
	}
	if parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 {
		return errors.New("UUID is not RFC 4122 version 4")
	}
	return nil
}

func incrementStateGeneration(state *persistedState) error {
	if state.StateGeneration >= maximumStateCounter-1 {
		return errors.New("productmetrics: state generation exhausted")
	}
	state.StateGeneration++
	return nil
}

func incrementCleanupEpoch(state *persistedState) error {
	if state.CleanupEpoch >= maximumStateCounter-1 {
		return errors.New("productmetrics: cleanup epoch exhausted")
	}
	state.CleanupEpoch++
	return nil
}

func advanceCounterNamespace(state *persistedState) error {
	if state.CounterNamespace >= terminalCounterNamespace {
		return errors.New("productmetrics: counter namespace exhausted")
	}
	state.CounterNamespace++
	return nil
}
