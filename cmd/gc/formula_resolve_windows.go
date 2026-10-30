//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/platform/filelock"
	"golang.org/x/sys/windows"
)

const (
	formulaManagedManifestName = ".gc-managed-formulas.json"
	formulaManagedLockName     = ".gc-managed-formulas.lock"
)

const (
	formulaResolutionLockWait = 10 * time.Second
	formulaResolutionLockPoll = 25 * time.Millisecond
)

var formulaSymlink = os.Symlink

type formulaResolutionState struct {
	manifest formulaManagedManifest
	lockFile *os.File
	lock     *filelock.Lock
	dirty    bool
}

type formulaManagedManifest struct {
	Version int                           `json:"version"`
	Entries map[string]formulaManagedCopy `json:"entries,omitempty"`
}

type formulaManagedCopy struct {
	Source        string `json:"source"`
	SHA256        string `json:"sha256,omitempty"`
	PendingSHA256 string `json:"pending_sha256,omitempty"`
}

func loadFormulaResolutionState(formulaDir string) (*formulaResolutionState, error) {
	state := &formulaResolutionState{
		manifest: formulaManagedManifest{
			Version: 1,
			Entries: make(map[string]formulaManagedCopy),
		},
	}
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating formula lock dir: %w", err)
	}
	lockPath := filepath.Join(formulaDir, formulaManagedLockName)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening formula lock %s: %w", lockPath, err)
	}
	state.lockFile = lockFile
	lock, err := filelock.Acquire(lockFile, filelock.Options{
		Mode:       filelock.Exclusive,
		Timeout:    formulaResolutionLockWait,
		RetryEvery: formulaResolutionLockPoll,
		Label:      lockPath,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("locking formula resolution %s: %w", lockPath, err), state.release())
	}
	state.lock = lock

	manifestPath := formulaManifestPath(formulaDir)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return nil, errors.Join(fmt.Errorf("reading formula manifest %s: %w", manifestPath, err), state.release())
	}
	if err := json.Unmarshal(data, &state.manifest); err != nil {
		return nil, errors.Join(fmt.Errorf("parsing formula manifest %s: %w; remove %s to treat existing formula copies as user-owned and rebuild managed copies", manifestPath, err, manifestPath), state.release())
	}
	if state.manifest.Version != 1 {
		return nil, errors.Join(fmt.Errorf("unsupported formula manifest version %d in %s; remove %s to treat existing formula copies as user-owned and rebuild managed copies", state.manifest.Version, manifestPath, manifestPath), state.release())
	}
	if state.manifest.Entries == nil {
		state.manifest.Entries = make(map[string]formulaManagedCopy)
	}
	return state, nil
}

func ensureFormulaEntry(formulaDir, linkName, srcPath string, state *formulaResolutionState) error {
	linkPath := filepath.Join(formulaDir, linkName)
	fi, err := os.Lstat(linkPath)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink == 0:
		if _, managed := state.managed(linkName); !managed || fi.IsDir() {
			if fi.IsDir() {
				state.unmanage(linkName)
				if err := state.save(formulaDir); err != nil {
					return err
				}
			}
			return nil
		}
		replaced, replaceErr := tryReplaceManagedCopyWithSymlink(formulaDir, linkName, linkPath, srcPath, state)
		if replaceErr != nil {
			return replaceErr
		}
		if replaced {
			return nil
		}
		if err := writeManagedFormulaCopy(linkPath, linkName, srcPath, state); err != nil {
			return fmt.Errorf("creating managed formula copy %q → %q: %w", linkName, srcPath, err)
		}
		return nil

	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		existing, readErr := os.Readlink(linkPath)
		if readErr == nil && existing == srcPath {
			state.unmanage(linkName)
			if err := state.save(formulaDir); err != nil {
				return fmt.Errorf("dropping formula manifest ownership for symlink %q: %w", linkName, err)
			}
			return nil
		}
		if removeErr := os.Remove(linkPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("removing stale formula symlink: %w", removeErr)
		}

	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("checking existing formula entry: %w", err)
	}

	if err := formulaSymlink(srcPath, linkPath); err != nil {
		if !isSymlinkPrivilegeError(err) {
			return fmt.Errorf("creating formula symlink %q → %q: %w", linkName, srcPath, err)
		}
		if err := writeManagedFormulaCopy(linkPath, linkName, srcPath, state); err != nil {
			return fmt.Errorf("creating managed formula copy %q → %q: %w", linkName, srcPath, err)
		}
		return nil
	}
	state.unmanage(linkName)
	if err := state.save(formulaDir); err != nil {
		return fmt.Errorf("dropping formula manifest ownership for symlink %q: %w", linkName, err)
	}
	return nil
}

