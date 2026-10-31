package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// mcpGitignoreEntry is the .gitignore entry for the target gc writes, which
// may hold expanded secrets: the target's path from the projection's root,
// anchored with a leading slash, since the .gitignore gc edits is the one at
// that root. It matches that file only, not a file of the same name deeper in
// the repository, as an unanchored ".mcp.json" would.
func mcpGitignoreEntry(p materialize.MCPProjection) string {
	rel, err := filepath.Rel(p.Root, p.Target)
	if err != nil {
		return ""
	}
	return "/" + filepath.ToSlash(rel)
}

// missingMCPGitignoreEntries returns the entries root's .gitignore lacks. The
// same path without its leading slash, as an earlier gc wrote it, counts as
// present: it matches the file too, and rewriting it would churn every
// existing .gitignore.
func missingMCPGitignoreEntries(fs fsys.FS, root string, entries []string) []string {
	var missing []string
	for _, entry := range missingGitignoreEntries(fs, root, entries) {
		if len(missingGitignoreEntries(fs, root, []string{strings.TrimPrefix(entry, "/")})) == 0 {
			continue
		}
		missing = append(missing, entry)
	}
	return missing
}

type mcpTargetSpec struct {
	Root       string
	Projection materialize.MCPProjection
	Agents     []string
}

type resolvedMCPProjection struct {
	Agent        *config.Agent
	Identity     string
	WorkDir      string
	ScopeRoot    string
	ProviderKind string
	Delivery     string
	Catalog      materialize.MCPCatalog
	Projection   materialize.MCPProjection
	// Skipped lists shared-layer servers the agent's provider cannot
	// receive. They are in neither Catalog nor Projection: the agent starts
	// without them.
	Skipped []mcpServerSkip
}

// agentMCPResolution is one agent's effective MCP after provider capability
// filtering. Catalog holds the servers the agent's provider receives and
// Projection their provider-native payload; Skipped holds the shared-layer
// servers it starts without.
type agentMCPResolution struct {
	Catalog    materialize.MCPCatalog
	Projection materialize.MCPProjection
	Skipped    []mcpServerSkip
}

// mcpProviderKinds are the harness families gc can write an MCP config for.
var mcpProviderKinds = []string{
	materialize.MCPProviderClaude,
	materialize.MCPProviderCodex,
	materialize.MCPProviderGemini,
	materialize.MCPProviderOpenCode,
	materialize.MCPProviderMimoCode,
	materialize.MCPProviderCursor,
}

func supportsMCPProviderKind(kind string) bool {
	kind = strings.TrimSpace(kind)
	for _, supported := range mcpProviderKinds {
		if kind == supported {
			return true
		}
	}
	return false
}

func loadEffectiveMCPForAgent(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	qualifiedName, workDir string,
) (materialize.MCPCatalog, error) {
	catalog, err := materialize.EffectiveMCPForSession(cfg, cityPath, agent, qualifiedName, workDir, config.QueryTopology{})
	if err != nil {
		return materialize.MCPCatalog{}, fmt.Errorf("loading effective MCP: %w", err)
	}
	return catalog, nil
}

// resolveAgentMCPProjection loads an agent's effective MCP catalog and builds
// the provider-native file projection of the servers its provider's MCP
// config can carry.
//
// inBand is set for ACP sessions, which receive their servers in the
// session/new request instead of through a file. They receive every server:
// Catalog is the whole effective catalog, nothing is skipped, and nothing is
// an error. Their file projection, when the harness has one, still holds
// only what the file can carry.
//
// For every other session, shared-layer servers the provider cannot receive
// come back in Skipped and are left out of Catalog and Projection, and
// agent-local ones are an error (partitionAgentMCPCatalog).
func resolveAgentMCPProjection(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	qualifiedName, workDir string,
	providerKind string,
	inBand bool,
) (agentMCPResolution, error) {
	catalog, err := loadEffectiveMCPForAgent(cityPath, cfg, agent, qualifiedName, workDir)
	if err != nil {
		return agentMCPResolution{}, err
	}
	if !supportsMCPProviderKind(providerKind) && shouldSkipDeterministicControlDispatcherMCP(agent, providerKind) {
		return agentMCPResolution{}, nil
	}
	var resolution agentMCPResolution
	fileServers := catalog.Servers
	if inBand {
		resolution.Catalog = catalog
		if supportsMCPProviderKind(providerKind) {
			fileServers = catalog.Filter(func(server materialize.MCPServer) bool {
				return materialize.MCPProjectionSupportsTransport(providerKind, server.Transport)
			}).Servers
		}
	} else {
		kept, skipped, err := partitionAgentMCPCatalog(providerKind, catalog)
		if err != nil {
			return agentMCPResolution{}, err
		}
		resolution.Catalog, resolution.Skipped = kept, skipped
		fileServers = kept.Servers
	}
	if !supportsMCPProviderKind(providerKind) {
		return resolution, nil
	}
	resolution.Projection, err = materialize.BuildMCPProjection(providerKind, workDir, fileServers)
	if err != nil {
		return agentMCPResolution{}, err
	}
	return resolution, nil
}

