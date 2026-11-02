//go:build integration

package beads

import (
	"context"
	"testing"
)

// OpenRealNativeDoltStoreForTest opens a NativeDoltStore over the real
// embedded storage, with gc's session type registered, for external
// integration tests.
func OpenRealNativeDoltStoreForTest(t *testing.T) Store {
	t.Helper()
	s, storage := openRealNativeDoltStoreForMetadata(t, "gc")
	if err := storage.SetConfig(context.Background(), "types.custom", "session"); err != nil {
		t.Fatalf("registering the session type: %v", err)
	}
	return s
}

// StartTestDoltServerForTest starts a private dolt sql-server on a free
// loopback port for external integration tests and returns the port. The
// server is killed when the test ends.
func StartTestDoltServerForTest(t testing.TB) int {
	t.Helper()
	return startTestDoltServer(t)
}
