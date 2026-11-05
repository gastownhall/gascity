package storebinding

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrManifestConflict reports a durable record that cannot be replaced by the
// one offered: a different attempt, or a journal that is not an append-only
// continuation of the recorded one.
var ErrManifestConflict = errors.New("storage migration manifest conflict")

const (
	manifestDirectoryName = "storage"
	activeManifestName    = "active-manifest.json"
	attemptPrefix         = "attempt-"
	attemptSuffix         = ".json"
	manifestDirMode       = 0o700
	manifestFileMode      = 0o600
)

// ManifestStore is the durable home of the attempt records and the active
// manifest. Every write is an atomic replacement followed by a parent-directory
// fsync, so a torn write is never observable and a rename that survives is
// durable.
//
// The store holds no lock of its own. Startup takes the advisory lock on
// the stable .gc directory inode before it loads anything (AcquireMigrationGuard);
// the manifest files are atomically replaced and therefore change inode, which is
// exactly why they must never be lock targets themselves.
type ManifestStore struct {
	directory string
}

// OpenManifestStore prepares the durable manifest directory under one city .gc
// directory. It creates nothing else and opens no provider.
func OpenManifestStore(gcDirectory string) (*ManifestStore, error) {
	if strings.TrimSpace(gcDirectory) == "" {
		return nil, fmt.Errorf("%w: no city directory", ErrInvalidManifest)
	}
	directory := filepath.Join(filepath.Clean(gcDirectory), manifestDirectoryName)
	if err := os.MkdirAll(directory, manifestDirMode); err != nil {
		return nil, fmt.Errorf("creating storage manifest directory: %w", err)
	}
	if err := syncDirectory(filepath.Dir(directory)); err != nil {
		return nil, err
	}
	return &ManifestStore{directory: directory}, nil
}

// Directory returns the durable manifest directory.
func (s *ManifestStore) Directory() string { return s.directory }

// LoadActive reads the active manifest. The found flag is separate from the
// error so an absent manifest — genesis — is never confused with an unreadable
// one.
func (s *ManifestStore) LoadActive() (*ActiveManifest, bool, error) {
	payload, found, err := s.read(activeManifestName)
	if err != nil || !found {
		return nil, found, err
	}
	manifest, err := decodeActiveManifest(payload)
	if err != nil {
		return nil, true, fmt.Errorf("reading active manifest: %w", err)
	}
	return manifest, true, nil
}

// SaveActive atomically replaces the active manifest.
func (s *ManifestStore) SaveActive(manifest *ActiveManifest) error {
	payload, err := encodeActiveManifest(manifest)
	if err != nil {
		return err
	}
	if existing, found, err := s.LoadActive(); err != nil {
		return err
	} else if found && existing.Generation > manifest.Generation {
		return fmt.Errorf("%w: durable active generation %d is ahead of %d", ErrManifestConflict, existing.Generation, manifest.Generation)
	}
	return s.replace(activeManifestName, payload)
}

// LoadAttempt reads the attempt record for one generation.
func (s *ManifestStore) LoadAttempt(generation Generation) (*AttemptRecord, bool, error) {
	if !generation.Valid() {
		return nil, false, fmt.Errorf("%w: generation zero is never an attempt", ErrInvalidManifest)
	}
	payload, found, err := s.read(attemptFileName(generation))
	if err != nil || !found {
		return nil, found, err
	}
	record, err := decodeAttemptRecord(payload)
	if err != nil {
		return nil, true, fmt.Errorf("reading attempt record for generation %d: %w", generation, err)
	}
	return record, true, nil
}

// LatestAttempt reads the attempt record for the highest recorded generation.
// Startup uses it to find an unfinished saga before it considers new
// configuration.
func (s *ManifestStore) LatestAttempt() (*AttemptRecord, bool, error) {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return nil, false, fmt.Errorf("reading storage manifest directory: %w", err)
	}
	var latest Generation
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		generation, ok := attemptGeneration(entry.Name())
		if ok && generation > latest {
			latest = generation
		}
	}
	if !latest.Valid() {
		return nil, false, nil
	}
	return s.LoadAttempt(latest)
}

// SaveAttempt atomically replaces the attempt record for its generation. It
// refuses a record that is not an append-only continuation of the durable one:
// a stale in-memory record that lost a phase transition would otherwise
// overwrite a further-advanced record and report success.
func (s *ManifestStore) SaveAttempt(record *AttemptRecord) error {
	payload, err := encodeAttemptRecord(record)
	if err != nil {
		return err
	}
	existing, found, err := s.LoadAttempt(record.Intent.DesiredGeneration)
	if err != nil {
		return err
	}
	if found {
		if err := continuesDurableAttempt(existing, record); err != nil {
			return err
		}
	}
	return s.replace(attemptFileName(record.Intent.DesiredGeneration), payload)
}