// mcpInBand reports whether an agent's sessions receive MCP in-band: an ACP
// session carries its servers in session/new rather than in a provider-native
// file. resolved may be nil when the provider does not resolve.
func mcpInBand(agent *config.Agent, resolved *config.ResolvedProvider) bool {
	return agent != nil && config.ResolveSessionCreateTransport(agent.Session, resolved) == config.SessionTransportACP
}

// shouldSkipDeterministicControlDispatcherMCP matches the providerless
// control-dispatcher worker. It never invokes provider MCP projection, so an
// inherited city MCP catalog must not make startup require a provider family.
func shouldSkipDeterministicControlDispatcherMCP(agent *config.Agent, providerKind string) bool {
	return config.IsDeterministicControlDispatcher(agent) &&
		strings.TrimSpace(providerKind) == ""
}

func mergeMCPFingerprintEntry(fpExtra map[string]string, projection materialize.MCPProjection) map[string]string {
	if projection.Provider == "" {
		return fpExtra
	}
	if fpExtra == nil {
		fpExtra = make(map[string]string, 1)
	}
	fpExtra["mcp:"+projection.Provider] = projection.Hash()
	return fpExtra
}

func appendProjectMCPPreStart(prestart []string, agentName, identity, workDir string) []string {
	cmd := `"${GC_BIN:-gc}" internal project-mcp --agent ` +
		shellquote.Join([]string{agentName}) +
		` --identity ` + shellquote.Join([]string{identity}) +
		` --workdir ` + shellquote.Join([]string{workDir})
	return append(prestart, cmd)
}

// ensureMCPGitignoreBestEffort adds entries, the .gitignore entries of the
// MCP targets gc projected under root, to root's .gitignore, unless it
// already has them (missingMCPGitignoreEntries).
func ensureMCPGitignoreBestEffort(root string, entries []string, stderr io.Writer) {
	if strings.TrimSpace(root) == "" {
		return
	}
	entries = missingMCPGitignoreEntries(fsys.OSFS{}, root, entries)
	if len(entries) == 0 {
		return
	}
	if err := ensureGitignoreEntries(fsys.OSFS{}, root, entries); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "gc: warning: updating %s/.gitignore for MCP: %v\n", root, err) //nolint:errcheck // best-effort stderr
	}
}

// stage1MCPSkipWarned remembers which stage-1 skip warnings have been
// printed, keyed by city and message, so a supervisor that runs stage 1 again
// (a city start retried after a failure, a config reload) warns once per
// process. A changed skip (another agent, another server) is a new message
// and warns again.
var stage1MCPSkipWarned sync.Map

// warnStage1MCPOnce prints a stage-1 skip warning naming its city, since a
// supervisor's log interleaves every city it runs.
func warnStage1MCPOnce(cityPath, cityName, message string, stderr io.Writer) {
	if stderr == nil {
		return
	}
	key := filepath.Clean(cityPath) + "\x00" + message
	if _, seen := stage1MCPSkipWarned.LoadOrStore(key, true); seen {
		return
	}
	fmt.Fprintf(stderr, "gc: warning: city %q: %s\n", cityName, message) //nolint:errcheck // best-effort stderr
}

// isProviderResolutionError reports whether err says an agent's provider
// does not resolve: its binary is not installed, or its name is not in the
// provider catalog. That agent cannot start, and its own session start says
// why, but it must not stop stage 1 for the rest of the city. Every provider
// a city declares has an implicit agent that inherits the city's MCP, so one
// declared provider that is not installed is enough.
func isProviderResolutionError(err error) bool {
	return errors.Is(err, config.ErrProviderNotInPATH) || errors.Is(err, config.ErrProviderNotFound)
}

// stage1MCPTargets is what a stage-1 pass reconciles: the targets to apply,
// and the managed keys of agents it skipped. A skipped agent cannot start,
// but the target it last projected is left as it was instead of being
// cleaned up as an orphan: a session started before the skip may still read
// it, and the next stage-1 pass (a city start or config reload) projects it
// again once the agent resolves.
type stage1MCPTargets struct {
	targets []mcpTargetSpec
	held    []string
}

func buildStage1MCPTargets(cityPath string, cfg *config.City, lookPath config.LookPathFunc, stderr io.Writer) ([]mcpTargetSpec, error) {
	set, err := collectStage1MCPTargets(cityPath, cfg, lookPath, stderr, false)
	return set.targets, err
}

