package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime/registry"
)

// latchRefusal is one configured feature v2 does not support yet, so the latch
// refuses v2 rather than run without it. ParityPR is the slice that lifts it
// (IMPLEMENTATION-PLAN §6), or "" for a runtime no slice covers yet. Fix,
// when set, is what the operator changes instead: an exec runtime's
// missing handshake capabilities (PAR-EXEC lifted the runtime itself).
type latchRefusal struct {
	Feature  string // what v2 would silently drop
	Config   string // the composed-config setting that enables it
	ParityPR string
	Fix      string
}

// String renders the refusal as it appears in the latch error and in doctor.
func (r latchRefusal) String() string {
	s := fmt.Sprintf("%s (%s) is not available under v2", r.Config, r.Feature)
	switch {
	case r.Fix != "":
		s += ": " + r.Fix
	case r.ParityPR != "":
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
// order. It reads cfg, lookupEnv's GC_SESSION (the provider the city runs,
// v2SessionRuntimeName) and the immutable builtin runtime registry, never a
// store, and outside config only the exec admission probe (execReadiness),
// one remembered handshake per version of the script file, so the latch,
// the doctor dry run, gc start and the provider swap agree.
func v2LatchRefusals(cfg *config.City, cityPath string, lookupEnv func(string) (string, bool)) []latchRefusal {
	var out []latchRefusal
	var dependsOn, maxAge []string
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if len(a.DependsOn) > 0 {
			dependsOn = append(dependsOn, a.QualifiedName())
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
		{maxAge, "max_session_age", "max-age restarts", "PAR-AGE"},
	} {
		if len(c.agents) > 0 {
			out = append(out, latchRefusal{Feature: c.feature, Config: fmt.Sprintf("agent %s on %s", c.key, strings.Join(c.agents, ", ")), ParityPR: c.parityPR})
		}
	}
	if cfg.ChatSessions.IdleTimeoutDuration() > 0 {
		out = append(out, latchRefusal{Feature: "chat auto-suspend", Config: "[chat_sessions] idle_timeout", ParityPR: "PAR-CHAT"})
	}
	if r, ok := v2ProviderSwapRefusal(cfg, cityPath, lookupEnv); ok {
		out = append(out, r)
	}
	return out
}

// v2IgnoredSettings lists the composed-config settings v2 accepts and ignores,
// in a fixed order: the identity circuit breaker (PAR-BRK) and the two stall
// recyclers (PAR-STALL-1, PAR-STALL-2), which the work ladder replaces, or,
// under [daemon] work_ladder = "off", the v2 backstops it replaced. Legacy
// keeps honoring them. Each entry is the deprecation the controller logs at
// boot and doctor reports.
func v2IgnoredSettings(cfg *config.City) []string {
	var keys []string
	if cfg.Daemon.SessionCircuitBreaker {
		keys = append(keys, "[daemon] session_circuit_breaker")
	}
	if cfg.Session.ProgressStallTimeoutDuration() > 0 {
		keys = append(keys, "[session] progress_stall_timeout")
	}
	if cfg.Session.ClaimHolderStallTimeoutDuration() > 0 {
		keys = append(keys, "[session] claim_holder_stall_timeout")
	}
	replacement := "the work ladder replaces it"
	if !cfg.Daemon.WorkLadderEnabled() {
		replacement = `with [daemon] work_ladder = "off", v2's idle-claim, continuation and execution backstops pace work instead`
	}
	out := make([]string, len(keys))
	for i, key := range keys {
		out[i] = key + " is deprecated under v2 and ignored: " + replacement + "; remove it, or set session_reconciler = \"legacy\" to keep it"
	}
	return out
}

// v2SessionRuntimeName is the session provider the city runs: GC_SESSION,
// when lookupEnv sets it, over the config's [session] provider, as
// effectiveProviderName reads it (a nil lookupEnv has none).
func v2SessionRuntimeName(cfg *config.City, lookupEnv func(string) (string, bool)) string {
	return strings.TrimSpace(v2SessionRuntimeRawName(cfg, lookupEnv))
}

// v2SessionRuntimeRawName is v2SessionRuntimeName untrimmed, as
// effectiveProviderName hands it to the runtime registry.
func v2SessionRuntimeRawName(cfg *config.City, lookupEnv func(string) (string, bool)) string {
	if lookupEnv != nil {
		if v, ok := lookupEnv("GC_SESSION"); ok && v != "" {
			return v
		}
	}
	return cfg.Session.Provider
}

// v2SessionRuntimeRefusal refuses a session provider name outside the
// allowlist that reg resolves. A name reg does not resolve reaches the tmux
// fallback and is admitted as tmux. exec:…/gc-session-t3 is the legacy
// t3bridge spelling. An exec runtime, `exec:<script>` or a pack's, is
// admitted iff its handshake is v2-ready (execReadiness); otherwise it is
// refused with the handshake's error or the capabilities it lacks as the
// Fix. raw is the name untrimmed: the registry builds the provider from
// it, so an exec runtime whose raw name differs (surrounding whitespace)
// would run a script the probe never read, and is refused.
func v2SessionRuntimeRefusal(cfg *config.City, cityPath string, reg *registry.Registry, raw, name string) (latchRefusal, bool) {
	if v2SessionRuntimesAdmitted[name] || !reg.Resolves(name) {
		return latchRefusal{}, false
	}
	if script, ok := execRuntimeScript(cfg, name); ok {
		feature := "the exec session runtime " + script
		if pack, isPack := cfg.Runtimes[name]; isPack {
			feature = "the pack " + pack.PackName + " exec session runtime"
		}
		if raw != name {
			return latchRefusal{Feature: feature, Config: fmt.Sprintf("[session] provider = %q", raw), Fix: "the name has surrounding whitespace, which the session provider keeps: remove it"}, true
		}
		h := execReadiness(script, cfg.EffectiveCityName(), cityPath)
		if h.ready() {
			return latchRefusal{}, false
		}
		return latchRefusal{Feature: feature, Config: fmt.Sprintf("[session] provider = %q", name), Fix: h.fix(script)}, true
	}
	feature, parityPR := "the "+name+" session runtime", ""
	switch {
	case name == "k8s" || name == "hybrid": // hybrid's remote leg is k8s
		parityPR = "PAR-K8S"
	case name == "herdr":
		parityPR = "PAR-HERDR"
	case name == "t3bridge":
		parityPR = "PAR-T3"
	case strings.HasPrefix(name, "exec:") && isLegacyT3BridgeExecScript(strings.TrimPrefix(name, "exec:")):
		feature, parityPR = "the t3bridge session runtime", "PAR-T3"
	case strings.HasPrefix(name, "ssh:"):
		feature, parityPR = "the ssh session runtime", "PAR-SSH"
	default:
		feature = "unsupported session runtime"
	}
	return latchRefusal{Feature: feature, Config: fmt.Sprintf("[session] provider = %q", name), ParityPR: parityPR}, true
}
