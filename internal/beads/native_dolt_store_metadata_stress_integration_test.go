//go:build integration

package beads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// metadataStressRounds is how many writes each of the two writers issues. It is
// sized so the writers overlap for hundreds of read-to-write windows on a
// loaded CI host while the whole test stays within a few seconds per backend.
const metadataStressRounds = 60

// metadataStressMaxErrorRate bounds the fraction of SetMetadataBatch calls
// that may give up with ErrVersionMismatch after the retry budget. A refused
// write is surfaced, never silently lost, so a nonzero rate is correct; the
// bound catches a retry loop that stopped re-reading (every call would fail)
// without making the test sensitive to host load.
const metadataStressMaxErrorRate = 0.25

// TestNativeDoltStoreMetadataMergeSurvivesAConcurrentUpdateLoop is the
// unscripted form of the compare-and-swap proof: one goroutine loops Update on
// a bead's metadata while another loops SetMetadataBatch on the same bead, on
// a real Dolt backend, with no hook choosing the interleaving. Every write
// either lands or returns an error — none is silently undone by the other
// writer's stale read-merge-write — and the metadata merge gives up only
// within a bounded rate. It runs against the issues table and the wisps table,
// whose checked writes are separate backend paths, and against both the
// embedded engine and a sql-server, whose transaction isolation differs.
func TestNativeDoltStoreMetadataMergeSurvivesAConcurrentUpdateLoop(t *testing.T) {
	backends := map[string]func(t *testing.T) *NativeDoltStore{
		"embedded": func(t *testing.T) *NativeDoltStore {
			return openRealNativeDoltStoreForMergeProof(t, "merge-stress")
		},
		"sql-server": openServerNativeDoltStoreForMergeProof,
	}
	tables := map[string]bool{"issues": false, "wisps": true}
	for backendName, open := range backends {
		for tableName, ephemeral := range tables {
			t.Run(backendName+"/"+tableName, func(t *testing.T) {
				runMetadataMergeStress(t, open(t), ephemeral)
			})
		}
	}
}

