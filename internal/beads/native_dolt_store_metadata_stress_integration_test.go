//go:build integration

package beads

import (
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

// metadataStressMaxErrorRate is a sanity ceiling on the fraction of
// SetMetadataBatch calls that may fail under contention.
//
// Both writers go through the facade, which resolves their keys against the row
// inside its own write transaction, so contention has one outcome a caller can
// see: a serialization conflict on the later of two overlapping transactions,
// which the store replays up to nativeWriteAttempts times. A call fails only
// when every replay conflicts again. That failure is surfaced, never silently
// lost, so a nonzero rate is correct, and how often the budget runs out depends
// on how the host schedules the two writers.
//
// The rate proves nothing about correctness. This test's correctness assertions
// are the lost-update checks, which stay strict: zero successful writes may be
// missing from the final row. The ceiling only catches a gross liveness failure.
const metadataStressMaxErrorRate = 0.5

// TestNativeDoltStoreMetadataMergeSurvivesAConcurrentUpdateLoop is the
// unscripted exercise of the facade merge: one goroutine loops Update on a
// bead's metadata while another loops SetMetadataBatch on the same bead, on a
// real Dolt backend, with no hook choosing the interleaving.
//
// Neither writer reads the row before its write transaction does, so the window
// a lost update needs — a commit landing between a writer's read and its write
// — only exists inside one transaction. There, Dolt's commit-time merge either
// combines the two edits or refuses the later transaction with a serialization
// conflict that the store replays. Every write either lands or returns that
// conflict, and none is silently undone by the other writer. It runs against the
// issues table and the wisps table, whose writes are separate backend paths, and
// against both the embedded engine and a sql-server, whose transaction
// isolation differs.
//
// No hook reaches inside a facade transaction, so this race is the only
// exercise of that window. The scripted ordering — a competing session's commit
// landing between two of one writer's batches — is
// TestNativeDoltStoreSetMetadataBatchKeepsAnotherSessionsUpdate.
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
			// Contention may surface only as a serialization conflict that
			// outlasted the store's replays. Neither writer sends an expected
			// version, so even ErrVersionMismatch is a defect this test found.
			if !isNativeDoltSerializationConflict(err) {
				t.Errorf("%s: unexpected write error under contention: %v", w.name, err)
			}
		}
	}

	batch := writers[1]
	rate := float64(len(batch.errs)) / float64(metadataStressRounds)
	t.Logf("update: %d landed, %d refused; batch: %d landed, %d refused (%.0f%%)",
		len(writers[0].landed), len(writers[0].errs), len(batch.landed), len(batch.errs), rate*100)
	if rate > metadataStressMaxErrorRate {
		t.Fatalf("SetMetadataBatch gave up on %d of %d calls (%.0f%%), want at most %.0f%%: the metadata merge retry is failing far more often than contention explains",
			len(batch.errs), metadataStressRounds, rate*100, metadataStressMaxErrorRate*100)
	}
	if len(writers[0].landed) == 0 || len(batch.landed) == 0 {
		t.Fatalf("a writer never landed (update %d, batch %d): the loops did not contend", len(writers[0].landed), len(batch.landed))
	}
}

// openServerNativeDoltStoreForMergeProof opens the native store against a
// fresh dolt sql-server, the deployment shape where concurrent writers run in
// separate server transactions.
func openServerNativeDoltStoreForMergeProof(t *testing.T) *NativeDoltStore {
	t.Helper()
	return openNativeDoltStoreHandleForMergeProof(t, initServerScopeForMergeProof(t))
}

// openNativeDoltStoreHandleForMergeProof opens one store handle on scopeRoot.
// Every call opens storage of its own, so two handles on one scope are two
// server sessions, which is how two processes write the same ledger.
func openNativeDoltStoreHandleForMergeProof(t *testing.T, scopeRoot string) *NativeDoltStore {
	t.Helper()
	store, err := newNativeDoltStoreAt(t.Context(), scopeRoot, nil)
	if err != nil {
		t.Fatalf("open the server-backed native store: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

// initServerScopeForMergeProof starts a fresh dolt sql-server and returns a
// scope root whose metadata points at it, with the issue prefix configured.
func initServerScopeForMergeProof(t *testing.T) string {
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
	return scopeRoot
}
