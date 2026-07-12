package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
)

type beadsBackendSetupContext struct {
	CityPath string
	Provider string
	Backend  string
}

type beadsBackendScopeContext struct {
	CityPath  string
	ScopeRoot string
	ScopeKind string
	ScopeName string
	Prefix    string
	Namespace string
	Backend   string
}

type beadsBackendPluginCapabilities struct {
	// NativeDriver names a storage backend compiled into stock upstream bd.
	// Native bundles have no plugin process or endpoint.
	NativeDriver string
	// SQLitePath is the pack default for the native SQLite database file.
	SQLitePath string
	// SetupHook means the plugin can initialize scope files and owns
	// .beads/metadata.json creation/normalization for this backend.
	SetupHook bool
	// ProviderLifecycle means the plugin can provide the bd-compatible command
	// surface GC uses for normal bead operations.
	ProviderLifecycle bool
	// BackendPluginMetadata means metadata.json may contain bd backend plugin
	// fields such as backend_plugin_command/backend_plugin_args.
	BackendPluginMetadata bool
	// GascityFastpathMetadata means metadata.json may contain GC fastpath fields
	// such as gascity_backend_command/gascity_backend_args.
	GascityFastpathMetadata bool
	// NativeReadStore means GC may use an optimized in-process read store for
	// hot paths when explicitly enabled and compatible with the metadata shape.
	NativeReadStore bool
	// StoreHealthPath means this backend owns its own on-disk health/size path.
	StoreHealthPath bool
	// JSONLExport means the backend can satisfy the core Dolt JSONL archive
	// maintenance order. Most plugin backends should leave this false and
	// provide their own export/snapshot order when needed.
	JSONLExport bool
	// BDCompatibility is the bd CLI contract the plugin expects by default.
	BDCompatibility string
}

type beadsBackendPluginScopePolicy struct {
	Model                  string
	Resource               string
	NamespaceFrom          string
	InheritsCityConnection bool
	MetadataOwner          string
	Routes                 string
	Adopt                  string
	Remove                 string
	RequiresGit            bool
}

type beadsBackendPluginEndpoint struct {
	Command  string
	Args     []string
	Protocol string
}

type beadsBackendPlugin interface {
	Name() string
	Capabilities(beadsBackendSetupContext) beadsBackendPluginCapabilities
	ScopePolicy(beadsBackendSetupContext) beadsBackendPluginScopePolicy
	SetupHook(beadsBackendSetupContext) (string, bool)
	StorePath(beadsBackendSetupContext) (string, bool)
	BeadsEndpoint(beadsBackendSetupContext) (beadsBackendPluginEndpoint, bool)
	GascityEndpoint(beadsBackendSetupContext) (beadsBackendPluginEndpoint, bool)
}

var beadsBackendSetupRegistry = struct {
	sync.RWMutex
	providers map[string]beadsBackendPlugin
}{providers: map[string]beadsBackendPlugin{}}

func registerBeadsBackendPlugin(provider beadsBackendPlugin) {
	if provider == nil {
		panic("beads backend plugin: nil provider")
	}
	name := strings.TrimSpace(provider.Name())
	if name == "" {
		panic("beads backend plugin: provider with empty name")
	}
	beadsBackendSetupRegistry.Lock()
	defer beadsBackendSetupRegistry.Unlock()
	if _, exists := beadsBackendSetupRegistry.providers[name]; exists {
		panic("beads backend plugin: duplicate provider " + name)
	}
	beadsBackendSetupRegistry.providers[name] = provider
}

func lookupBeadsBackendPlugin(name string) (beadsBackendPlugin, bool) {
	beadsBackendSetupRegistry.RLock()
	defer beadsBackendSetupRegistry.RUnlock()
	provider, ok := beadsBackendSetupRegistry.providers[strings.TrimSpace(name)]
	return provider, ok
}

func beadsProviderSetupHook(cityPath string) (string, bool) {
	if script := strings.TrimSpace(os.Getenv("GC_BEADS_SETUP")); script != "" {
		return script, true
	}
	provider, ctx, ok := beadsBackendPluginForCity(cityPath)
	if !ok {
		return "", false
	}
	return provider.SetupHook(ctx)
}

