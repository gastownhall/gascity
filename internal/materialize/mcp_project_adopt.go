package materialize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

// MCPAdoptionRefusedError reports a provider-native MCP file gc will not
// adopt. gc does not manage the file yet, and adopting it would drop content
// gc does not manage: MCP servers the projection does not carry, the file's
// own definition of a server the projection defines differently, or, for
// Claude's .mcp.json, which gc owns whole, any other top-level key. Those
// are typically a repository's own servers, committed for everyone who works
// in it. gc also leaves alone a file it cannot read or parse, and a symlinked
// one, since it writes MCP config only to regular files.
//
// Apply leaves the file as it is and returns this error. Callers report it
// and carry on: the agents that read the file keep its own servers and start
// without gc's.
type MCPAdoptionRefusedError struct {
	Provider string
	Target   string
	// Servers lists the file's MCP servers that the projection does not
	// carry, sorted.
	Servers []string
	// Redefined lists the file's MCP servers that the projection carries
	// under the same name but defines differently, sorted.
	Redefined []string
	// Keys lists other top-level keys adopting the file would drop, sorted:
	// for Claude's whole-file target, every key besides its servers, and
	// for any provider, an MCP key whose value is not a table of servers.
	Keys []string
	// ReadErr is set instead when gc cannot read the file.
	ReadErr error
	// ParseErr is set instead when gc cannot parse the file.
	ParseErr error
	// Symlink is set instead when the target, or the provider directory
	// that holds it, is a symlink: it names that path.
	Symlink string
}

func (e *MCPAdoptionRefusedError) Error() string {
	return "not adopting " + e.Target + ": " + e.Reason()
}

// Reason says why gc does not adopt the file, without naming it.
func (e *MCPAdoptionRefusedError) Reason() string {
	switch {
	case e.Symlink != "":
		return fmt.Sprintf("%s is a symlink, and gc writes MCP config only to regular files", e.Symlink)
	case e.ReadErr != nil:
		return fmt.Sprintf("gc cannot read it (%v)", e.ReadErr)
	case e.ParseErr != nil:
		return fmt.Sprintf("gc cannot parse it (%v), so adopting it could drop config of your own", e.ParseErr)
	}
	return "gc does not manage it yet, and adopting it would drop " + e.dropped()
}

// Remedy says how to let gc manage the file.
func (e *MCPAdoptionRefusedError) Remedy() string {
	switch {
	case e.Symlink != "":
		return "to let gc manage it, replace the symlink with a regular file or directory"
	case e.ReadErr != nil:
		return "to let gc manage it, make it a readable file or remove it"
	case e.ParseErr != nil && e.Provider == MCPProviderCodex:
		return "to let gc manage it, fix its TOML or remove it"
	case e.ParseErr != nil:
		return "to let gc manage it, make it plain JSON (gc does not read comments or trailing commas) or remove it"
	case len(e.Servers) == 0 && len(e.Redefined) == 0:
		return "to let gc manage it, remove those entries from the file"
	}
	return "to let gc manage it, define the servers you still need in gc's MCP catalog " +
		"(an mcp/ directory) and remove those entries from the file"
}

// dropped renders what adopting the file would drop: `MCP server "a"`,
// `its own definition of MCP server "b"`, `top-level key "c"`.
func (e *MCPAdoptionRefusedError) dropped() string {
	var parts []string
	if len(e.Servers) > 0 {
		parts = append(parts, quotedList("MCP server", e.Servers))
	}
	if len(e.Redefined) > 0 {
		parts = append(parts, "its own definition of "+quotedList("MCP server", e.Redefined))
	}
	if len(e.Keys) > 0 {
		parts = append(parts, quotedList("top-level key", e.Keys))
	}
	switch len(parts) {
	case 0:
		return "nothing"
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}

// quotedList renders `<noun> "a"` or `<noun>s "a", "b"`.
func quotedList(noun string, items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = strconv.Quote(item)
	}
	if len(quoted) == 1 {
		return noun + " " + quoted[0]
	}
	return noun + "s " + strings.Join(quoted, ", ")
}

// AdoptionRefusal reports whether Apply would refuse to adopt p's target,
// and why. It reads the target and writes nothing. It returns nil when p has
// nothing to project, gc already manages the target, the target does not
// exist, or adopting it would drop nothing gc does not manage.
func (p MCPProjection) AdoptionRefusal(fs fsys.FS) (*MCPAdoptionRefusedError, error) {
	return p.adoptionConflict(fs)
}

// mcpServersKey is the document key each provider keeps its MCP servers
// under.
func mcpServersKey(provider string) string {
	switch provider {
	case MCPProviderCodex:
		return "mcp_servers"
	case MCPProviderOpenCode, MCPProviderMimoCode:
		return "mcp"
	default:
		return "mcpServers"
	}
}

// parseMCPDocument decodes a provider-native MCP file. Empty or
// whitespace-only content, and a JSON null, decode to an empty document.
func parseMCPDocument(provider string, data []byte) (map[string]any, error) {
	if provider == MCPProviderCodex {
		return parseTOMLDocument(data)
	}
	return parseJSONDocument(data)
}

