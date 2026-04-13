package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
	sessionrouted "github.com/gastownhall/gascity/internal/runtime/routed"
	sessionsubprocess "github.com/gastownhall/gascity/internal/runtime/subprocess"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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

func execProviderStateDir(providerSpec, cityPath string) string {
	if cityPath == "" {
		return ""
	}
	citySum := sha256.Sum256([]byte(filepath.Clean(cityPath)))
	specSum := sha256.Sum256([]byte(providerSpec))
	return filepath.Join(
		supervisor.RuntimeDir(),
		"exec",
		hex.EncodeToString(citySum[:4]),
		hex.EncodeToString(specSum[:8]),
	)
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
		return sessionexec.NewProviderWithOptions(
			strings.TrimPrefix(name, "exec:"),
			sessionexec.WithStateDir(execProviderStateDir(name, cityPath)),
		), nil
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
// name (env var → city.toml → default), with per-session routes for agents
// that override their runtime substrate. Startup path — exits on error.
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
	if ctx.cityPath == "" {
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
	ctx.providerName = canonicalSessionProviderName(ctx.providerName)

	defaultKey := runtime.ProviderBackendKey(ctx.providerName, "")
	sp, err := newSessionProviderByName(ctx.providerName, ctx.sc, ctx.cityName, ctx.cityPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err) //nolint:errcheck // best-effort stderr
		os.Exit(1)
	}

	routes, err := configuredSessionProviderRoutes(sessionBeads, ctx.cityName, ctx.cityPath, ctx.sessionTemplate, ctx.providerName, ctx.agents)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err) //nolint:errcheck // best-effort stderr
		os.Exit(1)
	}
	needsRouter := false
	backendProfiles := make(map[string]sessionProviderRoute)
	for _, route := range routes {
		backendKey := runtime.ProviderBackendKey(route.provider, route.profile)
		if backendKey == "" || backendKey == defaultKey {
			continue
		}
		needsRouter = true
		backendProfiles[backendKey] = route
	}
	if !needsRouter {
		return sp
	}

	routedSP := sessionrouted.New(defaultKey, sp)
	backendKeys := make([]string, 0, len(backendProfiles))
	for backendKey := range backendProfiles {
		backendKeys = append(backendKeys, backendKey)
	}
	sort.Strings(backendKeys)
	for _, backendKey := range backendKeys {
		route := backendProfiles[backendKey]
		backend, backendErr := newSessionBackendByProviderKey(route.provider, route.profile, ctx)
		if backendErr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", backendErr) //nolint:errcheck // best-effort stderr
			os.Exit(1)
		}
		if err := routedSP.Register(backendKey, backend); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err) //nolint:errcheck // best-effort stderr
			os.Exit(1)
		}
	}
	for _, route := range routes {
		backendKey := runtime.ProviderBackendKey(route.provider, route.profile)
		if backendKey == "" || backendKey == defaultKey || route.deferRoute {
			continue
		}
		routedSP.Route(route.name, backendKey)
	}
	return routedSP
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

type sessionProviderRoute struct {
	name       string
	provider   string
	profile    string
	deferRoute bool
}

