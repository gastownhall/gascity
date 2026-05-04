package materialize

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/BurntSushi/toml"
)

var validMCPServerName = regexp.MustCompile(`^[a-z0-9-]+$`)

// MCPTransport identifies the neutral transport type supported by the MCP
// catalog and provider projections.
type MCPTransport string

const (
	// MCPTransportStdio is a stdio-launched MCP server.
	MCPTransportStdio MCPTransport = "stdio"
	// MCPTransportHTTP is a streamable HTTP MCP server.
	MCPTransportHTTP MCPTransport = "http"
	// MCPTransportSSE is an SSE-connected MCP server.
	MCPTransportSSE MCPTransport = "sse"
)

// MCPServer is the canonical neutral MCP model after parsing,
// template expansion, transport validation, and relative path
// resolution.
type MCPServer struct {
	Name        string
	Description string
	Transport   MCPTransport
	Command     string
	Args        []string
	Env         map[string]string
	URL         string
	Headers     map[string]string
	SourceFile  string
	SourceDir   string
	Template    bool
	Layer       string
	Origin      string
}

// MCPShadow records a same-name replacement across precedence layers.
type MCPShadow struct {
	Name       string
	Winner     string
	Loser      string
	WinnerFile string
	LoserFile  string
}

// MCPCatalog is a precedence-resolved MCP server set.
type MCPCatalog struct {
	Servers  []MCPServer
	Shadows  []MCPShadow
	ByName   map[string]MCPServer
	ByLayer  map[string][]MCPServer
	RawOrder []string
}

// MCPKV is a canonical map entry used for deterministic equality and hashing.
type MCPKV struct {
	Key   string
	Value string
}

// NormalizedMCPServer is the deterministic behavioral form used for equality
// and drift hashing. Metadata and source provenance are intentionally excluded.
type NormalizedMCPServer struct {
	Name      string
	Transport MCPTransport
	Command   string
	Args      []string
	Env       []MCPKV
	URL       string
	Headers   []MCPKV
}

type rawMCPServer struct {
	Name        string            `toml:"name"`
	Description string            `toml:"description"`
	Command     string            `toml:"command"`
	Args        []string          `toml:"args"`
	Env         map[string]string `toml:"env"`
	URL         string            `toml:"url"`
	Headers     map[string]string `toml:"headers"`
}

// MCPDirSource identifies one directory contributing MCP definitions.
// Sources are merged in the order supplied: later entries win.
type MCPDirSource struct {
	Dir    string
	Label  string
	Origin string
}

// MCPIdentityForFilename returns the logical server name for a supported MCP
// filename. Supported names are "<name>.toml" and "<name>.template.toml".
func MCPIdentityForFilename(name string) (string, bool) {
	switch {
	case strings.HasSuffix(name, ".template.toml"):
		return strings.TrimSuffix(name, ".template.toml"), true
	case strings.HasSuffix(name, ".toml"):
		return strings.TrimSuffix(name, ".toml"), true
	default:
		return "", false
	}
}

// LoadMCPDir parses every MCP definition in dir. Hidden files and unsupported
// extensions are ignored. Duplicate logical names within one directory are a
// hard error.
func LoadMCPDir(dir, label string, templateData map[string]string) ([]MCPServer, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading mcp dir %q: %w", dir, err)
	}

	seen := make(map[string]string, len(entries))
	servers := make([]MCPServer, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		identity, ok := MCPIdentityForFilename(name)
		if !ok {
			continue
		}
		if prev, dup := seen[identity]; dup {
			return nil, fmt.Errorf("mcp directory %q: duplicate logical server %q via %q and %q", dir, identity, prev, name)
		}
		seen[identity] = name

		server, err := loadMCPFile(filepath.Join(dir, name), label, templateData)
		if err != nil {
			return nil, err
		}
		if label != "" {
			server.Origin = label
		}
		servers = append(servers, server)
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return servers, nil
}

// MergeMCPDirs loads and overlays MCP definitions from low to high precedence.
// Later directories win on same-name collisions.
func MergeMCPDirs(sources []MCPDirSource, templateData map[string]string) (MCPCatalog, error) {
	out := MCPCatalog{
		ByName:  make(map[string]MCPServer),
		ByLayer: make(map[string][]MCPServer),
	}
	for _, source := range sources {
		servers, err := LoadMCPDir(source.Dir, source.Label, templateData)
		if err != nil {
			return MCPCatalog{}, err
		}
		for i := range servers {
			if strings.TrimSpace(source.Origin) != "" {
				servers[i].Origin = source.Origin
			}
		}
		for _, server := range servers {
			if prior, exists := out.ByName[server.Name]; exists {
				out.Shadows = append(out.Shadows, MCPShadow{
					Name:       server.Name,
					Winner:     shadowOrigin(server),
					Loser:      shadowOrigin(prior),
					WinnerFile: server.SourceFile,
					LoserFile:  prior.SourceFile,
				})
			}
			out.ByName[server.Name] = server
			out.RawOrder = append(out.RawOrder, server.Name)
		}
	}

	names := make([]string, 0, len(out.ByName))
	for name := range out.ByName {
		names = append(names, name)
	}
	sort.Strings(names)
	out.Servers = make([]MCPServer, 0, len(names))
	for _, name := range names {
		server := out.ByName[name]
		out.Servers = append(out.Servers, server)
		out.ByLayer[server.Layer] = append(out.ByLayer[server.Layer], server)
	}
	sort.Slice(out.Shadows, func(i, j int) bool {
		if out.Shadows[i].Name != out.Shadows[j].Name {
			return out.Shadows[i].Name < out.Shadows[j].Name
		}
		if out.Shadows[i].Winner != out.Shadows[j].Winner {
			return out.Shadows[i].Winner < out.Shadows[j].Winner
		}
		return out.Shadows[i].Loser < out.Shadows[j].Loser
	})
	return out, nil
}

