package beads

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/pathutil"
)

const (
	lifecycleMutationLockFilename = "gc-lifecycle-mutation.lock"
	lifecycleMutationScopeEnv     = "GC_LIFECYCLE_MUTATION_SCOPE"
	lifecycleMutationTokenEnv     = "GC_LIFECYCLE_MUTATION_TOKEN"
	lifecycleMutationTokenSep     = "."
	lifecycleMutationScopePrefix  = "sha256:"

	// Compatibility names keep existing scope-lock tests and downstream
	// package-local helpers pointed at the single unified lease file.
	closeTransitionScopeLockFilename = lifecycleMutationLockFilename
	lifecycleMetadataLockFilename    = lifecycleMutationLockFilename
)

type lifecycleMutationInheritance struct {
	scope string
	token string
}

type lifecycleMutationDelegation struct {
	rootToken  string
	generation uint64
}

type lifecycleMutationOwnerRecord struct {
	Scope string `json:"scope"`
	Token string `json:"token"`
}

var errLifecycleMutationOwnerVacant = errors.New("lifecycle mutation owner record is vacant")

type lifecycleMutationMutexEntry struct {
	mu         sync.Mutex
	refs       int
	ownerToken string
}

var lifecycleMutationMutexRegistry = struct {
	sync.Mutex
	entries map[string]*lifecycleMutationMutexEntry
}{entries: make(map[string]*lifecycleMutationMutexEntry)}

// lifecycleMutationLease serializes every Gas City lifecycle mutation within
// one beads scope. A top-level owner holds the root process mutex and stable
// flock. Synchronous descendants retain the root delegation and serialize with
// their siblings on a generation-specific process mutex and flock.
type lifecycleMutationLease struct {
	key        string
	entryKey   string
	token      string
	generation uint64
	entry      *lifecycleMutationMutexEntry
	fileLock   *lifecycleMutationFileLock
	ownerFile  *lifecycleMutationFileLock
	ownerProof *lifecycleMutationFileLock
	ownsOwner  bool
	once       sync.Once
}

// CommandEnv returns the command-local inheritance values for a mutating bd
// child. Callers must apply them only to that child; the parent process
// environment is intentionally never changed.
func (l *lifecycleMutationLease) CommandEnv() map[string]string {
	if l == nil || l.token == "" {
		return nil
	}
	nextGeneration := l.generation + 1
	if nextGeneration == 0 {
		return nil
	}
	return map[string]string{
		lifecycleMutationScopeEnv: l.key,
		lifecycleMutationTokenEnv: encodeLifecycleMutationDelegation(l.token, nextGeneration),
	}
}

// Unlock releases the portion of the lease owned by this caller. Reentrant
// joiners never clear or unlock the top-level owner's flock.
func (l *lifecycleMutationLease) Unlock() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.fileLock != nil && l.ownsOwner {
			// An empty record is the handoff fence. Descendants that observe it
			// cannot authenticate the old delegation and queue as a fresh owner
			// behind the still-held root flock.
			_ = l.fileLock.WriteLocked(nil)
		}
		if l.ownerProof != nil {
			if l.ownsOwner {
				_ = l.ownerProof.removeLocked()
			}
			_ = l.ownerProof.Unlock()
			l.ownerProof = nil
		}
		if l.ownsOwner {
			lifecycleMutationMutexRegistry.Lock()
			if l.entry.ownerToken == l.token {
				l.entry.ownerToken = ""
			}
			lifecycleMutationMutexRegistry.Unlock()
		}
		if l.fileLock != nil {
			_ = l.fileLock.Unlock()
		}
		if l.ownerFile != nil {
			_ = l.ownerFile.Close()
		}
		l.entry.mu.Unlock()
		releaseLifecycleMutationEntry(l.entryKey, l.entry)
	})
}

func inheritedLifecycleMutationFromEnv() lifecycleMutationInheritance {
	return lifecycleMutationInheritance{
		scope: os.Getenv(lifecycleMutationScopeEnv),
		token: strings.TrimSpace(os.Getenv(lifecycleMutationTokenEnv)),
	}
}