func cleanStaleFormulaEntries(formulaDir string, winners map[string]string, state *formulaResolutionState) error {
	entries, err := os.ReadDir(formulaDir)
	if err != nil {
		return nil
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		if name == formulaManagedManifestName || name == formulaManagedLockName {
			continue
		}
		if e.IsDir() || !formula.IsTOMLFilename(name) {
			continue
		}
		seen[name] = true
		linkPath := filepath.Join(formulaDir, name)
		fi, err := os.Lstat(linkPath)
		if err != nil {
			state.unmanage(name)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			state.unmanage(name)
			if _, isWinner := winners[name]; !isWinner {
				if removeErr := os.Remove(linkPath); removeErr != nil && !os.IsNotExist(removeErr) {
					return fmt.Errorf("removing stale formula symlink %q: %w", name, removeErr)
				}
				continue
			}
			if _, statErr := os.Stat(linkPath); statErr != nil {
				if removeErr := os.Remove(linkPath); removeErr != nil && !os.IsNotExist(removeErr) {
					return fmt.Errorf("removing dangling formula symlink %q: %w", name, removeErr)
				}
			}
			continue
		}
		if _, managed := state.managed(name); !managed {
			continue
		}
		owned, err := state.ensureManagedCopyOwned(formulaDir, name, linkPath)
		if err != nil {
			return fmt.Errorf("checking managed formula copy %q: %w", name, err)
		}
		if !owned {
			continue
		}
		if _, isWinner := winners[name]; !isWinner {
			if removeErr := os.Remove(linkPath); removeErr != nil && !os.IsNotExist(removeErr) {
				return fmt.Errorf("removing stale managed formula copy %q: %w", name, removeErr)
			}
			state.unmanage(name)
		}
	}

	for name := range state.manifest.Entries {
		if seen[name] {
			continue
		}
		state.unmanage(name)
	}

	return state.save(formulaDir)
}

func tryReplaceManagedCopyWithSymlink(formulaDir, linkName, linkPath, srcPath string, state *formulaResolutionState) (bool, error) {
	owned, err := state.ensureManagedCopyOwned(formulaDir, linkName, linkPath)
	if err != nil {
		return false, fmt.Errorf("checking managed formula copy before symlink replacement: %w", err)
	}
	if !owned {
		return true, nil
	}
	tmpPath := filepath.Join(formulaDir, "."+linkName+".symlink-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := formulaSymlink(srcPath, tmpPath); err != nil {
		if isSymlinkPrivilegeError(err) {
			return false, nil
		}
		return false, err
	}
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup after replace failures
	if err := replacePath(tmpPath, linkPath); err != nil {
		return false, fmt.Errorf("replacing managed formula copy with symlink: %w", err)
	}
	state.unmanage(linkName)
	if err := state.save(formulaDir); err != nil {
		return false, fmt.Errorf("dropping formula manifest ownership after symlink replacement: %w", err)
	}
	return true, nil
}

