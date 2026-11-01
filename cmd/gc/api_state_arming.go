package main

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout"
)

// ArmingConfigurationError reports that a scope cannot arm ADR-0019 leases or
// fencing because of how it is configured or what its store can do.
type ArmingConfigurationError struct {
	Scope  string
	Reason string
}

// Error names the scope and the reason it refuses to arm.
func (e *ArmingConfigurationError) Error() string {
	return fmt.Sprintf("scope %s refuses to arm: configuration error: %s", e.Scope, e.Reason)
}

func scopeArmingPrecondition(scope string, store beads.Store, flags rollout.Flags) error {
	for _, gate := range []struct {
		key  string
		mode rollout.Mode
	}{
		{rollout.KeyBeadsConditionalWrites, flags.BeadsConditionalWrites()},
		{rollout.KeyBeadsGuardedRelease, flags.BeadsGuardedRelease()},
	} {
		origin := flags.OriginOf(gate.key)
		explicit := origin == rollout.OriginConfig || origin == rollout.OriginEnv
		switch {
		case gate.mode == rollout.ModeUnset || !explicit:
			return &ArmingConfigurationError{Scope: scope, Reason: fmt.Sprintf("%s is unset (resolved %q from origin %s); arming requires an explicit %q", gate.key, gate.mode, origin, rollout.Require)}
		case gate.mode != rollout.Require:
			return &ArmingConfigurationError{Scope: scope, Reason: fmt.Sprintf("%s is %q (origin %s); arming requires an explicit %q", gate.key, gate.mode, origin, rollout.Require)}
		}
	}
	if capable, reason := beads.ProbeConditionalWrites(store); !capable {
		return &ArmingConfigurationError{Scope: scope, Reason: fmt.Sprintf("store %T cannot fence: %s", store, reason)}
	}
	return nil
}

type armingScope struct {
	name  string
	store beads.Store
}

func (cs *controllerState) armingScopes() []armingScope {
	scopes := []armingScope{{"city", cs.cityBeadStore}}
	rigs := make([]string, 0, len(cs.beadStores))
	for name := range cs.beadStores {
		rigs = append(rigs, name)
	}
	sort.Strings(rigs)
	for _, name := range rigs {
		scopes = append(scopes, armingScope{"rig/" + name, cs.beadStores[name]})
	}
	for _, store := range cs.storageRoutes.distinctEngines() {
		scopes = append(scopes, armingScope{"binding/" + cs.storageRoutes.binding, store})
	}
	return scopes
}

func (cs *controllerState) recordArmingReadiness() {
	readiness := make(map[string]error)
	for _, scope := range cs.armingScopes() {
		err := scopeArmingPrecondition(scope.name, scope.store, cs.rolloutFlags)
		if err != nil {
			cs.rolloutWarnf("api: WARNING: %v\n", err)
		}
		if prev := readiness[scope.name]; prev == nil {
			readiness[scope.name] = err
		}
	}
	cs.armingReadiness = readiness
}

// ArmingReadiness returns the startup arming-precondition result for a scope
// named "city", "rig/<name>" or "binding/<name>": nil when the scope may arm,
// an *ArmingConfigurationError otherwise, including for a scope that startup
// never checked.
func (cs *controllerState) ArmingReadiness(scope string) error {
	if err, ok := cs.armingReadiness[scope]; ok {
		return err
	}
	return &ArmingConfigurationError{Scope: scope, Reason: "scope was not checked at startup"}
}
