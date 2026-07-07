package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	eventsexec "github.com/gastownhall/gascity/internal/events/exec"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	mailexec "github.com/gastownhall/gascity/internal/mail/exec"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/usage"
)

type sessionProviderContext struct {
	providerName    string
	cfg             *config.City
	sc              config.SessionConfig
	cityName        string
	cityPath        string
	agents          []config.Agent
	sessionTemplate string
}

func loadSessionProviderContext() sessionProviderContext {
	ctx := sessionProviderContext{
		providerName: os.Getenv("GC_SESSION"),
	}
	if cp, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cp, io.Discard); err == nil {
			return sessionProviderContextForCity(cfg, cp, ctx.providerName)
		}
	}
	return ctx
}

func sessionProviderContextForCity(cfg *config.City, cityPath, providerOverride string) sessionProviderContext {
	ctx := sessionProviderContext{
		providerName: providerOverride,
		cfg:          cfg,
		cityPath:     cityPath,
	}
	if cfg == nil {
		return ctx
	}
	ctx.sc = cfg.Session
	ctx.cityName = loadedCityName(cfg, cityPath)
	ctx.agents = cfg.Agents
	ctx.sessionTemplate = cfg.Workspace.SessionTemplate
	if ctx.providerName == "" {
		ctx.providerName = cfg.Session.Provider
	}
	return ctx
}

var (
	openSessionProviderStore   = openCityStoreAt
	buildSessionProviderByName = newSessionProviderForCityByName
)

// tmuxConfigFromSession converts a config.SessionConfig into a
// sessiontmux.Config with resolved durations and defaults. If the
// config has no explicit socket name, cityName is used. cityPath, when set,
// supplies the runtime root for per-session start-crash diagnostics.
func tmuxConfigFromSession(sc config.SessionConfig, cityName, cityPath string) sessiontmux.Config {
	socketName := sc.Socket
	if socketName == "" {
		socketName = cityName
	}
	var runtimeDir string
	if cityPath != "" {
		runtimeDir = citylayout.RuntimePath(cityPath)
	}
	return sessiontmux.Config{
		SetupTimeout:       sc.SetupTimeoutDuration(),
		NudgeReadyTimeout:  sc.NudgeReadyTimeoutDuration(),
		NudgeRetryInterval: sc.NudgeRetryIntervalDuration(),
		NudgeLockTimeout:   sc.NudgeLockTimeoutDuration(),
		DebounceMs:         sc.DebounceMsOrDefault(),
		DisplayMs:          sc.DisplayMsOrDefault(),
		SocketName:         socketName,
		RuntimeDir:         runtimeDir,
	}
}

func providerStateDir(providerName, cityPath string) string {
	if cityPath == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(cityPath)))
	return filepath.Join(supervisor.RuntimeDir(), providerName, hex.EncodeToString(sum[:4]))
}

// newSessionProviderForCityByName resolves a selection name through the
// city's runtime registry: the builtins plus any pack-declared runtimes
// from cfg (RUNTIME-SEL-011). cfg may be nil (no city context), in which
// case only builtin names resolve. A pack runtime colliding with a builtin
// name surfaces here as a construction error.
// See cmd/gc/runtime_registry.go for the builtin registrations and
// internal/runtime/REQUIREMENTS.md for the selection contract:
// cityName auto-defaults the tmux socket when none is configured,
// cityPath isolates socket-based providers per city, and errors return
// instead of os.Exit, making this safe for the hot-reload path.
//
//   - "fake" → in-memory fake (all ops succeed)
//   - "fail" → broken fake (all ops return errors)
//   - "subprocess" → headless child processes
//   - "acp" → ACP (Agent Client Protocol) JSON-RPC over stdio
//   - "exec:<script>" → user-supplied script (absolute path or PATH lookup)
//   - "k8s" → native Kubernetes provider (client-go)
//   - "<pack runtime>" → exec proxy bound to the pack's declared command
//   - default → real tmux provider
func newSessionProviderForCityByName(cfg *config.City, name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
	// Selection flows through the de-conflated WorkerSpec atom and the Resolver.
	// The Runtime axis is the selection name; Transport is populated from it
	// (transport is bundled with the runtime today — "acp" is a whole provider,
	// not a transport over a box — so the name determines it). Per-session
	// Transport composition (tmux↔acp routing) lives in resolveSessionTransportProvider.
	// Model/Upstream/Harness are carried by the session config, not this seam.
	return resolveWorkerSpec(cfg, runtime.WorkerSpec{Runtime: name, Transport: transportForRuntimeName(name)}, sc, cityName, cityPath)
}

// transportForRuntimeName reports the Transport axis value bundled with a Runtime
// selection name. Transport (HOW gc drives the agent: tmux vs acp) is coupled to
// the runtime today — acp is its own provider — so the name fixes it: "acp" → acp,
// "t3bridge" → its bespoke turn transport, everything else (tmux/exec/ssh/k8s/…)
// → the tmux carrier. This makes WorkerSpec.Transport explicit at the Resolver
// seam (where per-spec Transport honoring would land when runtime↔transport are
// genuinely decoupled).
func transportForRuntimeName(name string) string {
	switch {
	case name == "acp":
		return config.SessionTransportACP
	case name == "t3bridge" || (strings.HasPrefix(name, "exec:") && isLegacyT3BridgeExecScript(strings.TrimPrefix(name, "exec:"))):
		return "t3"
	default:
		return config.SessionTransportTmux
	}
}