// acquireLifecycleMutationLease takes the unified process/file scope lease or
// joins the currently-held lease when inherited proves that this process is a
// synchronous descendant of its owner.
func acquireLifecycleMutationLease(scope string, inherited lifecycleMutationInheritance) (*lifecycleMutationLease, error) {
	key := closeTransitionScopeKey(scope)
	delegation, inheritedOK := parseLifecycleMutationInheritance(inherited, key)
	if !inheritedOK {
		return acquireFreshLifecycleMutationLease(scope, key, nil)
	}

	ownerFile, err := openLifecycleMutationScopeLock(scope)
	if err != nil {
		return nil, err
	}
	if ownerFile == nil {
		if processLifecycleMutationOwnerMatches(key, delegation.rootToken) {
			return acquireProcessLifecycleMutationDescendant(scope, key, delegation)
		}
		return acquireFreshLifecycleMutationLease(scope, key, nil)
	}

	acquired, err := ownerFile.TryLock()
	if err != nil {
		_ = ownerFile.Close()
		return nil, fmt.Errorf("try-locking lifecycle mutations for scope %q: %w", scope, err)
	}
	if acquired {
		// The inherited token is stale. Release the opportunistically acquired
		// inode before entering the canonical process->file order as a new owner.
		_ = ownerFile.Unlock()
		return acquireFreshLifecycleMutationLease(scope, key, nil)
	}

	owner, readErr := readLifecycleMutationOwner(ownerFile)
	if readErr != nil {
		if errors.Is(readErr, errLifecycleMutationOwnerVacant) {
			return acquireFreshLifecycleMutationLease(scope, key, ownerFile)
		}
		_ = ownerFile.Close()
		return nil, fmt.Errorf("reading lifecycle mutation owner for scope %q: %w", scope, readErr)
	}
	if lifecycleMutationOwnerMatches(owner, key, delegation.rootToken) {
		ownerProof, held, proofErr := openHeldLifecycleMutationOwnerProof(ownerFile, delegation.rootToken)
		if proofErr != nil {
			_ = ownerFile.Close()
			return nil, fmt.Errorf("checking lifecycle mutation owner proof for scope %q: %w", scope, proofErr)
		}
		if held {
			return acquireFileLifecycleMutationDescendant(scope, key, delegation, ownerFile, ownerProof)
		}
		return acquireFreshLifecycleMutationLease(scope, key, ownerFile)
	}

	// Keep the exact descriptor that reported contention. Acquiring the process
	// mutex before blocking on it preserves the top-level process->file order
	// without reopening a pathname that may now refer to another inode.
	return acquireFreshLifecycleMutationLease(scope, key, ownerFile)
}

func acquireFreshLifecycleMutationLease(
	scope, key string,
	openedFile *lifecycleMutationFileLock,
) (*lifecycleMutationLease, error) {
	entryKey := lifecycleMutationEntryKey(key, 0)
	entry := retainLifecycleMutationEntry(entryKey)
	entry.mu.Lock()
	lease := &lifecycleMutationLease{
		key:       key,
		entryKey:  entryKey,
		entry:     entry,
		ownsOwner: true,
	}
	fail := func(err error) (*lifecycleMutationLease, error) {
		if lease.ownerProof != nil {
			if lease.ownerProof.locked {
				_ = lease.ownerProof.removeLocked()
			}
			_ = lease.ownerProof.Close()
		}
		if lease.fileLock != nil {
			_ = lease.fileLock.Close()
		}
		entry.mu.Unlock()
		releaseLifecycleMutationEntry(entryKey, entry)
		return nil, err
	}

	fileLock := openedFile
	var err error
	if fileLock == nil {
		fileLock, err = openLifecycleMutationScopeLock(scope)
		if err != nil {
			return fail(err)
		}
	}
	lease.fileLock = fileLock
	if fileLock != nil {
		if err := fileLock.Lock(); err != nil {
			return fail(fmt.Errorf("locking lifecycle mutations for scope %q: %w", scope, err))
		}
	}
	if err := initializeLifecycleMutationOwner(lease); err != nil {
		return fail(err)
	}
	return lease, nil
}

