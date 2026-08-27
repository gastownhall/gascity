package storebinding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/pathutil"
)

var (
	// ErrInvalidMigrationGuard reports a malformed or uninitialized city migration guard.
	ErrInvalidMigrationGuard = errors.New("invalid storage migration guard")
	// ErrInvalidMigrationGuardClaim reports a released or forged guard claim.
	ErrInvalidMigrationGuardClaim = errors.New("invalid storage migration guard claim")
	// ErrMigrationGuardClaimsHeld reports an attempt to release a guard with live claims.
	ErrMigrationGuardClaimsHeld = errors.New("storage migration guard still has claims")
	// ErrMigrationGuardReleased reports a claim attempt after the city guard has closed.
	ErrMigrationGuardReleased = errors.New("storage migration guard is released")
	// ErrMigrationGuardCleanupPending reports a guard whose outer lock release must be retried.
	ErrMigrationGuardCleanupPending = errors.New("storage migration guard cleanup is pending")
	// ErrMigrationGuardBusy reports a city whose stable .gc directory is already guarded.
	ErrMigrationGuardBusy = errors.New("storage migration guard is already held")
	// ErrMigrationGuardUnsupported reports a platform without the required stable directory lock.
	ErrMigrationGuardUnsupported = errors.New("storage migration guard is unsupported on this platform")
	// ErrMigrationGuardIdentityChanged reports replacement of the .gc directory pinned by a guard.
	ErrMigrationGuardIdentityChanged = errors.New("storage migration guard directory identity changed")
	// ErrMigrationGuardScopeMismatch reports a fence request expected for another city guard scope.
	ErrMigrationGuardScopeMismatch = errors.New("storage migration guard scope does not match fence request")
)

// MigrationGuardIdentity binds a guard to one canonical city .gc directory,
// its opened directory identity, and durable migration generation.
type MigrationGuardIdentity struct {
	directory  string
	device     uint64
	inode      uint64
	generation Generation
}

// MigrationGuardScope names the canonical city .gc directory expected to own
// a fence request. It intentionally does not mint a guard or a claim.
type MigrationGuardScope struct {
	directory string
}

// NewMigrationGuardScope validates the stable city .gc directory expected for
// one provider fence request.
func NewMigrationGuardScope(directory string) (MigrationGuardScope, error) {
	if err := validateCanonicalMigrationGuardDirectory(directory); err != nil {
		return MigrationGuardScope{}, err
	}
	return MigrationGuardScope{directory: directory}, nil
}

// Directory returns the canonical city .gc directory expected by the request.
func (s MigrationGuardScope) Directory() string { return s.directory }

func (s MigrationGuardScope) valid() bool {
	return validateMigrationGuardDirectoryPath(s.directory) == nil
}

// Directory returns the canonical city .gc directory bound to the guard.
func (i MigrationGuardIdentity) Directory() string { return i.directory }

// Generation returns the durable migration generation bound to the guard.
func (i MigrationGuardIdentity) Generation() Generation { return i.generation }

// Equal reports whether two guard identities bind the same opened city
// directory object and generation.
func (i MigrationGuardIdentity) Equal(other MigrationGuardIdentity) bool {
	return i.directory == other.directory && i.device == other.device && i.inode == other.inode && i.generation == other.generation
}

func (i MigrationGuardIdentity) matchesScope(scope MigrationGuardScope) bool {
	return scope.valid() && i.directory == scope.directory
}

func newMigrationGuardIdentity(directory string, device, inode uint64, generation Generation) (MigrationGuardIdentity, error) {
	if err := validateMigrationGuardDirectoryPath(directory); err != nil || device == 0 || inode == 0 || !generation.Valid() {
		return MigrationGuardIdentity{}, ErrInvalidMigrationGuard
	}
	return MigrationGuardIdentity{directory: directory, device: device, inode: inode, generation: generation}, nil
}

func (i MigrationGuardIdentity) valid() bool {
	_, err := newMigrationGuardIdentity(i.directory, i.device, i.inode, i.generation)
	return err == nil
}

