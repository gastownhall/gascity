package materialize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// mcpRelTarget is the slash-relative path, under a workdir, of the
// provider-native MCP config file gc projects for providerKind. It returns ""
// for an unsupported provider. This is the single source of the
// provider-to-file mapping: BuildMCPProjection turns it into an absolute
// target, and ManagedMCPTargets reverses a marker back into the path gc wrote.
func mcpRelTarget(providerKind string) string {
	switch providerKind {
	case MCPProviderClaude:
		return ".mcp.json"
	case MCPProviderCodex:
		return ".codex/config.toml"
	case MCPProviderGemini:
		return ".gemini/settings.json"
	case MCPProviderOpenCode:
		return "opencode.json"
	case MCPProviderMimoCode:
		return "mimocode.json"
	case MCPProviderCursor:
		return ".cursor/mcp.json"
	default:
		return ""
	}
}

// ManagedMCPTarget is one provider-native MCP file gc has adopted in a
// worktree, as recorded by the managed marker gc writes beside it.
type ManagedMCPTarget struct {
	// Provider is the MCP provider kind the marker was written for.
	Provider string
	// RelPath is the target file's slash-relative path within the worktree.
	RelPath string
	// SHA256 is the hex content hash gc recorded when it last wrote the
	// target. It is empty for markers written before gc recorded hashes, and
	// for a marker whose target could not be read back; callers must treat an
	// empty hash as "ownership unproven".
	SHA256 string
}

// ManagedMCPTargets reports the MCP config files gc manages under root, read
// from the ".gc/mcp-managed" markers gc wrote when it adopted each file.
//
// It is best-effort and read-only: a missing directory, an unreadable entry,
// a corrupt marker, or an unrecognized provider each yield no target rather
// than an error, so a caller's worst case is treating gc's own file as
// somebody else's.
func ManagedMCPTargets(root string) []ManagedMCPTarget {
	dir := filepath.Join(root, ".gc", "mcp-managed")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []ManagedMCPTarget
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var marker struct {
			ManagedBy     string `json:"managed_by"`
			Provider      string `json:"provider"`
			ContentSHA256 string `json:"content_sha256"`
		}
		if err := json.Unmarshal(data, &marker); err != nil || marker.ManagedBy != "gc" {
			continue
		}
		rel := mcpRelTarget(marker.Provider)
		if rel == "" {
			continue
		}
		out = append(out, ManagedMCPTarget{
			Provider: marker.Provider,
			RelPath:  rel,
			SHA256:   marker.ContentSHA256,
		})
	}
	return out
}