// resolveWorkerSpec resolves a [runtime.WorkerSpec] to a session provider. It is
// the seam where axis-based selection lives: today only the Runtime axis drives
// selection (mapping to the registry's seam-backed construction), but it is the
// single place the Transport/Upstream axes will be honored as the de-conflation
// completes.
func resolveWorkerSpec(cfg *config.City, spec runtime.WorkerSpec, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
	reg, err := runtimeRegistryForCity(cfg)
	if err != nil {
		return nil, err
	}
	return reg.New(spec.Runtime, sc, cityName, cityPath)
}

func isLegacyT3BridgeExecScript(script string) bool {
	return filepath.Base(strings.TrimSpace(script)) == "gc-session-t3"
}

// newSessionProvider returns a runtime.Provider based on the session provider
// name (env var → city.toml → default). When the city-level provider is not
// "acp" but some agents have session = "acp", returns an auto.Provider that
// routes per-session. Startup path — exits on error.
func newSessionProvider() runtime.Provider {
	ctx := loadSessionProviderContext()
	sessionBeads := loadProviderSessionSnapshot(ctx)
	return newSessionProviderFromContext(ctx, sessionBeads)
}

func newSessionProviderForCity(cfg *config.City, cityPath string) runtime.Provider {
	ctx := sessionProviderContextForCity(cfg, cityPath, os.Getenv("GC_SESSION"))
	sessionBeads := loadProviderSessionSnapshot(ctx)
	return newSessionProviderFromContext(ctx, sessionBeads)
}

func newStatusSessionProviderForCity(cfg *config.City, cityPath string) runtime.Provider {
	ctx := sessionProviderContextForCity(cfg, cityPath, os.Getenv("GC_SESSION"))
	return newBoundedStatusProvider(newSessionProviderFromContext(ctx, nil))
}

func newStatusSessionProviderForCityWithSnapshot(cfg *config.City, cityPath string, sessionBeads *sessionBeadSnapshot) runtime.Provider {
	ctx := sessionProviderContextForCity(cfg, cityPath, os.Getenv("GC_SESSION"))
	return newBoundedStatusProvider(newSessionProviderFromContext(ctx, sessionBeads))
}

func registerStatusProviderACPRoutes(sp runtime.Provider, snapshot *sessionBeadSnapshot, cityName string, cfg *config.City) {
	router, ok := sp.(interface{ RouteACP(string) })
	if !ok {
		return
	}
	for _, sessName := range configuredACPRouteNames(snapshot, cityName, cfg) {
		router.RouteACP(sessName)
	}
}

func loadProviderSessionSnapshot(ctx sessionProviderContext) *sessionBeadSnapshot {
	if ctx.cityPath == "" || ctx.providerName == "acp" {
		return nil
	}
	store, err := openSessionProviderStore(ctx.cityPath)
	if err != nil {
		return nil
	}
	// This snapshot reads only session-class beads (the gc:session label) to
	// drive transport/ACP routing decisions, so route through the session
	// coordination-class store for relocation-safety. openSessionProviderStore
	// opens its own generic store (independent of the caller), so routing here
	// closes the gap on both the CLI and controller provider-construction paths.
	// Identity to the opened store today (resolveClassStore is pure identity).
	sessStore := cliSessionStore(store, ctx.cfg, ctx.cityPath)
	all, err := sessStore.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		return nil
	}
	return newSessionBeadSnapshot(all)
}

func newSessionProviderFromContext(ctx sessionProviderContext, sessionBeads *sessionBeadSnapshot) runtime.Provider {
	sp, err := newSessionProviderFromContextWithError(ctx, sessionBeads)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err) //nolint:errcheck // best-effort stderr
		os.Exit(1)
	}
	return sp
}

func newSessionProviderFromContextWithError(ctx sessionProviderContext, sessionBeads *sessionBeadSnapshot) (runtime.Provider, error) {
	return resolveSessionTransportProvider(ctx, sessionBeads)
}

// resolveSessionTransportProvider is the single Resolver seam that composes the
// session Transport axis (tmux vs acp). It builds the base provider for the
// city's session-provider name (the Runtime axis, via buildSessionProviderByName
// → resolveWorkerSpec) and — when the base is not acp but some agents select the
// acp transport — composes an auto.Provider that routes those sessions to an acp
// backend. Per-session transport is the auto router's job; this is where the
// composition is owned (construction time). Dynamically-created sessions are
// routed at start via the same auto.Provider (build_desired_state RouteACP).
// Behavior is identical to the prior inline composition.
func resolveSessionTransportProvider(ctx sessionProviderContext, sessionBeads *sessionBeadSnapshot) (runtime.Provider, error) {
	base, err := buildSessionProviderByName(ctx.cfg, ctx.providerName, ctx.sc, ctx.cityName, ctx.cityPath)
	if err != nil {
		return nil, err
	}
	// If the city-level provider is not ACP but some agents need ACP, wrap in an
	// auto provider that routes per-session.
	// NOTE: agents comes from loadCityConfig which applies pack overrides, so the
	// Session field from overrides is already resolved here.
	// acpRouteNames is computed once and reused for both the requires/needs
	// checks and the route registration below, instead of recomputing the
	// (agent + named-session) x provider-resolution walk up to 3x per call.
	acpRouteNames := configuredACPRouteNames(sessionBeads, ctx.cityName, ctx.cfg)
	requireACPWrapper := len(acpRouteNames) > 0
	needsACPWrapper := requireACPWrapper || (ctx.cfg != nil && hasACPProviderTargets(ctx.cfg))
	if ctx.providerName != "acp" && needsACPWrapper {
		acpSP, acpErr := buildSessionProviderByName(ctx.cfg, "acp", ctx.sc, ctx.cityName, ctx.cityPath)
		if acpErr != nil {
			if requireACPWrapper {
				return nil, fmt.Errorf("acp provider: %w", acpErr)
			}
			return base, nil
		}
		autoSP := sessionauto.New(base, acpSP)
		for _, sessName := range acpRouteNames {
			autoSP.RouteACP(sessName)
		}
		return autoSP, nil
	}
	return base, nil
}

