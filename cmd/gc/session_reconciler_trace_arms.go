package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

type SessionReconcilerTraceArmStore struct {
	cityPath string
	rootDir  string
	lockPath string
}

func newSessionReconcilerTraceArmStore(cityPath string) *SessionReconcilerTraceArmStore {
	rootDir := filepath.Join(citylayout.RuntimeDataDir(cityPath), sessionReconcilerTraceRootDir)
	return &SessionReconcilerTraceArmStore{
		cityPath: cityPath,
		rootDir:  rootDir,
		lockPath: filepath.Join(rootDir, sessionReconcilerTraceLockFile),
	}
}

func (s *SessionReconcilerTraceArmStore) load() (TraceArmState, error) {
	path := filepath.Join(s.rootDir, sessionReconcilerTraceArmsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return TraceArmState{SchemaVersion: sessionReconcilerTraceSchemaVersion}, nil
		}
		return TraceArmState{}, err
	}
	var state TraceArmState
	if err := json.Unmarshal(data, &state); err != nil {
		return TraceArmState{}, err
	}
	if state.SchemaVersion == 0 {
		state.SchemaVersion = sessionReconcilerTraceSchemaVersion
	}
	state = state.normalized()
	return state, nil
}

func (s *SessionReconcilerTraceArmStore) save(state TraceArmState) error {
	if err := os.MkdirAll(s.rootDir, sessionReconcilerTraceOwnerDirPerm); err != nil {
		return err
	}
	state.SchemaVersion = sessionReconcilerTraceSchemaVersion
	state.UpdatedAt = time.Now().UTC()
	state = state.normalized()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.rootDir, sessionReconcilerTraceArmsFile)
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, data, sessionReconcilerTraceOwnerFilePerm); err != nil {
		return err
	}
	return os.Chmod(path, sessionReconcilerTraceOwnerFilePerm)
}

func (s *SessionReconcilerTraceArmStore) withLock(fn func() error) error {
	if err := os.MkdirAll(s.rootDir, sessionReconcilerTraceOwnerDirPerm); err != nil {
		return err
	}
	f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, sessionReconcilerTraceOwnerFilePerm)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

func (s *SessionReconcilerTraceArmStore) upsertArm(arm TraceArm) (TraceArmState, error) {
	var out TraceArmState
	err := s.withLock(func() error {
		state, err := s.load()
		if err != nil {
			return err
		}
		key := traceScopeKey(arm.ScopeType, arm.ScopeValue, arm.Source)
		next := make([]TraceArm, 0, len(state.Arms)+1)
		updated := false
		for _, existing := range state.Arms {
			if traceScopeKey(existing.ScopeType, existing.ScopeValue, existing.Source) == key {
				arm.ArmedAt = existing.ArmedAt
				if arm.LastExtendedAt.IsZero() {
					arm.LastExtendedAt = time.Now().UTC()
				}
				next = append(next, arm)
				updated = true
				continue
			}
			next = append(next, existing)
		}
		if !updated {
			if arm.ArmedAt.IsZero() {
				arm.ArmedAt = time.Now().UTC()
			}
			if arm.LastExtendedAt.IsZero() {
				arm.LastExtendedAt = arm.ArmedAt
			}
			next = append(next, arm)
		}
		state.Arms = next
		if err := s.save(state); err != nil {
			return err
		}
		out = state
		return nil
	})
	return out, err
}

func (s *SessionReconcilerTraceArmStore) remove(scopeType TraceArmScopeType, scopeValue string, all bool) (TraceArmState, error) {
	var out TraceArmState
	err := s.withLock(func() error {
		state, err := s.load()
		if err != nil {
			return err
		}
		key := traceScopeKey(scopeType, scopeValue, TraceArmSourceManual)
		next := state.Arms[:0]
		for _, arm := range state.Arms {
			if arm.ScopeType != scopeType || arm.ScopeValue != scopeValue {
				next = append(next, arm)
				continue
			}
			if all {
				continue
			}
			if traceScopeKey(arm.ScopeType, arm.ScopeValue, arm.Source) != key {
				next = append(next, arm)
			}
		}
		state.Arms = append([]TraceArm(nil), next...)
		if err := s.save(state); err != nil {
			return err
		}
		out = state
		return nil
	})
	return out, err
}

func (s *SessionReconcilerTraceArmStore) list() (TraceArmState, error) {
	state, err := s.load()
	if err != nil {
		return TraceArmState{}, err
	}
	sortTraceArms(state.Arms)
	return state, nil
}

func traceArmStatus(state TraceArmState, now time.Time) []TraceArm {
	var out []TraceArm
	for _, arm := range state.Arms {
		if !arm.ExpiresAt.IsZero() && arm.ExpiresAt.Before(now) {
			continue
		}
		out = append(out, arm)
	}
	sortTraceArms(out)
	return out
}
