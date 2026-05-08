package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/runtime"
)

func (m *Manager) syncStoredMCPServers(id string, b *beads.Bead, servers []runtime.MCPServerConfig) error {
	snapshot, err := EncodeMCPServersSnapshot(servers)
	if err != nil {
		return err
	}
	current := ""
	if b != nil && b.Metadata != nil {
		current = strings.TrimSpace(b.Metadata[MCPServersSnapshotMetadataKey])
	}
	if current != snapshot {
		if err := m.store.SetMetadata(id, MCPServersSnapshotMetadataKey, snapshot); err != nil {
			return fmt.Errorf("storing MCP server snapshot: %w", err)
		}
		if b != nil {
			if b.Metadata == nil {
				b.Metadata = make(map[string]string)
			}
			b.Metadata[MCPServersSnapshotMetadataKey] = snapshot
		}
	}
	if err := PersistRuntimeMCPServersSnapshot(m.cityPath, id, servers); err != nil {
		return fmt.Errorf("storing runtime MCP server snapshot: %w", err)
	}
	return nil
}

// PersistRuntimeMCPServersSnapshot stores the full normalized MCP server
// snapshot for a session in the controller-local runtime cache. The cache is
// not exposed on the bead metadata wire and is used only as a degraded resume
// fallback when the live MCP catalog cannot be materialized.
func PersistRuntimeMCPServersSnapshot(cityPath, sessionID string, servers []runtime.MCPServerConfig) error {
	path := runtimeMCPServersSnapshotPath(cityPath, sessionID)
	if path == "" {
		return nil
	}
	if len(servers) == 0 {
		return clearRuntimeMCPServersSnapshot(cityPath, sessionID)
	}
	data, err := json.Marshal(runtime.NormalizeMCPServerConfigs(servers))
	if err != nil {
		return fmt.Errorf("marshal runtime MCP snapshot: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir runtime MCP snapshot dir: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create runtime MCP snapshot temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod runtime MCP snapshot temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write runtime MCP snapshot: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close runtime MCP snapshot temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename runtime MCP snapshot: %w", err)
	}
	return nil
}

// LoadRuntimeMCPServersSnapshot loads the full normalized MCP server snapshot
// for a session from the controller-local runtime cache. It returns nil, nil
// when no cache file exists.
func LoadRuntimeMCPServersSnapshot(cityPath, sessionID string) ([]runtime.MCPServerConfig, error) {
	path := runtimeMCPServersSnapshotPath(cityPath, sessionID)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runtime MCP snapshot: %w", err)
	}
	var servers []runtime.MCPServerConfig
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, fmt.Errorf("unmarshal runtime MCP snapshot: %w", err)
	}
	return runtime.NormalizeMCPServerConfigs(servers), nil
}

func runtimeMCPServersSnapshotPath(cityPath, sessionID string) string {
	cityPath = strings.TrimSpace(cityPath)
	sessionID = strings.TrimSpace(sessionID)
	if cityPath == "" || sessionID == "" {
		return ""
	}
	return citylayout.RuntimePath(cityPath, "session-mcp", sessionID+".json")
}

func clearRuntimeMCPServersSnapshot(cityPath, sessionID string) error {
	path := runtimeMCPServersSnapshotPath(cityPath, sessionID)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove runtime MCP snapshot: %w", err)
	}
	return nil
}