func agentSessionCreateTransport(cfg *config.City, agentCfg config.Agent) string {
	if cfg == nil {
		return strings.TrimSpace(agentCfg.Session)
	}
	// StartCommand is ResolveProvider's escape hatch (step 1): it bypasses
	// provider-catalog resolution entirely, so a cache entry for
	// agentCfg.Provider would not describe this agent's actual resolution.
	if agentCfg.StartCommand == "" {
		name := agentCfg.Provider
		if name == "" {
			name = cfg.Workspace.Provider
		}
		if name != "" {
			if resolved, ok := config.ResolvedProviderCached(cfg, name); ok {
				return config.ResolveSessionCreateTransport(agentCfg.Session, &resolved)
			}
		}
	}
	resolved, err := config.ResolveProvider(
		&agentCfg,
		&cfg.Workspace,
		cfg.Providers,
		func(name string) (string, error) { return name, nil },
	)
	if err != nil {
		return strings.TrimSpace(agentCfg.Session)
	}
	return config.ResolveSessionCreateTransport(agentCfg.Session, resolved)
}

// configuredACPSessionNames resolves the runtime session names for ACP-backed
// agents using a single session-bead snapshot. When the snapshot is unavailable
// or bead lookup fails, it falls back to the legacy deterministic name.
func configuredACPSessionNames(snapshot *sessionBeadSnapshot, cityName, sessionTemplate string, cfg *config.City, agents []config.Agent) []string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		if agentSessionCreateTransport(cfg, a) != "acp" {
			continue
		}
		sessName := agent.SessionNameFor(cityName, a.QualifiedName(), sessionTemplate)
		if snapshot != nil {
			if beadName := snapshot.FindSessionNameByTemplate(a.QualifiedName()); beadName != "" {
				sessName = beadName
			}
		}
		names = append(names, sessName)
	}
	return names
}

func hasACPProviderTargets(cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	candidates := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" {
			candidates[name] = true
		}
	}
	add(cfg.Workspace.Provider)
	for name := range cfg.Providers {
		add(name)
	}
	for _, agentCfg := range cfg.Agents {
		add(agentCfg.Provider)
	}
	for name := range candidates {
		if providerSessionCreateUsesACP(cfg, name) {
			return true
		}
	}
	return false
}

func resolveProviderForACPTransport(cfg *config.City, providerName string) *config.ResolvedProvider {
	if cfg == nil || strings.TrimSpace(providerName) == "" {
		return nil
	}
	if resolved, ok := config.ResolvedProviderCached(cfg, providerName); ok {
		return &resolved
	}
	resolved, err := config.ResolveProvider(
		&config.Agent{Provider: providerName},
		&cfg.Workspace,
		cfg.Providers,
		func(name string) (string, error) { return name, nil },
	)
	if err != nil {
		return nil
	}
	return resolved
}

func providerSessionCreateUsesACP(cfg *config.City, providerName string) bool {
	resolved := resolveProviderForACPTransport(cfg, providerName)
	return resolved != nil && resolved.ProviderSessionCreateTransport() == "acp"
}

func providerLegacyDefaultsToACP(cfg *config.City, providerName string) bool {
	resolved := resolveProviderForACPTransport(cfg, providerName)
	return resolved != nil && resolved.ProviderSessionCreateTransport() == "acp"
}

func observedACPSessionNames(snapshot *sessionBeadSnapshot, cfg *config.City) []string {
	if snapshot == nil {
		return nil
	}
	open := snapshot.OpenInfos()
	names := make([]string, 0, len(open))
	seen := make(map[string]bool, len(open))
	for _, info := range open {
		if !infoUsesACPTransport(info, cfg) {
			continue
		}
		sessionName := strings.TrimSpace(info.SessionNameMetadata)
		if sessionName == "" || seen[sessionName] {
			continue
		}
		seen[sessionName] = true
		names = append(names, sessionName)
	}
	return names
}

