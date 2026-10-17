package materialize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	// MCPProviderClaude projects to Claude Code's project-native MCP file.
	MCPProviderClaude = "claude"
	// MCPProviderCodex projects to Codex's project-native TOML config.
	MCPProviderCodex = "codex"
	// MCPProviderGemini projects to Gemini CLI's project-native settings file.
	MCPProviderGemini = "gemini"
	// MCPProviderAntigravity projects to Antigravity CLI's native MCP config file.
	MCPProviderAntigravity = "antigravity"
	// MCPProviderOpenCode projects to OpenCode's project-native JSON config.
	MCPProviderOpenCode = "opencode"
	// MCPProviderMimoCode projects to MiMo Code's project-native JSON config.
	// MiMo Code is an OpenCode fork: it honors a root-level
	// <workdir>/mimocode.json with the same `mcp` document schema (verified
	// live via `mimo mcp list` against a root-level config).
	MCPProviderMimoCode = "mimocode"
	// MCPProviderCursor projects to Cursor Agent's project-native MCP file.
	MCPProviderCursor = "cursor"
)

var userHomeDir = os.UserHomeDir

// SetUserHomeDirForTest overrides the home directory resolver during tests.
func SetUserHomeDirForTest(fn func() (string, error)) (restore func()) {
	prev := userHomeDir
	userHomeDir = fn
	return func() { userHomeDir = prev }
}

// MCPProjection is one provider-native MCP payload for a single target file.
type MCPProjection struct {
	Provider string
	Root     string
	Target   string
	Servers  []MCPServer
}

// BuildMCPProjection maps the neutral MCP catalog into one provider-native
// target file rooted at workdir. An empty server list still produces a valid
// projection so callers can reconcile stale managed config away.
func BuildMCPProjection(providerKind, workdir string, servers []MCPServer) (MCPProjection, error) {
	workdir = filepath.Clean(workdir)
	switch providerKind {
	case MCPProviderClaude:
	case MCPProviderCodex:
	case MCPProviderGemini:
	case MCPProviderAntigravity:
	case MCPProviderOpenCode:
	case MCPProviderMimoCode:
	case MCPProviderCursor:
	default:
		return MCPProjection{}, fmt.Errorf("unsupported MCP provider %q", providerKind)
	}

	out := MCPProjection{
		Provider: providerKind,
		Root:     workdir,
		Servers:  append([]MCPServer(nil), servers...),
	}
	sort.Slice(out.Servers, func(i, j int) bool { return out.Servers[i].Name < out.Servers[j].Name })

	switch providerKind {
	case MCPProviderClaude:
		out.Target = filepath.Join(workdir, ".mcp.json")
	case MCPProviderCodex:
		out.Target = filepath.Join(workdir, ".codex", "config.toml")
	case MCPProviderGemini:
		out.Target = filepath.Join(workdir, ".gemini", "settings.json")
	case MCPProviderAntigravity:
		home, err := userHomeDir()
		if err != nil {
			return MCPProjection{}, fmt.Errorf("resolving user home for antigravity MCP: %w", err)
		}
		out.Target = filepath.Join(home, ".gemini", "config", "mcp_config.json")
	case MCPProviderOpenCode:
		out.Target = filepath.Join(workdir, "opencode.json")
	case MCPProviderMimoCode:
		out.Target = filepath.Join(workdir, "mimocode.json")
	case MCPProviderCursor:
		out.Target = filepath.Join(workdir, ".cursor", "mcp.json")
	}
	return out, nil
}

// Hash returns the deterministic behavioral hash for the projected provider
// payload only. It intentionally excludes the target path and source metadata.
func (p MCPProjection) Hash() string {
	sum := sha256.Sum256(p.normalizedBytes())
	return hex.EncodeToString(sum[:])
}

// Apply reconciles the provider-native MCP target. A non-empty projection
// adopts the provider-native MCP surface on first write: GC snapshots the
// existing content to .gc/mcp-adopted/<provider>/<timestamp>.<ext>, emits a
// one-line stderr warning, then overwrites from the neutral catalog. The
// managed marker gates later cleanup when the effective catalog becomes
// empty so GC does not remove an unmanaged file it never adopted.
//
// Claude owns the whole file; Gemini, Codex, Cursor, OpenCode, and MiMo Code
// preserve unrelated config while replacing the MCP subtree.
//
// Apply is safe against concurrent writers for the same target: when the
// backing FS is the real OS filesystem, the read-validate-write sequence
// runs under an flock keyed by (provider, target). Concurrent supervisor
// ticks and stage-2 pre-start commands therefore serialize instead of
// overwriting each other's work.
//
// Symlinked target paths are rejected unconditionally — managed targets
// must be regular files or directories.
func (p MCPProjection) Apply(fs fsys.FS) error {
	return p.applyWithStderr(fs, adoptionStderr)
}

