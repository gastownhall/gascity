// ABOUTME: Snapshot file backing ordinal session references (issue #2031):
// ABOUTME: `gc session list` writes IDs in display order; resolver indexes them.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gastownhall/gascity/internal/session"
)

const sessionListSnapshotFile = "last-session-list.json"

func sessionListSnapshotPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", sessionListSnapshotFile)
}

// writeSessionListSnapshot persists the bead-ID order shown by the most
// recent `gc session list` invocation. Last write wins. Atomic via
// temp file + rename. Caller is responsible for ensuring `.gc/` exists.
func writeSessionListSnapshot(cityPath string, ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("marshaling session list snapshot: %w", err)
	}
	dir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ensuring .gc/: %w", err)
	}
	tmp, err := os.CreateTemp(dir, sessionListSnapshotFile+".*")
	if err != nil {
		return fmt.Errorf("creating snapshot temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing snapshot temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing snapshot temp: %w", err)
	}
	if err := os.Rename(tmpName, sessionListSnapshotPath(cityPath)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming snapshot temp: %w", err)
	}
	return nil
}

// readSessionListSnapshot returns the bead-ID order from the most recent
// `gc session list` invocation. Missing snapshot returns ErrSessionNotFound
// so callers in the resolver chain can fall through.
func readSessionListSnapshot(cityPath string) ([]string, error) {
	data, err := os.ReadFile(sessionListSnapshotPath(cityPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: no prior `gc session list`", session.ErrSessionNotFound)
		}
		return nil, fmt.Errorf("reading session list snapshot: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("parsing session list snapshot: %w", err)
	}
	return ids, nil
}

// resolveOrdinalFromSnapshot interprets identifier as a 0-based ordinal
// into the most recent `gc session list` snapshot. Only canonical decimal
// representations of non-negative integers match; non-canonical forms
// ("01", "+0", whitespace) and non-digit strings return ErrSessionNotFound
// so they flow back to alias/ID lookup paths in the resolver.
func resolveOrdinalFromSnapshot(cityPath, identifier string) (string, error) {
	notFound := func() (string, error) {
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	n, err := strconv.Atoi(identifier)
	if err != nil || n < 0 || strconv.Itoa(n) != identifier {
		return notFound()
	}
	ids, err := readSessionListSnapshot(cityPath)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return notFound()
		}
		return "", err
	}
	if n >= len(ids) {
		return notFound()
	}
	return ids[n], nil
}