func beadsBackendPluginForCity(cityPath string) (beadsBackendPlugin, beadsBackendSetupContext, bool) {
	ctx := beadsBackendSetupContext{
		CityPath: cityPath,
		Provider: rawBeadsProvider(cityPath),
		Backend:  beadsBackend(cityPath),
	}
	if !providerUsesBdStoreContract(ctx.Provider) {
		return nil, ctx, false
	}
	provider, ok := beadsBackendPluginNamedForCity(ctx.CityPath, ctx.Backend)
	return provider, ctx, ok
}

func beadsBackendPluginNamedForCity(cityPath, backend string) (beadsBackendPlugin, bool) {
	if provider, ok := discoveredBeadsBackendPluginForCity(cityPath, backend); ok {
		return provider, true
	}
	return lookupBeadsBackendPlugin(backend)
}

func isRegisteredPluginBackendForCity(cityPath, backend string) bool {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend == "" || backend == "dolt" || backend == "postgres" {
		return false
	}
	provider, ok := beadsBackendPluginNamedForCity(cityPath, backend)
	if !ok {
		return false
	}
	return strings.TrimSpace(provider.Capabilities(beadsBackendSetupContext{CityPath: cityPath, Backend: backend}).NativeDriver) == ""
}

func nativeBeadsBackendForCity(cityPath string) (driver, sqlitePath string, ok bool) {
	provider, ctx, found := beadsBackendPluginForCity(cityPath)
	if !found {
		return "", "", false
	}
	caps := provider.Capabilities(ctx)
	driver = strings.ToLower(strings.TrimSpace(caps.NativeDriver))
	if driver != "postgres" && driver != "sqlite" {
		return "", "", false
	}
	return driver, strings.TrimSpace(caps.SQLitePath), true
}

func cityUsesNativeBeadsBackend(cityPath string) bool {
	_, _, ok := nativeBeadsBackendForCity(cityPath)
	return ok
}

func beadsBackendPluginCapabilitiesForCity(cityPath string) (beadsBackendPluginCapabilities, bool) {
	provider, ctx, ok := beadsBackendPluginForCity(cityPath)
	if !ok {
		return beadsBackendPluginCapabilities{}, false
	}
	return provider.Capabilities(ctx), true
}

func cityUsesPluginBeadsBackendSetup(cityPath string) bool {
	if rawBeadsProvider(cityPath) != "plugin" && beadsBackend(cityPath) == "dolt" {
		return false
	}
	caps, ok := beadsBackendPluginCapabilitiesForCity(cityPath)
	return ok && caps.SetupHook
}

func beadsBackendPluginStorePath(cityPath string) (string, bool) {
	provider, ctx, ok := beadsBackendPluginForCity(cityPath)
	if !ok {
		return "", false
	}
	return provider.StorePath(ctx)
}

func beadsBackendPluginScopePolicyForCity(cityPath string) (beadsBackendPluginScopePolicy, bool) {
	provider, ctx, ok := beadsBackendPluginForCity(cityPath)
	if !ok {
		return beadsBackendPluginScopePolicy{}, false
	}
	return normalizeBeadsBackendScopePolicy(provider.ScopePolicy(ctx)), true
}

type discoveredBeadsBackendPlugin struct {
	plugin config.DiscoveredBackendPlugin
}

func discoveredBeadsBackendPluginForCity(cityPath, backend string) (beadsBackendPlugin, bool) {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return nil, false
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	for _, plugin := range cfg.BackendPlugins {
		if strings.ToLower(strings.TrimSpace(plugin.Backend)) == backend {
			return discoveredBeadsBackendPlugin{plugin: plugin}, true
		}
	}
	return nil, false
}

func (p discoveredBeadsBackendPlugin) Name() string {
	return p.plugin.Backend
}

