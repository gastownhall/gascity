//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package storebinding

import "context"

func acquireMigrationGuard(context.Context, string, Generation) (MigrationGuard, error) {
	return MigrationGuard{}, ErrMigrationGuardUnsupported
}

func validateMigrationGuardDirectoryIdentity(MigrationGuardIdentity) error {
	return ErrMigrationGuardUnsupported
}
