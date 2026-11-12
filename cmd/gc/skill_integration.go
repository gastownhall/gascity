package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const sharedSkillCatalogSnapshotEnvVar = "GC_SHARED_SKILL_CATALOG_SNAPSHOT"

// materializationRuntime is the city's session runtime as the skill and MCP
// materialization gates see it: what the runtime guarantees about the
// controller's host, read from its registry registration.
type materializationRuntime struct {
	// name is the [session] provider selection. Only the legacy admission
	// path reads it (legacyStage1RuntimeNames, legacyStage2RuntimeNames).
	name string
	// host is what the selected runtime guarantees about the host.
	host runtime.HostCapabilities
}

// cityMaterializationRuntime returns the materialization view of cfg's
// [session] provider.
func cityMaterializationRuntime(cfg *config.City, cityPath string) materializationRuntime {
	if cfg == nil {
		return materializationRuntimeFor(nil, "", cityPath, "")
	}
	return materializationRuntimeFor(cfg, cfg.EffectiveCityName(), cityPath, cfg.Session.Provider)
}

// materializationRuntimeFor returns the materialization view of a session
// runtime selection name, resolved against cfg's runtimes (the builtins plus
// its pack-declared runtimes; a nil cfg has only the builtins). The name
// resolves exactly as provider construction resolves it, so an unknown name
// reports the tmux fallback it runs on. A city whose pack runtimes fail to
// register reports no host capabilities: its config load already fails on
// that (validatePackRuntimeRegistrations), and claiming nothing is the safe
// reading meanwhile.
func materializationRuntimeFor(cfg *config.City, cityName, cityPath, name string) materializationRuntime {
	name = strings.TrimSpace(name)
	reg, err := runtimeRegistryForCity(cfg)
	if err != nil {
		return materializationRuntime{name: name}
	}
	return materializationRuntime{name: name, host: reg.HostCapabilities(name, cityName, cityPath)}
}

// legacyStage1RuntimeNames and legacyStage2RuntimeNames are the selection
// names the gates admitted before they were keyed on host capabilities
// (gastownhall/gascity#9006 item 3a). They are the expand half of an
// expand/contract migration: every name here already passes on its
// registered capabilities alone, and the sets stay frozen until the contract
// step deletes them. Never add a name — declare the runtime's
// runtime.HostCapabilities at its registration (buildRuntimeRegistry).
var (
	legacyStage1RuntimeNames = map[string]bool{"": true, "tmux": true, "subprocess": true, "herdr": true}
	legacyStage2RuntimeNames = map[string]bool{"": true, "tmux": true, "herdr": true}
)

// canStage1Materialize reports whether stage-1 skill/MCP materialization
// (supervisor-tick-level writes into the agent's scope root) should run for
// this agent. Stage 1 happens in the gc controller process on the host
// filesystem, so it requires only that the agent's runtime read that host
// filesystem — not that it execute PreStart. By registration today, tmux
// (the default), subprocess, herdr and an exec runtime whose handshake
// declares fs.host with locality=local guarantee it; acp, k8s, ssh:, hybrid
// (whose sessions may route to k8s at spawn time) and every other exec
// runtime do not.
//
// Separate from isStage2EligibleSession, which also requires the runtime to
// execute PreStart on the host.
//
// Agent.Session == "acp" overrides the city-level runtime per agent: an ACP
// session runs in-process, and scope-root files aren't what it reads.
func canStage1Materialize(rt materializationRuntime, agent *config.Agent) bool {
	if agent == nil {
		return false
	}
	if agent.Session == "acp" {
		return false
	}
	return rt.host.SharesHostFilesystem || legacyStage1RuntimeNames[rt.name]
}