// heldStage1MCPKey returns the managed key of the stage-1 target a skipped
// agent would reconcile: set when its provider family has an MCP projection
// and its work dir is its scope root. The family resolves from config alone,
// since the provider binary may be what is missing. With pathOnly, the work
// dir is resolved without being created.
func heldStage1MCPKey(cityPath string, cfg *config.City, agent *config.Agent, pathOnly bool) (string, bool) {
	resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, resolveAnyProviderBinary)
	if err != nil {
		return "", false
	}
	kind := resolvedProviderLaunchFamily(resolved)
	if !supportsMCPProviderKind(kind) {
		return "", false
	}
	scopeRoot := agentScopeRoot(agent, cityPath, cfg.Rigs)
	resolveWorkDir := resolveWorkDirForQualifiedName
	if pathOnly {
		resolveWorkDir = resolveWorkDirPathForQualifiedName
	}
	workDir, err := resolveWorkDir(cityPath, cfg, agent, agent.QualifiedName())
	if err != nil || canonicaliseFilePath(workDir, cityPath) != scopeRoot {
		return "", false
	}
	return managedKey(kind, scopeRoot), true
}

// collectStage1MCPTargets resolves every stage-1 agent's MCP target, and the
// targets of the agents it skips. With pathOnly, agents' work dirs are
// resolved without being created, for a plan that must write nothing.
func collectStage1MCPTargets(cityPath string, cfg *config.City, lookPath config.LookPathFunc, stderr io.Writer, pathOnly bool) (stage1MCPTargets, error) {
	if cfg == nil {
		return stage1MCPTargets{}, nil
	}
	byKey := make(map[string]mcpTargetSpec)
	var (
		skips mcpSkipGroups
		held  []string
	)
	blocked := make(map[string]string)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if !canStage1Materialize(cfg.Session.Provider, agent) {
			continue
		}
		view, err := resolveConfiguredAgentMCPProjectionWith(cityPath, cfg, agent, lookPath, pathOnly)
		if isAgentMCPCapabilityError(err) || isProviderResolutionError(err) {
			// One agent that cannot start must not fail the whole city:
			// skip it here. Its own session start reports the same error.
			blocked[mcpAgentLabel(agent)] = err.Error()
			if key, ok := heldStage1MCPKey(cityPath, cfg, agent, pathOnly); ok {
				held = append(held, key)
			}
			continue
		}
		if err != nil {
			return stage1MCPTargets{}, fmt.Errorf("agent %q: %w", agent.QualifiedName(), err)
		}
		// Shared-layer servers the provider cannot receive are already out
		// of view.Projection; say which agents start without them.
		skips.add(mcpAgentLabel(agent), view.Skipped)
		if view.Delivery != "stage1" {
			continue
		}
		if view.Projection.Provider == "" && len(view.Catalog.Servers) == 0 {
			continue
		}
		key := view.Projection.Provider + "|" + view.Projection.Target
		existing, ok := byKey[key]
		if ok {
			if existing.Projection.Hash() != view.Projection.Hash() {
				return stage1MCPTargets{}, fmt.Errorf(
					"MCP target conflict at %s (%s): %s projects %s but %s projects %s",
					view.Projection.Target,
					view.Projection.Provider,
					strings.Join(existing.Agents, ", "),
					existing.Projection.Hash(),
					agent.QualifiedName(),
					view.Projection.Hash(),
				)
			}
			existing.Agents = append(existing.Agents, agent.QualifiedName())
			sort.Strings(existing.Agents)
			byKey[key] = existing
			continue
		}
		byKey[key] = mcpTargetSpec{
			Root:       view.ScopeRoot,
			Projection: view.Projection,
			Agents:     []string{agent.QualifiedName()},
		}
	}

	cityName := loadedCityName(cfg, cityPath)
	for _, line := range append(skips.lines(), groupBlockedAgents(blocked)...) {
		warnStage1MCPOnce(cityPath, cityName, line, stderr)
	}

	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]mcpTargetSpec, 0, len(keys))
	for _, key := range keys {
		out = append(out, byKey[key])
	}
	return stage1MCPTargets{targets: out, held: held}, nil
}