// beadUsesACPTransport is the raw-bead form retained as the byte-identical
// oracle for infoUsesACPTransport (TestSessionClassifierInfoEquivalence). No
// production caller reads it — observedACPSessionNames consumes the Info form.
func beadUsesACPTransport(bead beads.Bead, cfg *config.City) bool {
	transport := strings.TrimSpace(bead.Metadata["transport"])
	if transport != "" {
		return transport == "acp"
	}
	providerName := strings.TrimSpace(bead.Metadata["provider"])
	if providerName == "acp" {
		return true
	}
	if strings.TrimSpace(bead.Metadata[session.MCPIdentityMetadataKey]) != "" ||
		strings.TrimSpace(bead.Metadata[session.MCPServersSnapshotMetadataKey]) != "" {
		return true
	}
	templateName := strings.TrimSpace(bead.Metadata["template"])
	if cfg != nil {
		if agentCfg, ok := resolveAgentIdentity(cfg, templateName, currentRigContext(cfg)); ok {
			if strings.TrimSpace(agentCfg.Session) != "" && agentSessionCreateTransport(cfg, agentCfg) == "acp" {
				return true
			}
			if strings.TrimSpace(bead.Metadata["command"]) == "" &&
				strings.TrimSpace(bead.Metadata["pending_create_claim"]) == "true" &&
				agentSessionCreateTransport(cfg, agentCfg) == "acp" {
				return true
			}
			if providerName == "" {
				providerName = strings.TrimSpace(agentCfg.Provider)
			}
		}
		if providerName == "" {
			providerName = templateName
		}
		resolved := resolveProviderForACPTransport(cfg, providerName)
		if resolved != nil {
			acpCommand := strings.TrimSpace(resolved.ACPCommandString())
			defaultCommand := strings.TrimSpace(resolved.CommandString())
			storedCommand := strings.TrimSpace(bead.Metadata["command"])
			if acpCommand != "" && acpCommand != defaultCommand &&
				(storedCommand == acpCommand || strings.HasPrefix(storedCommand, acpCommand+" ")) {
				return true
			}
		}
		if strings.TrimSpace(bead.Metadata["command"]) == "" &&
			strings.TrimSpace(bead.Metadata["pending_create_claim"]) == "true" {
			return providerLegacyDefaultsToACP(cfg, providerName)
		}
	}
	return false
}

func infoUsesACPTransport(info session.Info, cfg *config.City) bool {
	transport := strings.TrimSpace(info.Transport)
	if transport != "" {
		return transport == "acp"
	}
	providerName := strings.TrimSpace(info.Provider)
	if providerName == "acp" {
		return true
	}
	if strings.TrimSpace(info.MCPIdentity) != "" ||
		strings.TrimSpace(info.MCPServersSnapshot) != "" {
		return true
	}
	templateName := strings.TrimSpace(info.Template)
	if cfg != nil {
		if agentCfg, ok := resolveAgentIdentity(cfg, templateName, currentRigContext(cfg)); ok {
			if strings.TrimSpace(agentCfg.Session) != "" && agentSessionCreateTransport(cfg, agentCfg) == "acp" {
				return true
			}
			if strings.TrimSpace(info.Command) == "" &&
				info.PendingCreateClaim &&
				agentSessionCreateTransport(cfg, agentCfg) == "acp" {
				return true
			}
			if providerName == "" {
				providerName = strings.TrimSpace(agentCfg.Provider)
			}
		}
		if providerName == "" {
			providerName = templateName
		}
		resolved := resolveProviderForACPTransport(cfg, providerName)
		if resolved != nil {
			acpCommand := strings.TrimSpace(resolved.ACPCommandString())
			defaultCommand := strings.TrimSpace(resolved.CommandString())
			storedCommand := strings.TrimSpace(info.Command)
			if acpCommand != "" && acpCommand != defaultCommand &&
				(storedCommand == acpCommand || strings.HasPrefix(storedCommand, acpCommand+" ")) {
				return true
			}
		}
		if strings.TrimSpace(info.Command) == "" &&
			info.PendingCreateClaim {
			return providerLegacyDefaultsToACP(cfg, providerName)
		}
	}
	return false
}

func configuredACPRouteNames(snapshot *sessionBeadSnapshot, cityName string, cfg *config.City) []string {
	names := observedACPSessionNames(snapshot, cfg)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	if cfg == nil {
		return names
	}
	for _, name := range configuredACPSessionNames(snapshot, cityName, cfg.Workspace.SessionTemplate, cfg, cfg.Agents) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, named := range cfg.NamedSessions {
		agentCfg := config.FindAgent(cfg, named.TemplateQualifiedName())
		if agentCfg == nil || agentSessionCreateTransport(cfg, *agentCfg) != "acp" {
			continue
		}
		sessionName := config.NamedSessionRuntimeName(cityName, cfg.Workspace, named.QualifiedName())
		if snapshot != nil {
			if snapName := snapshot.FindSessionNameByNamedIdentity(named.QualifiedName()); snapName != "" {
				sessionName = snapName
			}
		}
		if sessionName == "" || seen[sessionName] {
			continue
		}
		seen[sessionName] = true
		names = append(names, sessionName)
	}
	return names
}

// displayProviderName returns a human-readable provider name for logging.
func displayProviderName(name string) string {
	if name == "" {
		return "tmux (default)"
	}
	return name
}

func configuredBeadsProviderValue(cityPath string) string {
	if v := strings.TrimSpace(os.Getenv("GC_BEADS")); v != "" {
		if scopedRoot := strings.TrimSpace(os.Getenv("GC_BEADS_SCOPE_ROOT")); scopedRoot != "" && cityPath != "" && !samePath(resolveStoreScopeRoot(cityPath, scopedRoot), cityPath) {
			return strings.TrimSpace(peekBeadsProvider(filepath.Join(cityPath, "city.toml")))
		}
		return v
	}
	return strings.TrimSpace(peekBeadsProvider(filepath.Join(cityPath, "city.toml")))
}

func scopedBeadsProviderOverride(cityPath, scopeRoot string) (string, bool) {
	provider := strings.TrimSpace(os.Getenv("GC_BEADS"))
	if provider == "" {
		return "", false
	}
	scopedRoot := strings.TrimSpace(os.Getenv("GC_BEADS_SCOPE_ROOT"))
	if scopedRoot == "" {
		return provider, true
	}
	if samePath(resolveStoreScopeRoot(cityPath, scopedRoot), scopeRoot) {
		return provider, true
	}
	return "", false
}