// AcquireMigrationGuard obtains an exclusive nonblocking lock on the stable,
// canonical city .gc directory. The returned guard remains live until Release.
func AcquireMigrationGuard(ctx context.Context, directory string, generation Generation) (MigrationGuard, error) {
	if err := ctx.Err(); err != nil {
		return MigrationGuard{}, err
	}
	if !generation.Valid() {
		return MigrationGuard{}, ErrInvalidMigrationGuard
	}
	scope, err := NewMigrationGuardScope(directory)
	if err != nil {
		return MigrationGuard{}, err
	}
	guard, err := acquireMigrationGuard(ctx, scope.Directory(), generation)
	if err != nil {
		return guard, err
	}
	if err := ctx.Err(); err != nil {
		if cleanupErr := guard.Release(); cleanupErr != nil {
			return guard, errors.Join(err, cleanupErr)
		}
		return MigrationGuard{}, err
	}
	return guard, nil
}

func validateMigrationGuardDirectoryPath(directory string) error {
	clean := filepath.Clean(directory)
	if strings.TrimSpace(directory) == "" || !filepath.IsAbs(clean) || clean != directory || filepath.Base(clean) != ".gc" {
		return ErrInvalidMigrationGuard
	}
	return nil
}

func validateCanonicalMigrationGuardDirectory(directory string) error {
	if err := validateMigrationGuardDirectoryPath(directory); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("%w: reading city directory: %w", ErrInvalidMigrationGuard, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidMigrationGuard
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("%w: resolving city directory: %w", ErrInvalidMigrationGuard, err)
	}
	// The directory must already be canonical, so callers cannot reach one
	// city under two names. Accept EITHER spelling of a canonical path: macOS
	// names the same location both as /var/... and as /private/var/... (same
	// for /tmp and /home), EvalSymlinks reports the /private form while
	// pathutil collapses to the alias form, and callers here legitimately
	// produce both. Requiring only the EvalSymlinks form rejected every
	// legitimate city under /var or /tmp on that host (gas-bsj).
	//
	// This stays strict about real symlinks: a directory reached through one
	// resolves to a path that is neither spelling of what it was given as, so
	// it still fails here — see
	// TestAcquireMigrationGuardRejectsNoncanonicalOrSymlinkedCityDirectory.
	// On Linux the two spellings coincide and this is exactly the old check.
	if directory != resolved && directory != pathutil.NormalizePathForCompare(resolved) {
		return ErrInvalidMigrationGuard
	}
	return nil
}

// MigrationGuard is the once-per-city outer migration guard. It cannot close
// while a provider-owned claim remains live.
type MigrationGuard struct {
	state *migrationGuardState
}

type migrationGuardReleaser interface {
	release() error
}

type migrationGuardReleaserFunc func() error

func (fn migrationGuardReleaserFunc) release() error {
	if fn == nil {
		return ErrInvalidMigrationGuard
	}
	return fn()
}

// RejectedMigrationGuardCleanupError retains cleanup ownership when migration
// guard acquisition rejects a directory before it can return a live guard.
// RetryCleanup is the only operation available to callers.
type RejectedMigrationGuardCleanupError struct {
	rejected   error
	cleanupErr error
	cleanup    migrationGuardReleaser
	mu         sync.Mutex
	cleaned    bool
}

// Error reports both the rejection and the incomplete cleanup.
func (e *RejectedMigrationGuardCleanupError) Error() string {
	if e == nil {
		return "rejected storage migration guard cleanup"
	}
	return errors.Join(e.rejected, e.cleanupErr).Error()
}

// Unwrap exposes both the rejection and cleanup failure to errors.Is and errors.As.
func (e *RejectedMigrationGuardCleanupError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{e.rejected, e.cleanupErr}
}