func runStage1MCPProjection(cityPath string, cfg *config.City, lookPath config.LookPathFunc, stderr io.Writer) error {
	set, err := collectStage1MCPTargets(cityPath, cfg, lookPath, stderr, false)
	if err != nil {
		return err
	}
	desired := make(map[string]bool, len(set.targets)+len(set.held)) // provider+root keys we still own
	// A skipped agent's target is left as it was, not cleaned up as an
	// orphan (stage1MCPTargets).
	for _, key := range set.held {
		desired[key] = true
	}
	// .gitignore entries for the targets projected so far, by root; written
	// on every way out, so a later target's failure does not leave a
	// projected file unignored.
	gitignore := make(map[string][]string)
	defer func() {
		for _, root := range sortedMapKeys(gitignore) {
			ensureMCPGitignoreBestEffort(root, gitignore[root], stderr)
		}
	}()
	for _, target := range set.targets {
		err := target.Projection.ApplyWithStderr(fsys.OSFS{}, stderr)
		var refused *materialize.MCPAdoptionRefusedError
		if errors.As(err, &refused) {
			// The file holds content gc does not manage. gc leaves it as it
			// is, and the agents that read it start without gc's servers.
			warnMCPAdoptionRefused(cityPath, target.Agents, target.Projection, refused, fsys.OSFS{}, stderr)
			continue
		}
		if err != nil {
			return fmt.Errorf("reconciling %s: %w", target.Projection.Target, err)
		}
		clearMCPAdoptionRefusal(cityPath, target.Projection.Target)
		desired[managedKey(target.Projection.Provider, target.Root)] = true
		if len(target.Projection.Servers) > 0 {
			gitignore[target.Root] = append(gitignore[target.Root], mcpGitignoreEntry(target.Projection))
		}
	}
	if err := cleanupOrphanStage1Targets(cityPath, cfg, desired, stderr); err != nil {
		return err
	}
	return nil
}

// mcpAdoptionRefusals remembers the refusals reported so far, so a refusal is
// reported once, and again when it comes back:
//
//   - byTarget maps a city and target (mcpRefusalTargetKey) to each refusal
//     message reported for it, with a stamp of the target's content then
//     (mcpTargetStamp; for a symlink refusal, the link alone). Agents that
//     share a target are named in messages of their own, so keying on the
//     message keeps them from evicting each other's entry.
//   - A message is reported again when the target's content changes: a team
//     edits the file, or a session's work dir is checked out with other
//     content. The content, not the modification time, is what counts, so
//     gc's own rewrites of the same bytes (overlay staging, for one) do not
//     report it on every tick. A work dir checked out afresh with the same
//     content is reported again only after a pass saw the file missing.
//   - clearMCPAdoptionRefusal forgets every message for a target once gc
//     writes it or finds nothing in its way, so a refusal that comes back
//     after it cleared is reported again.
var mcpAdoptionRefusals = struct {
	sync.Mutex
	byTarget map[string]map[string]string
}{byTarget: map[string]map[string]string{}}

func mcpRefusalTargetKey(cityPath, target string) string {
	return filepath.Clean(cityPath) + "\x00" + filepath.Clean(target)
}

// mcpTargetStamp identifies the target's current content: the SHA-256 of a
// regular file of at most materialize.MaxMCPFileSize bytes. It never reads
// anything else (a symlink, a named pipe, a device, a larger file), since
// reading one can block or never end; such a target is stamped by its type
// or its size instead. "" when it cannot be inspected.
func mcpTargetStamp(fs fsys.FS, target string) string {
	data, err := materialize.ReadMCPFile(fs, target)
	var notRegular *materialize.NotRegularFileError
	var tooLarge *materialize.FileTooLargeError
	switch {
	case err == nil:
		sum := sha256.Sum256(data)
		return "sha256:" + hex.EncodeToString(sum[:])
	case errors.As(err, &notRegular):
		return "type:" + notRegular.Mode.Type().String()
	case errors.As(err, &tooLarge):
		return fmt.Sprintf("size:%d", tooLarge.Size)
	default:
		return ""
	}
}

// warnMCPAdoptionRefused reports a target gc will not adopt: who starts
// without which of gc's servers, why, and what to do about it. It reports a
// refusal once until the target's content changes or the refusal clears
// (mcpAdoptionRefusals).
func warnMCPAdoptionRefused(cityPath string, agents []string, projection materialize.MCPProjection, refused *materialize.MCPAdoptionRefusedError, fs fsys.FS, stderr io.Writer) {
	if stderr == nil {
		return
	}
	message := describeMCPAdoptionRefused(agents, projection, refused)
	key := mcpRefusalTargetKey(cityPath, projection.Target)
	// A symlink refusal does not depend on what the link points at, and gc
	// never reads through a link.
	stamp := "symlink:" + refused.Symlink
	if refused.Symlink == "" {
		stamp = mcpTargetStamp(fs, projection.Target)
	}
	mcpAdoptionRefusals.Lock()
	reported := mcpAdoptionRefusals.byTarget[key]
	if previous, seen := reported[message]; seen && previous == stamp {
		mcpAdoptionRefusals.Unlock()
		return
	}
	if reported == nil {
		reported = make(map[string]string, 1)
		mcpAdoptionRefusals.byTarget[key] = reported
	}
	reported[message] = stamp
	mcpAdoptionRefusals.Unlock()
	fmt.Fprintf(stderr, "gc: warning: %s\n", message) //nolint:errcheck // best-effort stderr
}

// clearMCPAdoptionRefusal forgets every refusal reported for a target, so a
// refusal that comes back is reported again.
func clearMCPAdoptionRefusal(cityPath, target string) {
	mcpAdoptionRefusals.Lock()
	delete(mcpAdoptionRefusals.byTarget, mcpRefusalTargetKey(cityPath, target))
	mcpAdoptionRefusals.Unlock()
}