func configuredSessionProviderRoutes(snapshot *sessionBeadSnapshot, cityName, cityPath, sessionTemplate, defaultProvider string, agents []config.Agent) ([]sessionProviderRoute, error) {
	routeByName := make(map[string]sessionProviderRoute)
	deferredBackendRoutes := make([]sessionProviderRoute, 0)
	unownedExisting := make(map[string]bool)
	if snapshot != nil {
		for _, b := range snapshot.Open() {
			name := strings.TrimSpace(b.Metadata["session_name"])
			if name == "" {
				continue
			}
			provider := strings.TrimSpace(b.Metadata[sessionpkg.MetadataSessionProvider])
			if provider == "" && sessionBeadTransport(b) == "acp" {
				provider = "acp"
			}
			if provider == "" {
				unownedExisting[name] = true
				continue
			}
			routeByName[name] = sessionProviderRoute{
				name:     name,
				provider: provider,
				profile:  strings.TrimSpace(b.Metadata[sessionpkg.MetadataSessionProviderProfile]),
			}
		}
	}

	defaultKey := runtime.ProviderBackendKey(defaultProvider, "")
	for _, a := range agents {
		provider, profile, err := desiredSessionProviderForAgent(&a, cityPath, defaultProvider)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", a.QualifiedName(), err)
		}
		name := agent.SessionNameFor(cityName, a.QualifiedName(), sessionTemplate)
		if snapshot != nil {
			if beadName := snapshot.FindSessionNameByTemplate(a.QualifiedName()); beadName != "" {
				name = beadName
			}
		}
		if existing, exists := routeByName[name]; exists {
			desiredKey := runtime.ProviderBackendKey(provider, profile)
			existingKey := runtime.ProviderBackendKey(existing.provider, existing.profile)
			if desiredKey != existingKey {
				if desiredKey == defaultKey {
					if existingKey != defaultKey {
						deferredBackendRoutes = append(deferredBackendRoutes, sessionProviderRoute{name: name, provider: existing.provider, profile: existing.profile, deferRoute: true})
					}
					routeByName[name] = sessionProviderRoute{name: name, provider: provider, profile: profile}
					continue
				}
				deferredBackendRoutes = append(deferredBackendRoutes, sessionProviderRoute{name: name, provider: provider, profile: profile, deferRoute: true})
			}
			continue
		}
		if unownedExisting[name] && strings.HasPrefix(provider, "exec:") {
			routeByName[name] = sessionProviderRoute{name: name, provider: provider, profile: profile, deferRoute: true}
			continue
		}
		routeByName[name] = sessionProviderRoute{name: name, provider: provider, profile: profile}
	}

	names := make([]string, 0, len(routeByName))
	for name := range routeByName {
		names = append(names, name)
	}
	sort.Strings(names)
	routes := make([]sessionProviderRoute, 0, len(names))
	for _, name := range names {
		routes = append(routes, routeByName[name])
	}
	sort.Slice(deferredBackendRoutes, func(i, j int) bool {
		if deferredBackendRoutes[i].name != deferredBackendRoutes[j].name {
			return deferredBackendRoutes[i].name < deferredBackendRoutes[j].name
		}
		return runtime.ProviderBackendKey(deferredBackendRoutes[i].provider, deferredBackendRoutes[i].profile) <
			runtime.ProviderBackendKey(deferredBackendRoutes[j].provider, deferredBackendRoutes[j].profile)
	})
	routes = append(routes, deferredBackendRoutes...)
	return routes, nil
}

func sessionBeadTransport(b beads.Bead) string {
	if transport := strings.TrimSpace(b.Metadata["transport"]); transport != "" {
		return transport
	}
	if strings.TrimSpace(b.Metadata["provider"]) == "acp" {
		return "acp"
	}
	return ""
}

func newSessionBackendByProviderKey(key, profile string, ctx sessionProviderContext) (runtime.Provider, error) {
	if strings.HasPrefix(key, "exec:") {
		script := strings.TrimPrefix(key, "exec:")
		switch profile {
		case "":
			return sessionexec.NewProviderWithOptions(script, sessionexec.WithStateDir(execProviderStateDir(key, ctx.cityPath))), nil
		case remoteWorkerProfile:
			return sessionexec.NewRemoteWorkerProvider(script, execProviderStateDir(key, ctx.cityPath)), nil
		default:
			return nil, fmt.Errorf("session provider %q uses unsupported profile %q", key, profile)
		}
	}
	if profile != "" {
		return nil, fmt.Errorf("session provider %q uses unsupported profile %q", key, profile)
	}
	return newSessionProviderByName(key, ctx.sc, ctx.cityName, ctx.cityPath)
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

// rawBeadsProvider returns the raw bead store provider name from config.
// Priority: GC_BEADS env var → city.toml [beads].provider → "bd" default.
// This is the unmodified config value; use beadsProvider() for lifecycle
// routing which remaps "bd" → exec:.
func rawBeadsProvider(cityPath string) string {
	if v := os.Getenv("GC_BEADS"); v != "" {
		return v
	}
	// Try to read provider from city.toml.
	cfg, err := loadCityConfig(cityPath)
	if err == nil && cfg.Beads.Provider != "" {
		return cfg.Beads.Provider
	}
	return "bd"
}

// beadsProvider returns the bead store provider name for lifecycle operations.
// Maps "bd" → "exec:<cityPath>/.gc/system/bin/gc-beads-bd" so all lifecycle operations
// route through the exec: protocol. Other providers pass through unchanged.
//
// Related env vars:
//   - GC_DOLT=skip — the gc-beads-bd script checks this and exits 2 for all
//     operations. Used by testscript and integration tests.
func beadsProvider(cityPath string) string {
	raw := rawBeadsProvider(cityPath)
	if raw == "bd" {
		return "exec:" + filepath.Join(cityPath, citylayout.SystemBinRoot, "gc-beads-bd")
	}
	return raw
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
