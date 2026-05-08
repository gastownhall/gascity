package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	eventsexec "github.com/gastownhall/gascity/internal/events/exec"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	mailexec "github.com/gastownhall/gascity/internal/mail/exec"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionacp "github.com/gastownhall/gascity/internal/runtime/acp"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
	sessionsubprocess "github.com/gastownhall/gascity/internal/runtime/subprocess"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
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
	buildSessionProviderByName = newSessionProviderByName
)

// tmuxConfigFromSession converts a config.SessionConfig into a
// sessiontmux.Config with resolved durations and defaults. If the
// config has no explicit socket name, cityName is used.
func tmuxConfigFromSession(sc config.SessionConfig, cityName, _ string) sessiontmux.Config {
	socketName := sc.Socket
	if socketName == "" {
		socketName = cityName
	}
	return sessiontmux.Config{
		SetupTimeout:       sc.SetupTimeoutDuration(),
		NudgeReadyTimeout:  sc.NudgeReadyTimeoutDuration(),
		NudgeRetryInterval: sc.NudgeRetryIntervalDuration(),
		NudgeLockTimeout:   sc.NudgeLockTimeoutDuration(),
		DebounceMs:         sc.DebounceMsOrDefault(),
		DisplayMs:          sc.DisplayMsOrDefault(),
		SocketName:         socketName,
	}
}

func providerStateDir(providerName, cityPath string) string {
	if cityPath == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(cityPath)))
	return filepath.Join(supervisor.RuntimeDir(), providerName, hex.EncodeToString(sum[:4]))
}

// newSessionProviderByName constructs a runtime.Provider from a provider name.
// cityName is used to auto-default the tmux socket when none is configured.
// cityPath is used to isolate socket-based providers per city.
// Returns error instead of os.Exit, making it safe for the hot-reload path.
//
//   - "fake" → in-memory fake (all ops succeed)
//   - "fail" → broken fake (all ops return errors)
//   - "subprocess" → headless child processes
//   - "acp" → ACP (Agent Client Protocol) JSON-RPC over stdio
//   - "exec:<script>" → user-supplied script (absolute path or PATH lookup)
//   - "k8s" → native Kubernetes provider (client-go)
//   - default → real tmux provider
func newSessionProviderByName(name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
	if strings.HasPrefix(name, "exec:") {
		return sessionexec.NewProvider(strings.TrimPrefix(name, "exec:")), nil
	}
	switch name {
	case "fake":
		return runtime.NewFake(), nil
	case "fail":
		return runtime.NewFailFake(), nil
	case "subprocess":
		if cityPath != "" {
			return sessionsubprocess.NewProviderWithDir(providerStateDir("subprocess", cityPath)), nil
		}
		return sessionsubprocess.NewProvider(), nil
	case "acp":
		cfg := sessionacp.Config{
			HandshakeTimeout:  sc.ACP.HandshakeTimeoutDuration(),
			NudgeBusyTimeout:  sc.ACP.NudgeBusyTimeoutDuration(),
			OutputBufferLines: sc.ACP.OutputBufferLinesOrDefault(),
		}
		if cityPath != "" {
			return sessionacp.NewProviderWithDir(providerStateDir("acp", cityPath), cfg), nil
		}
		return sessionacp.NewProvider(cfg), nil
	case "k8s":
		return sessionk8s.NewProvider()
	case "hybrid":
		return newHybridProvider(sc, cityName, cityPath)
	default:
		return sessiontmux.NewProviderWithConfig(tmuxConfigFromSession(sc, cityName, cityPath)), nil
	}
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
	return newSessionProviderFromContext(ctx, nil)
}