// normalizeRawBeadsProvider maps the city-managed gc-beads-bd wrapper back to
// the logical "bd" provider for command-time store selection. Managed sessions
// set GC_BEADS=exec:<cityPath>/.gc/scripts/gc-beads-bd.sh (the stable shim)
// so lifecycle operations stay pinned to the city's Dolt server, but general
// gc commands still need a CRUD-capable store.
func normalizeRawBeadsProvider(cityPath, provider string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" || !strings.HasPrefix(provider, "exec:") || execProviderBase(provider) != "gc-beads-bd" || cityPath == "" {
		return provider
	}
	script := strings.TrimSpace(strings.TrimPrefix(provider, "exec:"))
	if samePath(script, gcBeadsBdScriptPath(cityPath)) || samePath(script, legacySystemPacksGcBeadsBdScriptPath(cityPath)) {
		return "bd"
	}
	return provider
}

// rawBeadsProvider returns the raw bead store provider name from config.
// Priority: GC_BEADS env var → city.toml [beads].provider → "bd" default.
// The city-managed lifecycle wrapper normalizes back to "bd" so nested agent
// sessions do not re-inherit exec:gc-beads-bd for raw data operations.
func rawBeadsProvider(cityPath string) string {
	if provider := configuredBeadsProviderValue(cityPath); provider != "" {
		return normalizeRawBeadsProvider(cityPath, provider)
	}
	return "bd"
}

func rawBeadsProviderFromConfig(cityPath string) string {
	if provider := strings.TrimSpace(peekBeadsProvider(filepath.Join(cityPath, "city.toml"))); provider != "" {
		return normalizeRawBeadsProvider(cityPath, provider)
	}
	return "bd"
}

func configuredBeadsBackendValue(cityPath string) string {
	if v := strings.TrimSpace(os.Getenv("GC_BEADS_BACKEND")); v != "" {
		return v
	}
	return strings.TrimSpace(peekBeadsBackend(filepath.Join(cityPath, "city.toml")))
}

func beadsBackend(cityPath string) string {
	backend := strings.ToLower(configuredBeadsBackendValue(cityPath))
	if backend == "" {
		return "dolt"
	}
	return backend
}

func cityUsesDoltliteBeadsBackend(cityPath string) bool {
	return beadsBackend(cityPath) == "doltlite"
}

func providerUsesBdStoreContract(provider string) bool {
	return contract.ProviderUsesBDContract(provider)
}

func cityUsesBdStoreContract(cityPath string) bool {
	return providerUsesBdStoreContract(rawBeadsProvider(cityPath))
}

func cityUsesManagedDoltBeadsLifecycle(cityPath string) bool {
	return cityUsesBdStoreContract(cityPath) && !cityUsesDoltliteBeadsBackend(cityPath)
}

func rawBeadsProviderForScope(scopeRoot, cityPath string) string {
	runtimeCityPath := cityPath
	if runtimeCityPath == "" {
		runtimeCityPath = cityForStoreDir(scopeRoot)
	}
	resolvedScopeRoot := resolveStoreScopeRoot(runtimeCityPath, scopeRoot)
	if explicit, ok := scopedBeadsProviderOverride(runtimeCityPath, resolvedScopeRoot); ok {
		return normalizeRawBeadsProvider(runtimeCityPath, explicit)
	}
	provider := rawBeadsProvider(runtimeCityPath)
	if strings.TrimSpace(os.Getenv("GC_BEADS_SCOPE_ROOT")) != "" {
		provider = rawBeadsProviderFromConfig(runtimeCityPath)
	}
	if samePath(resolvedScopeRoot, runtimeCityPath) {
		return provider
	}
	if strings.HasPrefix(provider, "exec:") && !providerUsesBdStoreContract(provider) {
		return provider
	}
	// Mixed-provider workspaces can keep legacy bd-backed rigs under a
	// file-backed city (and vice versa). Prefer explicit scope-local store
	// markers over the city default so scoped commands keep talking to the
	// rig's actual beads backend. The bd routing identity is metadata.json;
	// config.yaml is a compatibility mirror and can survive migrations.
	if scopeUsesBdStoreContract(resolvedScopeRoot) {
		return "bd"
	}
	if scopeUsesFileStoreContract(resolvedScopeRoot) {
		return "file"
	}
	return provider
}

func scopeUsesManagedBdStoreContract(cityPath, scopeRoot string) bool {
	return providerUsesBdStoreContract(rawBeadsProviderForScope(scopeRoot, cityPath))
}

func rigUsesManagedBdStoreContract(cityPath string, rig config.Rig) bool {
	if strings.TrimSpace(rig.Path) == "" {
		return false
	}
	return scopeUsesManagedBdStoreContract(cityPath, rig.Path)
}

func workspaceUsesManagedBdStoreContract(cityPath string, rigs []config.Rig) bool {
	if scopeUsesManagedBdStoreContract(cityPath, cityPath) {
		return true
	}
	for _, rig := range rigs {
		if rigUsesManagedBdStoreContract(cityPath, rig) {
			return true
		}
	}
	return false
}

func scopeUsesBdStoreContract(scopeRoot string) bool {
	_, err := os.Stat(filepath.Join(scopeRoot, ".beads", "metadata.json"))
	return err == nil
}

func scopeUsesFileStoreContract(scopeRoot string) bool {
	if scopeUsesBdStoreContract(scopeRoot) {
		return false
	}
	_, err := os.Stat(filepath.Join(scopeRoot, ".gc", "beads.json"))
	return err == nil
}

