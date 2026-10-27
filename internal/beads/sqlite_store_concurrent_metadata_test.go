package beads_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestSQLiteStoreConcurrentDifferentKeyMetadataWritesBothLand pins the
// #1596 composition property on the native SQLite store: two handles opened
// on one store directory are two independent writers (their connection
// pools lock against each other exactly as two processes would), and a
// concurrent Update writing a DIFFERENT metadata key must not erase the
// other writer's key. rewriteBead merges the edit onto the row its own
// transaction read, so different keys compose and only a same-key race is
// last-writer-wins; the class-owned in-process id path (doBdByIDUpdate ->
// graph.Update) is this exact shape, and the bd-ledger counterpart against
// a real shared dolt server is
// TestBdStoreConcurrentDifferentKeySetMetadataComposesOnSharedServer.
func TestSQLiteStoreConcurrentDifferentKeyMetadataWritesBothLand(t *testing.T) {
	dir := t.TempDir()
	open := func() *beads.SQLiteStore {
		t.Helper()
		opened, err := beads.OpenSQLiteStore(dir)
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		store := opened.(*beads.SQLiteStore)
		t.Cleanup(func() { _ = store.CloseStore() })
		return store
	}
	writers := []beads.Store{open(), open()}
	created, err := writers[0].Create(beads.Bead{Title: "different-key metadata composition probe"})
	if err != nil {
		t.Fatalf("Create probe bead: %v", err)
	}

	// A fixed value per round hides every loss after the first, so each
	// round writes its own. One barrier releases both writers together.
	var lost []string
	for round := 1; round <= 50; round++ {
		value := fmt.Sprintf("v%d", round)
		errs := make([]error, len(writers))
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		for i, key := range []string{"k1", "k2"} {
			wg.Add(1)
			go func(i int, key string) {
				defer wg.Done()
				<-barrier
				errs[i] = writers[i].Update(created.ID, beads.UpdateOpts{Metadata: map[string]string{key: value}})
			}(i, key)
		}
		close(barrier)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d writer %d Update: %v", round, i, err)
			}
		}
		got, err := writers[0].Get(created.ID)
		if err != nil {
			t.Fatalf("round %d Get: %v", round, err)
		}
		if got.Metadata["k1"] != value || got.Metadata["k2"] != value {
			lost = append(lost, fmt.Sprintf("round %d: k1=%q k2=%q, want both %q", round, got.Metadata["k1"], got.Metadata["k2"], value))
		}
	}
	if len(lost) > 0 {
		t.Fatalf("%d of 50 rounds lost a concurrent different-key metadata write:\n%s", len(lost), strings.Join(lost, "\n"))
	}

	// Same-key races have no composition to check — last writer wins — but
	// both writers must still report nil and leave one of the two values.
	errs := make([]error, len(writers))
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for i, value := range []string{"same-a", "same-b"} {
		wg.Add(1)
		go func(i int, value string) {
			defer wg.Done()
			<-barrier
			errs[i] = writers[i].Update(created.ID, beads.UpdateOpts{Metadata: map[string]string{"k1": value}})
		}(i, value)
	}
	close(barrier)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("same-key race writer %d Update: %v", i, err)
		}
	}
	got, err := writers[0].Get(created.ID)
	if err != nil {
		t.Fatalf("Get after same-key race: %v", err)
	}
	if got.Metadata["k1"] != "same-a" && got.Metadata["k1"] != "same-b" {
		t.Fatalf("after same-key race k1 = %q, want one of the two written values", got.Metadata["k1"])
	}
}