// ApplyWithStderr is identical to Apply but routes the one-time adoption
// warning to the caller-supplied writer. Callers that already plumb their
// own stderr sink (cmd surfaces, supervisor) prefer this entrypoint so
// warnings land in a deterministic place.
func (p MCPProjection) ApplyWithStderr(fs fsys.FS, stderr io.Writer) error {
	return p.applyWithStderr(fs, stderr)
}

func (p MCPProjection) applyWithStderr(fs fsys.FS, stderr io.Writer) error {
	if err := p.validateProviderContract(); err != nil {
		return err
	}
	if err := ensureNotSymlink(fs, p); err != nil {
		return err
	}
	// Short-circuit: nothing to do for an empty catalog on an unmanaged
	// target. Skipping early avoids taking an adoption snapshot of a
	// file we are not about to overwrite (the prior-review fix moved
	// snapshotting into Apply, but emitting a snapshot + warning with
	// no corresponding write violates the "snapshot only before first
	// overwrite" contract and lets .gc/mcp-adopted/ grow unbounded on
	// repeated stage-2 pre-starts that resolve to empty catalogs).
	if len(p.Servers) == 0 && !p.isManaged(fs) {
		return nil
	}
	lockRoot := ""
	if _, ok := fs.(fsys.OSFS); ok {
		lockRoot = lockRootForProjection(p)
	}
	return withTargetLock(lockRoot, p.Provider, p.Target, func() error {
		// Snapshot inside the lock so the backup reflects the exact
		// content about to be replaced — no TOCTOU against another
		// writer.
		if err := snapshotExistingIfUnmanaged(fs, p, nil, stderr); err != nil {
			return err
		}
		switch p.Provider {
		case MCPProviderClaude:
			return p.applyClaude(fs)
		case MCPProviderCodex:
			return p.applyCodex(fs)
		case MCPProviderGemini:
			return p.applyGemini(fs)
		case MCPProviderAntigravity:
			return p.applyAntigravity(fs)
		case MCPProviderOpenCode:
			return p.applyOpenCode(fs)
		case MCPProviderMimoCode:
			// MiMo Code is an OpenCode fork sharing the same `mcp` document
			// schema; only the target file name differs (set at build time).
			return p.applyOpenCode(fs)
		case MCPProviderCursor:
			return p.applyCursor(fs)
		default:
			return fmt.Errorf("unsupported MCP provider %q", p.Provider)
		}
	})
}

func (p MCPProjection) normalizedBytes() []byte {
	type normalizedProjection struct {
		Provider string                `json:"provider"`
		Servers  []NormalizedMCPServer `json:"servers"`
	}
	normalized := normalizedProjection{
		Provider: p.Provider,
		Servers:  make([]NormalizedMCPServer, 0, len(p.Servers)),
	}
	for _, server := range p.Servers {
		normalized.Servers = append(normalized.Servers, NormalizeMCPServer(server))
	}
	data, _ := json.Marshal(normalized)
	return data
}

func (p MCPProjection) applyClaude(fs fsys.FS) error {
	if len(p.Servers) == 0 {
		if !p.isManaged(fs) {
			return nil
		}
		if err := removeManagedMCPFile(fs, p.Target); err != nil {
			return err
		}
		return removeManagedMCPFile(fs, p.markerPath())
	}
	doc := map[string]any{
		"mcpServers": p.claudeServersDoc(),
	}
	data, err := marshalJSONDoc(doc)
	if err != nil {
		return err
	}
	if err := writeManagedMCPFile(fs, p.Target, data); err != nil {
		return err
	}
	return p.writeManagedMarker(fs)
}

func (p MCPProjection) applyGemini(fs fsys.FS) error {
	managed := p.isManaged(fs)
	if len(p.Servers) == 0 && !managed {
		return nil
	}
	doc, err := readJSONDoc(fs, p.Target)
	if err != nil {
		return err
	}
	if len(p.Servers) == 0 {
		delete(doc, "mcpServers")
		if len(doc) == 0 {
			if err := removeManagedMCPFile(fs, p.Target); err != nil {
				return err
			}
			return removeManagedMCPFile(fs, p.markerPath())
		}
	} else {
		doc["mcpServers"] = p.geminiServersDoc()
	}
	data, err := marshalJSONDoc(doc)
	if err != nil {
		return err
	}
	if err := writeManagedMCPFile(fs, p.Target, data); err != nil {
		return err
	}
	if len(p.Servers) == 0 {
		return removeManagedMCPFile(fs, p.markerPath())
	}
	return p.writeManagedMarker(fs)
}

