package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// canStage1Materialize reports whether stage-1 skill materialization
// (supervisor-tick-level writes into the agent's scope root) should
// run for this agent. Stage 1 happens in the gc controller process on
// the host filesystem, so it requires only that the agent's runtime
// be able to SEE that filesystem path — not that it execute
// PreStart.
//
//	tmux, subprocess → eligible. Scope root on the host; agent reads
//	                   files from that host filesystem.
//	""               → eligible (workspace default is tmux).
//	acp              → ineligible. In-process agent; scope-root files
//	                   aren't what it reads from.
//	k8s              → ineligible. Agent runs in a pod that doesn't
//	                   share the host scope root.
//	hybrid           → ineligible in v0.15.1 (per-session routing
//	                   decides at spawn time whether the session
//	                   goes local-tmux or remote-k8s; can't predict
//	                   at supervisor tick without the session name).
//
// Separate from isStage2EligibleSession (which gates PreStart
// injection and has a stricter "runtime actually executes PreStart"
// requirement). A PR to add PreStart support to the subprocess
// runtime will collapse the two predicates in a future release.
func canStage1Materialize(citySessionProvider string, agent *config.Agent) bool {
	if agent == nil {
		return false
	}
	if agent.Session == "acp" {
		return false
	}
	switch strings.TrimSpace(citySessionProvider) {
	case "", "tmux", "subprocess":
		return true
	default:
		return false
	}
}

// isStage2EligibleSession reports whether skill materialization should
// run for the given agent's session runtime. Per the skill-
// materialization spec (§ "Stage 2 runtime gate") and the runtime
// reality of which providers actually execute PreStart:
//
//	tmux  → eligible. PreStart runs on the host via tmux/adapter.go
//	        runPreStart before the tmux session is created.
//	""    → eligible (workspace default maps to tmux).
//	acp   → ineligible. Session runs in-process; out of scope v0.15.1.
//	k8s   → ineligible. PreStart runs inside the pod; gc binary and
//	        host skill paths aren't available there.
//
// The spec lists subprocess as eligible, but as of v0.15.1 the
// subprocess runtime in internal/runtime/subprocess does NOT execute
// cfg.PreStart — it only stages CopyFiles and overlay content before
// exec'ing the command. Marking subprocess eligible would inject a
// PreStart entry that never runs, silently dropping materialization.
// The conservative fix is to exclude subprocess from eligibility here
// until the subprocess runtime gains PreStart support (tracked as a
// follow-up for Phase 4 / post-v0.15.1).
//
// Hybrid is also ineligible. A default-config hybrid city routes every
// session to local tmux and would work, but once the user configures
// RemoteMatch (or GC_HYBRID_REMOTE_MATCH), some sessions route to
// k8s — and a host-side PreStart would execute on the controller box
// instead of the pod, materializing into the wrong workdir.
// Per-session routing-aware eligibility is Phase 4A work.
//
// Agent.Session == "acp" overrides the city-level session selector at
// the per-agent level — even in a tmux city, an ACP agent is
// ineligible because the session runs in-process.
func isStage2EligibleSession(citySessionProvider string, agent *config.Agent) bool {
	if agent == nil {
		return false
	}
	if agent.Session == "acp" {
		return false
	}
	switch strings.TrimSpace(citySessionProvider) {
	case "", "tmux":
		return true
	default:
		// subprocess, k8s, acp, fake, fail, hybrid, exec:<script>, ...
		// — all conservatively ineligible until individually verified.
		return false
	}
}

// agentScopeRoot returns the canonical absolute filesystem root into
// which stage-1 materialization writes for this agent. City-scoped
// agents resolve to cityPath; rig-scoped agents resolve to the rig's
// configured Path (looked up by agent.Dir). Per spec, empty scope
// defaults to "rig".
//
// The returned path is always absolute and cleaned so callers can
// compare it against an already-resolved workDir without worrying
// about trailing slashes, `./` prefixes, or the user-authored rig path
// being relative to cityPath. This matters because Phase 3B uses
// `workDir != scopeRoot` to decide whether to inject a per-session
// PreStart — a spurious mismatch (e.g., "/city/rig" vs "rig/")
// triggers useless materialization on every spawn.
//
// When the agent is rig-scoped but no matching rig exists in the
// config (e.g., an inline [[agent]] with a bespoke dir), the path
// falls back to cityPath. Callers should treat this as a conservative
// best-effort identifier; a mismatched scope root is used for stage
// discrimination, not as a security boundary.
func agentScopeRoot(agent *config.Agent, cityPath string, rigs []config.Rig) string {
	root := resolveAgentScopeRoot(agent, cityPath, rigs)
	return canonicaliseFilePath(root, cityPath)
}