func parseJSONDocument(data []byte) (map[string]any, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func parseTOMLDocument(data []byte) (map[string]any, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return doc, nil
	}
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// mcpServerNames returns the server names in a decoded provider document, and
// whether its MCP subtree has the expected shape. An absent or null subtree
// holds no servers.
func mcpServerNames(provider string, doc map[string]any) ([]string, bool) {
	switch servers := doc[mcpServersKey(provider)].(type) {
	case nil:
		return nil, true
	case map[string]any:
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		sort.Strings(names)
		return names, true
	default:
		return nil, false
	}
}

// adoptionConflict reports what adopting p's target would drop; see
// AdoptionRefusal.
func (p MCPProjection) adoptionConflict(fs fsys.FS) (*MCPAdoptionRefusedError, error) {
	if len(p.Servers) == 0 || p.isManaged(fs) {
		return nil, nil
	}
	link, err := symlinkedPath(fs, p)
	if err != nil {
		return nil, err
	}
	if link != "" {
		return &MCPAdoptionRefusedError{Provider: p.Provider, Target: p.Target, Symlink: link}, nil
	}
	data, err := fs.ReadFile(p.Target)
	if err != nil {
		if errorsIsNotExist(err) {
			return nil, nil
		}
		return &MCPAdoptionRefusedError{Provider: p.Provider, Target: p.Target, ReadErr: err}, nil
	}
	return p.adoptionConflictIn(data)
}

// adoptionConflictIn is adoptionConflict for an existing, unmanaged target
// holding data. A server p carries under the same name, defined the same
// way, is not in the way: adopting the file keeps it as it is.
func (p MCPProjection) adoptionConflictIn(data []byte) (*MCPAdoptionRefusedError, error) {
	refused := &MCPAdoptionRefusedError{Provider: p.Provider, Target: p.Target}
	doc, err := parseMCPDocument(p.Provider, data)
	if err != nil {
		refused.ParseErr = err
		return refused, nil
	}
	key, rendered, err := p.serversDoc()
	if err != nil {
		return nil, err
	}
	switch servers := doc[key].(type) {
	case nil:
	case map[string]any:
		for name, entry := range servers {
			want, ok := rendered[name]
			switch {
			case !ok:
				refused.Servers = append(refused.Servers, name)
			case !p.sameMCPEntry(entry, want):
				refused.Redefined = append(refused.Redefined, name)
			}
		}
	default:
		refused.Keys = append(refused.Keys, key)
	}
	if p.Provider == MCPProviderClaude {
		// Claude's file is gc's whole: anything beside the servers would go.
		for k := range doc {
			if k != key {
				refused.Keys = append(refused.Keys, k)
			}
		}
	}
	if len(refused.Servers) == 0 && len(refused.Redefined) == 0 && len(refused.Keys) == 0 {
		return nil, nil
	}
	sort.Strings(refused.Servers)
	sort.Strings(refused.Redefined)
	sort.Strings(refused.Keys)
	return refused, nil
}

// sameMCPEntry reports whether a server entry decoded from p's provider file
// means what the entry gc writes for it means. Both are compared as JSON
// values, with what gc leaves out anyway left out of both: nulls, empty
// lists and tables, and a provider's defaults written out (Claude's and
// Cursor's "type": "stdio", Codex's, OpenCode's and MiMo Code's
// "enabled": true). Claude's "streamable-http" is its alias for "http".
func (p MCPProjection) sameMCPEntry(have, want any) bool {
	a, errA := p.normalizedMCPEntry(have)
	b, errB := p.normalizedMCPEntry(want)
	return errA == nil && errB == nil && reflect.DeepEqual(a, b)
}

func (p MCPProjection) normalizedMCPEntry(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	out = dropEmptyMCPValues(out)
	entry, ok := out.(map[string]any)
	if !ok {
		return out, nil
	}
	switch p.Provider {
	case MCPProviderClaude, MCPProviderCursor:
		switch entry["type"] {
		case "stdio":
			delete(entry, "type")
		case "streamable-http":
			if p.Provider == MCPProviderClaude {
				entry["type"] = "http"
			}
		}
	case MCPProviderCodex, MCPProviderOpenCode, MCPProviderMimoCode:
		if enabled, ok := entry["enabled"].(bool); ok && enabled {
			delete(entry, "enabled")
		}
	}
	return entry, nil
}

// dropEmptyMCPValues removes nulls and empty lists and tables, at any depth.
// An empty string is kept: an env var set to "" is a setting of its own.
func dropEmptyMCPValues(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			val = dropEmptyMCPValues(val)
			if isEmptyMCPValue(val) {
				continue
			}
			out[k] = val
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = dropEmptyMCPValues(val)
		}
		return out
	default:
		return v
	}
}

func isEmptyMCPValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	default:
		return false
	}
}

// DroppedAtAdoption lists the MCP servers the target held when gc adopted it
// that the projection does not carry: servers the adoption replaced and the
// catalog has not taken over since. Their definitions survive only in the
// adoption snapshots under the returned directory. A server the catalog now
// defines under the same name is not listed. It reads the snapshots and
// writes nothing; a snapshot gc cannot parse is skipped.
func (p MCPProjection) DroppedAtAdoption(fs fsys.FS) ([]string, string, error) {
	dir := p.adoptionSnapshotDir()
	if !p.isManaged(fs) {
		return nil, dir, nil
	}
	entries, err := fs.ReadDir(dir)
	if err != nil {
		if errorsIsNotExist(err) {
			return nil, dir, nil
		}
		return nil, dir, fmt.Errorf("reading %s: %w", dir, err)
	}
	projected := make(map[string]bool, len(p.Servers))
	for _, server := range p.Servers {
		projected[server.Name] = true
	}
	dropped := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := fs.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, dir, fmt.Errorf("reading adoption snapshot %s: %w", filepath.Join(dir, entry.Name()), err)
		}
		doc, err := parseMCPDocument(p.Provider, data)
		if err != nil {
			continue
		}
		names, _ := mcpServerNames(p.Provider, doc)
		for _, name := range names {
			if !projected[name] {
				dropped[name] = true
			}
		}
	}
	out := make([]string, 0, len(dropped))
	for name := range dropped {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, dir, nil
}
