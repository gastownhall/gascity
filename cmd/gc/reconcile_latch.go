package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// latchRefusal is one configured feature v2 does not support yet. While any
// refusal applies, the latch refuses v2 rather than run the city without the
// feature; ParityPR is the parity slice that lifts it (IMPLEMENTATION-PLAN §6).
type latchRefusal struct {
	Feature  string // what v2 would silently drop
	Config   string // the composed-config setting that enables it
	ParityPR string
}

// String renders the refusal as it appears in the latch error and in doctor.
func (r latchRefusal) String() string {
	return fmt.Sprintf("%s (%s) is not available under v2 until %s", r.Config, r.Feature, r.ParityPR)
}

// v2SessionRuntimeParity maps the [session] provider names whose runtime v2
// does not support yet to the parity slice for each. hybrid routes sessions
// to tmux or k8s, so it waits on the k8s backend. The exec: and ssh: prefixes
// and pack-declared runtimes (exec proxies) are matched in
// v2SessionRuntimeRefusal.
var v2SessionRuntimeParity = map[string]string{
	"k8s":      "PAR-K8S",
	"hybrid":   "PAR-K8S",
	"herdr":    "PAR-HERDR",
	"t3bridge": "PAR-T3",
}

// v2LatchRefusals lists every refusal the composed config hits, in a fixed
// order. It is pure: it reads only cfg, never a store or the environment, so
// the latch and the doctor dry run see the same list.
func v2LatchRefusals(cfg *config.City) []latchRefusal {
	var out []latchRefusal
	if cfg.Daemon.SessionCircuitBreaker {
		out = append(out, latchRefusal{"the identity circuit breaker", "[daemon] session_circuit_breaker = true", "PAR-BRK"})
	}
	var dependsOn, scaleCheck, maxAge []string
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if len(a.DependsOn) > 0 {
			dependsOn = append(dependsOn, a.QualifiedName())
		}
		if strings.TrimSpace(a.ScaleCheck) != "" {
			scaleCheck = append(scaleCheck, a.QualifiedName())
		}
		if a.MaxSessionAgeDuration() > 0 {
			maxAge = append(maxAge, a.QualifiedName())
		}
	}
	for _, c := range []struct {
		agents                 []string
		key, feature, parityPR string
	}{
		{dependsOn, "depends_on", "dependency-gated wakes and floors", "PAR-DEP"},
		{scaleCheck, "scale_check", "custom scale_check demand", "PAR-SC"},
		{maxAge, "max_session_age", "max-age restarts", "PAR-AGE"},
	} {
		if len(c.agents) > 0 {
			out = append(out, latchRefusal{c.feature, fmt.Sprintf("agent %s on %s", c.key, strings.Join(c.agents, ", ")), c.parityPR})
		}
	}
	if cfg.Session.ProgressStallTimeoutDuration() > 0 {
		out = append(out, latchRefusal{"progress-stall recycling", "[session] progress_stall_timeout", "PAR-STALL-1"})
	}
	if cfg.Session.ClaimHolderStallTimeoutDuration() > 0 {
		out = append(out, latchRefusal{"claim-holder stall recycling", "[session] claim_holder_stall_timeout", "PAR-STALL-2"})
	}
	if cfg.ChatSessions.IdleTimeoutDuration() > 0 {
		out = append(out, latchRefusal{"chat auto-suspend", "[chat_sessions] idle_timeout", "PAR-CHAT"})
	}
	if r, ok := v2SessionRuntimeRefusal(cfg); ok {
		out = append(out, r)
	}
	return out
}

// v2SessionRuntimeRefusal refuses a [session] provider whose runtime v2 does
// not support yet, spelled as the runtime registry resolves it
// (buildRuntimeRegistry, runtimeRegistryForCity). The exec: script
// gc-session-t3 is the legacy t3bridge spelling.
func v2SessionRuntimeRefusal(cfg *config.City) (latchRefusal, bool) {
	name := strings.TrimSpace(cfg.Session.Provider)
	backend, parityPR := name, v2SessionRuntimeParity[name]
	switch {
	case parityPR != "":
	case strings.HasPrefix(name, "exec:") && isLegacyT3BridgeExecScript(strings.TrimPrefix(name, "exec:")):
		backend, parityPR = "t3bridge", "PAR-T3"
	case strings.HasPrefix(name, "exec:"):
		backend, parityPR = "exec", "PAR-EXEC"
	case strings.HasPrefix(name, "ssh:"):
		backend, parityPR = "ssh", "PAR-SSH"
	default:
		rt, declared := cfg.Runtimes[name]
		if !declared {
			return latchRefusal{}, false
		}
		backend, parityPR = "pack "+rt.PackName+" exec", "PAR-EXEC" // a pack-declared runtime is an exec proxy
	}
	return latchRefusal{"the " + backend + " session runtime", fmt.Sprintf("[session] provider = %q", name), parityPR}, true
}

// v2LatchRefusalMessage joins refusals into one clause for the latch error.
func v2LatchRefusalMessage(refusals []latchRefusal) string {
	parts := make([]string, len(refusals))
	for i, r := range refusals {
		parts[i] = r.String()
	}
	return strings.Join(parts, "; ")
}