// warnStage2MCPAdoptionRefusal reports a stage-2 target the session's
// project-mcp pre-start will leave alone (warnMCPAdoptionRefused). The
// pre-start warns too, but a runtime discards a pre-start's output when it
// succeeds, so this is the warning the controller log shows. It reads the
// target and writes nothing; a target that cannot be inspected is the
// pre-start's to report.
func warnStage2MCPAdoptionRefusal(cityPath, identity string, projection materialize.MCPProjection, fs fsys.FS, stderr io.Writer) {
	if stderr == nil || fs == nil {
		return
	}
	refused, err := projection.AdoptionRefusal(fs)
	if err != nil {
		return
	}
	if refused == nil {
		clearMCPAdoptionRefusal(cityPath, projection.Target)
		return
	}
	warnMCPAdoptionRefused(cityPath, []string{identity}, projection, refused, fs, stderr)
}

// describeMCPAdoptionRefused renders a refused adoption for a warning:
// `agent "a" starts without gc's MCP server "s": not adopting <target>:
// <reason>; <remedy>`.
func describeMCPAdoptionRefused(agents []string, projection materialize.MCPProjection, refused *materialize.MCPAdoptionRefusedError) string {
	quoted := make([]string, len(agents))
	for i, agent := range agents {
		quoted[i] = strconv.Quote(agent)
	}
	who := "agent " + strings.Join(quoted, ", ") + " starts"
	if len(quoted) > 1 {
		who = "agents " + strings.Join(quoted, ", ") + " start"
	}
	return fmt.Sprintf("%s without %s: %v; %s", who, describeGCMCPServers(projection), refused, refused.Remedy())
}

// describeGCMCPServers renders `gc's MCP server "a"` or `gc's MCP servers
// "a", "b"`.
func describeGCMCPServers(projection materialize.MCPProjection) string {
	servers := make([]string, len(projection.Servers))
	for i, server := range projection.Servers {
		servers[i] = strconv.Quote(server.Name)
	}
	if len(servers) == 1 {
		return "gc's MCP server " + servers[0]
	}
	return "gc's MCP servers " + strings.Join(servers, ", ")
}

// managedKey combines provider and scope-root into the key used by stage-1
// reconcile to recognize which managed markers still have a live claimant.
func managedKey(provider, root string) string {
	return provider + "|" + filepath.Clean(root)
}

// cleanupOrphanStage1Targets walks .gc/mcp-managed/ under every scope root
// reachable from the current config and reconciles away managed markers
// that have no desired claimant. Covered: agents removed from city.toml,
// provider changes that move a target between provider subtrees, and
// managed markers under still-attached rig roots that no longer have an
// agent claiming them.
//
// Not covered: rigs detached from city.toml (their path is no longer in
// cfg.Rigs, so we cannot reach their .gc/mcp-managed/ to sweep it).
// Detached-rig cleanup is explicit work for an operator — `gc rig detach`
// or a future `gc mcp reconcile --root <path>` command. If needed, the
// operator can also delete .gc/mcp-managed/ under the detached rig root
// by hand; the managed files that remain outside GC's view are
// structurally equivalent to any other hand-authored MCP surface.
//
// Stage-2 workdirs are also not swept here: they are self-reconciled on
// every session start and have no stable root registry.
func cleanupOrphanStage1Targets(cityPath string, cfg *config.City, desired map[string]bool, stderr io.Writer) error {
	if cfg == nil {
		return nil
	}
	roots := collectStage1ScopeRoots(cityPath, cfg)
	for _, root := range roots {
		if err := cleanupOrphansAtRoot(root, desired, stderr); err != nil {
			return err
		}
	}
	return nil
}

func collectStage1ScopeRoots(cityPath string, cfg *config.City) []string {
	seen := make(map[string]bool)
	add := func(root string) {
		if strings.TrimSpace(root) == "" {
			return
		}
		clean := filepath.Clean(root)
		if !filepath.IsAbs(clean) {
			clean = filepath.Clean(filepath.Join(cityPath, clean))
		}
		seen[clean] = true
	}
	add(cityPath)
	for _, rig := range cfg.Rigs {
		add(rig.Path)
	}
	out := make([]string, 0, len(seen))
	for root := range seen {
		out = append(out, root)
	}
	sort.Strings(out)
	return out
}

func cleanupOrphansAtRoot(root string, desired map[string]bool, stderr io.Writer) error {
	providers, err := orphanStage1Markers(root, desired)
	if err != nil {
		return err
	}
	for _, provider := range providers {
		orphan, err := materialize.BuildMCPProjection(provider, root, nil)
		if err != nil {
			return fmt.Errorf("building orphan projection for %s at %s: %w", provider, root, err)
		}
		if err := orphan.ApplyWithStderr(fsys.OSFS{}, stderr); err != nil {
			return fmt.Errorf("cleaning up orphan %s marker at %s: %w", provider, root, err)
		}
		if stderr != nil {
			fmt.Fprintf(stderr, "gc: cleaned up orphan MCP marker %s at %s\n", provider, root) //nolint:errcheck // best-effort stderr
		}
	}
	return nil
}