func (p MCPProjection) applyAntigravity(fs fsys.FS) error {
	managed := p.isManaged(fs)
	if len(p.Servers) == 0 && !managed {
		return nil
	}
	doc, err := readJSONDoc(fs, p.Target)
	if err != nil {
		return err
	}
	mcpServers, _ := doc["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = make(map[string]any)
	}

	previouslyManaged := p.readManagedServers(fs)
	currentNames := make(map[string]bool, len(p.Servers))
	for _, s := range p.Servers {
		currentNames[s.Name] = true
	}
	for _, name := range previouslyManaged {
		if !currentNames[name] {
			delete(mcpServers, name)
		}
	}

	if len(p.Servers) == 0 {
		doc["mcpServers"] = mcpServers
		data, err := marshalJSONDoc(doc)
		if err != nil {
			return err
		}
		if err := writeManagedMCPFile(fs, p.Target, data); err != nil {
			return err
		}
		return removeManagedMCPFile(fs, p.markerPath())
	}

	newServers := p.antigravityServersDoc()
	for name, serverDoc := range newServers {
		mcpServers[name] = serverDoc
	}
	doc["mcpServers"] = mcpServers

	data, err := marshalJSONDoc(doc)
	if err != nil {
		return err
	}
	if err := writeManagedMCPFile(fs, p.Target, data); err != nil {
		return err
	}

	serverNames := make([]string, 0, len(p.Servers))
	for _, s := range p.Servers {
		serverNames = append(serverNames, s.Name)
	}
	sort.Strings(serverNames)
	return p.writeManagedMarkerWithServers(fs, serverNames)
}

func (p MCPProjection) antigravityServersDoc() map[string]any {
	out := make(map[string]any, len(p.Servers))
	for _, server := range p.Servers {
		entry := map[string]any{}
		switch server.Transport {
		case MCPTransportStdio:
			entry["command"] = server.Command
			if len(server.Args) > 0 {
				entry["args"] = append([]string(nil), server.Args...)
			}
			if len(server.Env) > 0 {
				entry["env"] = cloneStringMap(server.Env)
			}
		case MCPTransportHTTP, MCPTransportSSE:
			entry["serverUrl"] = server.URL
			if len(server.Headers) > 0 {
				entry["headers"] = cloneStringMap(server.Headers)
			}
		}
		out[server.Name] = entry
	}
	return out
}

func (p MCPProjection) applyCodex(fs fsys.FS) error {
	managed := p.isManaged(fs)
	if len(p.Servers) == 0 && !managed {
		return nil
	}
	doc, err := readTOMLDoc(fs, p.Target)
	if err != nil {
		return err
	}
	if len(p.Servers) == 0 {
		delete(doc, "mcp_servers")
		if len(doc) == 0 {
			if err := removeManagedMCPFile(fs, p.Target); err != nil {
				return err
			}
			return removeManagedMCPFile(fs, p.markerPath())
		}
	} else {
		doc["mcp_servers"] = p.codexServersDoc()
	}
	data, err := marshalTOMLDoc(doc)
	if err != nil {
		return err
	}
	if err := writeManagedMCPFile(fs, p.Target, data); err != nil {
		return err
	}
	if len(p.Servers) == 0 {
		return removeManagedMCPFile(fs, p.markerPath())
	}
	return p.writeManagedMarker(fs)
}

func (p MCPProjection) applyOpenCode(fs fsys.FS) error {
	managed := p.isManaged(fs)
	if len(p.Servers) == 0 && !managed {
		return nil
	}
	doc, err := readJSONDoc(fs, p.Target)
	if err != nil {
		return err
	}
	if len(p.Servers) == 0 {
		delete(doc, "mcp")
		if len(doc) == 0 {
			if err := removeManagedMCPFile(fs, p.Target); err != nil {
				return err
			}
			return removeManagedMCPFile(fs, p.markerPath())
		}
	} else {
		doc["mcp"] = p.opencodeServersDoc()
	}
	data, err := marshalJSONDoc(doc)
	if err != nil {
		return err
	}
	if err := writeManagedMCPFile(fs, p.Target, data); err != nil {
		return err
	}
	if len(p.Servers) == 0 {
		return removeManagedMCPFile(fs, p.markerPath())
	}
	return p.writeManagedMarker(fs)
}

