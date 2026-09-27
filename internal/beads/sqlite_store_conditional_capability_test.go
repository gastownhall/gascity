package beads

import "testing"

// TestSQLiteStoreProbeConditionalWriteCapabilityFollowsSchema proves the
// SQLite capability prober tracks the actual on-disk schema rather than the
// store's static ConditionalWriter type assertion: a legacy layout with no
// revision column must report incapable even though SQLiteStore structurally
// implements MetadataCASWriter regardless of schema.
func TestSQLiteStoreProbeConditionalWriteCapabilityFollowsSchema(t *testing.T) {
	for _, revision := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy schema", true: "revision column present"}[revision], func(t *testing.T) {
			dir := t.TempDir()
			createSQLiteSchemaFixture(t, dir, revision, false, nil)
			opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
			if err != nil {
				t.Fatalf("OpenSQLiteStore: %v", err)
			}
			store := opened.(*SQLiteStore)
			defer func() { _ = store.CloseStore() }()

			capable, reason := store.probeConditionalWriteCapability()
			if capable != revision {
				t.Fatalf("probeConditionalWriteCapability() = (%t, %q), want capable=%t", capable, reason, revision)
			}
			if !revision && reason == "" {
				t.Fatal("probeConditionalWriteCapability() returned false with no reason")
			}

			gotCapable, gotReason := MetadataCASCapableFor(store)
			if gotCapable != revision {
				t.Fatalf("MetadataCASCapableFor() = (%t, %q), want capable=%t", gotCapable, gotReason, revision)
			}
		})
	}
}