// bdProviderMismatchHint returns an actionable diagnostic when gc bd
// rejects a scope as non-bd-backed. It names the marker that tipped
// the resolver and suggests a fix. Returns "" when the cause is not
// a local scope-marker issue (e.g., explicit city/env provider).
func bdProviderMismatchHint(scopeRoot, resolvedProvider string) string {
	if resolvedProvider == "file" && scopeUsesFileStoreContract(scopeRoot) {
		return fmt.Sprintf(
			"%s/.gc/beads.json exists, which marks this scope as file-backed. "+
				"If it is a stale artifact from a previous city or pre-migration "+
				"layout, move it aside (e.g., rename to .gc/beads.json.bak). To "+
				"positively mark this scope as bd-backed, add "+
				"%s/.beads/metadata.json (with backend=dolt and the dolt_database "+
				"name).",
			scopeRoot, scopeRoot)
	}
	if strings.TrimSpace(os.Getenv("GC_BEADS")) != "" {
		return "GC_BEADS env var overrides the provider. Unset it, or set GC_BEADS=bd for this scope."
	}
	return "check city.toml [beads].provider and any per-rig provider overrides."
}

// beadsProvider returns the bead store provider name for lifecycle operations.
// Maps "bd" → "exec:<cityPath>/.gc/scripts/gc-beads-bd.sh" (the stable shim)
// so all lifecycle operations route through the exec: protocol. Other providers
// pass through unchanged.
//
// Related env vars:
//   - GC_DOLT=skip — the gc-beads-bd script checks this and exits 2 for all
//     operations. Used by testscript and integration tests.
func beadsProvider(cityPath string) string {
	raw := rawBeadsProvider(cityPath)
	if raw == "bd" {
		return "exec:" + gcBeadsBdScriptPath(cityPath)
	}
	return raw
}

// gcBeadsBdScriptPath returns the stable per-city gc-beads-bd entrypoint:
// a generated shim under .gc/scripts that execs the bundled bd pack's
// lifecycle script in the user-global repo cache. The shim path never
// changes across binary upgrades, so session environments and provider
// pins stay valid while the cache target moves with the binary content.
func gcBeadsBdScriptPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "scripts", "gc-beads-bd.sh")
}

// legacySystemPacksGcBeadsBdScriptPath is the retired materialized-pack
// location (.gc/system/packs/bd/assets/scripts). Sessions and provider
// pins created by older binaries may still reference it; provider
// normalization keeps matching it.
func legacySystemPacksGcBeadsBdScriptPath(cityPath string) string {
	return filepath.Join(cityPath, citylayout.SystemPacksRoot, "bd", "assets", "scripts", "gc-beads-bd.sh")
}

// mailProviderName returns the mail provider name.
// Priority: GC_MAIL env var → city.toml [mail].provider → "" (default: beadmail).
func mailProviderName() string {
	if v := os.Getenv("GC_MAIL"); v != "" {
		return v
	}
	if cp, err := resolveCity(); err == nil {
		return mailProviderNameForCity(cp)
	}
	return ""
}

func mailProviderNameForCity(cityPath string) string {
	if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil && cfg.Mail.Provider != "" {
		return cfg.Mail.Provider
	}
	return ""
}

// newMailProvider returns a mail.Provider based on the mail provider name
// (env var → city.toml → default) and the given bead store (used as the
// default backend). Shared callers such as the API use the cached beadmail
// provider so repeated mail reads reuse one session-topology enumeration.
// The cache lasts for the provider lifetime; topology refresh for long-lived
// providers is handled by rebuilding the provider.
//
//   - "fake" → in-memory fake (all ops succeed)
//   - "fail" → broken fake (all ops return errors)
//   - "exec:<script>" → user-supplied script (absolute path or PATH lookup)
//   - default → beadmail (backed by beads.Store, no subprocess)
func newMailProvider(store beads.Store) mail.Provider {
	return newMailProviderNamed(mailProviderName(), store, true)
}

// newMailProviderWithSessionStore builds the configured mail provider with
// distinct message-persistence (messaging class) and session-addressing (session
// class) stores. Byte-identical to newMailProvider(store) at the single-store bd
// backend where msgStore == sessStore; once a class relocates, mail's message
// beads and its session reads follow their respective backends instead of
// splitting off one generic store.
func newMailProviderWithSessionStore(msgStore, sessStore beads.Store) mail.Provider {
	return newMailProviderNamedWithSessionStore(mailProviderName(), msgStore, sessStore, true)
}

func newCommandMailProvider(store beads.Store) mail.Provider {
	return newMailProviderNamed(mailProviderName(), store, true)
}

func newCommandMailProviderNamed(v string, store beads.Store) mail.Provider {
	return newMailProviderNamed(v, store, true)
}

func newMailProviderNamed(v string, store beads.Store, cached bool) mail.Provider {
	return newMailProviderNamedWithSessionStore(v, store, store, cached)
}

func newMailProviderNamedWithSessionStore(v string, msgStore, sessStore beads.Store, cached bool) mail.Provider {
	if strings.HasPrefix(v, "exec:") {
		return mailexec.NewProvider(strings.TrimPrefix(v, "exec:"))
	}
	switch v {
	case "fake":
		return mail.NewFake()
	case "fail":
		return mail.NewFailFake()
	default:
		if cached {
			return beadmail.NewCachedWithStores(msgStore, sessStore)
		}
		return beadmail.NewWithStores(msgStore, sessStore)
	}
}