func writeManagedFormulaCopy(linkPath, linkName, srcPath string, state *formulaResolutionState) error {
	formulaDir := filepath.Dir(linkPath)
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("reading formula source for copy fallback: %w", err)
	}
	sum := sha256Bytes(data)
	verifiedSum := ""
	if entry, managed := state.managed(linkName); managed {
		existing, readErr := os.ReadFile(linkPath)
		switch {
		case readErr == nil:
			existingSum := sha256Bytes(existing)
			if !entry.ownsHash(existingSum) {
				state.unmanage(linkName)
				if err := state.save(formulaDir); err != nil {
					return fmt.Errorf("dropping formula manifest ownership for %q: %w", linkName, err)
				}
				return nil
			}
			verifiedSum = existingSum
			if existingSum == sum {
				state.manage(linkName, srcPath, sum)
				if err := state.save(formulaDir); err != nil {
					return fmt.Errorf("recording managed formula copy %q: %w", linkName, err)
				}
				return nil
			}

		case os.IsNotExist(readErr):

		default:
			return fmt.Errorf("reading managed formula copy for ownership check: %w", readErr)
		}
	}
	state.managePending(linkName, srcPath, verifiedSum, sum)
	if err := state.save(formulaDir); err != nil {
		return fmt.Errorf("recording pending managed formula copy %q: %w", linkName, err)
	}
	if err := writeFileAtomic(linkPath, data, 0o644); err != nil {
		return fmt.Errorf("writing managed formula copy: %w", err)
	}
	state.manage(linkName, srcPath, sum)
	if err := state.save(formulaDir); err != nil {
		return fmt.Errorf("recording managed formula copy %q: %w", linkName, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // preserve write error
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close() //nolint:errcheck // preserve chmod error
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck // preserve sync error
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replacePath(tmpPath, path); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

func replacePath(src, dst string) error {
	srcp, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dstp, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(srcp, dstp, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func (e formulaManagedCopy) ownsHash(sum string) bool {
	return sum != "" && (e.SHA256 == sum || e.PendingSHA256 == sum)
}

func (s *formulaResolutionState) ensureManagedCopyOwned(formulaDir, name, path string) (bool, error) {
	entry, ok := s.managed(name)
	if !ok {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if entry.ownsHash(sha256Bytes(data)) {
		return true, nil
	}
	s.unmanage(name)
	if err := s.save(formulaDir); err != nil {
		return false, err
	}
	return false, nil
}

func (s *formulaResolutionState) managed(name string) (formulaManagedCopy, bool) {
	entry, ok := s.manifest.Entries[name]
	return entry, ok
}

func (s *formulaResolutionState) manage(name, srcPath, sum string) {
	entry := formulaManagedCopy{Source: srcPath, SHA256: sum}
	if existing, ok := s.manifest.Entries[name]; ok && existing == entry {
		return
	}
	s.manifest.Entries[name] = entry
	s.dirty = true
}

func (s *formulaResolutionState) managePending(name, srcPath, verifiedSum, pendingSum string) {
	entry := formulaManagedCopy{Source: srcPath, SHA256: verifiedSum, PendingSHA256: pendingSum}
	if existing, ok := s.manifest.Entries[name]; ok {
		if entry.SHA256 == "" {
			entry.SHA256 = existing.SHA256
		}
	}
	if entry.SHA256 == pendingSum {
		entry.PendingSHA256 = ""
	}
	if existing, ok := s.manifest.Entries[name]; ok && existing == entry {
		return
	}
	s.manifest.Entries[name] = entry
	s.dirty = true
}

func (s *formulaResolutionState) unmanage(name string) {
	if _, ok := s.manifest.Entries[name]; !ok {
		return
	}
	delete(s.manifest.Entries, name)
	s.dirty = true
}

func (s *formulaResolutionState) save(formulaDir string) error {
	if !s.dirty {
		return nil
	}
	path := formulaManifestPath(formulaDir)
	if len(s.manifest.Entries) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing empty formula manifest: %w", err)
		}
		s.dirty = false
		return nil
	}
	data, err := json.MarshalIndent(s.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding formula manifest: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("writing formula manifest: %w", err)
	}
	s.dirty = false
	return nil
}

func (s *formulaResolutionState) release() error {
	var err error
	if s.lock != nil {
		err = errors.Join(err, s.lock.Release())
		s.lock = nil
	}
	if s.lockFile != nil {
		err = errors.Join(err, s.lockFile.Close())
		s.lockFile = nil
	}
	return err
}

func formulaManifestPath(formulaDir string) string {
	return filepath.Join(formulaDir, formulaManagedManifestName)
}

func sha256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isSymlinkPrivilegeError(err error) bool {
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}
