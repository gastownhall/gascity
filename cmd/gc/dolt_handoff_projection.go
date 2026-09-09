package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// committedBeadsHandoffOwnsScope is the read-only ownership projection used
// by GC's normal lifecycle resolver. A committed direct-local handoff is
// provider-owned. Only a byte-exact restored rollback is legacy-owned;
// pending, corrupt, and conflicting records fail closed.
func committedBeadsHandoffOwnsScope(scopeRoot string) (bool, error) {
	scopeRoot = normalizePathForCompare(scopeRoot)
	path := filepath.Join(scopeRoot, ".beads", "ownership-handoff.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read ownership handoff journal: %w", err)
	}
	var journal struct {
		Request struct {
			CityRoot string `json:"city_root"`
			Root     string `json:"root"`
		} `json:"request"`
		Phase    string `json:"phase"`
		Owner    string `json:"owner"`
		Snapshot struct {
			WorkspaceMetadata        []byte `json:"workspace_metadata"`
			WorkspaceConfig          []byte `json:"workspace_config"`
			WorkspacePort            []byte `json:"workspace_port"`
			WorkspaceMetadataPresent bool   `json:"workspace_metadata_present"`
			WorkspaceConfigPresent   bool   `json:"workspace_config_present"`
			WorkspacePortPresent     bool   `json:"workspace_port_present"`
			WorkspaceMetadataMode    uint32 `json:"workspace_metadata_mode"`
			WorkspaceConfigMode      uint32 `json:"workspace_config_mode"`
			WorkspacePortMode        uint32 `json:"workspace_port_mode"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return false, fmt.Errorf("parse ownership handoff journal: %w", err)
	}
	if normalizePathForCompare(journal.Request.CityRoot) != scopeRoot || normalizePathForCompare(journal.Request.Root) != scopeRoot {
		return false, errors.New("ownership handoff journal does not bind this scope root")
	}
	switch journal.Phase {
	case "committed":
		if journal.Owner != "bd" {
			return false, errors.New("ownership handoff journal has invalid committed owner")
		}
		return true, nil
	case "legacy_config_restored", "rolled_back":
		if journal.Owner != "legacy-gc" {
			return false, errors.New("ownership handoff journal has invalid restored owner")
		}
		if err := handoffJournalRestoredArtifactsMatch(scopeRoot, journal.Snapshot.WorkspaceMetadata, journal.Snapshot.WorkspaceConfig, journal.Snapshot.WorkspacePort, journal.Snapshot.WorkspaceMetadataPresent, journal.Snapshot.WorkspaceConfigPresent, journal.Snapshot.WorkspacePortPresent, journal.Snapshot.WorkspaceMetadataMode, journal.Snapshot.WorkspaceConfigMode, journal.Snapshot.WorkspacePortMode); err != nil {
			return false, err
		}
		return false, nil
	case "prepared", "target_configured", "old_owner_stopped", "verified", "rollback_started":
		if journal.Owner != "legacy-gc" {
			return false, errors.New("ownership handoff journal has invalid pending owner")
		}
		return false, errors.New("ownership handoff journal is pending")
	default:
		return false, errors.New("ownership handoff journal has unknown phase")
	}
}

// handoffJournalRestoredArtifactsMatch is deliberately byte-exact. The
// rollback checkpoint is the permission for GC to resume legacy management.
func handoffJournalRestoredArtifactsMatch(cityPath string, metadata, config, port []byte, metadataPresent, configPresent, portPresent bool, metadataMode, configMode, portMode uint32) error {
	for _, artifact := range []struct {
		name    string
		want    []byte
		present bool
		mode    uint32
	}{
		{name: "metadata.json", want: metadata, present: metadataPresent || len(metadata) != 0, mode: metadataMode},
		{name: "config.yaml", want: config, present: configPresent || len(config) != 0, mode: configMode},
		{name: "dolt-server.port", want: port, present: portPresent || len(port) != 0, mode: portMode},
	} {
		path := filepath.Join(cityPath, ".beads", artifact.name)
		got, err := os.ReadFile(path)
		if !artifact.present && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read restored handoff %s: %w", artifact.name, err)
		}
		if string(got) != string(artifact.want) {
			return fmt.Errorf("restored handoff %s does not match its journal", artifact.name)
		}
		if artifact.mode != 0 && uint32(infoMode(path)) != artifact.mode {
			return fmt.Errorf("restored handoff %s mode does not match its journal", artifact.name)
		}
	}
	return nil
}

func infoMode(path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Mode().Perm()
}
