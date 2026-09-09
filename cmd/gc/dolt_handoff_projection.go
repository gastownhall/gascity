package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type handoffProjectionJournal struct {
	Request struct {
		CityRoot  string `json:"city_root"`
		Root      string `json:"root"`
		Database  string `json:"database"`
		Workspace string `json:"workspace"`
		Endpoint  struct {
			Host   string `json:"host"`
			Port   int    `json:"port"`
			Socket string `json:"socket"`
		} `json:"endpoint"`
		Owner string `json:"owner"`
	} `json:"request"`
	Snapshot struct {
		Metadata                 []byte `json:"metadata"`
		TargetPID                int    `json:"target_pid"`
		TargetBirth              string `json:"target_birth"`
		TargetDataDir            string `json:"target_data_dir"`
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
	SnapshotCaptured     bool   `json:"snapshot_captured"`
	CommitHookInProgress bool   `json:"commit_hook_in_progress"`
	CommitHookRan        bool   `json:"commit_hook_ran"`
	MutationOccurred     bool   `json:"mutation_occurred"`
	Phase                string `json:"phase"`
	Owner                string `json:"owner"`
}

// committedBeadsHandoffOwnsScope is the read-only ownership projection used
// by GC's normal lifecycle resolver. A committed direct-local handoff is
// provider-owned. Only a byte-exact restored rollback is legacy-owned;
// pending, corrupt, and conflicting records fail closed.
func committedBeadsHandoffOwnsScope(scopeRoot string) (bool, error) {
	path := filepath.Join(scopeRoot, ".beads", "ownership-handoff.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read ownership handoff journal: %w", err)
	}
	var journal handoffProjectionJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return false, fmt.Errorf("parse ownership handoff journal: %w", err)
	}
	scopeRoot, err = filepath.EvalSymlinks(scopeRoot)
	if err != nil {
		return false, fmt.Errorf("resolve ownership handoff scope root: %w", err)
	}
	scopeRoot = filepath.Clean(scopeRoot)
	if err := validateProjectionRequest(scopeRoot, journal); err != nil {
		return false, err
	}
	switch journal.Phase {
	case "committed":
		if err := validateCommittedProjection(scopeRoot, journal); err != nil {
			return false, err
		}
		return true, nil
	case "legacy_config_restored", "rolled_back":
		if journal.Owner != "legacy-gc" {
			return false, errors.New("ownership handoff journal has invalid restored owner")
		}
		if err := validateRestoredProjection(journal); err != nil {
			return false, err
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

func validateRestoredProjection(journal handoffProjectionJournal) error {
	if !journal.SnapshotCaptured || journal.CommitHookInProgress || journal.CommitHookRan {
		return errors.New("ownership handoff journal has incomplete restored checkpoint")
	}
	var legacy struct {
		SchemaVersion int    `json:"schema_version"`
		Operation     string `json:"operation"`
		Result        string `json:"result"`
		Owner         string `json:"owner"`
		IdentityToken string `json:"identity_token"`
	}
	if err := json.Unmarshal(journal.Snapshot.Metadata, &legacy); err != nil || legacy.SchemaVersion != 1 || legacy.Operation != "handoff-inspect" || legacy.Result != "eligible" || legacy.Owner != "legacy-gc" || strings.TrimSpace(legacy.IdentityToken) == "" {
		return errors.New("ownership handoff journal has invalid legacy protocol snapshot")
	}
	return nil
}

func validateProjectionRequest(scopeRoot string, journal handoffProjectionJournal) error {
	r := journal.Request
	if r.Owner != "legacy-gc" || r.Database == "" || r.Workspace == "" || r.Endpoint.Socket != "" ||
		(r.Endpoint.Host != "127.0.0.1" && r.Endpoint.Host != "localhost" && r.Endpoint.Host != "::1") || r.Endpoint.Port < 1 || r.Endpoint.Port > 65535 {
		return errors.New("ownership handoff journal has invalid request identity")
	}
	if filepath.Clean(r.CityRoot) != scopeRoot || filepath.Clean(r.Root) != scopeRoot {
		return errors.New("ownership handoff journal does not bind this scope root")
	}
	return nil
}

func validateCommittedProjection(scopeRoot string, journal handoffProjectionJournal) error {
	if journal.Owner != "bd" || !journal.SnapshotCaptured || !journal.MutationOccurred || !journal.CommitHookRan || journal.CommitHookInProgress {
		return errors.New("ownership handoff journal has incomplete committed checkpoint")
	}
	s := journal.Snapshot
	if s.TargetPID <= 0 || strings.TrimSpace(s.TargetBirth) == "" || filepath.Clean(s.TargetDataDir) != filepath.Join(scopeRoot, ".beads", "dolt") {
		return errors.New("ownership handoff journal has invalid direct target identity")
	}
	var legacy struct {
		SchemaVersion int    `json:"schema_version"`
		Operation     string `json:"operation"`
		Result        string `json:"result"`
		Owner         string `json:"owner"`
		IdentityToken string `json:"identity_token"`
	}
	if err := json.Unmarshal(s.Metadata, &legacy); err != nil || legacy.SchemaVersion != 1 || legacy.Operation != "handoff-inspect" || legacy.Result != "eligible" || legacy.Owner != "legacy-gc" || strings.TrimSpace(legacy.IdentityToken) == "" {
		return errors.New("ownership handoff journal has invalid legacy protocol snapshot")
	}
	return nil
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
		info, err := os.Lstat(path)
		if !artifact.present {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("stat restored handoff %s: %w", artifact.name, err)
			}
			return fmt.Errorf("restored handoff %s unexpectedly exists", artifact.name)
		}
		if err != nil {
			return fmt.Errorf("stat restored handoff %s: %w", artifact.name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("restored handoff %s is not a regular file", artifact.name)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read restored handoff %s: %w", artifact.name, err)
		}
		if string(got) != string(artifact.want) {
			return fmt.Errorf("restored handoff %s does not match its journal", artifact.name)
		}
		if artifact.mode != 0 && uint32(info.Mode().Perm()) != artifact.mode {
			return fmt.Errorf("restored handoff %s mode does not match its journal", artifact.name)
		}
	}
	return nil
}