func acquireProcessLifecycleMutationDescendant(
	scope, key string,
	delegation lifecycleMutationDelegation,
) (*lifecycleMutationLease, error) {
	entryKey := lifecycleMutationEntryKey(key, delegation.generation)
	entry := retainLifecycleMutationEntry(entryKey)
	entry.mu.Lock()
	if !processLifecycleMutationOwnerMatches(key, delegation.rootToken) {
		entry.mu.Unlock()
		releaseLifecycleMutationEntry(entryKey, entry)
		return acquireFreshLifecycleMutationLease(scope, key, nil)
	}
	return &lifecycleMutationLease{
		key:        key,
		entryKey:   entryKey,
		token:      delegation.rootToken,
		generation: delegation.generation,
		entry:      entry,
	}, nil
}

func acquireFileLifecycleMutationDescendant(
	scope, key string,
	delegation lifecycleMutationDelegation,
	ownerFile *lifecycleMutationFileLock,
	ownerProof *lifecycleMutationFileLock,
) (*lifecycleMutationLease, error) {
	entryKey := lifecycleMutationEntryKey(key, delegation.generation)
	entry := retainLifecycleMutationEntry(entryKey)
	entry.mu.Lock()
	lease := &lifecycleMutationLease{
		key:        key,
		entryKey:   entryKey,
		token:      delegation.rootToken,
		generation: delegation.generation,
		entry:      entry,
		ownerFile:  ownerFile,
		ownerProof: ownerProof,
	}
	fail := func(err error) (*lifecycleMutationLease, error) {
		if lease.fileLock != nil {
			_ = lease.fileLock.Close()
		}
		if lease.ownerFile != nil {
			_ = lease.ownerFile.Close()
		}
		if lease.ownerProof != nil {
			_ = lease.ownerProof.Close()
		}
		entry.mu.Unlock()
		releaseLifecycleMutationEntry(entryKey, entry)
		return nil, err
	}

	joinLock, err := openLifecycleMutationDelegationFileLock(
		ownerFile,
		lifecycleMutationDelegationLockFilename(delegation.generation),
	)
	if err != nil {
		return fail(err)
	}
	if joinLock == nil {
		return fail(fmt.Errorf("locking inherited lifecycle mutation for scope %q: .beads directory disappeared", scope))
	}
	lease.fileLock = joinLock
	if err := joinLock.Lock(); err != nil {
		return fail(fmt.Errorf("locking inherited lifecycle mutation for scope %q: %w", scope, err))
	}
	fallbackFresh := func(openedFile *lifecycleMutationFileLock) (*lifecycleMutationLease, error) {
		lease.ownerFile = nil
		if lease.ownerProof != nil {
			_ = lease.ownerProof.Close()
			lease.ownerProof = nil
		}
		_ = joinLock.Unlock()
		lease.fileLock = nil
		entry.mu.Unlock()
		releaseLifecycleMutationEntry(entryKey, entry)
		return acquireFreshLifecycleMutationLease(scope, key, openedFile)
	}

	// Waiting for a sibling may outlive the root owner. Recheck the exact owner
	// inode after admission so a stale asynchronous descendant cannot overlap a
	// new top-level owner.
	proofReleased, err := ownerProof.TryLock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fallbackFresh(ownerFile)
		}
		return fail(fmt.Errorf("rechecking lifecycle mutation owner proof for scope %q: %w", scope, err))
	}
	if proofReleased {
		return fallbackFresh(ownerFile)
	}
	rootStillHeld, err := ownerFile.TryLock()
	if err != nil {
		return fail(fmt.Errorf("rechecking lifecycle mutation owner for scope %q: %w", scope, err))
	}
	if rootStillHeld {
		_ = ownerFile.Unlock()
		return fallbackFresh(nil)
	}
	owner, err := readLifecycleMutationOwner(ownerFile)
	if err != nil {
		if errors.Is(err, errLifecycleMutationOwnerVacant) {
			return fallbackFresh(ownerFile)
		}
		return fail(fmt.Errorf("rechecking lifecycle mutation owner for scope %q: %w", scope, err))
	}
	if !lifecycleMutationOwnerMatches(owner, key, delegation.rootToken) {
		// The root lock can pass directly from the inherited owner to a new
		// top-level owner while this descendant waits behind a sibling. Transfer
		// the exact contended descriptor into normal top-level acquisition so the
		// stale delegation serializes behind that replacement instead of failing.
		return fallbackFresh(ownerFile)
	}
	return lease, nil
}