// orphanStage1Markers lists the providers whose managed marker under root has
// no claimant in desired.
func orphanStage1Markers(root string, desired map[string]bool) ([]string, error) {
	markersDir := filepath.Join(root, ".gc", "mcp-managed")
	entries, err := fsys.OSFS{}.ReadDir(markersDir)
	if err != nil {
		// Missing directory is the steady state when no targets have ever
		// been adopted at this root — not an error.
		if os.IsNotExist(err) {
			return nil, nil
		}
		// Permission or corruption problems must surface — silently
		// skipping would leave stale managed state unreconciled and
		// hide operator-facing diagnostics.
		return nil, fmt.Errorf("scanning %s: %w", markersDir, err)
	}
	var providers []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		provider := strings.TrimSuffix(name, ".json")
		if !supportsMCPProviderKind(provider) {
			continue
		}
		if desired[managedKey(provider, root)] {
			continue
		}
		providers = append(providers, provider)
	}
	return providers, nil
}

// agentMayHaveSessionSpecificMCPTargets reports whether an agent's MCP
// projection can legitimately differ between concurrent session identities,
// so resolveDeterministicAgentMCPProjection's alt-identity probe is a
// meaningful check rather than a false positive. This must match
// internal/workdir.RequiresPoolWorkDirIsolationCheck exactly: that is the
// same "does this agent carry an explicit pool signal" question
// ResolveWorkDirPathStrict answers when deciding whether a dir-less agent's
// alt identity gets its own isolated working directory. Using the broader
// config.Agent.SupportsMultipleSessions() here (true for any agent with a
// merely-unset max_active_sessions) would false-positive on ordinary
// dir-less, non-pooled agents, since ResolveWorkDirPathStrict's isolation
// fallback now gives such an agent's alt identity a genuinely different
// (but not "session-specific") working directory.
func agentMayHaveSessionSpecificMCPTargets(a config.Agent) bool {
	return workdirutil.RequiresPoolWorkDirIsolationCheck(a)
}

func resolveDeterministicAgentMCPProjection(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	lookPath config.LookPathFunc,
) (resolvedMCPProjection, error) {
	view, err := resolveConfiguredAgentMCPProjection(cityPath, cfg, agent, lookPath)
	if err != nil || agent == nil || !agentMayHaveSessionSpecificMCPTargets(*agent) || len(view.Catalog.Servers) == 0 {
		return view, err
	}

	altIdentity := agent.QualifiedName() + "-alt"
	altWorkDir, err := resolveWorkDirForQualifiedName(cityPath, cfg, agent, altIdentity)
	if err != nil {
		return resolvedMCPProjection{}, fmt.Errorf("agent %q has session-specific MCP targets; use --session", agent.QualifiedName())
	}
	altView, err := resolveProjectedMCPForTarget(cityPath, cfg, agent, altIdentity, altWorkDir, "", "", lookPath)
	if err != nil {
		return resolvedMCPProjection{}, fmt.Errorf("agent %q has session-specific MCP targets; use --session", agent.QualifiedName())
	}
	if view.Projection.Target != altView.Projection.Target || view.Projection.Hash() != altView.Projection.Hash() {
		return resolvedMCPProjection{}, fmt.Errorf("agent %q has session-specific MCP targets; use --session", agent.QualifiedName())
	}
	return view, nil
}

func resolveConfiguredAgentMCPProjection(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	lookPath config.LookPathFunc,
) (resolvedMCPProjection, error) {
	return resolveConfiguredAgentMCPProjectionWith(cityPath, cfg, agent, lookPath, false)
}

// resolveConfiguredAgentMCPProjectionWith is resolveConfiguredAgentMCPProjection
// with pathOnly: resolve the agent's work dir path without creating it, for a
// plan that must write nothing.
func resolveConfiguredAgentMCPProjectionWith(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	lookPath config.LookPathFunc,
	pathOnly bool,
) (resolvedMCPProjection, error) {
	if cfg == nil || agent == nil {
		return resolvedMCPProjection{}, fmt.Errorf("agent unavailable")
	}
	identity := agent.QualifiedName()
	resolveWorkDir := resolveWorkDirForQualifiedName
	if pathOnly {
		resolveWorkDir = resolveWorkDirPathForQualifiedName
	}
	workDir, err := resolveWorkDir(cityPath, cfg, agent, identity)
	if err != nil {
		catalog, catErr := loadEffectiveMCPForAgent(cityPath, cfg, agent, identity, agentScopeRoot(agent, cityPath, cfg.Rigs))
		if catErr != nil {
			return resolvedMCPProjection{}, fmt.Errorf("loading effective MCP: %w", catErr)
		}
		if len(catalog.Servers) == 0 {
			return resolvedMCPProjection{}, nil
		}
		return resolvedMCPProjection{}, fmt.Errorf("resolving workdir for agent %q: %w", identity, err)
	}
	return resolveProjectedMCPForTarget(cityPath, cfg, agent, identity, workDir, "", "", lookPath)
}

