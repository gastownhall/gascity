package main

import (
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/session"
)

// doctorSkillStaticSinks resolves the config-derived skill sink
// directories the dangling-sink doctor check scans: every agent's
// scope-root × provider vendor sink, mirroring the stage-1
// materializer's targeting (skill_supervisor.go) so the check sees
// exactly the directories the materializer writes.
func doctorSkillStaticSinks(cityPath string, cfg *config.City) []string {
	var sinks []string
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		provider := effectiveAgentProviderFamily(agent, cfg.Workspace.Provider, cfg.Providers)
		vendor, ok := materialize.VendorSink(provider)
		if !ok {
			continue
		}
		scopeRoot := resolveAgentScopeRoot(agent, cityPath, cfg.Rigs)
		if !filepath.IsAbs(scopeRoot) {
			scopeRoot = filepath.Join(cityPath, scopeRoot)
		}
		sinks = append(sinks, filepath.Join(scopeRoot, vendor))
	}
	return sinks
}

// doctorLiveSessionSinks returns a lazy enumerator for the dangling-sink
// doctor check: each live (non-closed) session's WorkDir × vendor sink.
// Stage-2 sessions materialize into their per-session worktree, not the
// scope root, so scope-root-only scanning misses exactly the crew sinks
// hq-38je found broken. Laziness keeps the session store out of doctor
// check construction; a store failure yields no live sinks rather than
// failing the whole check (the static sinks still scan).
func doctorLiveSessionSinks(cityPath string, cfg *config.City) func() []string {
	return func() []string {
		var sinks []string
		for _, c := range doctorLiveSessionSinkCandidates(cityPath, cfg)() {
			sinks = append(sinks, c.SessionSink)
		}
		return sinks
	}
}

// doctorLiveSessionSinkCandidate pairs one live session's own skill sink
// with the scope-root sink of the agent it was started from, so the
// missing-sink check can tell "this agent has no skills to deliver" apart
// from "this session's cwd cannot see the skills its agent has".
type doctorLiveSessionSinkCandidate struct {
	SessionName string
	SessionSink string // WorkDir × vendor sink for this session
	ScopeSink   string // the originating agent's scope-root × vendor sink
}

// doctorLiveSessionSinkCandidates is the shared lazy session-store scan
// behind doctorLiveSessionSinks (dangling links) and the missing-sink
// check registered alongside it: both need the live WorkDir × vendor sink,
// and the latter additionally needs the originating agent's scope-root
// sink to know whether skills exist to have been materialized at all.
func doctorLiveSessionSinkCandidates(cityPath string, cfg *config.City) func() []doctorLiveSessionSinkCandidate {
	return func() []doctorLiveSessionSinkCandidate {
		store, err := openSessionProviderStore(cityPath)
		if err != nil {
			return nil
		}
		infos, err := session.NewStore(beads.SessionStore{Store: cliSessionStore(store, cfg, cityPath)}).ListLabeledSessionInfosUnfiltered()
		if err != nil {
			return nil
		}
		var out []doctorLiveSessionSinkCandidate
		for _, info := range infos {
			if info.Closed || info.WorkDir == "" {
				continue
			}
			vendor, ok := materialize.VendorSink(info.Provider)
			if !ok {
				continue
			}
			sessionSink := filepath.Join(info.WorkDir, vendor)
			scopeSink := doctorAgentScopeSink(cityPath, cfg, info.AgentName, vendor)
			// Every live session sink is kept here, including where
			// scopeSink == sessionSink (the common case): the sibling
			// dangling-sink check (doctorLiveSessionSinks) needs every
			// live sink scanned regardless of scope-root equality. The
			// missing-sink check filters that case out itself, since
			// identical dirs can never be "missing" relative to each
			// other.
			out = append(out, doctorLiveSessionSinkCandidate{
				SessionName: info.SessionName,
				SessionSink: sessionSink,
				ScopeSink:   scopeSink,
			})
		}
		return out
	}
}

// doctorSkillMissingSinkCandidates adapts doctorLiveSessionSinkCandidates
// to the doctor package's public candidate type for the missing-sink check.
func doctorSkillMissingSinkCandidates(cityPath string, cfg *config.City) func() []doctor.SkillMissingSinkCandidate {
	fn := doctorLiveSessionSinkCandidates(cityPath, cfg)
	return func() []doctor.SkillMissingSinkCandidate {
		src := fn()
		out := make([]doctor.SkillMissingSinkCandidate, len(src))
		for i, c := range src {
			out[i] = doctor.SkillMissingSinkCandidate{
				SessionName: c.SessionName,
				SessionSink: c.SessionSink,
				ScopeSink:   c.ScopeSink,
			}
		}
		return out
	}
}

// doctorAgentScopeSink resolves the given named agent's scope-root × vendor
// sink path, or "" when the agent is unknown or its provider has no
// registered vendor sink.
func doctorAgentScopeSink(cityPath string, cfg *config.City, agentName, vendor string) string {
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Name != agentName {
			continue
		}
		scopeRoot := resolveAgentScopeRoot(agent, cityPath, cfg.Rigs)
		if !filepath.IsAbs(scopeRoot) {
			scopeRoot = filepath.Join(cityPath, scopeRoot)
		}
		return filepath.Join(scopeRoot, vendor)
	}
	return ""
}
