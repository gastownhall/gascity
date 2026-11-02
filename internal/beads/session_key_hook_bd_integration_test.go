//go:build integration

package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Kills a BdStore or real-storage NativeDoltStore write verb that skips the
// session-key hook (the unit suite covers MemStore, SQLiteStore and the
// in-memory NativeDoltStore fixture).
func TestRealBackendWriteVerbsReportSessionKeys(t *testing.T) {
	t.Run("BdStore", func(t *testing.T) {
		s, dir := newConditionalIntegrationBdStore(t)
		if out, err := newConditionalIntegrationRunner(t, dir)(dir, "bd", "config", "set", "types.custom", "session"); err != nil {
			t.Fatalf("registering the session type: %v\n%s", err, out)
		}
		exerciseSessionKeyHook(t, s)
	})
	t.Run("NativeDoltStore", func(t *testing.T) { exerciseSessionKeyHook(t, beads.OpenRealNativeDoltStoreForTest(t)) })
}