// continuesDurableAttempt proves that a candidate record only extends the
// durable one. The journal is append-only even across the PREPARED-to-
// PREPARING return, so prefix equality is the exact test for phase transitions —
// but it is not sufficient on its own. Destination residue, guard receipts,
// guard releases, and participant receipts are all appended under the journal
// entry that is already current, so a stale clone taken before one of those
// appends carries a byte-identical journal. Each of them records that something
// irreversible happened to a store, so each is checked separately.
func continuesDurableAttempt(durable, candidate *AttemptRecord) error {
	if durable.Intent.Attempt != candidate.Intent.Attempt {
		return fmt.Errorf("%w: generation %d already records attempt %q", ErrManifestConflict, durable.Intent.DesiredGeneration, durable.Intent.Attempt)
	}
	if durable.Intent.ConfigDigest != candidate.Intent.ConfigDigest {
		return fmt.Errorf("%w: attempt %q already records a different config digest", ErrManifestConflict, durable.Intent.Attempt)
	}
	if len(candidate.Journal) < len(durable.Journal) {
		return fmt.Errorf("%w: candidate journal has %d entries, the durable record has %d", ErrManifestConflict, len(candidate.Journal), len(durable.Journal))
	}
	for index, entry := range durable.Journal {
		if candidate.Journal[index] != entry {
			return fmt.Errorf("%w: candidate journal rewrites durable entry %d", ErrManifestConflict, entry.Sequence)
		}
	}
	if durable.Decision != nil && candidate.Decision == nil {
		return fmt.Errorf("%w: candidate drops the durable commit decision", ErrManifestConflict)
	}
	if durable.Decision != nil && candidate.Decision != nil && durable.Decision.Decision != candidate.Decision.Decision {
		return fmt.Errorf("%w: the commit decision is immutable", ErrManifestConflict)
	}
	if err := continuesDurableResidue(durable, candidate); err != nil {
		return err
	}
	if err := continuesDurableReleases(durable, candidate); err != nil {
		return err
	}
	if err := continuesDurableReceipts(durable, candidate); err != nil {
		return err
	}
	return continuesDurableGuards(durable, candidate)
}

// continuesDurableResidue proves the candidate still knows about every
// destination the durable record recorded as mutated, and knows about it at
// least as far along. Residue is upgraded in place — reserved to initialized to
// written — so the test is monotonicity per destination rather than prefix
// equality: a downgrade would let recovery discard a destination that already
// holds logical records.
func continuesDurableResidue(durable, candidate *AttemptRecord) error {
	if len(durable.Residue) == 0 {
		return nil
	}
	known := make(map[residueKey]ResidueKind, len(candidate.Residue))
	for _, residue := range candidate.Residue {
		key := newResidueKey(residue)
		if kind, exists := known[key]; !exists || residue.Kind > kind {
			known[key] = residue.Kind
		}
	}
	for _, residue := range durable.Residue {
		kind, exists := known[newResidueKey(residue)]
		if !exists {
			return fmt.Errorf("%w: candidate drops the durable %s residue on %q under journal entry %d", ErrManifestConflict, residue.Kind, residue.Component, residue.Entry)
		}
		if kind < residue.Kind {
			return fmt.Errorf("%w: candidate downgrades %q from %s to %s under journal entry %d", ErrManifestConflict, residue.Component, residue.Kind, kind, residue.Entry)
		}
	}
	return nil
}

type residueKey struct {
	entry    uint64
	binding  BindingName
	physical PhysicalIdentity
}

func newResidueKey(residue DestinationResidue) residueKey {
	return residueKey{entry: residue.Entry, binding: residue.Binding, physical: residue.PhysicalIdentity}
}

// continuesDurableReleases proves the candidate carries every durable release.
// A release is the only thing that tells a guard that was installed and taken
// off from one that was never installed, and it survives every phase transition
// including the return to PREPARING, so nothing may ever drop one.
func continuesDurableReleases(durable, candidate *AttemptRecord) error {
	if len(durable.ReleasedGuards) == 0 {
		return nil
	}
	known := make(map[GuardRelease]struct{}, len(candidate.ReleasedGuards))
	for _, release := range candidate.ReleasedGuards {
		known[release] = struct{}{}
	}
	for _, release := range durable.ReleasedGuards {
		if _, exists := known[release]; !exists {
			return fmt.Errorf("%w: candidate drops the durable release of %q receipt %s", ErrManifestConflict, release.Component, release.ReceiptID)
		}
	}
	return nil
}