func resolveSessionMCPProjection(
	cityPath string,
	cfg *config.City,
	sessFront *session.Store,
	sessionID string,
	lookPath config.LookPathFunc,
) (resolvedMCPProjection, error) {
	if cfg == nil {
		return resolvedMCPProjection{}, fmt.Errorf("city config unavailable")
	}
	if !sessFront.Backed() {
		return resolvedMCPProjection{}, fmt.Errorf("session store unavailable")
	}
	// Reach the raw session-class store the front door wraps for the named-session
	// resolver and the by-id load; same underlying store, so behavior is unchanged.
	store := sessFront.Store().Store
	id, err := resolveSessionIDAllowClosedWithConfig(cityPath, cfg, store, sessionID)
	if err != nil {
		return resolvedMCPProjection{}, err
	}
	info, err := sessFront.Get(id)
	if err != nil {
		// Name the user-supplied identifier, not the resolved bead id.
		return resolvedMCPProjection{}, fmt.Errorf("loading session %q: %w", sessionID, err)
	}
	template := normalizedSessionTemplateInfo(info, cfg)
	if template == "" {
		template = strings.TrimSpace(info.AgentName)
	}
	template = resolveAgentTemplate(template, cfg)
	agent := findAgentByTemplate(cfg, template)
	if agent == nil {
		return resolvedMCPProjection{}, fmt.Errorf("session %q maps to unknown agent template %q", sessionID, template)
	}
	identity := strings.TrimSpace(info.AgentName)
	if identity == "" {
		identity = agent.QualifiedName()
	}
	workDir := strings.TrimSpace(info.WorkDir)
	if workDir == "" {
		workDir, err = resolveWorkDirForQualifiedName(cityPath, cfg, agent, identity)
		if err != nil {
			return resolvedMCPProjection{}, fmt.Errorf("resolving workdir for session %q: %w", sessionID, err)
		}
	}
	providerKind := strings.TrimSpace(info.ProviderKind)
	if providerKind == "" {
		providerKind = strings.TrimSpace(info.Provider)
	}
	return resolveProjectedMCPForTarget(cityPath, cfg, agent, identity, workDir, providerKind, strings.TrimSpace(info.Transport), lookPath)
}

// validateStage2TargetClaimants enforces that every configured agent
// that would actually land on the same provider-native MCP target as
// the caller projects an identical payload. Stage-1 runs the same check
// at build-time (buildStage1MCPTargets); stage-2 must run it at
// write-time because multiple agents can share a workdir and last-
// writer-wins would otherwise let any of them silently lose their MCP
// surface.
//
// Critically, each candidate agent must be resolved against its **own**
// provider kind and **own** workdir (template expansion against that
// agent's identity) rather than the caller's. Reusing the caller's
// providerKind mis-projects mixed-provider peers (a Codex peer
// projected as "claude" would appear to collide at the caller's
// `.mcp.json` even though at runtime the two write disjoint files);
// reusing the caller's workdir collapses every peer onto the caller's
// target regardless of the peer's own WorkDir template.
//
// The caller's projection is the reference; any other configured agent
// whose own workdir resolves to the same target with a different hash
// aborts the write with a conflict error naming both agents.
//
// Scope limitation: this check iterates *configured* agents, not live
// sessions. A pooled template that produces session-varying MCP
// payloads (e.g., MCP catalogs that expand `{{.AgentName}}`) whose
// concrete sessions share a workdir is not detected here — the
// validator sees only one configured agent and no conflict is raised.
// This is an operator-misconfiguration scenario: if two live sessions
// share a workdir they are already racing for many shared resources
// (git state, skill materialization, hook state) beyond MCP. A future
// enhancement could plumb the session store in and validate against
// live claimants; for now, stage-2 validation covers the
// multi-template same-workdir case and defers same-template live-
// session conflicts to stage-1's build-time check for non-pooled
// scope-root overlaps and to operator review of pool configuration.
func validateStage2TargetClaimants(
	cityPath string,
	cfg *config.City,
	caller *config.Agent,
	want materialize.MCPProjection,
	lookPath config.LookPathFunc,
) error {
	if cfg == nil || want.Provider == "" {
		return nil
	}
	callerName := ""
	if caller != nil {
		callerName = caller.QualifiedName()
	}
	wantHash := want.Hash()
	for i := range cfg.Agents {
		other := &cfg.Agents[i]
		if other.QualifiedName() == callerName {
			continue
		}
		// Implicit agents are synthetic provider-coverage entries
		// (config.InjectImplicitAgents) used for sling target
		// materialization, not real MCP writers. They never invoke
		// `gc internal project-mcp`, so they cannot race with the
		// caller for this target.
		if other.Implicit {
			continue
		}
		identity := other.QualifiedName()
		// Resolve the candidate's own provider BEFORE projecting.
		// Reusing the caller's providerKind would mis-project mixed-
		// provider peers into the caller's target shape — a Codex
		// peer's catalog projected as "claude" would appear to
		// collide at the caller's `.mcp.json` even though the peer
		// would actually write `.codex/config.toml` at runtime.
		otherResolved, err := config.ResolveProvider(other, &cfg.Workspace, cfg.Providers, lookPath)
		if err != nil {
			// Cannot resolve this peer's provider — it can't claim
			// any target. Not a conflict.
			continue
		}
		otherKind := resolvedProviderLaunchFamily(otherResolved)
		if otherKind != want.Provider {
			// Different provider family — targets live in disjoint
			// subtrees (`.mcp.json` / `.gemini/settings.json` /
			// `.codex/config.toml`), so no collision is possible.
			continue
		}
		// Resolve the candidate's own workdir. Failures here are
		// treated as "this agent can't claim any target in this city
		// right now" — not a conflict.
		otherWorkDir, err := resolveWorkDirForQualifiedName(cityPath, cfg, other, identity)
		if err != nil {
			continue
		}
		peer, err := resolveAgentMCPProjection(cityPath, cfg, other, identity, otherWorkDir, otherKind, false)
		if err != nil {
			continue
		}
		projection := peer.Projection
		if projection.Provider != want.Provider || projection.Target != want.Target {
			// Different physical target, no conflict.
			continue
		}
		if projection.Hash() != wantHash {
			return fmt.Errorf(
				"MCP target conflict at %s (%s): agent %q projects %s but agent %q projects %s",
				want.Target,
				want.Provider,
				callerName,
				wantHash,
				other.QualifiedName(),
				projection.Hash(),
			)
		}
	}
	return nil
}

