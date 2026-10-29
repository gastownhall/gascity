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
// Apply does not adopt a file that holds content gc does not manage: MCP
// servers the projection does not carry, the file's own definition of a
// server the projection defines differently, or, for Claude's whole-file
// target, any other top-level key; nor a file it cannot parse, nor a
// symlinked one. It returns *MCPAdoptionRefusedError and writes no target,
// marker, or snapshot. Once gc manages a target, it owns its MCP servers,
// and out-of-band edits to them are drift it reverts.
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
// A target gc manages must be a regular file in a regular directory: a
// symlinked one is an error. An empty catalog on a target gc does not
// manage is a no-op, whatever the target is.
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

// beforeMCPTargetLock, when set, runs after Apply's unlocked adoption check
// and before it takes the target lock. Tests use it to change the target in
// between.
var beforeMCPTargetLock func()

func (p MCPProjection) applyWithStderr(fs fsys.FS, stderr io.Writer) error {
	if err := p.validateProviderContract(); err != nil {
		return err
	}
	// Short-circuit: nothing to do for an empty catalog on an unmanaged
	// target, which is someone else's file, whatever it is. Skipping early
	// avoids taking an adoption snapshot of a file we are not about to
	// overwrite (the prior-review fix moved snapshotting into Apply, but
	// emitting a snapshot + warning with no corresponding write violates
	// the "snapshot only before first overwrite" contract and lets
	// .gc/mcp-adopted/ grow unbounded on repeated stage-2 pre-starts that
	// resolve to empty catalogs).
	if len(p.Servers) == 0 && !p.isManaged(fs) {
		return nil
	}
	// A refused adoption writes nothing, not even the lock file, so check
	// before taking the lock. The check repeats under the lock, on the
	// content the snapshot saves.
	if refused, err := p.adoptionConflict(fs); err != nil || refused != nil {
		return refusalOrErr(refused, err)
	}
	if err := ensureNotSymlink(fs, p); err != nil {
		return err
	}
	if beforeMCPTargetLock != nil {
		beforeMCPTargetLock()
	}
	lockRoot := ""
	if _, ok := fs.(fsys.OSFS); ok {
		lockRoot = lockRootForProjection(p)
	}
	return withTargetLock(lockRoot, p.Provider, p.Target, func() error {
		// The target may have changed since the checks above; check again
		// under the lock, on the content this pass reads, snapshots, and
		// rewrites.
		managed := p.isManaged(fs)
		if !managed && len(p.Servers) == 0 {
			return nil
		}
		adopting := !managed
		link, err := symlinkedPath(fs, p)
		if err != nil {
			return err
		}
		if link != "" {
			if adopting {
				return &MCPAdoptionRefusedError{Provider: p.Provider, Target: p.Target, Symlink: link}
			}
			return symlinkError(link)
		}
		var current mcpTargetContent
		// Claude's file is gc's whole, so a target gc manages is rewritten
		// without being read.
		if adopting || p.Provider != MCPProviderClaude {
			current, err = readMCPTarget(fs, p.Target)
			if err != nil {
				if adopting {
					return &MCPAdoptionRefusedError{Provider: p.Provider, Target: p.Target, ReadErr: err}
				}
				return fmt.Errorf("reading %s: %w", p.Target, err)
			}
		}
		if current.exists && adopting {
			refused, err := p.adoptionConflictIn(current.data)
			if err != nil || refused != nil {
				return refusalOrErr(refused, err)
			}
			// Snapshot inside the lock so the backup holds the exact
			// content about to be replaced — no TOCTOU against another
			// writer.
			if err := snapshotAdoption(fs, p, current.data, stderr); err != nil {
				return err
			}
		}
		change, err := p.targetChangeFrom(fs, current)
		if err != nil {
			return err
		}
		return p.writeChange(fs, change)
	})
}

// mcpTargetContent is a target file as read: its bytes, or that it does not
// exist.
type mcpTargetContent struct {
	data   []byte
	exists bool
}

// readMCPTarget reads a target that is a regular file of at most
// MaxMCPFileSize bytes (readRegularFile).
func readMCPTarget(fs fsys.FS, path string) (mcpTargetContent, error) {
	data, err := readRegularFile(fs, path)
	if err != nil {
		if errorsIsNotExist(err) {
			return mcpTargetContent{}, nil
		}
		return mcpTargetContent{}, err
	}
	return mcpTargetContent{data: data, exists: true}, nil
}

func refusalOrErr(refused *MCPAdoptionRefusedError, err error) error {
	if err != nil {
		return err
	}
	return refused
}

// mcpTargetChange is what reconciling a projection does to its target file
// and to the managed marker beside it.
type mcpTargetChange struct {
	// noop is set when there is nothing to do: an empty catalog on a target
	// gc does not manage.
	noop bool
	// remove deletes the target; otherwise content replaces it.
	remove  bool
	content []byte
	// managed is whether the managed marker exists afterwards.
	managed bool
}