// RetryCleanup retries release of the rejected acquisition's file resource.
func (e *RejectedMigrationGuardCleanupError) RetryCleanup() error {
	if e == nil || e.cleanup == nil {
		return ErrInvalidMigrationGuard
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cleaned {
		return nil
	}
	if err := e.cleanup.release(); err != nil {
		return fmt.Errorf("retrying rejected city migration guard cleanup: %w", err)
	}
	e.cleaned = true
	return nil
}

func closeRejectedMigrationGuard(rejected error, cleanup migrationGuardReleaser) (MigrationGuard, error) {
	if cleanup == nil {
		return MigrationGuard{}, errors.Join(rejected, ErrInvalidMigrationGuard)
	}
	if err := cleanup.release(); err != nil {
		return MigrationGuard{}, &RejectedMigrationGuardCleanupError{
			rejected:   rejected,
			cleanupErr: fmt.Errorf("closing rejected city directory: %w", err),
			cleanup:    cleanup,
		}
	}
	return MigrationGuard{}, rejected
}

type migrationGuardState struct {
	mu             sync.Mutex
	identity       MigrationGuardIdentity
	closeFn        func() error
	claims         int
	cleanupPending bool
	released       bool
}

func newMigrationGuard(identity MigrationGuardIdentity, closeFn func() error) (MigrationGuard, error) {
	if !identity.valid() || closeFn == nil {
		return MigrationGuard{}, ErrInvalidMigrationGuard
	}
	return MigrationGuard{state: &migrationGuardState{identity: identity, closeFn: closeFn}}, nil
}

func newCleanupPendingMigrationGuard(identity MigrationGuardIdentity, closeFn func() error) MigrationGuard {
	return MigrationGuard{state: &migrationGuardState{identity: identity, closeFn: closeFn, cleanupPending: true}}
}

func (g MigrationGuard) claim(ctx context.Context) (MigrationGuardClaim, error) {
	if err := ctx.Err(); err != nil {
		return MigrationGuardClaim{}, err
	}
	if g.state == nil {
		return MigrationGuardClaim{}, ErrInvalidMigrationGuard
	}
	g.state.mu.Lock()
	defer g.state.mu.Unlock()
	if g.state.released {
		return MigrationGuardClaim{}, ErrMigrationGuardReleased
	}
	if g.state.cleanupPending {
		return MigrationGuardClaim{}, ErrMigrationGuardCleanupPending
	}
	claim := &migrationGuardClaimState{guard: g.state}
	g.state.claims++
	return MigrationGuardClaim{state: claim}, nil
}

// Release closes the outer guard only after every provider-owned claim releases.
func (g MigrationGuard) Release() error {
	if g.state == nil {
		return ErrInvalidMigrationGuard
	}
	g.state.mu.Lock()
	defer g.state.mu.Unlock()
	if g.state.released {
		return nil
	}
	if g.state.claims != 0 {
		return ErrMigrationGuardClaimsHeld
	}
	g.state.cleanupPending = true
	if err := g.state.closeFn(); err != nil {
		return err
	}
	g.state.cleanupPending = false
	g.state.released = true
	return nil
}

// MigrationGuardClaim is an unforgeable, close-once provider reference to a
// city migration guard. A zero claim is invalid.
type MigrationGuardClaim struct {
	state *migrationGuardClaimState
}

type migrationGuardClaimState struct {
	guard    *migrationGuardState
	released bool
}

// Identity returns the exact city directory and generation protected by a live claim.
func (c MigrationGuardClaim) Identity() (MigrationGuardIdentity, error) {
	if c.state == nil || c.state.guard == nil {
		return MigrationGuardIdentity{}, ErrInvalidMigrationGuardClaim
	}
	c.state.guard.mu.Lock()
	defer c.state.guard.mu.Unlock()
	if c.state.released || c.state.guard.released {
		return MigrationGuardIdentity{}, ErrInvalidMigrationGuardClaim
	}
	return c.state.guard.identity, nil
}

func (c MigrationGuardClaim) validateLiveDirectory() error {
	identity, err := c.Identity()
	if err != nil {
		return err
	}
	if err := validateMigrationGuardDirectoryIdentity(identity); err != nil {
		return err
	}
	return nil
}

// Held reports whether the claim still protects its city migration guard.
func (c MigrationGuardClaim) Held() bool {
	err := c.validateLiveDirectory()
	return err == nil
}

// owned reports whether this claim still owns one outer-guard reference. It
// intentionally does not inspect the live directory identity: cleanup must
// release an owned claim even after the directory was replaced.
func (c MigrationGuardClaim) owned() bool {
	if c.state == nil || c.state.guard == nil {
		return false
	}
	c.state.guard.mu.Lock()
	defer c.state.guard.mu.Unlock()
	return !c.state.released && !c.state.guard.released
}

// Release drops this provider-owned reference. Repeated release is idempotent.
func (c MigrationGuardClaim) Release() error {
	if c.state == nil || c.state.guard == nil {
		return ErrInvalidMigrationGuardClaim
	}
	c.state.guard.mu.Lock()
	defer c.state.guard.mu.Unlock()
	if c.state.released {
		return nil
	}
	if c.state.guard.released || c.state.guard.claims == 0 {
		return ErrInvalidMigrationGuardClaim
	}
	c.state.released = true
	c.state.guard.claims--
	return nil
}
