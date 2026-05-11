package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

const lookPathCacheTTL = 30 * time.Second

// agentVisibilityPollInterval is how often WaitForAgentVisibilityIn re-reads
// the cfg snapshot while waiting for a freshly created agent to become
// resolvable through findAgent. Kept short because the typical race window
// (a runtime config-reload tick that started before the mutation but applies
// after it) is sub-second; the fast cadence keeps the POST /agents response
// from blocking the caller for a perceptible time on the happy path.
const agentVisibilityPollInterval = 50 * time.Millisecond

// defaultAgentVisibilityWaitTimeout bounds the POST /agents read-after-write wait.
// The controller should converge much faster; this timeout prevents a broken
// projection from tying up the handler after the config mutation succeeded.
const defaultAgentVisibilityWaitTimeout = 3 * time.Second

func (s *Server) agentCreateVisibilityWaitTimeout() time.Duration {
	if s.agentVisibilityWaitTimeout > 0 {
		return s.agentVisibilityWaitTimeout
	}
	return defaultAgentVisibilityWaitTimeout
}

type agentResponse struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Running     bool         `json:"running"`
	Suspended   bool         `json:"suspended"`
	Rig         string       `json:"rig,omitempty"`
	Pool        string       `json:"pool,omitempty"`
	Session     *sessionInfo `json:"session,omitempty"`
	ActiveBead  string       `json:"active_bead,omitempty"`

	Provider    string `json:"provider,omitempty"`
	DisplayName string `json:"display_name,omitempty"`

	State string `json:"state"`

	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`

	LastOutput string `json:"last_output,omitempty"`

	// Activity indicates session turn state: "idle", "in-turn", or omitted.
	Activity string `json:"activity,omitempty"`

	Model         string `json:"model,omitempty"`
	ContextPct    *int   `json:"context_pct,omitempty"`
	ContextWindow *int   `json:"context_window,omitempty"`
}

type sessionInfo struct {
	Name         string     `json:"name"`
	LastActivity *time.Time `json:"last_activity,omitempty"`
	Attached     bool       `json:"attached"`
}

// expandedAgent holds a single (possibly pool-expanded) agent identity.
type expandedAgent struct {
	qualifiedName string
	rig           string
	pool          string
	suspended     bool
	provider      string
	description   string
}

// expandAgent expands a config.Agent into its effective runtime agents.
// For bounded pool agents, this generates pool-1..pool-max members.
// For unlimited pools (max < 0), it discovers running instances via session
// provider prefix matching — the same approach as discoverPoolInstances.
func expandAgent(a config.Agent, cityName, sessTmpl string, sp sessionLister) []expandedAgent {
	maxSess := a.EffectiveMaxActiveSessions()
	isMultiSession := maxSess == nil || *maxSess != 1

	if !isMultiSession {
		return []expandedAgent{{
			qualifiedName: a.QualifiedName(),
			rig:           a.Dir,
			suspended:     a.Suspended,
			provider:      a.Provider,
			description:   a.Description,
		}}
	}

	poolName := a.QualifiedName()

	// Unlimited: discover running instances via session prefix.
	isUnlimited := maxSess == nil || *maxSess < 0
	if isUnlimited && sp != nil {
		return discoverUnlimitedPool(a, poolName, cityName, sessTmpl, sp)
	}

	// Bounded: static enumeration.
	poolMax := 1
	if maxSess != nil && *maxSess > 1 {
		poolMax = *maxSess
	}

	var result []expandedAgent
	for i := 1; i <= poolMax; i++ {
		memberName := poolInstanceNameForAPI(a.Name, i, a)
		qn := a.QualifiedInstanceName(memberName)
		result = append(result, expandedAgent{
			qualifiedName: qn,
			rig:           a.Dir,
			pool:          poolName,
			suspended:     a.Suspended,
			provider:      a.Provider,
			description:   a.Description,
		})
	}
	return result
}

// sessionLister is the subset of session.Provider needed for pool discovery.
type sessionLister interface {
	ListRunning(prefix string) ([]string, error)
}

// discoverUnlimitedPool finds running instances of an unlimited pool by
// listing sessions with a matching prefix, then reverse-mapping session
// names back to qualified agent names.
func discoverUnlimitedPool(a config.Agent, poolName, cityName, sessTmpl string, sp sessionLister) []expandedAgent {
	// Build session name prefix: e.g. "city--myrig--polecat-"
	qnPrefix := a.QualifiedName() + "-"
	snPrefix := agent.SessionNameFor(cityName, qnPrefix, sessTmpl)

	running, err := sp.ListRunning(snPrefix)
	if err != nil || len(running) == 0 {
		return nil
	}

	// Reverse session names back to qualified agent names.
	templatePrefix := agent.SessionNameFor(cityName, "", sessTmpl)
	var result []expandedAgent
	for _, sn := range running {
		qnSanitized := sn
		if templatePrefix != "" && strings.HasPrefix(qnSanitized, templatePrefix) {
			qnSanitized = qnSanitized[len(templatePrefix):]
		}
		qn := agent.UnsanitizeQualifiedNameFromSession(qnSanitized)
		result = append(result, expandedAgent{
			qualifiedName: qn,
			rig:           a.Dir,
			pool:          poolName,
			suspended:     a.Suspended,
			provider:      a.Provider,
			description:   a.Description,
		})
	}
	return result
}

// agentSessionName converts a qualified agent name to a tmux session name
// using the canonical naming contract from agent.SessionNameFor.
func agentSessionName(cityName, qualifiedName, sessionTemplate string) string {
	return agent.SessionNameFor(cityName, qualifiedName, sessionTemplate)
}

// WaitForAgentVisibilityIn polls cfgSnapshot() until findAgent resolves the
// given qualified agent name, or returns an error if ctx is done. It is the
// shared building block for AgentVisibilityWaiter implementations.
//
// Callers pass cs.Config (or any other snapshot accessor that returns the
// hot-reloaded *config.City) so the polling reads the live snapshot, not a
// stale capture. The first check happens before the first sleep so the
// happy path returns immediately when no runtime race occurred.
func WaitForAgentVisibilityIn(ctx context.Context, cfgSnapshot func() *config.City, qualifiedName string) error {
	return waitForAgentVisibilityIn(ctx, cfgSnapshot, qualifiedName, agentVisibilityPollInterval)
}

func waitForAgentVisibilityIn(ctx context.Context, cfgSnapshot func() *config.City, qualifiedName string, interval time.Duration) error {
	check := func() bool {
		cfg := cfgSnapshot()
		if cfg == nil {
			return false
		}
		_, ok := findAgent(cfg, qualifiedName)
		return ok
	}
	if check() {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for agent %q to become visible: %w", qualifiedName, ctx.Err())
		case <-ticker.C:
		}
		if check() {
			return nil
		}
	}
}

// findAgent looks up an agent by qualified name in the config.
// For multi-session agents, it matches instance names.
func findAgent(cfg *config.City, name string) (config.Agent, bool) {
	dir, baseName := config.ParseQualifiedName(name)
	for _, a := range cfg.Agents {
		if config.AgentMatchesIdentity(&a, name) {
			return a, true
		}
		// Check multi-session instance members.
		maxSess := a.EffectiveMaxActiveSessions()
		isMultiSession := maxSess == nil || *maxSess != 1
		if isMultiSession && a.Dir == dir {
			isUnlimited := maxSess == nil || *maxSess < 0
			if isUnlimited {
				// Unlimited: match "{name}-{N}" or "{binding.name}-{N}" where N >= 1.
				// For V2 agents, try binding-qualified prefix first.
				prefixes := []string{a.Name + "-"}
				if a.BindingName != "" {
					prefixes = append([]string{a.BindingName + "." + a.Name + "-"}, prefixes...)
				}
				matched := false
				for _, prefix := range prefixes {
					if strings.HasPrefix(baseName, prefix) {
						suffix := baseName[len(prefix):]
						if n, err := strconv.Atoi(suffix); err == nil && n >= 1 {
							matched = true
							break
						}
					}
				}
				if matched {
					return a, true
				}
				continue
			}
			// Bounded: enumerate.
			poolMax := *maxSess
			if poolMax <= 0 {
				poolMax = 1
			}
			for i := 1; i <= poolMax; i++ {
				memberName := poolInstanceNameForAPI(a.Name, i, a)
				// V2 agents address instances with the binding prefix
				// (matching Agent.QualifiedInstanceName), so accept both
				// the bare and binding-qualified forms — same shape the
				// unlimited path applies above.
				if memberName == baseName {
					return a, true
				}
				if a.BindingName != "" && a.BindingName+"."+memberName == baseName {
					return a, true
				}
			}
		}
	}
	return config.Agent{}, false
}

// findActiveBeadForAssignees returns the ID of the first in_progress bead
// assigned to the given identities using the cached active snapshot. If rig is
// non-empty, only that rig's store is searched; otherwise all stores are
// searched. Returns "" if no match.
func (s *Server) findActiveBeadForAssignees(rig string, assignees ...string) string {
	return s.findActiveBeadForAssigneesWithFreshness(rig, false, assignees...)
}

// findLiveActiveBeadForAssignees returns the ID of the first in_progress bead
// assigned to the given identities, bypassing the cache. Use this on
// lower-frequency detail views where external reassignment freshness matters.
func (s *Server) findLiveActiveBeadForAssignees(rig string, assignees ...string) string {
	return s.findActiveBeadForAssigneesWithFreshness(rig, true, assignees...)
}

// findActiveBeadForAssigneesWithFreshness uses a targeted ListQuery with
// Limit=1 instead of broad scans so active-bead lookup stays cheap even when
// bead counts are large.
func (s *Server) findActiveBeadForAssigneesWithFreshness(rig string, live bool, assignees ...string) string {
	stores := s.state.BeadStores()
	var rigNames []string
	if rig != "" {
		if _, ok := stores[rig]; ok {
			rigNames = []string{rig}
		}
	}
	if rigNames == nil {
		rigNames = sortedRigNames(stores)
	}
	seen := make(map[string]bool, len(assignees))
	var unique []string
	for _, assignee := range assignees {
		assignee = strings.TrimSpace(assignee)
		if assignee == "" || seen[assignee] {
			continue
		}
		seen[assignee] = true
		unique = append(unique, assignee)
	}
	for _, assignee := range unique {
		for _, rn := range rigNames {
			query := beads.ListQuery{
				Assignee: assignee,
				Status:   "in_progress",
				Live:     live,
				Limit:    1,
				Sort:     beads.SortCreatedDesc,
			}
			if !live {
				if cached, ok := stores[rn].(cachedListStore); ok {
					matches, cacheOK := cached.CachedList(query)
					if cacheOK {
						if len(matches) > 0 {
							return matches[0].ID
						}
						continue
					}
				}
			}
			matches, err := stores[rn].List(query)
			if err != nil {
				continue
			}
			if len(matches) > 0 {
				return matches[0].ID
			}
		}
	}
	return ""
}

// providerPathCheck returns the binary name to check for PATH availability.
// Uses the provider's PathCheck field if set (e.g., "claude" for the sh -c wrapper),
// otherwise falls back to the provider's Command.
//
// Lookup order:
//  1. Resolved-provider cache (ResolvedProviderCached) — picks up
//     inherited Command/PathCheck for base-only descendants.
//  2. Raw city-level spec — fallback for Phase A configs without `base`.
//  3. Builtin spec — covers pure-builtin providers with no city override.
//  4. The provider name itself — last-resort sentinel so callers can
//     still exec.LookPath something readable.
func providerPathCheck(providerName string, cfg *config.City) string {
	if resolved, ok := config.ResolvedProviderCached(cfg, providerName); ok {
		// ResolvedProvider.Command is fully inherited; PathCheck is
		// on the raw spec, so check it first on the raw then fall
		// through to the resolved Command.
		if spec, ok := cfg.Providers[providerName]; ok && spec.PathCheck != "" {
			return spec.PathCheck
		}
		if resolved.Command != "" {
			return resolved.Command
		}
	}
	if spec, ok := cfg.Providers[providerName]; ok {
		if spec.PathCheck != "" {
			return spec.PathCheck
		}
		if spec.Command != "" {
			return spec.Command
		}
	}
	builtins := config.BuiltinProviders()
	if spec, ok := builtins[providerName]; ok {
		if spec.PathCheck != "" {
			return spec.PathCheck
		}
		return spec.Command
	}
	return providerName
}

// resolveProviderInfo resolves the provider name and display name for an agent.
// Falls back to workspace default if the agent doesn't specify a provider.
//
// DisplayName lookup consults the resolved-provider cache first so
// base-only descendants inherit their ancestor's display name when the
// leaf didn't declare its own. Raw city spec and builtin spec are
// fallbacks for Phase A configs where the cache may not have an entry.
func resolveProviderInfo(agentProvider string, cfg *config.City) (provider, displayName string) {
	provider = agentProvider
	if provider == "" {
		provider = cfg.Workspace.Provider
	}
	if provider == "" {
		return "", ""
	}

	// Prefer the raw spec's DisplayName when explicitly set — leaf
	// authors expect their city.toml's display_name to win. If the
	// leaf didn't set one, consult the cache (so inherited names
	// surface for base-only descendants). Fall through to builtins.
	if spec, ok := cfg.Providers[provider]; ok && spec.DisplayName != "" {
		return provider, spec.DisplayName
	}
	// Cached resolution doesn't carry DisplayName today (the field
	// sits on ProviderSpec, not ResolvedProvider). Use the base
	// chain's builtin ancestor as a proxy: if the cache reports a
	// BuiltinAncestor, look up its DisplayName.
	if resolved, ok := config.ResolvedProviderCached(cfg, provider); ok && resolved.BuiltinAncestor != "" {
		if bspec, bok := config.BuiltinProviders()[resolved.BuiltinAncestor]; bok && bspec.DisplayName != "" {
			return provider, bspec.DisplayName
		}
	}
	// Fall back to built-in providers.
	if spec, ok := config.BuiltinProviders()[provider]; ok {
		return provider, spec.DisplayName
	}
	// Unknown provider — title-case the name.
	return provider, strings.ToUpper(provider[:1]) + provider[1:]
}

// computeAgentState derives the state enum from existing agent data.
func computeAgentState(suspended, quarantined, running bool, activeBead string, lastActivity *time.Time) string {
	if suspended {
		return "suspended"
	}
	if quarantined {
		return "quarantined"
	}
	if !running {
		return "stopped"
	}
	if activeBead != "" {
		if lastActivity != nil && time.Since(*lastActivity) < 10*time.Minute {
			return "working"
		}
		return "waiting"
	}
	return "idle"
}

// enrichSessionMeta populates model and context usage fields on the agent
// response by reading the tail of the agent's session JSONL file.
func (s *Server) enrichSessionMeta(resp *agentResponse, agentCfg config.Agent, qualifiedName string) {
	factory, err := s.workerFactory(s.state.CityBeadStore())
	if err != nil {
		return
	}
	transcriptState, err := s.resolveAgentTranscript(qualifiedName, agentCfg)
	if err != nil {
		return
	}
	sessionFile := transcriptState.path
	if sessionFile == "" {
		return
	}
	meta, err := factory.TailMeta(sessionFile)
	if err != nil || meta == nil {
		return
	}
	resp.Model = meta.Model
	if meta.ContextUsage != nil {
		resp.ContextPct = &meta.ContextUsage.Percentage
		resp.ContextWindow = &meta.ContextUsage.ContextWindow
	}
	resp.Activity = meta.Activity
}

// canAttributeSession reports whether session file attribution is unambiguous
// for the given agent in its rig. Returns false when multiple Claude agents
// or multi-session instances share the same rig directory, since we can't
// reliably determine which session file belongs to which agent.
func canAttributeSession(agentCfg config.Agent, qualifiedName string, cfg *config.City, cityPath string) bool {
	// Multi-session agents derive per-instance workdirs from the qualified
	// name, but the API only has the base config when attributing list rows.
	// Keep them on the safe side and skip attribution.
	if isMultiSessionAgent(agentCfg) {
		return false
	}
	cityName := workdirutil.CityName(cityPath, cfg)
	target := workdirutil.ResolveWorkDirPath(cityPath, cityName, qualifiedName, agentCfg, cfg.Rigs)
	if target == "" {
		return false
	}
	count := 0
	for _, a := range cfg.Agents {
		provider := a.Provider
		if provider == "" {
			provider = cfg.Workspace.Provider
		}
		if provider == "claude" {
			if isMultiSessionAgent(a) {
				if multiSessionSharesWorkDir(cityPath, cityName, target, a, cfg.Rigs) {
					return false
				}
				continue
			}
			if workdirutil.ResolveWorkDirPath(cityPath, cityName, a.QualifiedName(), a, cfg.Rigs) == target {
				count++
			}
		}
	}
	return count <= 1
}

func multiSessionSharesWorkDir(cityPath, cityName, target string, a config.Agent, rigs []config.Rig) bool {
	if !isMultiSessionAgent(a) {
		return false
	}

	maxSess := a.EffectiveMaxActiveSessions()
	isUnlimited := maxSess == nil || *maxSess < 0
	if !isUnlimited {
		for slot := 1; slot <= *maxSess; slot++ {
			if workdirutil.ResolveWorkDirPath(cityPath, cityName, poolQualifiedNameForSlot(a, slot), a, rigs) == target {
				return true
			}
		}
		return false
	}

	for _, qualifiedName := range []string{
		poolQualifiedNameForSlot(a, 1),
		poolQualifiedNameForSlot(a, 2),
	} {
		if workdirutil.ResolveWorkDirPath(cityPath, cityName, qualifiedName, a, rigs) == target {
			return true
		}
	}
	return false
}

func poolQualifiedNameForSlot(a config.Agent, slot int) string {
	name := poolInstanceNameForAPI(a.Name, slot, a)
	return a.QualifiedInstanceName(name)
}

// isMultiSessionAgent reports whether the agent can have more than one
// concurrent session. This is the replacement for the removed IsPool() method.
func isMultiSessionAgent(a config.Agent) bool {
	maxSess := a.EffectiveMaxActiveSessions()
	return maxSess == nil || *maxSess != 1
}

func poolInstanceNameForAPI(base string, slot int, a config.Agent) string {
	maxSess := a.EffectiveMaxActiveSessions()
	isMultiInstance := maxSess != nil && (*maxSess > 1 || *maxSess < 0)
	if !isMultiInstance {
		return base
	}
	if slot >= 1 && slot <= len(a.NamepoolNames) {
		return a.NamepoolNames[slot-1]
	}
	return fmt.Sprintf("%s-%d", base, slot)
}