// targetChange computes what reconciling p does to its target, reading the
// current file and writing nothing.
//
// Claude owns the whole file. Gemini, Codex, Cursor, OpenCode, and MiMo Code
// keep unrelated config and replace only the MCP subtree; with an empty
// catalog the subtree is removed, and the file with it once nothing else is
// left.
func (p MCPProjection) targetChange(fs fsys.FS) (mcpTargetChange, error) {
	if len(p.Servers) == 0 && !p.isManaged(fs) {
		return mcpTargetChange{noop: true}, nil
	}
	if p.Provider == MCPProviderClaude {
		// Claude's file is gc's whole: its current content does not matter.
		return p.targetChangeFrom(fs, mcpTargetContent{})
	}
	current, err := readMCPTarget(fs, p.Target)
	if err != nil {
		return mcpTargetChange{}, fmt.Errorf("reading %s: %w", p.Target, err)
	}
	return p.targetChangeFrom(fs, current)
}

// targetChangeFrom is targetChange for the target's current content, as read.
func (p MCPProjection) targetChangeFrom(fs fsys.FS, current mcpTargetContent) (mcpTargetChange, error) {
	if len(p.Servers) == 0 && !p.isManaged(fs) {
		return mcpTargetChange{noop: true}, nil
	}
	key, servers, err := p.serversDoc()
	if err != nil {
		return mcpTargetChange{}, err
	}
	if p.Provider == MCPProviderClaude {
		if len(p.Servers) == 0 {
			return mcpTargetChange{remove: true}, nil
		}
		data, err := marshalJSONDoc(map[string]any{key: servers})
		if err != nil {
			return mcpTargetChange{}, err
		}
		return mcpTargetChange{content: data, managed: true}, nil
	}

	parse, marshal := parseJSONDocument, marshalJSONDoc
	if p.Provider == MCPProviderCodex {
		parse, marshal = parseTOMLDocument, marshalTOMLDoc
	}
	doc, err := parse(current.data)
	if err != nil {
		return mcpTargetChange{}, fmt.Errorf("parsing %s: %w", p.Target, err)
	}
	if len(p.Servers) == 0 {
		delete(doc, key)
		if len(doc) == 0 {
			return mcpTargetChange{remove: true}, nil
		}
	} else {
		doc[key] = servers
	}
	data, err := marshal(doc)
	if err != nil {
		return mcpTargetChange{}, err
	}
	return mcpTargetChange{content: data, managed: len(p.Servers) > 0}, nil
}

// serversDoc returns the document key p's provider keeps MCP servers under,
// and the server entries gc writes there.
func (p MCPProjection) serversDoc() (string, map[string]any, error) {
	key := mcpServersKey(p.Provider)
	switch p.Provider {
	case MCPProviderClaude:
		return key, p.claudeServersDoc(), nil
	case MCPProviderCodex:
		return key, p.codexServersDoc(), nil
	case MCPProviderGemini:
		return key, p.geminiServersDoc(), nil
	case MCPProviderOpenCode, MCPProviderMimoCode:
		// MiMo Code is an OpenCode fork sharing the same `mcp` document
		// schema; only the target file name differs (set at build time).
		return key, p.opencodeServersDoc(), nil
	case MCPProviderCursor:
		return key, p.cursorServersDoc(), nil
	default:
		return "", nil, fmt.Errorf("unsupported MCP provider %q", p.Provider)
	}
}

// writeChange applies a computed change: the target first, then the managed
// marker.
func (p MCPProjection) writeChange(fs fsys.FS, change mcpTargetChange) error {
	if change.noop {
		return nil
	}
	if change.remove {
		if err := removeManagedMCPFile(fs, p.Target); err != nil {
			return err
		}
	} else if err := writeManagedMCPFile(fs, p.Target, change.content); err != nil {
		return err
	}
	if change.managed {
		return p.writeManagedMarker(fs)
	}
	return removeManagedMCPFile(fs, p.markerPath())
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

// MCPProjectionSupportsTransport reports whether providerKind's native MCP
// config can carry a server of the given transport. Every projection carries
// stdio servers. Cursor's is stdio-only: its HTTP/SSE mcpServers wire
// contract is not verified. No projection renders SSE. An unknown provider
// supports nothing.
//
// Callers that build an agent's projection consult this first and leave out
// what the provider cannot receive; Apply rejects anything that slips
// through instead of writing a config block the provider would ignore.
func MCPProjectionSupportsTransport(providerKind string, transport MCPTransport) bool {
	switch providerKind {
	case MCPProviderClaude, MCPProviderCodex, MCPProviderGemini, MCPProviderOpenCode, MCPProviderMimoCode:
		return transport == MCPTransportStdio || transport == MCPTransportHTTP
	case MCPProviderCursor:
		return transport == MCPTransportStdio
	default:
		return false
	}
}

func (p MCPProjection) validateProviderContract() error {
	for _, server := range p.Servers {
		if MCPProjectionSupportsTransport(p.Provider, server.Transport) {
			continue
		}
		if p.Provider == MCPProviderCursor {
			return fmt.Errorf(
				"cursor MCP projection does not support %s server %q: Cursor's HTTP/SSE mcpServers wire contract is not verified",
				server.Transport,
				server.Name,
			)
		}
		return fmt.Errorf("%s MCP projection does not support %s server %q", p.Provider, server.Transport, server.Name)
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
	return nil
}

func errorsIsNotExist(err error) bool {
	return err != nil && (os.IsNotExist(err) || errors.Is(err, iofs.ErrNotExist))
}
