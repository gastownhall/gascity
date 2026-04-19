package materialize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	MCPProviderClaude = "claude"
	MCPProviderCodex  = "codex"
	MCPProviderGemini = "gemini"
)

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
// intentionally adopts the provider-native MCP surface immediately: once an
// agent/workdir has effective MCP, GC overwrites the provider's MCP target from
// the neutral source of truth. The managed marker only gates later cleanup when
// the effective catalog becomes empty, so GC does not remove an unmanaged file
// it never adopted.
//
// Claude owns the whole file; Gemini and Codex preserve unrelated config while
// replacing the MCP subtree.
func (p MCPProjection) Apply(fs fsys.FS) error {
	switch p.Provider {
	case MCPProviderClaude:
		return p.applyClaude(fs)
	case MCPProviderCodex:
		return p.applyCodex(fs)
	case MCPProviderGemini:
		return p.applyGemini(fs)
	default:
		return fmt.Errorf("unsupported MCP provider %q", p.Provider)
	}
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
	if err := fsys.WriteFileAtomic(fs, path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := fs.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
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

func (p MCPProjection) writeManagedMarker(fs fsys.FS) error {
	if err := fs.MkdirAll(filepath.Dir(p.markerPath()), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(p.markerPath()), err)
	}
	data, err := json.Marshal(map[string]string{
		"managed_by": "gc",
		"provider":   p.Provider,
	})
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", p.markerPath(), err)
	}
	data = append(data, '\n')
	if err := fsys.WriteFileAtomic(fs, p.markerPath(), data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", p.markerPath(), err)
	}
	if err := fs.Chmod(p.markerPath(), 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", p.markerPath(), err)
	}
	return nil
}

func errorsIsNotExist(err error) bool {
	return err != nil && (os.IsNotExist(err) || errors.Is(err, iofs.ErrNotExist))
}
