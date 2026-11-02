package beads

import (
	"fmt"
	"sync"
	"testing"
)

// TestSQLiteStoreConcurrentDifferentKeyMetadataWritesBothLand races two
// handles on one store directory, each setting its own metadata key on the
// same bead. Each handle has its own pools, so they contend as two processes
// would; the merge happens against the row read inside the write transaction,
// so different keys compose (gascity#1596).
func TestSQLiteStoreConcurrentDifferentKeyMetadataWritesBothLand(t *testing.T) {
	dir := t.TempDir()
	stores := []*SQLiteStore{openSeqStore(t, dir), openSeqStore(t, dir)}
	created, err := stores[0].Create(Bead{Title: "compose"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created.ID

	const rounds = 50
	var lost []string
	for round := 1; round <= rounds; round++ {
		value := fmt.Sprintf("v%d", round)
		errs := raceSQLiteMetadataWrites(stores, id, []map[string]string{
			{"k1": value},
			{"k2": value},
		})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: store %d Update: %v", round, i, err)
			}
		}
		got, err := stores[0].Get(id)
		if err != nil {
			t.Fatalf("round %d: Get: %v", round, err)
		}
		if got.Metadata["k1"] != value || got.Metadata["k2"] != value {
			lost = append(lost, fmt.Sprintf("round %d: k1=%q k2=%q (want %q)",
				round, got.Metadata["k1"], got.Metadata["k2"], value))
		}
	}
	if len(lost) > 0 {
		t.Fatalf("%d of %d rounds lost a key:\n%v", len(lost), rounds, lost)
	}

	// Same key: both writes succeed and the last writer wins.
	errs := raceSQLiteMetadataWrites(stores, id, []map[string]string{
		{"k1": "same-a"},
		{"k1": "same-b"},
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("same-key round: store %d Update: %v", i, err)
		}
	}
	got, err := stores[0].Get(id)
	if err != nil {
		t.Fatalf("same-key round: Get: %v", err)
	}
	if v := got.Metadata["k1"]; v != "same-a" && v != "same-b" {
		t.Fatalf("same-key round: k1=%q, want one of the two writes", v)
	}
	if v := got.Metadata["k2"]; v != fmt.Sprintf("v%d", rounds) {
		t.Fatalf("same-key round: k2=%q, want it untouched", v)
	}
}

// raceSQLiteMetadataWrites releases one Update per store from a shared
// barrier, store i writing writes[i], and returns each store's error.
func raceSQLiteMetadataWrites(stores []*SQLiteStore, id string, writes []map[string]string) []error {
	errs := make([]error, len(stores))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func(i int, store *SQLiteStore) {
			defer wg.Done()
			<-start
			errs[i] = store.Update(id, UpdateOpts{Metadata: writes[i]})
		}(i, store)
	}
	close(start)
	wg.Wait()
	return errs
}
