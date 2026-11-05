//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package storebinding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

func acquireMigrationGuard(ctx context.Context, directory string, generation Generation) (MigrationGuard, error) {
	return acquireMigrationGuardWithReleaser(ctx, directory, generation, func(file *os.File) migrationGuardReleaser {
		return newMigrationGuardFileReleaser(file)
	})
}

func acquireMigrationGuardWithReleaser(ctx context.Context, directory string, generation Generation, newReleaser func(*os.File) migrationGuardReleaser) (MigrationGuard, error) {
	file, err := os.Open(directory)
	if err != nil {
		return MigrationGuard{}, fmt.Errorf("%w: opening city directory: %w", ErrInvalidMigrationGuard, err)
	}
	closeRejected := func(rejected error) (MigrationGuard, error) {
		return closeRejectedMigrationGuard(rejected, migrationGuardReleaserFunc(file.Close))
	}
	info, err := file.Stat()
	if err != nil {
		return closeRejected(fmt.Errorf("%w: stat city directory: %w", ErrInvalidMigrationGuard, err))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return closeRejected(ErrMigrationGuardUnsupported)
	}
	identity, err := newMigrationGuardIdentity(directory, uint64(stat.Dev), uint64(stat.Ino), generation) //nolint:unconvert // Stat_t.Dev is int32 on darwin; normalize to one cross-platform representation.
	if err != nil {
		return closeRejected(err)
	}
	if err := validateMigrationGuardDirectoryIdentity(identity); err != nil {
		return closeRejected(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return closeRejected(fmt.Errorf("%w: %w", ErrMigrationGuardBusy, err))
		}
		return closeRejected(fmt.Errorf("acquiring city directory guard: %w", err))
	}
	if newReleaser == nil {
		releaser := newMigrationGuardFileReleaser(file)
		return cleanupAcquiredMigrationGuard(identity, releaser, ErrInvalidMigrationGuard)
	}
	releaser := newReleaser(file)
	if releaser == nil {
		fallback := newMigrationGuardFileReleaser(file)
		return cleanupAcquiredMigrationGuard(identity, fallback, ErrInvalidMigrationGuard)
	}
	guard, err := newMigrationGuard(identity, releaser.release)
	if err != nil {
		return cleanupAcquiredMigrationGuard(identity, releaser, err)
	}
	if err := ctx.Err(); err != nil {
		if cleanupErr := guard.Release(); cleanupErr != nil {
			return guard, errors.Join(err, cleanupErr)
		}
		return MigrationGuard{}, err
	}
	return guard, nil
}

func cleanupAcquiredMigrationGuard(identity MigrationGuardIdentity, releaser migrationGuardReleaser, cause error) (MigrationGuard, error) {
	if releaser == nil {
		return MigrationGuard{}, errors.Join(cause, ErrInvalidMigrationGuard)
	}
	guard := newCleanupPendingMigrationGuard(identity, releaser.release)
	if err := guard.Release(); err != nil {
		return guard, errors.Join(cause, err)
	}
	return MigrationGuard{}, cause
}

func validateMigrationGuardDirectoryIdentity(identity MigrationGuardIdentity) error {
	if !identity.valid() {
		return ErrInvalidMigrationGuard
	}
	if err := validateCanonicalMigrationGuardDirectory(identity.directory); err != nil {
		return err
	}
	info, err := os.Stat(identity.directory)
	if err != nil {
		return fmt.Errorf("%w: stat city directory: %w", ErrMigrationGuardIdentityChanged, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != identity.device || uint64(stat.Ino) != identity.inode { //nolint:unconvert // Stat_t.Dev is int32 on darwin; normalize to one cross-platform representation.
		return ErrMigrationGuardIdentityChanged
	}
	return nil
}

// migrationGuardFileReleaser remembers which stages have released. A
// successful close releases the operating-system lock even when an explicit
// unlock previously failed, so subsequent cleanup must not touch the closed
// descriptor again.
type migrationGuardFileReleaser struct {
	unlock func() error
	close  func() error

	unlocked bool
	closed   bool
}

func newMigrationGuardFileReleaser(file *os.File) *migrationGuardFileReleaser {
	return newMigrationGuardFileReleaserWithOperations(
		func() error { return syscall.Flock(int(file.Fd()), syscall.LOCK_UN) },
		file.Close,
	)
}

func newMigrationGuardFileReleaserWithOperations(unlock, closeFile func() error) *migrationGuardFileReleaser {
	return &migrationGuardFileReleaser{unlock: unlock, close: closeFile}
}

func (r *migrationGuardFileReleaser) release() error {
	if r == nil || r.unlock == nil || r.close == nil {
		return ErrInvalidMigrationGuard
	}
	if r.closed {
		return nil
	}

	var unlockErr error
	if !r.unlocked {
		if err := r.unlock(); err != nil {
			unlockErr = fmt.Errorf("unlocking city directory guard: %w", err)
		} else {
			r.unlocked = true
		}
	}

	var closeErr error
	if err := r.close(); err != nil {
		closeErr = fmt.Errorf("closing city directory guard: %w", err)
	} else {
		r.closed = true
		// Closing the descriptor releases any flock held by this process.
		r.unlocked = true
	}
	return errors.Join(unlockErr, closeErr)
}