// NormalizeMCPServer returns the deterministic behavioral representation used
// for equality and hashing.
func NormalizeMCPServer(server MCPServer) NormalizedMCPServer {
	n := NormalizedMCPServer{
		Name:      server.Name,
		Transport: server.Transport,
		Command:   server.Command,
		URL:       server.URL,
	}
	if len(server.Args) > 0 {
		n.Args = append([]string(nil), server.Args...)
	}
	if len(server.Env) > 0 {
		n.Env = normalizeMCPMap(server.Env)
	}
	if len(server.Headers) > 0 {
		n.Headers = normalizeMCPMap(server.Headers)
	}
	return n
}

func shadowOrigin(server MCPServer) string {
	if strings.TrimSpace(server.Origin) != "" {
		return server.Origin
	}
	return server.Layer
}

func loadMCPFile(path, label string, templateData map[string]string) (MCPServer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MCPServer{}, fmt.Errorf("reading %s: %w", path, err)
	}

	isTemplate := strings.HasSuffix(path, ".template.toml")
	if isTemplate {
		data, err = expandMCPTemplate(data, templateData)
		if err != nil {
			return MCPServer{}, fmt.Errorf("expanding %s: %w", path, err)
		}
	}

	var raw rawMCPServer
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return MCPServer{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	identity, ok := MCPIdentityForFilename(filepath.Base(path))
	if !ok {
		return MCPServer{}, fmt.Errorf("unsupported MCP filename %q", path)
	}
	if strings.TrimSpace(raw.Name) != identity {
		return MCPServer{}, fmt.Errorf("%s: name %q must match filename stem %q", path, raw.Name, identity)
	}
	if !validMCPServerName.MatchString(identity) {
		return MCPServer{}, fmt.Errorf("%s: invalid server name %q", path, identity)
	}

	command := strings.TrimSpace(raw.Command)
	url := strings.TrimSpace(raw.URL)
	switch {
	case command == "" && url == "":
		return MCPServer{}, fmt.Errorf("%s: exactly one of command or url must be set", path)
	case command != "" && url != "":
		return MCPServer{}, fmt.Errorf("%s: command and url are mutually exclusive", path)
	}

	server := MCPServer{
		Name:        identity,
		Description: strings.TrimSpace(raw.Description),
		Args:        append([]string(nil), raw.Args...),
		Env:         cloneStringMap(raw.Env),
		Headers:     cloneStringMap(raw.Headers),
		SourceFile:  path,
		SourceDir:   filepath.Dir(path),
		Template:    isTemplate,
		Layer:       label,
		Origin:      label,
	}
	if command != "" {
		if len(raw.Headers) > 0 {
			return MCPServer{}, fmt.Errorf("%s: stdio server may not set headers", path)
		}
		server.Transport = MCPTransportStdio
		server.Command = resolveMCPCommand(command, filepath.Dir(path))
	} else {
		if len(raw.Args) > 0 || len(raw.Env) > 0 {
			return MCPServer{}, fmt.Errorf("%s: http server may not set args or env", path)
		}
		server.Transport = MCPTransportHTTP
		server.URL = url
	}
	return server, nil
}

func expandMCPTemplate(data []byte, templateData map[string]string) ([]byte, error) {
	tmpl, err := template.New("mcp").Option("missingkey=error").Parse(string(data))
	if err != nil {
		return nil, err
	}
	if templateData == nil {
		templateData = map[string]string{}
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, templateData); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func resolveMCPCommand(command, dir string) string {
	if strings.TrimSpace(command) == "" {
		return ""
	}
	if filepath.IsAbs(command) {
		return filepath.Clean(command)
	}
	if !strings.ContainsAny(command, `/\`) {
		return command
	}
	if abs, err := filepath.Abs(filepath.Join(dir, command)); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(filepath.Join(dir, command))
}

func normalizeMCPMap(in map[string]string) []MCPKV {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]MCPKV, 0, len(keys))
	for _, key := range keys {
		out = append(out, MCPKV{Key: key, Value: in[key]})
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