func runMetadataMergeStress(t *testing.T, store *NativeDoltStore, ephemeral bool) {
	t.Helper()
	created, err := store.Create(Bead{
		Title:     "contended bead",
		Ephemeral: ephemeral,
		Metadata:  map[string]string{"gc.seed": "kept"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Ephemeral != ephemeral {
		t.Fatalf("Ephemeral = %v, want %v: the variant does not exercise the table it names", created.Ephemeral, ephemeral)
	}
	id := created.ID

	type writer struct {
		name   string
		write  func(key string) error
		landed []string
		errs   []error
	}
	writers := []*writer{
		{name: "update", write: func(key string) error {
			return store.Update(id, UpdateOpts{Metadata: map[string]string{key: "set"}})
		}},
		{name: "batch", write: func(key string) error {
			return store.SetMetadataBatch(id, map[string]string{key: "set"})
		}},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, w := range writers {
		wg.Add(1)
		go func(w *writer) {
			defer wg.Done()
			<-start
			for i := range metadataStressRounds {
				key := fmt.Sprintf("gc.stress.%s.%03d", w.name, i)
				if err := w.write(key); err != nil {
					w.errs = append(w.errs, fmt.Errorf("%s: %w", key, err))
					continue
				}
				w.landed = append(w.landed, key)
			}
		}(w)
	}
	close(start)
	wg.Wait()

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["gc.seed"] != "kept" {
		t.Errorf("gc.seed = %q, want %q: a stale merge dropped the key the bead was created with", got.Metadata["gc.seed"], "kept")
	}
	for _, w := range writers {
		lost := 0
		for _, key := range w.landed {
			if got.Metadata[key] != "set" {
				lost++
				if lost <= 5 {
					t.Errorf("%s: %s reported success but is %q in the final row: the other writer's stale merge undid it", w.name, key, got.Metadata[key])
				}
			}
		}
		if lost > 0 {
			t.Errorf("%s: %d of %d successful writes lost", w.name, lost, len(w.landed))
		}
		for _, err := range w.errs {
			// Contention may surface only as the two retryable races, each after
			// its own retry budget; anything else is a defect this test found.
			if !errors.Is(err, beadslib.ErrVersionMismatch) && !isNativeDoltSerializationConflict(err) {
				t.Errorf("%s: unexpected write error under contention: %v", w.name, err)
			}
		}
	}

	batch := writers[1]
	rate := float64(len(batch.errs)) / float64(metadataStressRounds)
	t.Logf("update: %d landed, %d refused; batch: %d landed, %d refused (%.0f%%)",
		len(writers[0].landed), len(writers[0].errs), len(batch.landed), len(batch.errs), rate*100)
	if rate > metadataStressMaxErrorRate {
		t.Fatalf("SetMetadataBatch gave up on %d of %d calls (%.0f%%), want at most %.0f%%: the merge retry is not re-reading onto the committed row",
			len(batch.errs), metadataStressRounds, rate*100, metadataStressMaxErrorRate*100)
	}
	if len(writers[0].landed) == 0 || len(batch.landed) == 0 {
		t.Fatalf("a writer never landed (update %d, batch %d): the loops did not contend", len(writers[0].landed), len(batch.landed))
	}
}

// TestNativeDoltStoreSetMetadataBatchKeepsAConcurrentUpdateOnAWisp is the
// scripted interleaving of the real-Dolt proof on the wisps table: wisp
// metadata writes take the backend's wisp checked-update path, which must
// refuse a stale swap exactly as the issues path does.
func TestNativeDoltStoreSetMetadataBatchKeepsAConcurrentUpdateOnAWisp(t *testing.T) {
	store := openRealNativeDoltStoreForMergeProof(t, "merge-race-wisp")
	created, err := store.Create(Bead{
		Title:     "fenced wisp step",
		Ephemeral: true,
		Metadata:  map[string]string{"gc.instantiating": "true", "gc.deferred_routed_to": "rig/pool"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.Ephemeral {
		t.Fatalf("Create returned a non-ephemeral bead %s; the test would not exercise the wisps table", created.ID)
	}
	id := created.ID

	reads := 0
	store.afterMetadataMergeRead = func(readID string) {
		reads++
		if reads != 1 || readID != id {
			return
		}
		if err := store.Update(id, UpdateOpts{Metadata: map[string]string{
			"gc.instantiating":      "",
			"gc.deferred_routed_to": "",
			"gc.routed_to":          "rig/pool",
		}}); err != nil {
			t.Errorf("competing Update: %v", err)
		}
	}
	if err := store.SetMetadataBatch(id, map[string]string{"gc.heartbeat": "now"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if reads != 2 {
		t.Fatalf("merge reads = %d, want 2 (the wisp swap refused after the competing write, then a fresh read)", reads)
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for key, want := range map[string]string{
		"gc.heartbeat":          "now",
		"gc.instantiating":      "",
		"gc.deferred_routed_to": "",
		"gc.routed_to":          "rig/pool",
	} {
		if got.Metadata[key] != want {
			t.Errorf("%s = %q, want %q (the competing write was lost or the merge was)", key, got.Metadata[key], want)
		}
	}
}

// openServerNativeDoltStoreForMergeProof opens the native store against a
// fresh dolt sql-server, the deployment shape where concurrent writers run in
// separate server transactions.
func openServerNativeDoltStoreForMergeProof(t *testing.T) *NativeDoltStore {
	t.Helper()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create .beads directory: %v", err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	storage, err := beadslib.OpenBestAvailable(t.Context(), beadsDir)
	if err != nil {
		t.Fatalf("open the server-backed native storage: %v", err)
	}
	if err := storage.SetConfig(t.Context(), "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("close the initializing storage: %v", err)
	}
	store, err := newNativeDoltStoreAt(t.Context(), scopeRoot, nil)
	if err != nil {
		t.Fatalf("open the server-backed native store: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}