// openCityMailProvider opens the city's bead store and wraps it in a
// mail.Provider. Returns (nil, exitCode) on failure.
//
// Relocation: the beadmail provider does messaging-class message persistence AND
// session-class reads/writes for mail addressing/identity (session.ListAllSessionBeads
// / ResolveSessionID / RepairEmptyType). Both are routed through their
// coordination-class seams — messages via resolveMailMessagesStore, session reads
// via cliSessionStore — so a [beads.classes.messaging] or [beads.classes.sessions]
// relocation reaches CLI mail the same way it reaches the running controller
// (newCityMailProvider, class_store.go). Byte-identical until a class backend is
// configured (both resolvers are identity at the single-store bd backend).
func openCityMailProvider(stderr io.Writer, cmdName string) (mail.Provider, int) {
	// For exec: and test doubles, no store needed.
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		return newCommandMailProvider(nil), 0
	}

	store, cityPath, code := openCityStoreWithPath(stderr, cmdName)
	if store == nil {
		return nil, code
	}
	// The no-refresh cfg loader matches the other hot CLI roots (cmd_prime,
	// completion): loadCityConfig's builtin-pack refresh is inappropriate here. A
	// failed load yields nil cfg, which the class resolvers treat as identity.
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	msgStore := resolveMailMessagesStore(store, cfg, cityPath, nil)
	sessStore := cliSessionStore(store, cfg, cityPath)
	return newMailProviderWithSessionStore(msgStore, sessStore), 0
}

// eventsProviderName returns the events provider name.
// Priority: GC_EVENTS env var → city.toml [events].provider → "" (default: file JSONL).
func eventsProviderName() string {
	return eventsProviderConfig().Provider
}

func eventsProviderConfig() config.EventsConfig {
	return eventsProviderConfigWithWarnings(io.Discard)
}

func eventsProviderConfigWithWarnings(w io.Writer) config.EventsConfig {
	cfg := config.EventsConfig{}
	if cp, err := resolveCity(); err == nil {
		if cityCfg, err := loadCityConfig(cp, w); err == nil {
			cfg = cityCfg.Events
		}
	}
	if v := os.Getenv("GC_EVENTS"); v != "" {
		cfg.Provider = v
	}
	return cfg
}

// fastEventsProviderName returns the events provider name for hook-driven
// event emission. It intentionally reads only top-level city.toml so bead
// hooks do not expand imports or validate remote pack caches on every write.
func fastEventsProviderName() string {
	if v := os.Getenv("GC_EVENTS"); v != "" {
		return v
	}
	if cp, err := resolveCity(); err == nil {
		if p := peekEventsProvider(filepath.Join(cp, "city.toml")); p != "" {
			return p
		}
	}
	return ""
}

// newEventsProviderForName returns an events.Provider based on the already
// resolved provider name and the given events file path (used as the default
// backend).
//
//   - "fake" → in-memory fake (all ops succeed)
//   - "fail" → broken fake (all ops return errors)
//   - "exec:<script>" → user-supplied script (absolute path or PATH lookup)
//   - default → file-backed JSONL provider
func newEventsProviderForName(v, eventsPath string, stderr io.Writer) (events.Provider, error) {
	return newEventsProviderForNameWithConfig(v, eventsPath, stderr, config.EventsConfig{})
}

func newEventsProviderForNameWithConfig(v, eventsPath string, stderr io.Writer, eventsCfg config.EventsConfig) (events.Provider, error) {
	if strings.HasPrefix(v, "exec:") {
		return eventsexec.NewProvider(strings.TrimPrefix(v, "exec:"), stderr), nil
	}
	switch v {
	case "fake":
		return events.NewFake(), nil
	case "fail":
		return events.NewFailFake(), nil
	default:
		return newFileEventsRecorder(eventsPath, eventsCfg, stderr)
	}
}

// newUsageSinkByName returns a usage.Sink for the resolved provider name.
//
//   - "discard" / "fake" → drop all facts
//   - "exec:<script>" → user-supplied script (JSON fact per line on stdin)
//   - default / "local" → durable file-backed JSONL sink at usagePath
func newUsageSinkByName(v, usagePath string) usage.Sink {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "exec:") {
		return usage.NewExecSink(strings.TrimPrefix(v, "exec:"))
	}
	switch v {
	case "discard", "fake":
		return usage.Discard
	default: // "" or "local"
		return usage.NewLocalSink(usagePath)
	}
}

// usageSinkForCity builds the usage-fact sink for a city from its configured
// [usage] provider, anchoring the durable local JSONL sink at
// <cityPath>/.gc/usage.jsonl. This is the single construction point shared by
// the controller state and the CLI worker factory so a configured provider
// reaches every worker path, not just the API server.
//
// A nil cfg (or empty provider) yields the default local sink. When cityPath is
// empty there is no durable home for a file-backed sink, so a file-backed
// provider falls back to usage.Discard; an exec: provider stays valid because
// it carries its own command and needs no path.
func usageSinkForCity(cfg *config.City, cityPath string) usage.Sink {
	provider := ""
	if cfg != nil {
		provider = strings.TrimSpace(cfg.Usage.Provider)
	}
	if strings.TrimSpace(cityPath) == "" && !strings.HasPrefix(provider, "exec:") {
		return usage.Discard
	}
	return newUsageSinkByName(provider, filepath.Join(cityPath, ".gc", "usage.jsonl"))
}

type eventsRotationSettings struct {
	enabled              bool
	maxSizeBytes         int64
	checkIntervalRecords int
	checkInterval        time.Duration
	archiveRetainAge     time.Duration
}

