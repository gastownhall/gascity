package beads

import (
	"bytes"
	"testing"
)

// A library extension backend is one gc does not implement, so the ready
// projection must not spend `bd sql` on it any more than on a name nothing
// registered — recognizing the name at the metadata layer does not make gc's
// issues/wisps schema assumption true for it.
func TestReadyProjectionOnALibraryExtensionBackendNeverSpendsBdSQL(t *testing.T) {
	const name = "gctest-ext"
	registerLibraryBackendForTest(t, name)
	scope := t.TempDir()
	writeScopeMetadata(t, scope, map[string]any{"backend": name, "dolt_mode": "server"})
	runner := blockedDoorRunner(`[{"id":"mc-2","blocked_by_count":1,"blocked_by":["mc-1"]}]`)
	s := NewBdStore(scope, runner.run, WithBdStoreNoticeSink(&bytes.Buffer{}))

	if _, err := s.enrichReadyProjectionForCache(activeWorkBeads()); err != nil {
		t.Fatalf("enrichReadyProjectionForCache on a library extension backend = %v, want the blocked door to serve it", err)
	}
	for _, call := range runner.calls {
		if len(call) > 1 && call[1] == "sql" {
			t.Fatalf("a library extension backend spent %v; gc cannot assume that backend's schema", runner.calls)
		}
	}
}