func initializeLifecycleMutationOwner(lease *lifecycleMutationLease) error {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("generating lifecycle mutation owner token: %w", err)
	}
	lease.token = hex.EncodeToString(tokenBytes)
	lease.generation = 0
	if lease.fileLock != nil {
		ownerProof, err := openLifecycleMutationChildFileLock(
			lease.fileLock,
			lifecycleMutationOwnerProofLockFilename(lease.token),
			true,
			true,
		)
		if err != nil {
			return fmt.Errorf("opening lifecycle mutation owner proof: %w", err)
		}
		lease.ownerProof = ownerProof
		if err := ownerProof.Lock(); err != nil {
			return fmt.Errorf("locking lifecycle mutation owner proof: %w", err)
		}
		record, err := json.Marshal(lifecycleMutationOwnerRecord{Scope: lease.key, Token: lease.token})
		if err != nil {
			return fmt.Errorf("encoding lifecycle mutation owner: %w", err)
		}
		record = append(record, '\n')
		if err := lease.fileLock.WriteLocked(record); err != nil {
			return fmt.Errorf("recording lifecycle mutation owner for scope %q: %w", lease.key, err)
		}
	}
	setLifecycleMutationProcessOwner(lease.entry, lease.token)
	return nil
}

func setLifecycleMutationProcessOwner(entry *lifecycleMutationMutexEntry, token string) {
	lifecycleMutationMutexRegistry.Lock()
	entry.ownerToken = token
	lifecycleMutationMutexRegistry.Unlock()
}

func processLifecycleMutationOwnerMatches(key, token string) bool {
	entryKey := lifecycleMutationEntryKey(key, 0)
	lifecycleMutationMutexRegistry.Lock()
	defer lifecycleMutationMutexRegistry.Unlock()
	entry := lifecycleMutationMutexRegistry.entries[entryKey]
	return entry != nil && entry.ownerToken == token
}

func readLifecycleMutationOwner(fileLock *lifecycleMutationFileLock) (lifecycleMutationOwnerRecord, error) {
	data, err := fileLock.ReadExact()
	if err != nil {
		return lifecycleMutationOwnerRecord{}, err
	}
	if len(data) == 0 {
		return lifecycleMutationOwnerRecord{}, errLifecycleMutationOwnerVacant
	}
	var owner lifecycleMutationOwnerRecord
	if err := json.Unmarshal(data, &owner); err != nil {
		return lifecycleMutationOwnerRecord{}, err
	}
	return owner, nil
}