func newStatusSessionProviderForCityWithSnapshot(cfg *config.City, cityPath string, sessionBeads *sessionBeadSnapshot) runtime.Provider {
	ctx := sessionProviderContextForCity(cfg, cityPath, os.Getenv("GC_SESSION"))
	return newSessionProviderFromContext(ctx, sessionBeads)
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
	all, err := store.ListByLabel(sessionBeadLabel, 0)
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
	sp, err := buildSessionProviderByName(ctx.providerName, ctx.sc, ctx.cityName, ctx.cityPath)
	if err != nil {
		return nil, err
	}
	// If the city-level provider is not ACP but some agents need ACP,
	// wrap in an auto provider that routes per-session.
	// NOTE: agents comes from loadCityConfig which applies pack overrides,
	// so the Session field from overrides is already resolved here.
	requireACPWrapper := requiresACPProviderWrapper(sessionBeads, ctx.cityName, ctx.cfg)
	if ctx.providerName != "acp" && needsACPProviderWrapper(sessionBeads, ctx.cityName, ctx.cfg) {
		acpSP, acpErr := buildSessionProviderByName("acp", ctx.sc, ctx.cityName, ctx.cityPath)
		if acpErr != nil {
			if requireACPWrapper {
				return nil, fmt.Errorf("acp provider: %w", acpErr)
			}
			return sp, nil
		}
		autoSP := sessionauto.New(sp, acpSP)
		for _, sessName := range configuredACPRouteNames(sessionBeads, ctx.cityName, ctx.cfg) {
			autoSP.RouteACP(sessName)
		}
		return autoSP, nil
	}
	return sp, nil
}

func agentSessionCreateTransport(cfg *config.City, agentCfg config.Agent) string {
	if cfg == nil {
		return strings.TrimSpace(agentCfg.Session)
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

func needsACPProviderWrapper(snapshot *sessionBeadSnapshot, cityName string, cfg *config.City) bool {
	return requiresACPProviderWrapper(snapshot, cityName, cfg) || (cfg != nil && hasACPProviderTargets(cfg))
}

func requiresACPProviderWrapper(snapshot *sessionBeadSnapshot, cityName string, cfg *config.City) bool {
	return len(configuredACPRouteNames(snapshot, cityName, cfg)) > 0
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
	names := make([]string, 0, len(snapshot.open))
	seen := make(map[string]bool, len(snapshot.open))
	for _, bead := range snapshot.Open() {
		if !beadUsesACPTransport(bead, cfg) {
			continue
		}
		sessionName := strings.TrimSpace(bead.Metadata["session_name"])
		if sessionName == "" || seen[sessionName] {
			continue
		}
		seen[sessionName] = true
		names = append(names, sessionName)
	}
	return names
}

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
// set GC_BEADS=exec:<cityPath>/.gc/system/packs/bd/assets/scripts/gc-beads-bd.sh
// so lifecycle operations stay pinned to the city's Dolt server, but general
// gc commands still need a CRUD-capable store.
func normalizeRawBeadsProvider(cityPath, provider string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" || !strings.HasPrefix(provider, "exec:") || execProviderBase(provider) != "gc-beads-bd" || cityPath == "" {
		return provider
	}
	script := strings.TrimSpace(strings.TrimPrefix(provider, "exec:"))
	if samePath(script, gcBeadsBdScriptPath(cityPath)) {
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

func providerUsesBdStoreContract(provider string) bool {
	provider = strings.TrimSpace(provider)
	if provider == "" || provider == "bd" {
		return true
	}
	if strings.HasPrefix(provider, "exec:") && execProviderBase(provider) == "gc-beads-bd" {
		return true
	}
	return false
}

func cityUsesBdStoreContract(cityPath string) bool {
	return providerUsesBdStoreContract(rawBeadsProvider(cityPath))
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
// Maps "bd" → "exec:<cityPath>/.gc/system/packs/bd/assets/scripts/gc-beads-bd.sh"
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

// gcBeadsBdScriptPath returns the absolute path to the gc-beads-bd script
// inside the materialized bd pack (.gc/system/packs/bd/assets/scripts/).
func gcBeadsBdScriptPath(cityPath string) string {
	return filepath.Join(cityPath, citylayout.SystemPacksRoot, "bd", "assets", "scripts", "gc-beads-bd.sh")
}

// mailProviderName returns the mail provider name.
// Priority: GC_MAIL env var → city.toml [mail].provider → "" (default: beadmail).
func mailProviderName() string {
	if v := os.Getenv("GC_MAIL"); v != "" {
		return v
	}
	if cp, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cp, io.Discard); err == nil && cfg.Mail.Provider != "" {
			return cfg.Mail.Provider
		}
	}
	return ""
}

// newMailProvider returns a mail.Provider based on the mail provider name
// (env var → city.toml → default) and the given bead store (used as the
// default backend). Shared callers such as the API use the stateless beadmail
// provider so long-lived instances observe fresh session state.
//
//   - "fake" → in-memory fake (all ops succeed)
//   - "fail" → broken fake (all ops return errors)
//   - "exec:<script>" → user-supplied script (absolute path or PATH lookup)
//   - default → beadmail (backed by beads.Store, no subprocess)
func newMailProvider(store beads.Store) mail.Provider {
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") {
		return mailexec.NewProvider(strings.TrimPrefix(v, "exec:"))
	}
	switch v {
	case "fake":
		return mail.NewFake()
	case "fail":
		return mail.NewFailFake()
	default:
		return beadmail.New(store)
	}
}

func newCommandMailProvider(store beads.Store) mail.Provider {
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") {
		return mailexec.NewProvider(strings.TrimPrefix(v, "exec:"))
	}
	switch v {
	case "fake":
		return mail.NewFake()
	case "fail":
		return mail.NewFailFake()
	default:
		return beadmail.NewCached(store)
	}
}

