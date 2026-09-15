package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// handoffJournalSchemaVersion is the only ownership-handoff journal schema gc
// reads.
//
// Version-gating is not a formality here, it is the whole safety of the reader.
// v1 and v2 share phase NAMES and order them differently: old_owner_stopped
// means "bd's replacement server is already configured" under v1 and "the
// legacy server is gone and there is no target yet" under v2. Interpreting a v1
// journal with v2's table — or the reverse — is the single worst misread
// available, because it is the phase that decides whether a second sql-server
// may be raised over a live one. So the version is checked before any phase is
// looked at, and anything that is not this version is refused by name rather
// than probed for familiar fields.
const handoffJournalSchemaVersion = 2

const handoffJournalName = "ownership-handoff.json"

// handoffProjectionEndpoint is the loopback endpoint the legacy owner served.
// v2 has no socket field: the transfer is TCP-only by contract.
type handoffProjectionEndpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// handoffProjectionArtifact is one file bd captured byte- and mode-exact before
// it touched anything. Present distinguishes "was empty" from "was absent",
// which is the difference between restoring a file and removing one.
type handoffProjectionArtifact struct {
	Present bool   `json:"present"`
	Data    []byte `json:"data,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
}

// handoffProjectionJournal is gc's view of bd's journal. It is deliberately a
// subset: bd owns the format, and every field named here is one gc's lifecycle
// decisions actually rest on.
//
// Two shapes moved in v2 and both matter to this reader. The request carries no
// city_root — bd knows about workspaces and nothing about cities — and the
// replacement server's identity moved out of the snapshot into its own target
// object, because the snapshot is what bd must put BACK and the target is what
// bd made. Mixing them made the snapshot's meaning ambiguous.
type handoffProjectionJournal struct {
	SchemaVersion int `json:"schema_version"`
	Request       struct {
		Root      string                    `json:"root"`
		Database  string                    `json:"database"`
		Workspace string                    `json:"workspace"`
		Endpoint  handoffProjectionEndpoint `json:"endpoint"`
		Owner     string                    `json:"owner"`
	} `json:"request"`
	Target struct {
		PID          int    `json:"pid"`
		Birth        string `json:"birth"`
		DataDir      string `json:"data_dir"`
		LaunchID     string `json:"launch_id"`
		LaunchConfig string `json:"launch_config"`
		Executable   string `json:"executable"`
		Host         string `json:"host"`
		Port         int    `json:"port"`
	} `json:"target"`
	Snapshot struct {
		Metadata handoffProjectionArtifact `json:"metadata"`
		Config   handoffProjectionArtifact `json:"config"`
		PortFile handoffProjectionArtifact `json:"port_file"`
	} `json:"snapshot"`
	Reservations struct {
		TargetLaunch   string `json:"target_launch"`
		CommitWriteSet string `json:"commit_write_set"`
	} `json:"reservations"`
	Phase     string `json:"phase"`
	Owner     string `json:"owner"`
	UpdatedAt string `json:"updated_at"`
}

// handoffBirthTokenPattern is the shape of a process-birth identity: a
// platform-versioned scheme and a payload. gc deliberately does not know how to
// mint or compare one — that is bd's, and only bd's, because only bd started
// the process it names. What gc can prove is that the committed journal carries
// one at all, in the versioned form bd writes, rather than an empty string or a
// pid restated as a birth.
var handoffBirthTokenPattern = regexp.MustCompile(`^[a-z0-9]+-v[0-9]+:[^\s]+$`)

// committedBeadsHandoffOwnsScope is the ownership projection used by GC's
// normal lifecycle resolver. A committed handoff is provider-owned. Only an
// admitted restored rollback is legacy-owned; pending, corrupt, and conflicting
// records fail closed.
//
// It is read-only but for one thing: the first time a rolled-back journal's
// artifacts are found byte-exact, that admission is recorded. See
// admitRolledBackHandoff for why the comparison cannot be a standing invariant.
func committedBeadsHandoffOwnsScope(scopeRoot string) (bool, error) {
	// A missing journal is the legacy condition. Do not impose physical-path
	// requirements on a fresh scope until there is a handoff record to admit.
	// A rollback that ran to completion archives its journal under a
	// `.rolled-back-<nanos>` suffix as its last act, so a settled rollback
	// arrives here as exactly this case: an absent journal, which is what the
	// legacy condition is.
	path := filepath.Join(scopeRoot, ".beads", handoffJournalName)
	info, err := os.Lstat(path)
	// ENOTDIR is the same answer as ENOENT here: a .beads that is not a
	// directory cannot hold a journal, so there is no handoff record to admit.
	// Reporting it as a malformed journal buries whatever really went wrong
	// with the scope under an ownership error it did not cause.
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ownership handoff journal is not a regular file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("ownership handoff journal is not a regular file")
	}
	physicalRoot, err := filepath.EvalSymlinks(scopeRoot)
	if err != nil {
		return false, fmt.Errorf("resolve ownership handoff scope root: %w", err)
	}
	scopeRoot = filepath.Clean(physicalRoot)
	beadsDir := filepath.Join(scopeRoot, ".beads")
	beadsInfo, err := os.Lstat(beadsDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ownership handoff beads directory is not a physical directory: %w", err)
	}
	if !beadsInfo.IsDir() || beadsInfo.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("ownership handoff beads directory is not a physical directory")
	}
	path = filepath.Join(beadsDir, handoffJournalName)
	info, err = os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("ownership handoff journal is not a regular file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("ownership handoff journal is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read ownership handoff journal: %w", err)
	}
	var journal handoffProjectionJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return false, fmt.Errorf("parse ownership handoff journal: %w", err)
	}
	// Version first. Nothing below this line may look at a phase name.
	if journal.SchemaVersion != handoffJournalSchemaVersion {
		return false, fmt.Errorf("unsupported handoff journal version %d at %s: gc reads version %d, and the phase names mean different things across versions",
			journal.SchemaVersion, path, handoffJournalSchemaVersion)
	}
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
		if err := admitRolledBackHandoff(scopeRoot, journal); err != nil {
			return false, err
		}
		return false, nil
	case "prepared", "old_owner_stopped", "target_configured", "verified", "rollback_started":
		// Every one of these is a transfer in flight, and none of them is
		// bd-owned. old_owner_stopped is worth naming: under v2 it means the
		// legacy server has been proven gone and bd has NOT yet configured a
		// replacement, so the scope has no running server at all. gc must not
		// read that as ownership of any kind, and must not start its own
		// server over the transfer — which is what failing closed here does.
		if journal.Owner != "legacy-gc" {
			return false, errors.New("ownership handoff journal has invalid pending owner")
		}
		return false, fmt.Errorf("ownership handoff journal is %s; the transfer is in progress", journal.Phase)
	default:
		return false, errors.New("ownership handoff journal has unknown phase")
	}
}

// validateRestoredProjection rejects a rollback checkpoint that captured
// nothing. Every legacy GC-managed city has a metadata.json — it is what makes
// it a beads scope at all — so a snapshot that records it absent is not a
// restoration of this city, and admitting one would let an empty journal hand
// the scope back on the strength of two files that are also absent.
func validateRestoredProjection(journal handoffProjectionJournal) error {
	if !journal.Snapshot.Metadata.Present || len(journal.Snapshot.Metadata.Data) == 0 {
		return errors.New("ownership handoff journal has incomplete restored checkpoint")
	}
	return nil
}

func validateProjectionRequest(scopeRoot string, journal handoffProjectionJournal) error {
	r := journal.Request
	if r.Owner != "legacy-gc" || r.Database == "" || r.Workspace == "" ||
		(r.Endpoint.Host != "127.0.0.1" && r.Endpoint.Host != "localhost" && r.Endpoint.Host != "::1") ||
		r.Endpoint.Port < 1 || r.Endpoint.Port > 65535 {
		return errors.New("ownership handoff journal has invalid request identity")
	}
	if !samePath(r.Root, scopeRoot) {
		return errors.New("ownership handoff journal does not bind this scope root")
	}
	return nil
}

// validateCommittedProjection authenticates the one thing a committed journal
// asserts that gc acts on: that bd is running a replacement server for this
// scope, identified strictly enough that bd can stop it again.
//
// There is no countersigned proof to verify here, and there deliberately never
// will be. Under v1 the journal embedded a typed response gc itself had minted,
// so the reader could recompute its digest; that protocol existed only so bd
// could spawn gc, which is the shape the inverted design removes. What is left
// is bd's own record, and what gc can hold it to is internal consistency: the
// target must be bound to THIS workspace (its data dir and its nonce-bound
// launch config both under this scope's .beads), it must carry the strict
// launch identity bd needs to stop it by identity rather than by pid, and its
// endpoint must be a real loopback endpoint. A journal that cannot satisfy
// those is not a transfer gc can safely stand down for.
func validateCommittedProjection(scopeRoot string, journal handoffProjectionJournal) error {
	if journal.Owner != "bd" {
		return errors.New("ownership handoff journal has invalid committed owner")
	}
	if journal.Reservations.TargetLaunch != "done" || journal.Reservations.CommitWriteSet != "done" {
		return errors.New("ownership handoff journal has incomplete committed checkpoint")
	}
	beadsDir := filepath.Join(scopeRoot, ".beads")
	t := journal.Target
	if t.PID <= 0 || !handoffBirthTokenPattern.MatchString(strings.TrimSpace(t.Birth)) {
		return errors.New("ownership handoff journal has invalid replacement server identity")
	}
	if !samePath(t.DataDir, filepath.Join(beadsDir, "dolt")) {
		return errors.New("ownership handoff journal has invalid direct target identity")
	}
	if len(t.LaunchID) != 32 || strings.Trim(t.LaunchID, "0123456789abcdef") != "" ||
		!samePath(t.LaunchConfig, filepath.Join(beadsDir, "dolt-handoff-"+t.LaunchID+".yaml")) ||
		!filepath.IsAbs(t.Executable) || filepath.Clean(t.Executable) != t.Executable {
		return errors.New("ownership handoff journal has incomplete strict launch identity")
	}
	if (t.Host != "127.0.0.1" && t.Host != "localhost" && t.Host != "::1") || t.Port < 1 || t.Port > 65535 {
		return errors.New("ownership handoff journal has invalid replacement endpoint")
	}
	return nil
}

// handoffJournalRestoredArtifactsMatch is deliberately byte-exact. The
// rollback checkpoint is the permission for GC to resume legacy management.
func handoffJournalRestoredArtifactsMatch(cityPath string, artifacts map[string]handoffProjectionArtifact) error {
	for _, name := range legacyHandoffArtifactNames {
		artifact := artifacts[name]
		path := filepath.Join(cityPath, ".beads", name)
		info, err := os.Lstat(path)
		if !artifact.Present {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("stat restored handoff %s: %w", name, err)
			}
			return fmt.Errorf("restored handoff %s unexpectedly exists", name)
		}
		if err != nil {
			return fmt.Errorf("stat restored handoff %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("restored handoff %s is not a regular file", name)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read restored handoff %s: %w", name, err)
		}
		if string(got) != string(artifact.Data) {
			return fmt.Errorf("restored handoff %s does not match its journal", name)
		}
		if uint32(info.Mode().Perm()) != artifact.Mode {
			return fmt.Errorf("restored handoff %s mode does not match its journal", name)
		}
	}
	return nil
}

// legacyHandoffArtifactNames are the three files the rollback checkpoint covers
// and gc's projection compares byte for byte before resuming legacy management.
// dolt-server.pid is deliberately absent: gc's own restart rewrites it, so it
// cannot be part of an admission gate that has to survive that restart.
var legacyHandoffArtifactNames = []string{"metadata.json", "config.yaml", "dolt-server.port"}

// restoredHandoffArtifacts maps the journal's snapshot onto the file names it
// restored.
func restoredHandoffArtifacts(journal handoffProjectionJournal) map[string]handoffProjectionArtifact {
	return map[string]handoffProjectionArtifact{
		"metadata.json":    journal.Snapshot.Metadata,
		"config.yaml":      journal.Snapshot.Config,
		"dolt-server.port": journal.Snapshot.PortFile,
	}
}
