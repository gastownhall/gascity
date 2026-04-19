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
		if cfg, err := loadCityConfig(cp); err == nil {
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
	ctx.cityName = cfg.Workspace.Name
	if ctx.cityName == "" {
		ctx.cityName = filepath.Base(cityPath)
	}
	ctx.agents = cfg.Agents
	ctx.sessionTemplate = cfg.Workspace.SessionTemplate
	if ctx.providerName == "" {
		ctx.providerName = cfg.Session.Provider
	}
	return ctx
}

var openSessionProviderStore = openCityStoreAt

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

func loadProviderSessionSnapshot(ctx sessionProviderContext) *sessionBeadSnapshot {
	if ctx.cityPath == "" || ctx.providerName == "acp" || !hasACPAgents(ctx.agents) {
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
	sp, err := newSessionProviderByName(ctx.providerName, ctx.sc, ctx.cityName, ctx.cityPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err) //nolint:errcheck // best-effort stderr
		os.Exit(1)
	}
	// If the city-level provider is not ACP but some agents need ACP,
	// wrap in an auto provider that routes per-session.
	// NOTE: agents comes from loadCityConfig which applies pack overrides,
	// so the Session field from overrides is already resolved here.
	if ctx.providerName != "acp" && hasACPAgents(ctx.agents) {
		acpSP, acpErr := newSessionProviderByName("acp", ctx.sc, ctx.cityName, ctx.cityPath)
		if acpErr != nil {
			fmt.Fprintf(os.Stderr, "acp provider: %v\n", acpErr) //nolint:errcheck // best-effort stderr
			os.Exit(1)
		}
		autoSP := sessionauto.New(sp, acpSP)
		for _, sessName := range configuredACPSessionNames(sessionBeads, ctx.cityName, ctx.sessionTemplate, ctx.agents) {
			autoSP.RouteACP(sessName)
		}
		return autoSP
	}
	return sp
}

// hasACPAgents reports whether any agent in the config uses session = "acp".
func hasACPAgents(agents []config.Agent) bool {
	for _, a := range agents {
		if a.Session == "acp" {
			return true
		}
	}
	return false
}

// configuredACPSessionNames resolves the runtime session names for ACP-backed
// agents using a single session-bead snapshot. When the snapshot is unavailable
// or bead lookup fails, it falls back to the legacy deterministic name.
func configuredACPSessionNames(snapshot *sessionBeadSnapshot, cityName, sessionTemplate string, agents []config.Agent) []string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.Session != "acp" {
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

// displayProviderName returns a human-readable provider name for logging.
func displayProviderName(name string) string {
	if name == "" {
		return "tmux (default)"
	}
	return name
}

func configuredBeadsProviderValue(cityPath string) string {
	if v := strings.TrimSpace(os.Getenv("GC_BEADS")); v != "" {
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
		if cfg, err := loadCityConfig(cp); err == nil && cfg.Mail.Provider != "" {
			return cfg.Mail.Provider
		}
	}
	return ""
}

// newMailProvider returns a mail.Provider based on the mail provider name
// (env var → city.toml → default) and the given bead store (used as the
// default backend).
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

// openCityMailProvider opens the city's bead store and wraps it in a
// mail.Provider. Returns (nil, exitCode) on failure.
func openCityMailProvider(stderr io.Writer, cmdName string) (mail.Provider, int) {
	// For exec: and test doubles, no store needed.
	v := mailProviderName()
	if strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		return newMailProvider(nil), 0
	}

	store, code := openCityStore(stderr, cmdName)
	if store == nil {
		return nil, code
	}
	return newMailProvider(store), 0
}

// eventsProviderName returns the events provider name.
// Priority: GC_EVENTS env var → city.toml [events].provider → "" (default: file JSONL).
func eventsProviderName() string {
	if v := os.Getenv("GC_EVENTS"); v != "" {
		return v
	}
	if cp, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cp); err == nil && cfg.Events.Provider != "" {
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
