//go:build !linux

package sqlite

import (
	"context"
	"errors"

	"github.com/gastownhall/gascity/internal/storebinding"
)

func sqliteWriterFencingSupported() bool { return false }

func qualifySQLiteStaticWriterFencing(context.Context, string, storebinding.PhysicalIdentity, storebinding.PhysicalIdentity) (bool, error) {
	return false, nil
}

func acquireSQLiteWriterReservation(context.Context, string, storebinding.MigrationGuardClaim, sqliteWriterReservationSource) (sqliteWriterReservation, error) {
	return nil, errors.New("SQLite writer fencing requires Linux OFD locks")
}