func (p MCPProjection) applyCursor(fs fsys.FS) error {
	managed := p.isManaged(fs)
	if len(p.Servers) == 0 && !managed {
		return nil
	}
	doc, err := readJSONDoc(fs, p.Target)
	if err != nil {
		return err
	}
	if len(p.Servers) == 0 {
		delete(doc, "mcpServers")
		if len(doc) == 0 {
			if err := removeManagedMCPFile(fs, p.Target); err != nil {
				return err
			}
			return removeManagedMCPFile(fs, p.markerPath())
		}
	} else {
		doc["mcpServers"] = p.cursorServersDoc()
	}
	data, err := marshalJSONDoc(doc)
	if err != nil {
		return err
	}
	if err := writeManagedMCPFile(fs, p.Target, data); err != nil {
		return err
	}
	if len(p.Servers) == 0 {
		return removeManagedMCPFile(fs, p.markerPath())
	}
	return p.writeManagedMarker(fs)
}

func (p MCPProjection) validateProviderContract() error {
	if p.Provider != MCPProviderCursor {
		return nil
	}
	for _, server := range p.Servers {
		switch server.Transport {
		case MCPTransportStdio:
		case MCPTransportHTTP, MCPTransportSSE:
			return fmt.Errorf(
				"cursor MCP projection does not support %s server %q: Cursor's HTTP/SSE mcpServers wire contract is not verified",
				server.Transport,
				server.Name,
			)
		}
	}
	return nil
}

func (p MCPProjection) claudeServersDoc() map[string]any {
	out := make(map[string]any, len(p.Servers))
	for _, server := range p.Servers {
		entry := map[string]any{}
		switch server.Transport {
		case MCPTransportStdio:
			entry["command"] = server.Command
			if len(server.Args) > 0 {
				entry["args"] = append([]string(nil), server.Args...)
			}
			if len(server.Env) > 0 {
				entry["env"] = cloneStringMap(server.Env)
			}
		case MCPTransportHTTP:
			entry["type"] = "http"
			entry["url"] = server.URL
			if len(server.Headers) > 0 {
				entry["headers"] = cloneStringMap(server.Headers)
			}
		}
		out[server.Name] = entry
	}
	return out
}

func (p MCPProjection) cursorServersDoc() map[string]any {
	out := make(map[string]any, len(p.Servers))
	for _, server := range p.Servers {
		entry := map[string]any{}
		if server.Transport == MCPTransportStdio {
			entry["command"] = server.Command
			if len(server.Args) > 0 {
				entry["args"] = append([]string(nil), server.Args...)
			}
			if len(server.Env) > 0 {
				entry["env"] = cloneStringMap(server.Env)
			}
		}
		out[server.Name] = entry
	}
	return out
}

func (p MCPProjection) geminiServersDoc() map[string]any {
	out := make(map[string]any, len(p.Servers))
	for _, server := range p.Servers {
		entry := map[string]any{}
		switch server.Transport {
		case MCPTransportStdio:
			entry["command"] = server.Command
			if len(server.Args) > 0 {
				entry["args"] = append([]string(nil), server.Args...)
			}
			if len(server.Env) > 0 {
				entry["env"] = cloneStringMap(server.Env)
			}
		case MCPTransportHTTP:
			entry["httpUrl"] = server.URL
			if len(server.Headers) > 0 {
				entry["headers"] = cloneStringMap(server.Headers)
			}
		}
		out[server.Name] = entry
	}
	return out
}

func (p MCPProjection) codexServersDoc() map[string]any {
	out := make(map[string]any, len(p.Servers))
	for _, server := range p.Servers {
		entry := map[string]any{}
		switch server.Transport {
		case MCPTransportStdio:
			entry["command"] = server.Command
			if len(server.Args) > 0 {
				entry["args"] = append([]string(nil), server.Args...)
			}
			if len(server.Env) > 0 {
				entry["env"] = cloneStringMap(server.Env)
			}
		case MCPTransportHTTP:
			entry["url"] = server.URL
			if len(server.Headers) > 0 {
				entry["http_headers"] = cloneStringMap(server.Headers)
			}
		}
		out[server.Name] = entry
	}
	return out
}

