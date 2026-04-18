package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sessionlog"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

const lookPathCacheTTL = 30 * time.Second

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

func (s *Server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	bp := parseBlockingParams(r)
	if bp.isBlocking() {
		waitForChange(r.Context(), s.state.EventProvider(), bp)
	}

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	sessTmpl := cfg.Workspace.SessionTemplate
	wantPeek := r.URL.Query().Get("peek") == "true"

	// Query filters.
	qPool := r.URL.Query().Get("pool")
	qRig := r.URL.Query().Get("rig")
	qRunning := r.URL.Query().Get("running")
	index := s.latestIndex()
	cacheKey := ""
	if !wantPeek {
		cacheKey = responseCacheKey("agents", r)
		if body, ok := s.cachedResponse(cacheKey, index); ok {
			writeCachedJSON(w, r, index, body)
			return
		}
	}

	var agents []agentResponse
	for _, a := range cfg.Agents {
		expanded := expandAgent(a, cityName, sessTmpl, sp)
		for _, ea := range expanded {
			// Apply filters.
			if qRig != "" && ea.rig != qRig {
				continue
			}
			if qPool != "" && ea.pool != qPool {
				continue
			}

			sessionName := agentSessionName(cityName, ea.qualifiedName, sessTmpl)
			running := sp.IsRunning(sessionName)

			if qRunning == "true" && !running {
				continue
			}
			if qRunning == "false" && running {
				continue
			}

			// Merge config + runtime suspended state.
			suspended := ea.suspended
			if v, err := sp.GetMeta(sessionName, "suspended"); err == nil && v == "true" {
				suspended = true
			}

			// Resolve provider and display name.
			provider, displayName := resolveProviderInfo(ea.provider, cfg)

			// Determine availability by checking if provider binary is in PATH.
			available := true
			var unavailableReason string
			if suspended {
				available = false
				unavailableReason = "agent is suspended"
			} else if provider != "" {
				if !s.cachedLookPath(providerPathCheck(provider, cfg)) {
					available = false
					unavailableReason = "provider '" + provider + "' not found in PATH"
				}
			}

			resp := agentResponse{
				Name:              ea.qualifiedName,
				Description:       ea.description,
				Running:           running,
				Suspended:         suspended,
				Rig:               ea.rig,
				Pool:              ea.pool,
				Provider:          provider,
				DisplayName:       displayName,
				Available:         available,
				UnavailableReason: unavailableReason,
			}

			var lastActivity *time.Time
			sessionID := ""
			if running {
				si := &sessionInfo{Name: sessionName}
				if t, err := sp.GetLastActivity(sessionName); err == nil && !t.IsZero() {
					si.LastActivity = &t
					lastActivity = &t
				}
				si.Attached = sp.IsAttached(sessionName)
				resp.Session = si
				if id, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
					sessionID = strings.TrimSpace(id)
				}
			}

			// Find active bead by querying bead stores.
			resp.ActiveBead = s.findActiveBeadForAssignees(ea.rig, sessionID, sessionName, ea.qualifiedName)

			// Compute state enum.
			quarantined := s.state.IsQuarantined(sessionName)
			resp.State = computeAgentState(suspended, quarantined, running, resp.ActiveBead, lastActivity)

			// Peek preview (opt-in).
			if wantPeek && running {
				if output, err := sp.Peek(sessionName, 5); err == nil {
					resp.LastOutput = output
				}
			}

			// Model + context usage (best-effort, Claude only).
			// Skip when session file attribution is ambiguous (pools,
			// multiple Claude agents in same rig).
			if running && provider == "claude" && canAttributeSession(a, ea.qualifiedName, cfg, s.state.CityPath()) {
				s.enrichSessionMeta(&resp, a, ea.qualifiedName, cfg)
			}

			agents = append(agents, resp)
		}
	}

	if agents == nil {
		agents = []agentResponse{}
	}
	resp := listResponse{Items: agents, Total: len(agents)}
	if cacheKey == "" {
		writeListJSON(w, index, agents, len(agents))
		return
	}
	body, err := s.storeResponse(cacheKey, index, resp)
	if err != nil {
		writeListJSON(w, index, agents, len(agents))
		return
	}
	writeCachedJSON(w, r, index, body)
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid", "agent name required")
		return
	}

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()

	// Try exact agent match first, then check for sub-resource suffix.
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		// Not found as exact agent — check for sub-resource suffixes.
		// Order matters: check longer suffixes first so /output doesn't
		// partially match /output/stream.
		if after, found := strings.CutSuffix(name, "/output/stream"); found {
			s.handleAgentOutputStream(w, r, after)
			return
		}
		if after, found := strings.CutSuffix(name, "/output"); found {
			s.handleAgentOutput(w, r, after)
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not found")
		return
	}

	sessionName := agentSessionName(cityName, name, cfg.Workspace.SessionTemplate)
	running := sp.IsRunning(sessionName)

	// Merge config + runtime suspended state.
	suspended := agentCfg.Suspended
	if v, err := sp.GetMeta(sessionName, "suspended"); err == nil && v == "true" {
		suspended = true
	}

	// Resolve provider and display name.
	provider, displayName := resolveProviderInfo(agentCfg.Provider, cfg)

	// Determine availability.
	available := true
	var unavailableReason string
	if suspended {
		available = false
		unavailableReason = "agent is suspended"
	} else if provider != "" {
		if !s.cachedLookPath(providerPathCheck(provider, cfg)) {
			available = false
			unavailableReason = "provider '" + provider + "' not found in PATH"
		}
	}

	resp := agentResponse{
		Name:              name,
		Description:       agentCfg.Description,
		Running:           running,
		Suspended:         suspended,
		Rig:               agentCfg.Dir,
		Provider:          provider,
		DisplayName:       displayName,
		Available:         available,
		UnavailableReason: unavailableReason,
	}
	if isMultiSessionAgent(agentCfg) {
		resp.Pool = agentCfg.QualifiedName()
	}

	var lastActivity *time.Time
	sessionID := ""
	if running {
		si := &sessionInfo{Name: sessionName}
		if t, err := sp.GetLastActivity(sessionName); err == nil && !t.IsZero() {
			si.LastActivity = &t
			lastActivity = &t
		}
		si.Attached = sp.IsAttached(sessionName)
		resp.Session = si
		if id, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
			sessionID = strings.TrimSpace(id)
		}
	}

	// Find active bead by querying bead stores.
	resp.ActiveBead = s.findActiveBeadForAssignees(agentCfg.Dir, sessionID, sessionName, name)

	// Compute state enum.
	quarantined := s.state.IsQuarantined(sessionName)
	resp.State = computeAgentState(suspended, quarantined, running, resp.ActiveBead, lastActivity)

	// Model + context usage (best-effort, Claude only).
	if running && provider == "claude" && canAttributeSession(agentCfg, name, cfg, s.state.CityPath()) {
		s.enrichSessionMeta(&resp, agentCfg, name, cfg)
	}

	writeIndexJSON(w, s.latestIndex(), resp)
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	// Parse action suffix before validating agent name.
	// Only config-level mutations (suspend/resume) are supported.
	// Runtime operations (kill, drain, nudge, restart) moved to session APIs.
	var action string
	if after, found := strings.CutSuffix(name, "/suspend"); found {
		name = after
		action = "suspend"
	} else if after, found := strings.CutSuffix(name, "/resume"); found {
		name = after
		action = "resume"
	} else {
		writeError(w, http.StatusNotFound, "not_found", "unknown agent action; runtime operations moved to /v0/session/{id}/*")
		return
	}

	// Validate agent exists in config.
	cfg := s.state.Config()
	if _, ok := findAgent(cfg, name); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not found")
		return
	}

	var err error
	switch action {
	case "suspend":
		err = sm.SuspendAgent(name)
	case "resume":
		err = sm.ResumeAgent(name)
	}

	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	if !a.SupportsInstanceExpansion() {
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
	if a.HasUnlimitedSessionCapacity() && sp != nil {
		return discoverUnlimitedPool(a, poolName, cityName, sessTmpl, sp)
	}

	// Bounded: static enumeration.
	poolMax := 1
	if maxSess := a.EffectiveMaxActiveSessions(); maxSess != nil && *maxSess > 1 {
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

// findAgent looks up an agent by qualified name in the config.
// For multi-session agents, it matches instance names.
func findAgent(cfg *config.City, name string) (config.Agent, bool) {
	dir, baseName := config.ParseQualifiedName(name)
	for _, a := range cfg.Agents {
		if config.AgentMatchesIdentity(&a, name) {
			return a, true
		}
		// Check multi-session instance members.
		if a.SupportsInstanceExpansion() && a.Dir == dir {
			if a.HasUnlimitedSessionCapacity() {
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
			maxSess := a.EffectiveMaxActiveSessions()
			poolMax := 1
			if maxSess != nil && *maxSess > 1 {
				poolMax = *maxSess
			}
			for i := 1; i <= poolMax; i++ {
				memberName := poolInstanceNameForAPI(a.Name, i, a)
				if memberName == baseName {
					return a, true
				}
			}
		}
	}
	return config.Agent{}, false
}

func (s *Server) findActiveBeadForAssignees(rig string, assignees ...string) string {
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
			matches, err := stores[rn].List(beads.ListQuery{
				Assignee: assignee,
				Status:   "in_progress",
				Limit:    1,
				Sort:     beads.SortCreatedDesc,
			})
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
func providerPathCheck(providerName string, cfg *config.City) string {
	if spec, ok := cfg.Providers[providerName]; ok {
		if spec.PathCheck != "" {
			return spec.PathCheck
		}
		return spec.Command
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
func resolveProviderInfo(agentProvider string, cfg *config.City) (provider, displayName string) {
	provider = agentProvider
	if provider == "" {
		provider = cfg.Workspace.Provider
	}
	if provider == "" {
		return "", ""
	}

	// Check city-level provider overrides first.
	if spec, ok := cfg.Providers[provider]; ok && spec.DisplayName != "" {
		return provider, spec.DisplayName
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
func (s *Server) enrichSessionMeta(resp *agentResponse, agentCfg config.Agent, qualifiedName string, cfg *config.City) {
	workDir := workdirutil.ResolveWorkDirPath(
		s.state.CityPath(),
		workdirutil.CityName(s.state.CityPath(), cfg),
		qualifiedName,
		agentCfg,
		cfg.Rigs,
	)
	if workDir == "" {
		return
	}
	// Resolve to absolute path for correct slug generation.
	if abs, err := filepath.Abs(workDir); err == nil {
		workDir = abs
	}
	searchPaths := s.sessionLogSearchPaths
	if searchPaths == nil {
		searchPaths = sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths)
	}
	provider := strings.TrimSpace(agentCfg.Provider)
	if provider == "" && cfg != nil {
		provider = strings.TrimSpace(cfg.Workspace.Provider)
	}
	sessionFile := sessionlog.FindSessionFileForProvider(searchPaths, provider, workDir)
	if sessionFile == "" {
		return
	}
	meta, err := sessionlog.ExtractTailMetaFromSearchPaths(searchPaths, sessionFile)
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

	if !a.HasUnlimitedSessionCapacity() {
		maxSess := a.EffectiveMaxActiveSessions()
		if maxSess == nil {
			return false
		}
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
	return a.SupportsInstanceExpansion()
}

func poolInstanceNameForAPI(base string, slot int, a config.Agent) string {
	if !a.SupportsInstanceExpansion() {
		return base
	}
	if slot >= 1 && slot <= len(a.NamepoolNames) {
		return a.NamepoolNames[slot-1]
	}
	return fmt.Sprintf("%s-%d", base, slot)
}
