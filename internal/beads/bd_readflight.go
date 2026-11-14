package beads

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/singleflight"
)

// bdReadFlights collapses concurrent identical bd READ-ONLY subprocess calls
// into a single spawn whose stdout and classified error every joined caller
// shares (workflow-rx4). The supervisor fans out the same probes — per-session
// `bd list --assignee=…`, /work/ready sweeps, order dispatch — from many
// goroutines at once; under dolt slowness each duplicate used to become its
// own bd child, piling past what the box can hold even with the subprocess
// semaphore (bd_concurrency.go) bounding total concurrency. Dedup shrinks the
// pile-up itself: N identical in-flight reads cost one child and one read-lane
// slot.
//
// This is single-flight, NOT a cache: the group forgets a key the moment its
// call returns, so a call arriving after the flight completes spawns fresh —
// bd state mutates constantly and a stale answer would be wrong. Only reads
// dedup (bdReadOnlyArgs); writes always execute because mutations must not be
// shared or elided.
//
// The flight wraps the semaphore acquisition, so the whole group costs ONE
// read-lane slot and waiters never queue on slots. The semaphore therefore
// still bounds distinct concurrent reads; dedup bounds identical ones.
var bdReadFlights singleflight.Group

// bdReadFlightResult is what one shared flight hands every joined caller:
// the leader's stdout bytes and classified error verbatim, plus the trace
// fields so each caller can emit its own status line (per-caller GC_BD_TRACE
// attribution is preserved even though only one subprocess ran).
type bdReadFlightResult struct {
	out       []byte
	resultErr error
	status    string
	traceErr  error
}

// bdReadFlightCounters tally deduped read invocations vs actual spawns
// (leaders) for observability; joins = calls - leaders. There is no
// dedicated telemetry event for this layer yet, so they stay package-local.
var (
	bdReadFlightCalls   atomic.Uint64
	bdReadFlightLeaders atomic.Uint64
)

// bdReadFlightKey builds the singleflight key for one bd read invocation.
// Everything that can change the child's stdout must be in the key: the
// working dir (which .beads/ it reads), the resolved executable (BD_BIN pins
// it), the full argument vector, and the EFFECTIVE child environment — two
// runners with different env overrides can legitimately see different bd
// state, so the merged pre-OTEL-translation environ is hashed whole. Hashing
// inputs rather than the post-translation env keeps key computation free of
// the `bd version` capability probe; the translation is a pure function of
// (environ, execName) via the per-binary cached probe answer, and execName is
// already in the key.
//
// The hash is over length-prefixed fields, so no tuple of (dir, execName,
// args…, environ…) can collide by concatenation ambiguity.
func bdReadFlightKey(dir, execName string, args []string, environ []string) string {
	h := sha256.New()
	writeField := func(s string) {
		h.Write([]byte(s)) //nolint:errcheck // hash.Hash never errors
		h.Write([]byte{0}) //nolint:errcheck // hash.Hash never errors
	}
	writeField(dir)
	writeField(execName)
	for _, a := range args {
		writeField(a)
	}
	for _, e := range environ {
		writeField(e)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// bdReadFlightEnv mirrors the environment merge execEnvFor applies for a bd
// child, stopping before the OTEL translation (see bdReadFlightKey). It must
// reproduce the withoutAmbientBeads scrub exactly or two runners with
// different effective envs could share a flight.
func bdReadFlightEnv(baseEnv []string, overrides map[string]string, withoutAmbientBeads bool) []string {
	if withoutAmbientBeads {
		baseEnv = envWithoutPrefix(baseEnv, beadsEnvPrefix)
		overrides = cloneEnvOverridesWithoutAmbientBeads(overrides)
	}
	return mergeEnv(baseEnv, overrides)
}

// cloneEnvOverridesWithoutAmbientBeads copies overrides and drops the
// BEADS_*-prefixed keys set to "" — the same deletion the runner applies so
// an ambient-strip runner cannot inherit a namespace value through an empty
// override.
func cloneEnvOverridesWithoutAmbientBeads(overrides map[string]string) map[string]string {
	out := make(map[string]string, len(overrides))
	for key, value := range overrides {
		if strings.HasPrefix(key, beadsEnvPrefix) && value == "" {
			continue
		}
		out[key] = value
	}
	return out
}