// isStage2EligibleSession reports whether stage-2 skill/MCP materialization
// — delivered as a PreStart entry that writes into the session's work dir —
// should run for the agent's session runtime (skill-materialization spec,
// "Stage 2 runtime gate"). The runtime must both execute PreStart on the
// host and read the host filesystem that entry writes into: a runtime that
// skips PreStart silently drops materialization, and one whose agents read
// another filesystem (a pod, a container) gets it in the wrong place. By
// registration today, tmux (the default), herdr and an exec runtime whose
// handshake declares fs.host with locality=local qualify; subprocess reads
// the host filesystem but does not execute PreStart; hybrid does not
// qualify because a configured RemoteMatch routes some sessions to k8s.
//
// Agent.Session == "acp" overrides the city-level runtime per agent: even in
// a tmux city, an ACP agent is ineligible because the session runs
// in-process.
func isStage2EligibleSession(rt materializationRuntime, agent *config.Agent) bool {
	if agent == nil {
		return false
	}
	if agent.Session == "acp" {
		return false
	}
	return (rt.host.SharesHostFilesystem && rt.host.RunsHostPreStart) || legacyStage2RuntimeNames[rt.name]
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
// entry to fpExtra for each desired skill, hashed with
// runtime.HashPathContent. The entries are NextStart
// (runtime.FingerprintExtraChangePolicy): the core and provision
// fingerprints leave them out, so a skill edit reaches the next start
// without draining a running session. The diagnostic drift breakdown still
// hashes every key, and the v6 recomputation that migrates a session
// started before the policy hashes them too
// (runtime.PrePolicyCoreFingerprint). Adding an agent's first skill or
// removing its last can still drain it, through the stage-2
// materialize-skills PreStart entry (resolveTemplate).
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
// SKILL.md frontmatter provided one, and the frontmatter name when it differs
// from the listed name.
func writeSkillBullets(b *strings.Builder, entries []materialize.SkillEntry, originTag string) {
	for _, e := range entries {
		b.WriteString("- `")
		b.WriteString(e.Name)
		b.WriteString("`")
		// An imported skill is listed under its binding-prefixed name, but
		// its SKILL.md keeps the pack's name, and a provider that registers
		// skills by frontmatter name loads it only under that one (#8501).
		if fm := strings.TrimSpace(e.FrontmatterName); fm != "" && fm != e.Name {
			b.WriteString(" (SKILL.md name: `")
			b.WriteString(fm)
			b.WriteString("`)")
		}
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
// <path>` for per-session-worktree materialization.
//
// The shared-catalog snapshot itself is staged to a deterministic file
// under the workdir (see writeSkillSnapshotFile) and materialize-skills
// re-discovers that path at runtime. Keeping the command shape stable
// avoids flipping the runtime fingerprint for already-running sessions
// during upgrade while still moving the large catalog blob off tmux's
// env/argv paths.
//
// The gc binary path comes from $GC_BIN (populated by the runtime env
// setup) with "gc" as a fallback if the env var isn't available at
// PreStart expansion time. Argument values are shell-quoted.
func appendMaterializeSkillsPreStart(prestart []string, qualifiedName, workDir string) []string {
	cmd := `"${GC_BIN:-gc}" internal materialize-skills --best-effort --agent ` +
		shellquote.Join([]string{qualifiedName}) + ` --workdir ` + shellquote.Join([]string{workDir})
	return append(prestart, cmd)
}

// skillSnapshotFilePath returns the deterministic path used to persist
// the shared skill catalog snapshot for one agent/workdir pair.
func skillSnapshotFilePath(workDir, qualifiedName string) string {
	if workDir == "" || qualifiedName == "" {
		return ""
	}
	safeName := strings.ReplaceAll(qualifiedName, string(filepath.Separator), "_")
	safeName = strings.ReplaceAll(safeName, "/", "_")
	return filepath.Join(workDir, ".gc", "tmp", "skill-catalog-"+safeName+".b64")
}

// removeSkillSnapshotFile clears the deterministic staged snapshot path
// so stage-2 materialize-skills falls back to live catalog loading
// instead of consuming stale shared-catalog data.
func removeSkillSnapshotFile(workDir, qualifiedName string) {
	path := skillSnapshotFilePath(workDir, qualifiedName)
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

// writeSkillSnapshotFile persists a base64-encoded shared skill catalog
// snapshot to a file under <workDir>/.gc/tmp so the PreStart materialize
// command can read it back without forcing the catalog through tmux's
// new-session protocol buffer or argv. Returns the absolute path on
// success, "" on any failure (caller falls back to letting
// materialize-skills load the live catalog from disk).
//
// The filename is keyed by agent so repeat spawns of the same agent
// reuse one file rather than littering .gc/tmp with one snapshot per
// reconciler tick. The blob itself is overwritten each call because the
// catalog can drift between ticks.
func writeSkillSnapshotFile(workDir, qualifiedName, snapshot string) string {
	path := skillSnapshotFilePath(workDir, qualifiedName)
	if path == "" || snapshot == "" {
		return ""
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		removeSkillSnapshotFile(workDir, qualifiedName)
		return ""
	}
	tmp, err := os.CreateTemp(dir, "skill-catalog-*.tmp")
	if err != nil {
		removeSkillSnapshotFile(workDir, qualifiedName)
		return ""
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write([]byte(snapshot)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		removeSkillSnapshotFile(workDir, qualifiedName)
		return ""
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		removeSkillSnapshotFile(workDir, qualifiedName)
		return ""
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		removeSkillSnapshotFile(workDir, qualifiedName)
		return ""
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		removeSkillSnapshotFile(workDir, qualifiedName)
		return ""
	}
	return path
}
