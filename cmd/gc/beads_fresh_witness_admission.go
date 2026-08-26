package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	freshManagedDoltAdmissionName       = ".gascity-fresh-dolt-admission"
	freshManagedDoltAdmissionSchema     = 1
	freshManagedDoltAdmissionAwaitingBD = "awaiting_bd"
	freshManagedDoltAdmissionSealed     = "sealed"
)

type freshManagedDoltAdmission struct {
	Schema         int    `json:"schema"`
	State          string `json:"state"`
	ScopeRoot      string `json:"scope_root"`
	DataRoot       string `json:"data_root"`
	ConfigSHA256   string `json:"config_sha256"`
	MetadataSHA256 string `json:"metadata_sha256"`
	BDBinary       string `json:"bd_binary,omitempty"`
	BDSHA256       string `json:"bd_sha256,omitempty"`
}

// freshManagedDoltDesired resolves the canonical city config and database
// identity that a fresh admission must still represent at the instant it is
// bound. Production callers must reload rather than capture these values so a
// city.toml change during the selected-BD probe cannot seal stale intent.
type freshManagedDoltDesired func() (contract.ConfigState, string, error)

// prepareFreshManagedDoltWitnessAdmission publishes a complete rootless
// .beads directory in one rename. The staging directory contains canonical
// config/metadata plus hashes before it becomes visible, so a crash cannot
// leave an ambiguous half-written workspace for a later run to relabel.
// Existing non-empty rootless directories without a valid admission are never
// changed; they may be clones, remote bindings, legacy stores, or recoveries.
func prepareFreshManagedDoltWitnessAdmission(scopeRoot string, state contract.ConfigState, doltDatabase string) (bool, error) {
	scopeRoot = normalizePathForCompare(scopeRoot)
	beadsDir := filepath.Join(scopeRoot, ".beads")
	dataRoot := filepath.Join(beadsDir, "dolt")
	if _, err := os.Lstat(dataRoot); err == nil {
		active, err := validateExistingManagedDoltRootBeforeCanonicalization(scopeRoot, dataRoot)
		if err != nil || !active {
			return active, err
		}
		if err := validateFreshManagedDoltAdmissionMatchesDesired(scopeRoot, dataRoot, state, doltDatabase, true, true); err != nil {
			return false, err
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect managed Dolt root %s: %w", dataRoot, err)
	}

	if exists, empty, err := freshAdmissionTargetState(beadsDir); err != nil {
		return false, err
	} else if exists && !empty {
		if err := validateFreshManagedDoltAdmissionMatchesDesired(scopeRoot, dataRoot, state, doltDatabase, false, false); err != nil {
			return false, err
		}
		return true, nil
	}

	stageRoot, stageBeadsDir, configHash, metadataHash, err := stageFreshManagedDoltDesiredBeads(scopeRoot, state, doltDatabase)
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()
	record := freshManagedDoltAdmission{
		Schema:         freshManagedDoltAdmissionSchema,
		State:          freshManagedDoltAdmissionAwaitingBD,
		ScopeRoot:      scopeRoot,
		DataRoot:       dataRoot,
		ConfigSHA256:   configHash,
		MetadataSHA256: metadataHash,
	}
	if err := writeFreshManagedDoltAdmission(filepath.Join(stageBeadsDir, freshManagedDoltAdmissionName), record, true); err != nil {
		return false, err
	}
	for _, path := range []string{
		filepath.Join(stageBeadsDir, "config.yaml"),
		filepath.Join(stageBeadsDir, "metadata.json"),
		filepath.Join(stageBeadsDir, freshManagedDoltAdmissionName),
		stageBeadsDir,
	} {
		if err := syncPath(path); err != nil {
			return false, fmt.Errorf("sync staged fresh managed-Dolt state %s: %w", path, err)
		}
	}
	if err := publishFreshManagedDoltBeadsDirectory(scopeRoot, stageBeadsDir); err != nil {
		return false, err
	}
	published, err := validateFreshManagedDoltAdmission(scopeRoot, dataRoot, "", false, false)
	if err != nil {
		return false, err
	}
	if err := requireFreshManagedDoltDesiredHashes(published, configHash, metadataHash); err != nil {
		return false, err
	}
	return true, nil
}

func stageFreshManagedDoltDesiredBeads(scopeRoot string, state contract.ConfigState, doltDatabase string) (stageRoot, stageBeadsDir, configHash, metadataHash string, err error) {
	stageRoot, err = os.MkdirTemp(filepath.Dir(scopeRoot), ".gascity-fresh-beads-stage-")
	if err != nil {
		return "", "", "", "", fmt.Errorf("create fresh managed-Dolt staging directory: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(stageRoot)
		}
	}()
	stageScope := filepath.Join(stageRoot, "scope")
	if err := os.Mkdir(stageScope, beadsDirPerm); err != nil {
		return "", "", "", "", fmt.Errorf("create staged scope: %w", err)
	}
	if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, stageScope, state); err != nil {
		return "", "", "", "", fmt.Errorf("stage canonical managed config: %w", err)
	}
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, stageScope, doltDatabase); err != nil {
		return "", "", "", "", fmt.Errorf("stage canonical managed metadata: %w", err)
	}
	stageBeadsDir = filepath.Join(stageScope, ".beads")
	configHash, err = regularFileSHA256(filepath.Join(stageBeadsDir, "config.yaml"))
	if err != nil {
		return "", "", "", "", fmt.Errorf("hash staged managed config: %w", err)
	}
	metadataHash, err = regularFileSHA256(filepath.Join(stageBeadsDir, "metadata.json"))
	if err != nil {
		return "", "", "", "", fmt.Errorf("hash staged managed metadata: %w", err)
	}
	complete = true
	return stageRoot, stageBeadsDir, configHash, metadataHash, nil
}