func (p discoveredBeadsBackendPlugin) Capabilities(beadsBackendSetupContext) beadsBackendPluginCapabilities {
	capSet := make(map[string]bool, len(p.plugin.Capabilities))
	for _, cap := range p.plugin.Capabilities {
		capSet[strings.ToLower(strings.TrimSpace(cap))] = true
	}
	return beadsBackendPluginCapabilities{
		NativeDriver:            p.plugin.Driver,
		SQLitePath:              p.plugin.SQLitePath,
		SetupHook:               p.plugin.SetupHook != "" || p.plugin.ProviderCommand != "" || capSet["setup"],
		ProviderLifecycle:       p.plugin.ProviderCommand != "" || p.plugin.BeadsEndpoint.Command != "" || capSet["provider"],
		BackendPluginMetadata:   p.plugin.BeadsEndpoint.Command != "" || capSet["metadata"] || capSet["backend-metadata"],
		GascityFastpathMetadata: p.plugin.GascityEndpoint.Command != "" || capSet["fastpath"] || capSet["gascity-fastpath"],
		NativeReadStore:         capSet["fastpath"] || capSet["native-read"] || capSet["native-read-store"],
		StoreHealthPath:         p.plugin.StorePath != "" || capSet["store-health"],
		JSONLExport:             capSet["jsonl-export"] || capSet["dolt-jsonl-export"],
		BDCompatibility:         p.plugin.BDCompatibility,
	}
}

func (p discoveredBeadsBackendPlugin) ScopePolicy(beadsBackendSetupContext) beadsBackendPluginScopePolicy {
	return beadsBackendPluginScopePolicy{
		Model:                  p.plugin.Scope.Model,
		Resource:               p.plugin.Scope.Resource,
		NamespaceFrom:          p.plugin.Scope.NamespaceFrom,
		InheritsCityConnection: p.plugin.Scope.InheritsCityConnection,
		MetadataOwner:          p.plugin.Scope.MetadataOwner,
		Routes:                 p.plugin.Scope.Routes,
		Adopt:                  p.plugin.Scope.Adopt,
		Remove:                 p.plugin.Scope.Remove,
		RequiresGit:            p.plugin.Scope.RequiresGit,
	}
}

func (p discoveredBeadsBackendPlugin) SetupHook(beadsBackendSetupContext) (string, bool) {
	for _, candidate := range []string{p.plugin.SetupHook, p.plugin.ProviderCommand} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}

func (p discoveredBeadsBackendPlugin) StorePath(ctx beadsBackendSetupContext) (string, bool) {
	storePath := strings.TrimSpace(p.plugin.StorePath)
	if storePath == "" {
		return "", false
	}
	if filepath.IsAbs(storePath) {
		return storePath, true
	}
	if strings.TrimSpace(ctx.CityPath) == "" {
		return "", false
	}
	return filepath.Join(ctx.CityPath, storePath), true
}

func (p discoveredBeadsBackendPlugin) BeadsEndpoint(beadsBackendSetupContext) (beadsBackendPluginEndpoint, bool) {
	return backendPluginEndpointFromConfig(p.plugin.BeadsEndpoint)
}

func (p discoveredBeadsBackendPlugin) GascityEndpoint(beadsBackendSetupContext) (beadsBackendPluginEndpoint, bool) {
	return backendPluginEndpointFromConfig(p.plugin.GascityEndpoint)
}

func backendPluginEndpointFromConfig(in config.DiscoveredBackendPluginEndpoint) (beadsBackendPluginEndpoint, bool) {
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return beadsBackendPluginEndpoint{}, false
	}
	return beadsBackendPluginEndpoint{
		Command:  command,
		Args:     append([]string(nil), in.Args...),
		Protocol: strings.TrimSpace(in.Protocol),
	}, true
}

type scriptBeadsBackendPlugin struct {
	name       string
	scriptBase string
	storeDir   string
	compat     string
}

func (p scriptBeadsBackendPlugin) Name() string {
	return p.name
}

func (p scriptBeadsBackendPlugin) Capabilities(beadsBackendSetupContext) beadsBackendPluginCapabilities {
	return beadsBackendPluginCapabilities{
		SetupHook:               true,
		ProviderLifecycle:       true,
		BackendPluginMetadata:   true,
		GascityFastpathMetadata: true,
		NativeReadStore:         true,
		StoreHealthPath:         true,
		BDCompatibility:         p.compat,
	}
}

