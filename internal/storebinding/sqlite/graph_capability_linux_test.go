//go:build linux

package sqlite

import "testing"

func TestSQLiteWriterFencingCapabilityIsAvailableOnLinux(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Fatal("Linux SQLite provider reported writer fencing unavailable")
	}
}