func openHeldLifecycleMutationOwnerProof(
	ownerFile *lifecycleMutationFileLock,
	token string,
) (*lifecycleMutationFileLock, bool, error) {
	ownerProof, err := openLifecycleMutationChildFileLock(
		ownerFile,
		lifecycleMutationOwnerProofLockFilename(token),
		false,
		false,
	)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	acquired, err := ownerProof.TryLock()
	if err != nil {
		_ = ownerProof.Close()
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if acquired {
		_ = ownerProof.Unlock()
		return nil, false, nil
	}
	return ownerProof, true, nil
}

func parseLifecycleMutationInheritance(
	inherited lifecycleMutationInheritance,
	key string,
) (lifecycleMutationDelegation, bool) {
	if strings.TrimSpace(inherited.token) == "" || inherited.scope == "" {
		return lifecycleMutationDelegation{}, false
	}
	if inherited.scope != key {
		return lifecycleMutationDelegation{}, false
	}
	return parseLifecycleMutationDelegation(inherited.token)
}

func parseLifecycleMutationDelegation(token string) (lifecycleMutationDelegation, bool) {
	rootToken, generationText, found := strings.Cut(strings.TrimSpace(token), lifecycleMutationTokenSep)
	if !found || strings.Contains(generationText, lifecycleMutationTokenSep) || !validLifecycleMutationRootToken(rootToken) {
		return lifecycleMutationDelegation{}, false
	}
	generation, err := strconv.ParseUint(generationText, 10, 64)
	if err != nil || generation == 0 || generation == ^uint64(0) {
		return lifecycleMutationDelegation{}, false
	}
	return lifecycleMutationDelegation{rootToken: rootToken, generation: generation}, true
}

func validLifecycleMutationRootToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(token)
	return err == nil && len(decoded) == 32 && token == strings.ToLower(token)
}

func encodeLifecycleMutationDelegation(rootToken string, generation uint64) string {
	return rootToken + lifecycleMutationTokenSep + strconv.FormatUint(generation, 10)
}

func lifecycleMutationOwnerMatches(owner lifecycleMutationOwnerRecord, key, rootToken string) bool {
	return owner.Scope == key && owner.Token == rootToken && validLifecycleMutationRootToken(owner.Token)
}

func lifecycleMutationEntryKey(key string, generation uint64) string {
	if generation == 0 {
		return key
	}
	return key + "\x00delegation:" + strconv.FormatUint(generation, 10)
}

func lifecycleMutationDelegationLockFilename(generation uint64) string {
	return "gc-lifecycle-mutation.delegate-" + strconv.FormatUint(generation, 10) + ".lock"
}

func lifecycleMutationOwnerProofLockFilename(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "gc-lifecycle-mutation.owner-" + hex.EncodeToString(sum[:]) + ".lock"
}

func retainLifecycleMutationEntry(key string) *lifecycleMutationMutexEntry {
	lifecycleMutationMutexRegistry.Lock()
	defer lifecycleMutationMutexRegistry.Unlock()
	entry := lifecycleMutationMutexRegistry.entries[key]
	if entry == nil {
		entry = &lifecycleMutationMutexEntry{}
		lifecycleMutationMutexRegistry.entries[key] = entry
	}
	entry.refs++
	return entry
}

func releaseLifecycleMutationEntry(key string, entry *lifecycleMutationMutexEntry) {
	lifecycleMutationMutexRegistry.Lock()
	defer lifecycleMutationMutexRegistry.Unlock()
	entry.refs--
	if entry.refs == 0 && lifecycleMutationMutexRegistry.entries[key] == entry {
		delete(lifecycleMutationMutexRegistry.entries, key)
	}
}

// lockCloseTransitionScope is retained for native stores and package-local
// tests. BdStore mutation paths use the lease directly so they can pass its
// inheritance only to the mutating bd child.
func lockCloseTransitionScope(scope string) (unlock func(), err error) {
	lease, err := acquireLifecycleMutationLease(scope, inheritedLifecycleMutationFromEnv())
	if err != nil {
		return nil, err
	}
	return lease.Unlock, nil
}

func closeTransitionScopeKey(scope string) string {
	if scope == "" {
		return "<direct-store>"
	}
	normalized := normalizeLifecycleMutationScope(scope)
	sum := sha256.Sum256([]byte(normalized))
	return lifecycleMutationScopePrefix + hex.EncodeToString(sum[:])
}

func normalizeLifecycleMutationScope(scope string) string {
	if scope == "" {
		return ""
	}
	return pathutil.NormalizePathForCompare(scope)
}

func openLifecycleMutationScopeLock(scope string) (*lifecycleMutationFileLock, error) {
	return openLifecycleMutationScopeFileLock(scope, lifecycleMutationLockFilename)
}