func (p scriptBeadsBackendPlugin) ScopePolicy(beadsBackendSetupContext) beadsBackendPluginScopePolicy {
	return beadsBackendPluginScopePolicy{
		Model:                  "per_scope",
		Resource:               "database",
		NamespaceFrom:          "city_or_prefix",
		InheritsCityConnection: true,
		MetadataOwner:          "plugin",
		Routes:                 "gc-prefix-routes",
		Adopt:                  "validate_or_repair",
		Remove:                 "preserve",
	}
}

func (p scriptBeadsBackendPlugin) SetupHook(ctx beadsBackendSetupContext) (string, bool) {
	if strings.TrimSpace(ctx.CityPath) == "" {
		return "", false
	}
	script := filepath.Join(ctx.CityPath, ".gc", "scripts", p.scriptBase)
	if st, err := os.Stat(script); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
		return script, true
	}
	return "", false
}

func (p scriptBeadsBackendPlugin) StorePath(ctx beadsBackendSetupContext) (string, bool) {
	if strings.TrimSpace(ctx.CityPath) == "" || strings.TrimSpace(p.storeDir) == "" {
		return "", false
	}
	return filepath.Join(ctx.CityPath, ".beads", p.storeDir), true
}

func (p scriptBeadsBackendPlugin) BeadsEndpoint(beadsBackendSetupContext) (beadsBackendPluginEndpoint, bool) {
	return beadsBackendPluginEndpoint{}, false
}

func (p scriptBeadsBackendPlugin) GascityEndpoint(beadsBackendSetupContext) (beadsBackendPluginEndpoint, bool) {
	return beadsBackendPluginEndpoint{}, false
}

func init() {
	registerBeadsBackendPlugin(scriptBeadsBackendPlugin{
		name:       "dolt",
		scriptBase: "gc-beads-bd.sh",
		storeDir:   "dolt",
		compat:     "bd-1.0.5",
	})
	registerBeadsBackendPlugin(scriptBeadsBackendPlugin{
		name:       "doltlite",
		scriptBase: "gc-beads-doltlite-bd.sh",
		storeDir:   "doltlite",
		compat:     "bd-1.0.5",
	})
}

func mustLookupBeadsBackendPlugin(name string) (beadsBackendPlugin, error) {
	provider, ok := lookupBeadsBackendPlugin(name)
	if !ok {
		return nil, fmt.Errorf("unknown beads backend plugin %q", strings.TrimSpace(name))
	}
	return provider, nil
}

func normalizeBeadsBackendScopePolicy(policy beadsBackendPluginScopePolicy) beadsBackendPluginScopePolicy {
	if strings.TrimSpace(policy.Model) == "" {
		policy.Model = "per_scope"
	}
	if strings.TrimSpace(policy.Resource) == "" {
		policy.Resource = "database"
	}
	if strings.TrimSpace(policy.NamespaceFrom) == "" {
		policy.NamespaceFrom = "city_or_prefix"
	}
	if strings.TrimSpace(policy.MetadataOwner) == "" {
		policy.MetadataOwner = "plugin"
	}
	if strings.TrimSpace(policy.Routes) == "" {
		policy.Routes = "gc-prefix-routes"
	}
	if strings.TrimSpace(policy.Adopt) == "" {
		policy.Adopt = "validate_or_repair"
	}
	if strings.TrimSpace(policy.Remove) == "" {
		policy.Remove = "preserve"
	}
	return policy
}

