package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// DiscoveredRuntime is a pack-declared runtime provider ([runtimes.<name>]
// in pack.toml) resolved during pack load: the selection name, the resolved
// command, the declared RPP protocol version, and the declaring pack for
// diagnostics. cmd/gc registers these into the runtime selection registry
// during city composition (RUNTIME-SEL rows in internal/runtime/REQUIREMENTS.md).
type DiscoveredRuntime struct {
	// Name is the selection name agents and city.toml [session] use.
	Name string
	// Command is absolute when declared pack-relative; bare PATH names
	// pass through unchanged and resolve at session start.
	Command string
	// Protocol is the declared RPP version (0 today).
	Protocol int
	// PackName and PackDir identify the declaring pack.
	PackName string
	PackDir  string
}

// supportedRuntimeProtocol is the highest RPP version this binary can
// host. Mirrors runtime.ProtocolVersion0; kept as a local constant so the
// config layer does not grow an import edge on internal/runtime.
const supportedRuntimeProtocol = 0

// packLocalRuntimes validates and resolves a pack's own [runtimes.<name>]
// declarations. Pack-relative commands resolve against packDir; invalid
// names, blank commands, and unsupported protocol versions are load errors
// so a broken runtime declaration fails at composition, not session start.
func packLocalRuntimes(tc *PackConfig, packDir string) ([]DiscoveredRuntime, error) {
	if len(tc.Runtimes) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(tc.Runtimes))
	for name := range tc.Runtimes {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]DiscoveredRuntime, 0, len(names))
	for _, name := range names {
		entry := tc.Runtimes[name]
		// ':' is reserved for prefix-form selection names (exec:…), '/'
		// and whitespace keep selection names shell- and TOML-friendly.
		if strings.ContainsAny(name, ":/") || strings.ContainsFunc(name, unicode.IsSpace) {
			return nil, fmt.Errorf("pack %q runtime %q: name must not contain ':', '/', or whitespace", tc.Pack.Name, name)
		}
		command := strings.TrimSpace(entry.Command)
		if command == "" {
			return nil, fmt.Errorf("pack %q runtime %q: command is required", tc.Pack.Name, name)
		}
		if entry.Protocol != supportedRuntimeProtocol {
			return nil, fmt.Errorf("pack %q runtime %q: protocol %d not supported (this gc speaks RPP version %d)",
				tc.Pack.Name, name, entry.Protocol, supportedRuntimeProtocol)
		}
		out = append(out, DiscoveredRuntime{
			Name:     name,
			Command:  resolveRuntimeCommand(command, packDir),
			Protocol: entry.Protocol,
			PackName: tc.Pack.Name,
			PackDir:  packDir,
		})
	}
	return out, nil
}

// resolveRuntimeCommand anchors pack-relative commands at the declaring
// pack directory. Bare names (no path separator) stay as-is so the exec
// provider resolves them on PATH at session start, matching `exec:`
// selection semantics (RUNTIME-SEL-004).
func resolveRuntimeCommand(command, packDir string) string {
	if filepath.IsAbs(command) || !strings.Contains(command, "/") {
		return command
	}
	return filepath.Join(packDir, command)
}

// mergeCityRuntimes registers pack-declared runtimes into the city-wide
// selection namespace. Identical re-declarations of the same pack (reached
// through a diamond import DAG) dedupe; any other re-declaration — a
// different command, protocol, or declaring pack — is a composition error.
// A runtime name must never be silently shadowed or re-attributed: doctor
// diagnostics and `gc runtime check` name the declaring pack.
func mergeCityRuntimes(cfg *City, runtimes []DiscoveredRuntime) error {
	for _, rt := range runtimes {
		if existing, ok := cfg.Runtimes[rt.Name]; ok {
			// Dedupe ONLY when the same resolved pack directory re-declares the
			// same runtime — the diamond-import DAG case where one pack is reached
			// twice. Two DIFFERENT declaring directories are a genuine conflict
			// even when pack.name, command, and protocol coincide (e.g. two packs
			// both named "shared" with a bare PATH command): a runtime row must
			// never silently collapse two distinct packs, or doctor and
			// `gc runtime check` would misattribute its provenance.
			if sameRuntimeDeclaration(existing, rt) {
				continue
			}
			return fmt.Errorf("runtime %q: pack %q (%s in %s) conflicts with declaration from pack %q (%s in %s)",
				rt.Name, rt.PackName, rt.Command, rt.PackDir, existing.PackName, existing.Command, existing.PackDir)
		}
		if cfg.Runtimes == nil {
			cfg.Runtimes = make(map[string]DiscoveredRuntime)
		}
		cfg.Runtimes[rt.Name] = rt
	}
	return nil
}

// sameRuntimeDeclaration reports whether two discovered runtimes are the same
// declaration reached more than once (a diamond import) rather than a conflict
// between distinct packs. Identity is the resolved pack DIRECTORY plus the
// resolved command and protocol; PackDir is compared by absolute path so a
// relative/absolute spelling of the same directory still dedupes.
func sameRuntimeDeclaration(a, b DiscoveredRuntime) bool {
	return samePackDir(a.PackDir, b.PackDir) &&
		a.Command == b.Command &&
		a.Protocol == b.Protocol
}

// samePackDir reports whether two pack directories resolve to the same absolute
// path, matching the normalization used by filterRuntimesByPackDir.
func samePackDir(a, b string) bool {
	absA, _ := filepath.Abs(a)
	absB, _ := filepath.Abs(b)
	return absA == absB
}

// filterRuntimesByPackDir keeps only runtimes declared directly by the
// pack at packDir — the non-transitive import surface.
func filterRuntimesByPackDir(runtimes []DiscoveredRuntime, packDir string) []DiscoveredRuntime {
	absPackDir, _ := filepath.Abs(packDir)
	var out []DiscoveredRuntime
	for _, rt := range runtimes {
		absDir, _ := filepath.Abs(rt.PackDir)
		if absDir == absPackDir {
			out = append(out, rt)
		}
	}
	return out
}

// cachedPackRuntimes returns the runtime declarations accumulated for a
// loaded pack directory (the pack's own plus its include/import closure).
func cachedPackRuntimes(cache *packLoadCache, topoDir string) []DiscoveredRuntime {
	if cache == nil {
		return nil
	}
	absDir, err := filepath.Abs(topoDir)
	if err != nil {
		absDir = topoDir
	}
	result, ok := cache.results[absDir]
	if !ok {
		return nil
	}
	return append([]DiscoveredRuntime(nil), result.runtimes...)
}