func validateFreshManagedDoltAdmissionMatchesDesired(scopeRoot, dataRoot string, state contract.ConfigState, doltDatabase string, allowCreatedRoot, requireSealed bool) error {
	stageRoot, _, configHash, metadataHash, err := stageFreshManagedDoltDesiredBeads(scopeRoot, state, doltDatabase)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()
	record, err := validateFreshManagedDoltAdmission(scopeRoot, dataRoot, "", allowCreatedRoot, requireSealed)
	if err != nil {
		return err
	}
	return requireFreshManagedDoltDesiredHashes(record, configHash, metadataHash)
}

func requireFreshManagedDoltDesiredHashes(record freshManagedDoltAdmission, configHash, metadataHash string) error {
	if record.ConfigSHA256 != configHash || record.MetadataSHA256 != metadataHash {
		return fmt.Errorf("fresh managed-Dolt admission does not match the current desired canonical config and metadata")
	}
	return nil
}

// validateExistingManagedDoltRootBeforeCanonicalization is the read-only gate
// in front of config/metadata normalization. An established local store must
// already carry beads' current-era witness; otherwise normalization could
// rewrite migration evidence (including issues.jsonl) before the provider
// script gets a chance to refuse the legacy root. The one exception is the
// narrowly admitted post-root crash state, which is already canonical and is
// returned as an active admission so its callers skip all writers.
func validateExistingManagedDoltRootBeforeCanonicalization(scopeRoot, dataRoot string) (bool, error) {
	beadsDir := filepath.Join(scopeRoot, ".beads")
	beadsInfo, err := os.Lstat(beadsDir)
	if err != nil {
		return false, fmt.Errorf("inspect existing managed beads directory %s: %w", beadsDir, err)
	}
	if beadsInfo.Mode()&os.ModeSymlink != 0 || !beadsInfo.IsDir() {
		return false, fmt.Errorf("refusing to normalize existing managed Dolt storage through non-directory or symlinked path %s", beadsDir)
	}
	rootInfo, err := os.Lstat(dataRoot)
	if err != nil {
		return false, fmt.Errorf("inspect existing managed Dolt root %s: %w", dataRoot, err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return false, fmt.Errorf("refusing to normalize non-directory or symlinked managed Dolt root %s", dataRoot)
	}
	admissionPath := filepath.Join(beadsDir, freshManagedDoltAdmissionName)
	if _, err := os.Lstat(admissionPath); err == nil {
		if _, err := validateFreshManagedDoltAdmission(scopeRoot, dataRoot, "", true, true); err != nil {
			return false, err
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect existing managed-Dolt admission: %w", err)
	}
	witness := filepath.Join(beadsDir, ".local_version")
	if !currentBeadsWitness(witness) {
		return false, fmt.Errorf("refusing to normalize existing managed Dolt root without a valid current-era witness at %s", witness)
	}
	return false, nil
}

func freshAdmissionTargetState(beadsDir string) (exists, empty bool, err error) {
	info, err := os.Lstat(beadsDir)
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("inspect managed beads directory %s: %w", beadsDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return true, false, fmt.Errorf("refusing fresh managed Dolt admission for non-directory or symlinked path %s", beadsDir)
	}
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return true, false, fmt.Errorf("read managed beads directory %s: %w", beadsDir, err)
	}
	return true, len(entries) == 0, nil
}

func publishFreshManagedDoltBeadsDirectory(scopeRoot, stageBeadsDir string) error {
	beadsDir := filepath.Join(scopeRoot, ".beads")
	// Serialize publication on the stable scope directory inode. Some
	// platforms cannot rename a directory over an existing empty directory;
	// under this lock we may remove that empty placeholder without another Gas
	// City creator observing or acting on the transient path gap.
	scopeLock, err := os.Open(scopeRoot)
	if err != nil {
		return fmt.Errorf("open managed scope for fresh publication lock: %w", err)
	}
	defer scopeLock.Close() //nolint:errcheck
	if err := syscall.Flock(int(scopeLock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock managed scope for fresh publication: %w", err)
	}
	defer syscall.Flock(int(scopeLock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	if exists, empty, err := freshAdmissionTargetState(beadsDir); err != nil {
		return err
	} else if exists {
		if !empty {
			if _, validateErr := validateFreshManagedDoltAdmission(scopeRoot, filepath.Join(beadsDir, "dolt"), "", false, false); validateErr == nil {
				return nil
			}
			return fmt.Errorf("refusing to replace non-empty managed beads directory %s", beadsDir)
		}
		if err := os.Remove(beadsDir); err != nil {
			return fmt.Errorf("remove serialized empty managed beads directory before publication: %w", err)
		}
	}
	if err := os.Rename(stageBeadsDir, beadsDir); err != nil {
		if _, validateErr := validateFreshManagedDoltAdmission(scopeRoot, filepath.Join(beadsDir, "dolt"), "", false, false); validateErr == nil {
			return nil
		}
		return fmt.Errorf("publish fresh managed beads directory %s: %w", beadsDir, err)
	}
	if err := syncPath(scopeRoot); err != nil {
		return fmt.Errorf("sync fresh managed beads parent %s: %w", scopeRoot, err)
	}
	return nil
}

// bindFreshManagedDoltAdmissionToBD completes the admission immediately before
// the provider subprocess starts. This uses the exact BD_BIN projected into
// that subprocess, preventing a different executable from minting the witness.
func bindFreshManagedDoltAdmissionToBD(scopeRoot, selectedBD string, desired freshManagedDoltDesired) error {
	if desired == nil {
		return fmt.Errorf("bind fresh managed-Dolt admission without current desired state")
	}
	admissionPath := filepath.Join(scopeRoot, ".beads", freshManagedDoltAdmissionName)
	if _, err := os.Lstat(admissionPath); os.IsNotExist(err) {
		// A concurrent endpoint transition may have removed the awaiting
		// admission after the caller's first topology check. Re-resolve desired
		// state at this exact no-admission boundary before allowing start.
		_, _, desiredErr := desired()
		if desiredErr != nil {
			return fmt.Errorf("revalidate desired state after fresh managed-Dolt admission disappeared: %w", desiredErr)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect fresh managed-Dolt admission: %w", err)
	}
	// Lock the admission inode before reading its state. The sealed update is
	// an atomic rename, so a contender that opened the old inode waits here and
	// then re-reads the new path; a contender that opens the new inode can only
	// observe the already-sealed record. No two selected executables can both
	// transition the same awaiting record.
	lockFile, err := os.Open(admissionPath)
	if err != nil {
		return fmt.Errorf("open fresh managed-Dolt admission for binding: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock fresh managed-Dolt admission for binding: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	dataRoot := filepath.Join(scopeRoot, ".beads", "dolt")
	// If the provider previously crashed after creating the admitted root but
	// before consuming the admission, never turn an awaiting record into a
	// sealed one here. Only the already-sealed, exact-BD, witness-backed crash
	// state may resume. The provider script will perform the same validation
	// immediately before removing the admission.
	if _, err := os.Lstat(dataRoot); err == nil {
		if err := validateFreshManagedDoltAdmissionAgainstDesired(scopeRoot, dataRoot, desired, true, true); err != nil {
			return err
		}
		_, err = validateFreshManagedDoltAdmission(scopeRoot, dataRoot, selectedBD, true, true)
		return err
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect fresh managed Dolt root before binding: %w", err)
	}
	record, err := validateFreshManagedDoltAdmission(scopeRoot, dataRoot, "", false, false)
	if err != nil {
		return err
	}
	if err := validateFreshManagedDoltAdmissionAgainstDesired(scopeRoot, dataRoot, desired, false, false); err != nil {
		return err
	}
	bdPath, bdHash, err := selectedCurrentBDBinaryIdentity(selectedBD)
	if err != nil {
		return err
	}
	// The version probe executes an external binary and may take several
	// seconds. Reload and compare desired state again while the admission lock
	// is still held so a concurrent config edit cannot seal the old hashes.
	if err := validateFreshManagedDoltAdmissionAgainstDesired(scopeRoot, dataRoot, desired, false, false); err != nil {
		return err
	}
	if record.State == freshManagedDoltAdmissionSealed {
		if record.BDBinary != bdPath || record.BDSHA256 != bdHash {
			return fmt.Errorf("fresh managed-Dolt admission is bound to a different bd executable")
		}
		return nil
	}
	record.State = freshManagedDoltAdmissionSealed
	record.BDBinary = bdPath
	record.BDSHA256 = bdHash
	if err := writeFreshManagedDoltAdmission(admissionPath, record, false); err != nil {
		return err
	}
	_, err = validateFreshManagedDoltAdmission(scopeRoot, dataRoot, selectedBD, false, true)
	return err
}

func validateFreshManagedDoltAdmissionAgainstDesired(scopeRoot, dataRoot string, desired freshManagedDoltDesired, allowCreatedRoot, requireSealed bool) error {
	state, doltDatabase, err := desired()
	if err != nil {
		return fmt.Errorf("resolve current desired fresh managed-Dolt state: %w", err)
	}
	if err := validateFreshManagedDoltAdmissionMatchesDesired(scopeRoot, dataRoot, state, doltDatabase, allowCreatedRoot, requireSealed); err != nil {
		return fmt.Errorf("validate fresh managed-Dolt admission against current desired state: %w", err)
	}
	return nil
}

// freshManagedDoltExternalTransitionGuard serializes a managed-to-external
// endpoint transition against admission binding. The stable lock remains held
// across canonical config writes and any rollback; the admission itself is
// never delegated to the generic snapshot restorer.
type freshManagedDoltExternalTransitionGuard struct {
	scopeRoot    string
	path         string
	file         *os.File
	original     []byte
	originalHash string
}

func lockAwaitingFreshManagedDoltAdmissionForExternalTransition(
	scopeRoot string,
	state contract.ConfigState,
	doltDatabase string,
) (*freshManagedDoltExternalTransitionGuard, error) {
	admissionPath := filepath.Join(scopeRoot, ".beads", freshManagedDoltAdmissionName)
	for attempts := 0; attempts < 3; attempts++ {
		if _, err := os.Lstat(admissionPath); os.IsNotExist(err) {
			return nil, nil
		} else if err != nil {
			return nil, fmt.Errorf("inspect fresh managed-Dolt admission for endpoint transition: %w", err)
		}
		lockFile, err := os.Open(admissionPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("open fresh managed-Dolt admission for endpoint transition: %w", err)
		}
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
			_ = lockFile.Close()
			return nil, fmt.Errorf("lock fresh managed-Dolt admission for endpoint transition: %w", err)
		}
		lockedInfo, statErr := lockFile.Stat()
		currentInfo, pathErr := os.Lstat(admissionPath)
		if statErr != nil || pathErr != nil || !os.SameFile(lockedInfo, currentInfo) {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			if pathErr != nil && !os.IsNotExist(pathErr) {
				return nil, fmt.Errorf("restat fresh managed-Dolt admission for endpoint transition: %w", pathErr)
			}
			continue
		}

		dataRoot := filepath.Join(scopeRoot, ".beads", "dolt")
		record, err := validateFreshManagedDoltAdmission(scopeRoot, dataRoot, "", false, false)
		if err != nil {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			return nil, fmt.Errorf("validate fresh managed-Dolt admission for endpoint transition: %w", err)
		}
		if record.State != freshManagedDoltAdmissionAwaitingBD {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			return nil, fmt.Errorf("refusing endpoint transition while fresh managed-Dolt admission is %q", record.State)
		}
		if err := validateFreshManagedDoltAdmissionMatchesDesired(scopeRoot, dataRoot, state, doltDatabase, false, false); err != nil {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			return nil, fmt.Errorf("validate fresh managed-Dolt admission against pre-transition desired state: %w", err)
		}
		original, err := os.ReadFile(admissionPath)
		if err != nil {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			return nil, fmt.Errorf("read locked fresh managed-Dolt admission: %w", err)
		}
		originalHash, err := regularFileSHA256(admissionPath)
		if err != nil {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			return nil, err
		}
		return &freshManagedDoltExternalTransitionGuard{
			scopeRoot:    scopeRoot,
			path:         admissionPath,
			file:         lockFile,
			original:     original,
			originalHash: originalHash,
		}, nil
	}
	return nil, fmt.Errorf("fresh managed-Dolt admission changed repeatedly during endpoint transition")
}

func (g *freshManagedDoltExternalTransitionGuard) release() {
	if g == nil || g.file == nil {
		return
	}
	_ = syscall.Flock(int(g.file.Fd()), syscall.LOCK_UN)
	_ = g.file.Close()
	g.file = nil
}

func (g *freshManagedDoltExternalTransitionGuard) discard() error {
	if g == nil || g.file == nil {
		return fmt.Errorf("fresh managed-Dolt endpoint transition guard is not held")
	}
	lockedInfo, err := g.file.Stat()
	if err != nil {
		return fmt.Errorf("stat locked fresh managed-Dolt admission before discard: %w", err)
	}
	currentInfo, err := os.Lstat(g.path)
	if err != nil {
		return fmt.Errorf("restat fresh managed-Dolt admission before discard: %w", err)
	}
	if !os.SameFile(lockedInfo, currentInfo) {
		return fmt.Errorf("fresh managed-Dolt admission changed during endpoint transition")
	}
	currentHash, err := regularFileSHA256(g.path)
	if err != nil {
		return err
	}
	if currentHash != g.originalHash {
		return fmt.Errorf("fresh managed-Dolt admission contents changed during endpoint transition")
	}
	beadsDir := filepath.Dir(g.path)
	for _, path := range []string{filepath.Join(beadsDir, ".local_version"), filepath.Join(beadsDir, "dolt")} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to discard fresh managed-Dolt admission after provider artifacts appeared at %s", path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect provider artifact before admission discard %s: %w", path, err)
		}
	}
	if err := os.Remove(g.path); err != nil {
		return fmt.Errorf("discard unstarted fresh managed-Dolt admission: %w", err)
	}
	if err := syncPath(beadsDir); err != nil {
		restoreErr := writeFileAtomicNoFollow(g.path, g.original, 0o600)
		if restoreErr == nil {
			restoreErr = syncPath(beadsDir)
		}
		if restoreErr != nil {
			return fmt.Errorf("sync fresh managed-Dolt admission discard: %w (exact restore failed: %w)", err, restoreErr)
		}
		return fmt.Errorf("sync fresh managed-Dolt admission discard: %w", err)
	}
	return nil
}

// freshManagedDoltAdmissionProvesProviderUnstarted recognizes only the strict
// rootless admission states. Whether awaiting or sealed, absence of both the
// witness and data root proves the provider shell never reached its pre-op root
// setup, so a stop operation has nothing to stop and must not be invoked.
func freshManagedDoltAdmissionProvesProviderUnstarted(scopeRoot string) (bool, error) {
	admissionPath := filepath.Join(scopeRoot, ".beads", freshManagedDoltAdmissionName)
	if _, err := os.Lstat(admissionPath); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect fresh managed-Dolt admission before stop: %w", err)
	}
	dataRoot := filepath.Join(scopeRoot, ".beads", "dolt")
	if _, err := validateFreshManagedDoltAdmission(scopeRoot, dataRoot, "", false, false); err != nil {
		return false, fmt.Errorf("validate fresh managed-Dolt admission before stop: %w", err)
	}
	return true, nil
}

// validateFreshManagedDoltWitnessAdmission is the read-only half invoked by
// gc-beads-bd immediately before witness installation and while resuming the
// narrow witness-created/root-created crash states.
func validateFreshManagedDoltWitnessAdmission(scopeRoot, dataRoot, selectedBD string, allowCreatedRoot bool) error {
	_, err := validateFreshManagedDoltAdmission(scopeRoot, dataRoot, selectedBD, allowCreatedRoot, true)
	return err
}

func validateFreshManagedDoltAdmission(scopeRoot, dataRoot, selectedBD string, allowCreatedRoot, requireSealed bool) (freshManagedDoltAdmission, error) {
	scopeRoot = normalizePathForCompare(scopeRoot)
	dataRoot = normalizePathForCompare(dataRoot)
	beadsDir := filepath.Join(scopeRoot, ".beads")
	defaultRoot := filepath.Join(beadsDir, "dolt")
	if !samePath(dataRoot, defaultRoot) {
		return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed-Dolt admission is scoped to %s, not %s", defaultRoot, dataRoot)
	}
	info, err := os.Lstat(beadsDir)
	if err != nil {
		return freshManagedDoltAdmission{}, fmt.Errorf("inspect managed beads directory %s: %w", beadsDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return freshManagedDoltAdmission{}, fmt.Errorf("managed beads path is not a real directory: %s", beadsDir)
	}
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return freshManagedDoltAdmission{}, fmt.Errorf("read managed beads directory %s: %w", beadsDir, err)
	}
	seen := map[string]bool{}
	rootPresent := false
	witnessPresent := false
	var witnessInfo os.FileInfo
	var witnessTemps []struct {
		path string
		info os.FileInfo
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(beadsDir, name)
		entryInfo, err := os.Lstat(path)
		if err != nil {
			return freshManagedDoltAdmission{}, fmt.Errorf("inspect fresh managed artifact %s: %w", path, err)
		}
		switch name {
		case freshManagedDoltAdmissionName, "config.yaml", "metadata.json":
			if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() {
				return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed artifact is not a regular non-symlink file: %s", path)
			}
			seen[name] = true
		case ".local_version":
			if !currentBeadsWitness(path) {
				return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed-Dolt resume has an invalid current-era witness at %s", path)
			}
			witnessPresent = true
			witnessInfo = entryInfo
		case "dolt":
			if !allowCreatedRoot || entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
				return freshManagedDoltAdmission{}, fmt.Errorf("unexpected managed Dolt root while validating fresh admission: %s", path)
			}
			rootEntries, err := os.ReadDir(path)
			if err != nil {
				return freshManagedDoltAdmission{}, fmt.Errorf("read freshly admitted managed Dolt root %s: %w", path, err)
			}
			if len(rootEntries) != 0 {
				return freshManagedDoltAdmission{}, fmt.Errorf("freshly admitted managed Dolt root is not empty: %s", path)
			}
			rootPresent = true
		default:
			if validFreshWitnessTempName(name) && entryInfo.Mode()&os.ModeSymlink == 0 && entryInfo.Mode().IsRegular() {
				witnessTemps = append(witnessTemps, struct {
					path string
					info os.FileInfo
				}{path: path, info: entryInfo})
				continue
			}
			return freshManagedDoltAdmission{}, fmt.Errorf("refusing fresh managed-Dolt admission beside unrecognized artifact %s", path)
		}
	}
	for _, required := range []string{freshManagedDoltAdmissionName, "config.yaml", "metadata.json"} {
		if !seen[required] {
			return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed-Dolt admission is missing %s", filepath.Join(beadsDir, required))
		}
	}
	if rootPresent && !witnessPresent {
		return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed Dolt root exists without a current-era witness")
	}
	if len(witnessTemps) > 0 {
		if !witnessPresent {
			return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed-Dolt witness temp exists without an installed witness")
		}
		for _, temp := range witnessTemps {
			if !os.SameFile(witnessInfo, temp.info) {
				return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed-Dolt witness temp is not a hard link to the installed witness: %s", temp.path)
			}
		}
	}
	if allowCreatedRoot && !rootPresent {
		return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed Dolt root has not been created")
	}
	record, err := readFreshManagedDoltAdmission(filepath.Join(beadsDir, freshManagedDoltAdmissionName))
	if err != nil {
		return freshManagedDoltAdmission{}, err
	}
	if record.ScopeRoot != scopeRoot || record.DataRoot != defaultRoot {
		return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed-Dolt admission path binding does not match this city")
	}
	if err := validateSHA256(record.ConfigSHA256, "config"); err != nil {
		return freshManagedDoltAdmission{}, err
	}
	if err := validateSHA256(record.MetadataSHA256, "metadata"); err != nil {
		return freshManagedDoltAdmission{}, err
	}
	configHash, err := regularFileSHA256(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		return freshManagedDoltAdmission{}, fmt.Errorf("verify fresh managed config: %w", err)
	}
	metadataHash, err := regularFileSHA256(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return freshManagedDoltAdmission{}, fmt.Errorf("verify fresh managed metadata: %w", err)
	}
	if configHash != record.ConfigSHA256 || metadataHash != record.MetadataSHA256 {
		return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed-Dolt admission no longer matches canonical config/metadata")
	}
	switch record.State {
	case freshManagedDoltAdmissionAwaitingBD:
		if witnessPresent || len(witnessTemps) != 0 || rootPresent {
			return freshManagedDoltAdmission{}, fmt.Errorf("unsealed fresh managed-Dolt admission contains post-binding artifacts")
		}
		if requireSealed {
			return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed-Dolt admission is not bound to the selected bd executable")
		}
		if record.BDBinary != "" || record.BDSHA256 != "" {
			return freshManagedDoltAdmission{}, fmt.Errorf("unsealed fresh managed-Dolt admission carries a partial bd binding")
		}
	case freshManagedDoltAdmissionSealed:
		if err := validateSHA256(record.BDSHA256, "bd executable"); err != nil {
			return freshManagedDoltAdmission{}, err
		}
		if strings.TrimSpace(record.BDBinary) == "" || !filepath.IsAbs(record.BDBinary) {
			return freshManagedDoltAdmission{}, fmt.Errorf("fresh managed-Dolt admission has an invalid bd executable path")
		}
		if selectedBD != "" {
			bdPath, bdHash, err := selectedBDBinaryIdentity(selectedBD)
			if err != nil {
				return freshManagedDoltAdmission{}, err
			}
			if record.BDBinary != bdPath || record.BDSHA256 != bdHash {
				return freshManagedDoltAdmission{}, fmt.Errorf("selected bd executable does not match fresh managed-Dolt admission")
			}
		}
	default:
		return freshManagedDoltAdmission{}, fmt.Errorf("unknown fresh managed-Dolt admission state %q", record.State)
	}
	return record, nil
}

func validFreshWitnessTempName(name string) bool {
	const prefix = ".local_version.tmp."
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+6 {
		return false
	}
	for _, ch := range strings.TrimPrefix(name, prefix) {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

func readFreshManagedDoltAdmission(path string) (freshManagedDoltAdmission, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return freshManagedDoltAdmission{}, fmt.Errorf("inspect fresh managed-Dolt admission %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4096 {
		return freshManagedDoltAdmission{}, fmt.Errorf("invalid fresh managed-Dolt admission file at %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return freshManagedDoltAdmission{}, fmt.Errorf("read fresh managed-Dolt admission %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck
	decoder := json.NewDecoder(bufio.NewReader(io.LimitReader(file, 4097)))
	decoder.DisallowUnknownFields()
	var record freshManagedDoltAdmission
	if err := decoder.Decode(&record); err != nil {
		return freshManagedDoltAdmission{}, fmt.Errorf("parse fresh managed-Dolt admission %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return freshManagedDoltAdmission{}, fmt.Errorf("parse fresh managed-Dolt admission %s: trailing data", path)
	}
	if record.Schema != freshManagedDoltAdmissionSchema {
		return freshManagedDoltAdmission{}, fmt.Errorf("unsupported fresh managed-Dolt admission schema %d", record.Schema)
	}
	return record, nil
}

func writeFreshManagedDoltAdmission(path string, record freshManagedDoltAdmission, exclusive bool) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode fresh managed-Dolt admission: %w", err)
	}
	data = append(data, '\n')
	if exclusive {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create fresh managed-Dolt admission %s: %w", path, err)
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			return fmt.Errorf("write fresh managed-Dolt admission %s: %w", path, err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync fresh managed-Dolt admission %s: %w", path, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close fresh managed-Dolt admission %s: %w", path, err)
		}
		return nil
	}
	if err := writeFileAtomicNoFollow(path, data, 0o600); err != nil {
		return fmt.Errorf("seal fresh managed-Dolt admission: %w", err)
	}
	return nil
}

func selectedBDBinaryIdentity(selected string) (string, string, error) {
	selected = strings.TrimSpace(selected)
	if selected == "" {
		selected = "bd"
	}
	path, err := exec.LookPath(selected)
	if err != nil {
		return "", "", fmt.Errorf("resolve selected bd executable %q: %w", selected, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve selected bd path %q: %w", path, err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	hash, err := regularFileSHA256(path)
	if err != nil {
		return "", "", fmt.Errorf("hash selected bd executable %s: %w", path, err)
	}
	return filepath.Clean(path), hash, nil
}

// selectedCurrentBDBinaryIdentity refuses to seal a fresh-layout admission for
// a legacy or unidentifiable bd executable. Freshness proves that no old data
// is being relabeled; it does not prove that an ambient bd will create the
// current storage era. The provider shell independently repeats this check at
// the final witness boundary.
func selectedCurrentBDBinaryIdentity(selected string) (string, string, error) {
	path, hashBefore, err := selectedBDBinaryIdentity(selected)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", "", fmt.Errorf("inspect selected bd version %s: %w", path, ctx.Err())
		}
		return "", "", fmt.Errorf("inspect selected bd version %s: %w", path, err)
	}
	version := bdVersionTokenFromOutput(string(out))
	if !currentBeadsVersion(version) {
		return "", "", fmt.Errorf("selected bd executable %s did not report a current-major version", path)
	}
	hashAfter, err := regularFileSHA256(path)
	if err != nil {
		return "", "", fmt.Errorf("rehash selected bd executable %s after version check: %w", path, err)
	}
	if hashAfter != hashBefore {
		return "", "", fmt.Errorf("selected bd executable changed during version check: %s", path)
	}
	return path, hashAfter, nil
}

func bdVersionTokenFromOutput(output string) string {
	line := strings.TrimRight(output, "\r\n")
	if newline := strings.IndexAny(line, "\r\n"); newline >= 0 {
		line = line[:newline]
	}
	const prefix = "bd version "
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	version := strings.TrimPrefix(line, prefix)
	if separator := strings.IndexAny(version, " \t"); separator >= 0 {
		version = version[:separator]
	}
	if !currentBeadsVersion(version) {
		return ""
	}
	return version
}

func currentBeadsVersion(value string) bool {
	value = strings.TrimPrefix(value, "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	major, err := strconv.Atoi(parts[0])
	return err == nil && major >= 1
}

func validateSHA256(value, label string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("invalid %s hash in fresh managed-Dolt admission", label)
	}
	return nil
}

func regularFileSHA256(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular non-symlink file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func currentBeadsWitness(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64 {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > 64 {
		return false
	}
	return currentBeadsVersion(strings.TrimRight(string(data), "\n"))
}

func syncPath(path string) error {
	if info, err := os.Stat(path); err != nil {
		return err
	} else if runtime.GOOS == "windows" && info.IsDir() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck
	return file.Sync()
}

func writeFileAtomicNoFollow(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gascity-fresh-admission-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return syncPath(dir)
}
