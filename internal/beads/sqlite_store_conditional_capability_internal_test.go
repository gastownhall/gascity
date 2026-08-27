package beads

import (
	"strings"
	"testing"
)

// TestSQLiteStoreConditionalCapabilityFollowsTheRevisionColumn pins the
// capability answer to the same condition the write path enforces:
// conditionalWrite returns ErrConditionalWriteUnsupported without a revision
// column, so the store must report itself incapable there and capable on the
// canonical schema.
//
// The answer is load-bearing twice over. RequiredConditionalWriter consults it
// to decide whether a session-class store can meet the reconciler's
// requirement, and the §12.5 status block renders it; a store that claimed
// capability on a pre-revision database would satisfy the requirement while
// every fenced write on it refused.
func TestSQLiteStoreConditionalCapabilityFollowsTheRevisionColumn(t *testing.T) {
	for _, revision := range []bool{false, true} {
		dir := t.TempDir()
		createSQLiteSchemaFixture(t, dir, revision, false, nil)
		opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
		if err != nil {
			t.Fatalf("OpenSQLiteStore(revision=%t): %v", revision, err)
		}
		store := opened.(*SQLiteStore)
		t.Cleanup(func() { _ = store.CloseStore() })

		capable, reason := store.probeConditionalWriteCapability()
		if capable != revision {
			t.Errorf("probeConditionalWriteCapability(revision=%t) = %t (%q), want %t", revision, capable, reason, revision)
		}
		if !capable && reason == "" {
			t.Error("an incapable probe gave no reason")
		}

		insp := InspectConditionalWrites(store)
		wantProbe := ConditionalWriteProbeCapable
		if !revision {
			wantProbe = ConditionalWriteProbeIncapable
		}
		if insp.Probe != wantProbe {
			t.Errorf("InspectConditionalWrites(revision=%t).Probe = %q, want %q", revision, insp.Probe, wantProbe)
		}
		if insp.Capable != revision {
			t.Errorf("InspectConditionalWrites(revision=%t).Capable = %t, want %t", revision, insp.Capable, revision)
		}
		if insp.StoreKind != BeadsStoreNameSQLiteStore {
			t.Errorf("InspectConditionalWrites().StoreKind = %q, want %q", insp.StoreKind, BeadsStoreNameSQLiteStore)
		}

		writer, err := RequiredConditionalWriter(store)
		switch {
		case revision && err != nil:
			t.Errorf("a canonical sqlite database failed the conditional-writes requirement: %v", err)
		case revision && writer == nil:
			t.Error("a canonical sqlite database met the requirement with no writer")
		case !revision && err == nil:
			t.Error("a pre-revision sqlite database satisfied the conditional-writes requirement")
		case !revision && !strings.Contains(err.Error(), BeadsStoreNameSQLiteStore):
			t.Errorf("the refusal does not name the store: %v", err)
		}
	}
}