func resolveAgentScopeRoot(agent *config.Agent, cityPath string, rigs []config.Rig) string {
	if agent == nil {
		return cityPath
	}
	scope := agent.Scope
	if scope == "" {
		scope = "rig"
	}
	if scope == "city" {
		return cityPath
	}
	for _, r := range rigs {
		if r.Name == agent.Dir && r.Path != "" {
			return r.Path
		}
	}
	return cityPath
}

// canonicaliseFilePath returns filepath.Clean(abs(path)), joining
// relative paths against base before cleaning. Falls back to Clean(path)
// when absolute resolution fails. Used to make scope-root and workDir
// strings directly comparable.
func canonicaliseFilePath(path, base string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// effectiveAgentProvider returns the vendor/provider name used for
// skill materialization, falling back from the per-agent `provider`
// field to `workspace.provider` when the agent didn't override.
// Matches how Gas City resolves the effective provider throughout
// the binary (config.ResolveProvider's input chain). Returns ""
// when both are empty.
//
// Empty string from this helper means "no provider configured" —
// the materializer treats it as no vendor sink, skipping the agent.
// A non-empty return value is still subject to materialize.VendorSink
// for the actual sink-directory lookup.
func effectiveAgentProvider(agent *config.Agent, workspaceProvider string) string {
	if agent == nil {
		return ""
	}
	if strings.TrimSpace(agent.Provider) != "" {
		return agent.Provider
	}
	return workspaceProvider
}

// effectiveAgentProviderFamily resolves the agent's effective provider to
// its built-in family name (e.g. a wrapped custom "my-fast-claude" with
// base = "builtin:claude" resolves to "claude"). When the effective
// provider is already a built-in, its name is returned unchanged. When it
// has no built-in ancestor, the raw name is returned so downstream lookups
// (e.g. materialize.VendorSink) fail closed on truly-unknown providers
// rather than silently widening the match.
//
// Vendor-sink and hook-family lookups use this helper so wrapped providers
// behave like their ancestor. cityProviders may be nil (tests, legacy
// paths with no custom providers) — the helper degrades to identity
// resolution.
func effectiveAgentProviderFamily(agent *config.Agent, workspaceProvider string, cityProviders map[string]config.ProviderSpec) string {
	raw := effectiveAgentProvider(agent, workspaceProvider)
	if raw == "" {
		return ""
	}
	if family := config.BuiltinFamily(raw, cityProviders); family != "" {
		return family
	}
	return raw
}

// effectiveSkillsForAgent returns the post-precedence desired skill set
// for one agent. Returns nil when the agent's effective provider has
// no vendor sink, when no catalog produced any entries, or when the
// agent is nil.
//
// Agent-catalog load failures are logged to stderr (matching the
// city-catalog pattern in newAgentBuildParams) so a permissions
// glitch on an agent's skills_dir is observable rather than silently
// dropping agent-local skills.
func effectiveSkillsForAgent(city *materialize.CityCatalog, agent *config.Agent, workspaceProvider string, cityProviders map[string]config.ProviderSpec, stderr io.Writer) []materialize.SkillEntry {
	if agent == nil {
		return nil
	}
	provider := effectiveAgentProviderFamily(agent, workspaceProvider, cityProviders)
	if _, ok := materialize.VendorSink(provider); !ok {
		return nil
	}

	var agentCat materialize.AgentCatalog
	if agent.SkillsDir != "" {
		c, err := materialize.LoadAgentCatalog(agent.SkillsDir)
		switch {
		case err != nil:
			if stderr != nil {
				fmt.Fprintf(stderr, "buildDesiredState: LoadAgentCatalog %q for agent %q: %v (agent-local skills will not contribute to fingerprints this tick)\n", //nolint:errcheck // best-effort stderr
					agent.SkillsDir, agent.QualifiedName(), err)
			}
		default:
			agentCat = c
		}
	}

	sharedCatalog := materialize.CityCatalog{}
	if city != nil {
		sharedCatalog = *city
	}
	desired := materialize.EffectiveSet(sharedCatalog, agentCat)
	if len(desired) == 0 {
		return nil
	}
	return desired
}

// mergeSkillFingerprintEntries adds one "skills:<name>" → content-hash
// entry to fpExtra for each desired skill. Hashes use
// runtime.HashPathContent so any byte-level change to a skill's source
// directory triggers a config-fingerprint drift and drains the agent.
//
// Nil-map safe: allocates fpExtra if the caller passed nil. Returns
// the (possibly new) map. The "skills:" prefix partitions the key
// space so entries cannot collide with other fpExtra keys
// (pool.min/pool.max/wake_mode/etc.).
func mergeSkillFingerprintEntries(fpExtra map[string]string, desired []materialize.SkillEntry) map[string]string {
	if len(desired) == 0 {
		return fpExtra
	}
	if fpExtra == nil {
		fpExtra = make(map[string]string, len(desired))
	}
	for _, e := range desired {
		fpExtra["skills:"+e.Name] = runtime.HashPathContent(e.Source)
	}
	return fpExtra
}

// effectiveInjectAssignedSkills resolves the agent's prompt-appendix
// preference. Returns true by default (nil pointer → inject) so the
// feature is opt-out rather than opt-in. An explicit agent-level
// `inject_assigned_skills = false` disables it for that agent.
func effectiveInjectAssignedSkills(agent *config.Agent) bool {
	if agent == nil {
		return false
	}
	if agent.InjectAssignedSkills != nil {
		return *agent.InjectAssignedSkills
	}
	return true
}

// buildAssignedSkillsPromptFragment renders a markdown appendix that
// lists every skill the agent sees, partitioned into (assigned-to-this-
// agent, shared-with-the-current-scope). The goal is that agents
// sharing a scope-root sink (multiple city-scoped agents, multiple
// rig-scoped agents on the same rig) can tell which skills are their
// specialisation vs which are the shared set — the materialiser
// physically delivers both into the same sink directory.
//
// Returns "" when the agent has no skills to list (no vendor sink, no
// catalog entries, or opt-out). Safe to append unconditionally:
// the caller's template gets nothing extra when the fragment is empty.
//
// The fragment uses the SKILL.md frontmatter description for each
// entry so agents see both the name and a one-line purpose. Origin
// tags identify where a shared skill came from: the city pack, an
// imported pack binding, or a legacy compatibility bootstrap pack.
func buildAssignedSkillsPromptFragment(
	agent *config.Agent,
	city *materialize.CityCatalog,
	agentCat materialize.AgentCatalog,
) string {
	if agent == nil {
		return ""
	}
	var shared []materialize.SkillEntry
	if city != nil {
		// Exclude entries that the agent-local catalog overrides —
		// the agent's own entry wins precedence and will appear in
		// the "assigned to you" section instead.
		byAgentName := make(map[string]struct{}, len(agentCat.Entries))
		for _, e := range agentCat.Entries {
			byAgentName[e.Name] = struct{}{}
		}
		for _, e := range city.Entries {
			if _, shadowed := byAgentName[e.Name]; shadowed {
				continue
			}
			shared = append(shared, e)
		}
	}
	if len(shared) == 0 && len(agentCat.Entries) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Skills available to this session\n\n")
	fmt.Fprintf(&b, "You are `%s`. The following skills are materialized in your provider's skill directory and load automatically — you don't need to invoke anything extra.\n\n", //nolint:errcheck // fmt.Fprintf into a strings.Builder never errors
		agent.QualifiedName())

	if len(agentCat.Entries) > 0 {
		b.WriteString("### Assigned to you\n\n")
		writeSkillBullets(&b, agentCat.Entries, "")
		b.WriteString("\n")
	}

	if len(shared) > 0 {
		b.WriteString("### Shared in this scope\n\n")
		writeSkillBullets(&b, shared, "origin")
		b.WriteString("\n")
	}

	b.WriteString("These are discovery-time hints, not execution gates — the vendor loads every skill from the sink directory regardless of what this appendix lists.\n")
	return b.String()
}

// writeSkillBullets renders a bullet list of skill entries. When
// originTag is non-empty, each bullet trails with " *(origin)*" so
// shared entries can show whether they came from the city pack, an
// import binding, or a compatibility bootstrap pack. Descriptions are included when the
// SKILL.md frontmatter provided one.
func writeSkillBullets(b *strings.Builder, entries []materialize.SkillEntry, originTag string) {
	for _, e := range entries {
		b.WriteString("- `")
		b.WriteString(e.Name)
		b.WriteString("`")
		if strings.TrimSpace(e.Description) != "" {
			b.WriteString(" — ")
			b.WriteString(strings.TrimSpace(e.Description))
		}
		if originTag != "" && strings.TrimSpace(e.Origin) != "" {
			b.WriteString(" *(")
			b.WriteString(e.Origin)
			b.WriteString(")*")
		}
		b.WriteString("\n")
	}
}

// appendMaterializeSkillsPreStart appends a PreStart command that
// invokes `gc internal materialize-skills --agent <name> --workdir
// <path>` for per-session-worktree materialization. The command is
// APPENDED to any existing user-configured PreStart so worktree
// creation and other setup runs first; materialization runs
// immediately before the agent command.
//
// The gc binary path comes from $GC_BIN (populated by the runtime env
// setup) with "gc" as a fallback if the env var isn't available at
// PreStart expansion time. Argument values are shell-quoted.
func appendMaterializeSkillsPreStart(prestart []string, qualifiedName, workDir string) []string {
	cmd := `"${GC_BIN:-gc}" internal materialize-skills --agent ` +
		shellquote.Join([]string{qualifiedName}) + ` --workdir ` + shellquote.Join([]string{workDir})
	return append(prestart, cmd)
}