// openCityMailProvider opens the city's bead store and wraps it in a
// mail.Provider. Returns (nil, exitCode) on failure.
func openCityMailProvider(stderr io.Writer, cmdName string) (mail.Provider, int) {
	// For exec: and test doubles, no store needed.
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		return newCommandMailProvider(nil), 0
	}

	store, code := openCityStore(stderr, cmdName)
	if store == nil {
		return nil, code
	}
	return newCommandMailProvider(store), 0
}

// eventsProviderName returns the events provider name.
// Priority: GC_EVENTS env var → city.toml [events].provider → "" (default: file JSONL).
func eventsProviderName() string {
	if v := os.Getenv("GC_EVENTS"); v != "" {
		return v
	}
	if cp, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cp, io.Discard); err == nil && cfg.Events.Provider != "" {
			return cfg.Events.Provider
		}
	}
	return ""
}

// newEventsProvider returns an events.Provider based on the events provider
// name (env var → city.toml → default) and the given events file path (used
// as the default backend).
//
//   - "fake" → in-memory fake (all ops succeed)
//   - "fail" → broken fake (all ops return errors)
//   - "exec:<script>" → user-supplied script (absolute path or PATH lookup)
//   - default → file-backed JSONL provider
func newEventsProvider(eventsPath string, stderr io.Writer) (events.Provider, error) {
	v := eventsProviderName()
	if strings.HasPrefix(v, "exec:") {
		return eventsexec.NewProvider(strings.TrimPrefix(v, "exec:"), stderr), nil
	}
	switch v {
	case "fake":
		return events.NewFake(), nil
	case "fail":
		return events.NewFailFake(), nil
	default:
		return events.NewFileRecorder(eventsPath, stderr)
	}
}

// openCityEventsProvider resolves the city and returns an events.Provider.
// Returns (nil, exitCode) on failure.
func openCityEventsProvider(stderr io.Writer, cmdName string) (events.Provider, int) {
	// For exec: and test doubles, no city needed.
	v := eventsProviderName()
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		p, err := newEventsProvider("", stderr)
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
	p, err := newEventsProvider(eventsPath, stderr)
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
	local := sessiontmux.NewProviderWithConfig(tmuxConfigFromSession(sc, cityName, cityPath))
	remote, err := sessionk8s.NewProvider()
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