func beadsBackendScopeContextForCityScope(cityPath, scopeRoot, prefix, namespaceOverride string) beadsBackendScopeContext {
	policy, ok := beadsBackendPluginScopePolicyForCity(cityPath)
	if !ok {
		policy = normalizeBeadsBackendScopePolicy(beadsBackendPluginScopePolicy{})
	}
	scopeKind, scopeName := backendPluginScopeKindAndName(cityPath, scopeRoot)
	namespace := strings.TrimSpace(namespaceOverride)
	if namespace == "" {
		namespace = backendPluginScopeNamespace(policy, scopeKind, scopeName, prefix)
	}
	return beadsBackendScopeContext{
		CityPath:  cityPath,
		ScopeRoot: scopeRoot,
		ScopeKind: scopeKind,
		ScopeName: scopeName,
		Prefix:    strings.TrimSpace(prefix),
		Namespace: namespace,
		Backend:   beadsBackend(cityPath),
	}
}

func backendPluginScopeKindAndName(cityPath, scopeRoot string) (string, string) {
	cityPath = filepath.Clean(cityPath)
	scopeRoot = filepath.Clean(scopeRoot)
	if samePath(cityPath, scopeRoot) {
		return "city", filepath.Base(cityPath)
	}
	if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil && cfg != nil {
		resolveRigPaths(cityPath, cfg.Rigs)
		for i := range cfg.Rigs {
			if strings.TrimSpace(cfg.Rigs[i].Path) == "" {
				continue
			}
			if samePath(resolveStoreScopeRoot(cityPath, cfg.Rigs[i].Path), scopeRoot) {
				return "rig", cfg.Rigs[i].Name
			}
		}
	}
	return "rig", filepath.Base(scopeRoot)
}

func backendPluginScopeNamespace(policy beadsBackendPluginScopePolicy, scopeKind, scopeName, prefix string) string {
	policy = normalizeBeadsBackendScopePolicy(policy)
	scopeKind = strings.TrimSpace(scopeKind)
	scopeName = strings.TrimSpace(scopeName)
	prefix = strings.TrimSpace(prefix)
	switch strings.TrimSpace(policy.NamespaceFrom) {
	case "none":
		return ""
	case "scope_name":
		return scopeName
	case "prefix":
		return prefix
	case "city_or_prefix":
		if scopeKind == "city" {
			return "hq"
		}
		if prefix != "" {
			return prefix
		}
		return scopeName
	default:
		if prefix != "" {
			return prefix
		}
		return scopeName
	}
}

func applyBackendPluginScopeEnv(overrides map[string]string, scope beadsBackendScopeContext, policy beadsBackendPluginScopePolicy) {
	if overrides == nil {
		return
	}
	policy = normalizeBeadsBackendScopePolicy(policy)
	overrides["GC_BEADS_BACKEND"] = scope.Backend
	overrides["GC_BEADS_SETUP_SCOPE"] = scope.ScopeRoot
	overrides["GC_BEADS_SETUP_SCOPE_ROOT"] = scope.ScopeRoot
	overrides["GC_BEADS_SETUP_SCOPE_KIND"] = scope.ScopeKind
	overrides["GC_BEADS_SETUP_SCOPE_NAME"] = scope.ScopeName
	overrides["GC_BEADS_SETUP_PREFIX"] = scope.Prefix
	overrides["GC_BEADS_SETUP_NAMESPACE"] = scope.Namespace
	overrides["GC_BEADS_SCOPE_ROOT"] = scope.ScopeRoot
	overrides["GC_BEADS_SCOPE_KIND"] = scope.ScopeKind
	overrides["GC_BEADS_SCOPE_NAME"] = scope.ScopeName
	overrides["GC_BEADS_SCOPE_NAMESPACE"] = scope.Namespace
	overrides["GC_BEADS_SCOPE_MODEL"] = policy.Model
	overrides["GC_BEADS_SCOPE_RESOURCE"] = policy.Resource
	overrides["GC_BEADS_SCOPE_NAMESPACE_FROM"] = policy.NamespaceFrom
	overrides["GC_BEADS_SCOPE_ROUTES"] = policy.Routes
	overrides["GC_BEADS_METADATA_OWNER"] = policy.MetadataOwner
	if policy.InheritsCityConnection {
		overrides["GC_BEADS_SCOPE_INHERITS_CITY_CONNECTION"] = "true"
	} else {
		overrides["GC_BEADS_SCOPE_INHERITS_CITY_CONNECTION"] = "false"
	}
}
