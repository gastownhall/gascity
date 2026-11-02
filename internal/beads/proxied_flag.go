package beads

import (
	"os"
	"strings"
	"time"
)

const (
	// proxiedNativeEnv opts a binary into serving reads natively over bd's
	// proxy data port. It is off by default and stays off for the whole of
	// PR2's rollout: with it unset, every proxied scope takes exactly the
	// store, the diagnostic and the fork count it takes today.
	proxiedNativeEnv = "GC_BEADS_PROXIED_NATIVE"

	// proxiedGuardIntervalEnv overrides the long-lived guard tick period.
	proxiedGuardIntervalEnv = "GC_BEADS_PROXIED_GUARD_INTERVAL"

	// proxiedReadBudgetEnv overrides the wall-clock budget one proxied native
	// read may spend on its whole reconnect-and-retry chain.
	proxiedReadBudgetEnv = "GC_BEADS_PROXIED_READ_BUDGET"

	// proxiedGuardIntervalDefault is the design's re-pin/drift cadence.
	proxiedGuardIntervalDefault = 15 * time.Second

	// proxiedKnobFloor is the shared floor of both duration knobs. It keeps a
	// misconfigured interval from turning the guard into a spin (the tick reads
	// a 200-byte record and runs one pooled cursor query, and doing that faster
	// than once a second buys nothing) and keeps a fat-fingered read budget
	// from expiring before a healthy round trip can finish.
	proxiedKnobFloor = time.Second

	// proxiedReadBudgetDefault is deliberately far below the direct lane's
	// 90s nativeReadRetryBudget. The 90s figure exists to span a MANAGED Dolt
	// hard-kill and rebind, which gc itself performs and can therefore wait
	// out. A bd-owned proxy is not gc's to restart: the recovery is to demote
	// to BdStore and let the next open re-admit, and a caller should not pay a
	// minute and a half of mysql i/o timeouts to find that out.
	proxiedReadBudgetDefault = 10 * time.Second
)

// proxiedNativeEnabled reports whether the proxied-native read lane is on.
//
// It reads the same two spellings forceNativeFallback accepts ("1" and a
// case-insensitive "true"), because an operator who has learned one gc boolean
// has learned them all, and a third spelling in a rollout flag is how a lane
// silently stays off in the run that was supposed to prove it.
//
// GC_BEADS_FORCE_FALLBACK still wins: it is checked in OpenStoreAtForCity
// before the proxied arm is reached, so the escape hatch cannot be defeated by
// also setting this one.
func proxiedNativeEnabled() bool {
	value := strings.TrimSpace(os.Getenv(proxiedNativeEnv))
	return value == "1" || strings.EqualFold(value, "true")
}

// proxiedGuardInterval returns the long-lived guard tick period: the default
// unless GC_BEADS_PROXIED_GUARD_INTERVAL parses to a positive duration, floored
// at proxiedKnobFloor. An unparseable or non-positive value takes the default
// rather than failing an open — a bad knob must not be able to turn a working
// city off.
func proxiedGuardInterval() time.Duration {
	return proxiedEnvDuration(proxiedGuardIntervalEnv, proxiedGuardIntervalDefault)
}

// ProxiedReadBudget returns the per-read wall-clock budget for a proxied native
// handle, under the same default-on-nonsense rule as the guard interval.
//
// It is exported because the OPENER lives in cmd/gc: the budget is applied with
// WithNativeReadRetryBudget at open time, so the composition root has to be able
// to read the knob. Applying it structurally inside the proxied open instead
// would hide the one number an operator is most likely to want to change.
func ProxiedReadBudget() time.Duration {
	return proxiedEnvDuration(proxiedReadBudgetEnv, proxiedReadBudgetDefault)
}

func proxiedEnvDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	if parsed < proxiedKnobFloor {
		return proxiedKnobFloor
	}
	return parsed
}
