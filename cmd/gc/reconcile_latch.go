package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime/registry"
)

// latchRefusal is one configured feature v2 does not support yet, so the latch
// refuses v2 rather than run without it. ParityPR is the slice that lifts it
// (IMPLEMENTATION-PLAN §6), or "" for a runtime no slice covers yet.
type latchRefusal struct {
	Feature  string // what v2 would silently drop
	Config   string // the composed-config setting that enables it
	ParityPR string
}

// String renders the refusal as it appears in the latch error and in doctor.
func (r latchRefusal) String() string {
	s := fmt.Sprintf("%s (%s) is not available under v2", r.Config, r.Feature)
	if r.ParityPR != "" {
		s += " until " + r.ParityPR
	}
	return s
}

// v2SessionRuntimesAdmitted is the allowlist of [session] provider names v2
// runs on (fake and fail are test doubles); any other registered name refuses.
var v2SessionRuntimesAdmitted = map[string]bool{
	"": true, "tmux": true, "acp": true, "subprocess": true, "fake": true, "fail": true,
}

// v2LatchRefusals lists every refusal the composed config hits, in a fixed
// order. It reads only cfg and the immutable builtin runtime registry, never a
// store or the environment, so the latch and the doctor dry run agree.
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
	reg, err := runtimeRegistryForCity(cfg)
	if err != nil { // a pack runtime collision; config load already rejects it
		reg = runtimeRegistry
	}
	if r, ok := v2SessionRuntimeRefusal(cfg, reg); ok {
		out = append(out, r)
	}
	return out
}

// v2SessionRuntimeRefusal refuses a [session] provider outside the allowlist
// that reg resolves. A name reg does not resolve reaches the tmux fallback and
// is admitted as tmux. exec:…/gc-session-t3 is the legacy t3bridge spelling.
func v2SessionRuntimeRefusal(cfg *config.City, reg *registry.Registry) (latchRefusal, bool) {
	name := strings.TrimSpace(cfg.Session.Provider)
	if v2SessionRuntimesAdmitted[name] || !reg.Resolves(name) {
		return latchRefusal{}, false
	}
	feature, parityPR := "the "+name+" session runtime", ""
	pack, packRuntime := cfg.Runtimes[name]
	switch {
	case name == "k8s" || name == "hybrid": // hybrid's remote leg is k8s
		parityPR = "PAR-K8S"
	case name == "herdr":
		parityPR = "PAR-HERDR"
	case name == "t3bridge":
		parityPR = "PAR-T3"
	case strings.HasPrefix(name, "exec:") && isLegacyT3BridgeExecScript(strings.TrimPrefix(name, "exec:")):
		feature, parityPR = "the t3bridge session runtime", "PAR-T3"
	case strings.HasPrefix(name, "exec:"):
		feature, parityPR = "the exec session runtime", "PAR-EXEC"
	case strings.HasPrefix(name, "ssh:"):
		feature, parityPR = "the ssh session runtime", "PAR-SSH"
	case packRuntime:
		feature, parityPR = "the pack "+pack.PackName+" exec session runtime", "PAR-EXEC"
	default:
		feature = "unsupported session runtime"
	}
	return latchRefusal{feature, fmt.Sprintf("[session] provider = %q", name), parityPR}, true
}