// continuesDurableReceipts proves the candidate carries every durable
// participant receipt. Receipts are post-decision, where recovery only rolls
// forward, so a dropped receipt would replay a commit the provider already
// completed.
func continuesDurableReceipts(durable, candidate *AttemptRecord) error {
	if len(durable.Receipts) == 0 {
		return nil
	}
	known := make(map[string]ParticipantReceipt, len(candidate.Receipts))
	for _, receipt := range candidate.Receipts {
		known[receipt.Participant] = receipt
	}
	for _, receipt := range durable.Receipts {
		existing, exists := known[receipt.Participant]
		if !exists {
			return fmt.Errorf("%w: candidate drops the durable receipt for participant %q", ErrManifestConflict, receipt.Participant)
		}
		if !existing.Equal(receipt) {
			return fmt.Errorf("%w: participant %q already has a different durable receipt", ErrManifestConflict, receipt.Participant)
		}
	}
	return nil
}

// continuesDurableGuards proves that every guard the durable record proves is
// installed is still accounted for. The candidate satisfies it either by
// carrying the same receipt under the same GUARDING entry, or — on the
// release-then-return path, which discards the whole GUARDING section — by
// carrying a matching release recorded under that entry. A candidate that
// carries neither was written from a record that never saw the install, and
// accepting it would leave the guard enforced on the source with nothing durable
// naming it.
func continuesDurableGuards(durable, candidate *AttemptRecord) error {
	installed := durable.installedGuardReceipts()
	if len(installed) == 0 {
		return nil
	}
	entry := durable.lastEntrySequence(PhaseGuarding)
	kept := map[guardReleaseKey]struct{}{}
	if candidate.lastEntrySequence(PhaseGuarding) == entry {
		kept = candidate.installedGuardReceipts()
	}
	released := make(map[guardReleaseKey]struct{}, len(candidate.ReleasedGuards))
	for _, release := range candidate.ReleasedGuards {
		if release.Entry < entry {
			continue
		}
		released[newGuardReleaseKey(release.Provider, release.Component, release.PhysicalIdentity, release.Role, release.ReceiptID)] = struct{}{}
	}
	missing := make([]string, 0, len(installed))
	for key := range installed {
		if _, exists := kept[key]; exists {
			continue
		}
		if _, exists := released[key]; exists {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s receipt %s", key.guard.component, key.receipt))
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("%w: candidate drops %d durable install receipts without releasing them: %v", ErrManifestConflict, len(missing), missing)
}

func attemptFileName(generation Generation) string {
	return attemptPrefix + strconv.FormatUint(uint64(generation), 10) + attemptSuffix
}

func attemptGeneration(name string) (Generation, bool) {
	if !strings.HasPrefix(name, attemptPrefix) || !strings.HasSuffix(name, attemptSuffix) {
		return 0, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, attemptPrefix), attemptSuffix)
	value, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || value == 0 {
		return 0, false
	}
	return Generation(value), true
}

func (s *ManifestStore) read(name string) ([]byte, bool, error) {
	payload, err := os.ReadFile(filepath.Join(s.directory, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", name, err)
	}
	return payload, true, nil
}

// replace writes payload to a temporary file in the same directory, fsyncs the
// file, renames it over the target, and fsyncs the directory. A crash before
// the rename leaves the previous record intact; a crash after it leaves the new
// one durable. There is no window in which a partially written record is
// readable.
func (s *ManifestStore) replace(name string, payload []byte) error {
	temporary, err := os.CreateTemp(s.directory, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary %s: %w", name, err)
	}
	path := temporary.Name()
	written := func() error {
		if err := temporary.Chmod(manifestFileMode); err != nil {
			return fmt.Errorf("securing temporary %s: %w", name, err)
		}
		if _, err := temporary.Write(payload); err != nil {
			return fmt.Errorf("writing temporary %s: %w", name, err)
		}
		return temporary.Sync()
	}()
	if closeErr := temporary.Close(); closeErr != nil && written == nil {
		written = fmt.Errorf("closing temporary %s: %w", name, closeErr)
	}
	if written != nil {
		return errors.Join(written, os.Remove(path))
	}
	if err := os.Rename(path, filepath.Join(s.directory, name)); err != nil {
		return errors.Join(fmt.Errorf("replacing %s: %w", name, err), os.Remove(path))
	}
	return syncDirectory(s.directory)
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("opening %s for fsync: %w", directory, err)
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil {
		return fmt.Errorf("fsyncing %s: %w", directory, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing %s after fsync: %w", directory, closeErr)
	}
	return nil
}