// resolveProjectedMCPForTarget resolves an agent's MCP for one concrete
// target. providerKind and transport describe a live session; empty values
// resolve them from config.
func resolveProjectedMCPForTarget(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	identity, workDir, providerKind, transport string,
	lookPath config.LookPathFunc,
) (resolvedMCPProjection, error) {
	if cfg == nil || agent == nil {
		return resolvedMCPProjection{}, fmt.Errorf("agent unavailable")
	}
	if strings.TrimSpace(identity) == "" {
		identity = agent.QualifiedName()
	}
	scopeRoot := agentScopeRoot(agent, cityPath, cfg.Rigs)
	inBand := transport == config.SessionTransportACP
	if strings.TrimSpace(providerKind) == "" {
		resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, lookPath)
		if err != nil {
			catalog, catErr := loadEffectiveMCPForAgent(cityPath, cfg, agent, identity, workDir)
			if catErr != nil {
				return resolvedMCPProjection{}, catErr
			}
			if len(catalog.Servers) == 0 {
				return resolvedMCPProjection{}, nil
			}
			return resolvedMCPProjection{}, err
		}
		providerKind = resolvedProviderLaunchFamily(resolved)
		if transport == "" {
			inBand = mcpInBand(agent, resolved)
		}
	} else if transport == "" {
		inBand = mcpInBand(agent, nil)
	}
	mcp, err := resolveAgentMCPProjection(cityPath, cfg, agent, identity, workDir, providerKind, inBand)
	if err != nil {
		return resolvedMCPProjection{}, err
	}
	canonWorkDir := canonicaliseFilePath(workDir, cityPath)
	stage1 := canStage1Materialize(cfg.Session.Provider, agent) && canonWorkDir == scopeRoot
	stage2 := isStage2EligibleSession(cfg.Session.Provider, agent) && canonWorkDir != scopeRoot
	if len(mcp.Catalog.Servers) > 0 && !stage1 && !stage2 && !inBand {
		return resolvedMCPProjection{}, fmt.Errorf(
			"effective MCP cannot be delivered to workdir %q with session provider %q",
			canonWorkDir,
			cfg.Session.Provider,
		)
	}
	delivery := ""
	switch {
	case stage1:
		delivery = "stage1"
	case stage2:
		delivery = "stage2"
	case inBand:
		delivery = "acp"
	}
	return resolvedMCPProjection{
		Agent:        agent,
		Identity:     identity,
		WorkDir:      canonWorkDir,
		ScopeRoot:    scopeRoot,
		ProviderKind: providerKind,
		Delivery:     delivery,
		Catalog:      mcp.Catalog,
		Projection:   mcp.Projection,
		Skipped:      mcp.Skipped,
	}, nil
}