func eventsRotationSettingsFromConfig(eventsCfg config.EventsConfig, stderr io.Writer) eventsRotationSettings {
	rot := eventsCfg.Rotation
	settings := eventsRotationSettings{
		enabled:              rot.EnabledOrDefault(),
		maxSizeBytes:         rot.MaxSizeBytesOrDefault(),
		checkIntervalRecords: rot.CheckIntervalRecordsOrDefault(),
		checkInterval:        rot.CheckIntervalDurationOrDefault(),
		archiveRetainAge:     rot.ArchiveRetainAgeDuration(),
	}
	if raw, ok := os.LookupEnv("GC_EVENTS_ROTATION_ENABLED"); ok {
		if parsed, parseOK := parseEventsRotationEnabled(raw); parseOK {
			settings.enabled = parsed
		} else {
			warnEventsRotation(stderr, "events.rotation: warning: ignoring invalid GC_EVENTS_ROTATION_ENABLED=%q\n", raw)
		}
	}
	if raw, ok := os.LookupEnv("GC_EVENTS_ROTATION_MAX_SIZE_BYTES"); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
			settings.maxSizeBytes = n
		} else {
			warnEventsRotation(stderr, "events.rotation: warning: ignoring invalid GC_EVENTS_ROTATION_MAX_SIZE_BYTES=%q\n", raw)
		}
	}
	if raw, ok := os.LookupEnv("GC_EVENTS_ROTATION_RETAIN_AGE"); ok {
		if strings.TrimSpace(raw) == "" {
			settings.archiveRetainAge = 0
		} else if d, err := time.ParseDuration(raw); err == nil {
			settings.archiveRetainAge = d
			if d > 0 && d < 168*time.Hour {
				warnEventsRotation(stderr, "events.rotation: warning: archive_retain_age=%s may delete recent archives\n", raw)
			}
		} else {
			warnEventsRotation(stderr, "events.rotation: warning: ignoring invalid GC_EVENTS_ROTATION_RETAIN_AGE=%q\n", raw)
		}
	}
	return settings
}

func parseEventsRotationEnabled(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on", "enabled":
		return true, true
	case "0", "f", "false", "n", "no", "off", "disabled":
		return false, true
	default:
		return false, false
	}
}

func warnEventsRotation(stderr io.Writer, format string, args ...any) {
	if stderr == nil {
		return
	}
	fmt.Fprintf(stderr, format, args...) //nolint:errcheck // best-effort operator warning
}

func eventsFileRecorderOptions(eventsCfg config.EventsConfig, stderr io.Writer) []events.FileRecorderOption {
	settings := eventsRotationSettingsFromConfig(eventsCfg, stderr)
	maxSize := settings.maxSizeBytes
	if !settings.enabled {
		maxSize = 0
	}
	return []events.FileRecorderOption{
		events.WithMaxSize(maxSize),
		events.WithRotationCheckRecords(settings.checkIntervalRecords),
		events.WithRotationCheckInterval(settings.checkInterval),
		events.WithArchiveRetainAge(settings.archiveRetainAge),
	}
}

func newFileEventsRecorder(eventsPath string, eventsCfg config.EventsConfig, stderr io.Writer) (*events.FileRecorder, error) {
	return events.NewFileRecorder(eventsPath, stderr, eventsFileRecorderOptions(eventsCfg, stderr)...)
}

// openCityEventsProvider resolves the city and returns an events.Provider.
// Returns (nil, exitCode) on failure.
func openCityEventsProvider(stderr io.Writer, cmdName string) (events.Provider, int) {
	return openCityEventsProviderWithConfig(func() config.EventsConfig {
		return eventsProviderConfigWithWarnings(stderr)
	}, stderr, cmdName)
}

func openCityEventEmitProvider(stderr io.Writer, cmdName string) (events.Provider, int) {
	return openCityEventsProviderWithName(fastEventsProviderName, stderr, cmdName)
}

func openCityEventsProviderWithName(providerName func() string, stderr io.Writer, cmdName string) (events.Provider, int) {
	return openCityEventsProviderWithConfig(func() config.EventsConfig {
		return config.EventsConfig{Provider: providerName()}
	}, stderr, cmdName)
}

func openCityEventsProviderWithConfig(providerConfig func() config.EventsConfig, stderr io.Writer, cmdName string) (events.Provider, int) {
	// For exec: and test doubles, no city needed.
	eventsCfg := providerConfig()
	v := eventsCfg.Provider
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		p, err := newEventsProviderForNameWithConfig(v, "", stderr, eventsCfg)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
			return nil, 1
		}
		return p, 0
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	eventsPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	p, err := newEventsProviderForNameWithConfig(v, eventsPath, stderr, eventsCfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return p, 0
}

// newHybridProvider constructs a composite provider that routes sessions to
// tmux (local) or k8s (remote) based on session name. The GC_HYBRID_REMOTE_MATCH
// env var controls which sessions go to k8s. If unset, all sessions route to
// local tmux.
func newHybridProvider(sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
	// Cut-over: hybrid routes to the seam-backed tmux/k8s providers, so
	// hybrid-routed sessions flow through the seams like every other path.
	local := sessiontmux.NewSeamBackedWithConfig(tmuxConfigFromSession(sc, cityName, cityPath))
	remote, err := sessionk8s.NewSeamBacked()
	if err != nil {
		return nil, fmt.Errorf("hybrid: k8s backend: %w", err)
	}
	pattern := sc.RemoteMatch
	if v := os.Getenv("GC_HYBRID_REMOTE_MATCH"); v != "" {
		pattern = v
	}
	return sessionhybrid.New(local, remote, func(name string) bool {
		return pattern != "" && strings.Contains(name, pattern)
	}), nil
}