func (p MCPProjection) opencodeServersDoc() map[string]any {
	out := make(map[string]any, len(p.Servers))
	for _, server := range p.Servers {
		entry := map[string]any{"enabled": true}
		switch server.Transport {
		case MCPTransportStdio:
			entry["type"] = "local"
			command := make([]string, 0, 1+len(server.Args))
			if server.Command != "" {
				command = append(command, server.Command)
			}
			command = append(command, server.Args...)
			entry["command"] = command
			if len(server.Env) > 0 {
				entry["environment"] = cloneStringMap(server.Env)
			}
		case MCPTransportHTTP:
			entry["type"] = "remote"
			entry["url"] = server.URL
			if len(server.Headers) > 0 {
				entry["headers"] = cloneStringMap(server.Headers)
			}
		}
		out[server.Name] = entry
	}
	return out
}

func readJSONDoc(fs fsys.FS, path string) (map[string]any, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		if errorsIsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func readTOMLDoc(fs fsys.FS, path string) (map[string]any, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		if errorsIsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc map[string]any
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func marshalJSONDoc(doc map[string]any) ([]byte, error) {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling MCP JSON: %w", err)
	}
	data = append(data, '\n')
	return data, nil
}

func marshalTOMLDoc(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return nil, fmt.Errorf("marshaling MCP TOML: %w", err)
	}
	data := buf.Bytes()
	if len(data) > 0 && !bytes.HasSuffix(data, []byte{'\n'}) {
		data = append(data, '\n')
	}
	return data, nil
}

func writeManagedMCPFile(fs fsys.FS, path string, data []byte) error {
	if err := fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	// WriteFileAtomic chmods the temp file pre-rename, so the final path is
	// never briefly group/world-readable. No post-rename chmod needed.
	if err := fsys.WriteFileAtomic(fs, path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func removeManagedMCPFile(fs fsys.FS, path string) error {
	if err := fs.Remove(path); err != nil && !errorsIsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

func (p MCPProjection) markerPath() string {
	return filepath.Join(p.Root, ".gc", "mcp-managed", p.Provider+".json")
}

func (p MCPProjection) isManaged(fs fsys.FS) bool {
	_, err := fs.Stat(p.markerPath())
	return err == nil
}

type managedMarkerPayload struct {
	ManagedBy string   `json:"managed_by"`
	Provider  string   `json:"provider"`
	Servers   []string `json:"servers,omitempty"`
}

func (p MCPProjection) readManagedServers(fs fsys.FS) []string {
	data, err := fs.ReadFile(p.markerPath())
	if err != nil {
		return nil
	}
	var marker managedMarkerPayload
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil
	}
	return marker.Servers
}

func (p MCPProjection) writeManagedMarker(fs fsys.FS) error {
	return p.writeManagedMarkerWithServers(fs, nil)
}

func (p MCPProjection) writeManagedMarkerWithServers(fs fsys.FS, serverNames []string) error {
	if err := fs.MkdirAll(filepath.Dir(p.markerPath()), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(p.markerPath()), err)
	}
	data, err := json.Marshal(managedMarkerPayload{
		ManagedBy: "gc",
		Provider:  p.Provider,
		Servers:   serverNames,
	})
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", p.markerPath(), err)
	}
	data = append(data, '\n')
	if err := fsys.WriteFileAtomic(fs, p.markerPath(), data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", p.markerPath(), err)
	}
	return nil
}

func errorsIsNotExist(err error) bool {
	return err != nil && (os.IsNotExist(err) || errors.Is(err, iofs.ErrNotExist))
}

// FindMCPServerConflict checks whether any server with the same name in a and b
// has differing behavioral definitions. Returns an error message describing the
// conflict, or "" if there is no conflict.
func FindMCPServerConflict(a, b []MCPServer) string {
	aByName := make(map[string]MCPServer, len(a))
	for _, s := range a {
		aByName[s.Name] = s
	}
	for _, sB := range b {
		sA, ok := aByName[sB.Name]
		if !ok {
			continue
		}
		normA := NormalizeMCPServer(sA)
		normB := NormalizeMCPServer(sB)
		dataA, _ := json.Marshal(normA)
		dataB, _ := json.Marshal(normB)
		if string(dataA) != string(dataB) {
			return fmt.Sprintf("server %q has conflicting definitions", sB.Name)
		}
	}
	return ""
}

// MergeMCPServers merges two server slices by server name, sorting the result.
func MergeMCPServers(a, b []MCPServer) []MCPServer {
	byName := make(map[string]MCPServer, len(a)+len(b))
	for _, s := range a {
		byName[s.Name] = s
	}
	for _, s := range b {
		byName[s.Name] = s
	}
	out := make([]MCPServer, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
