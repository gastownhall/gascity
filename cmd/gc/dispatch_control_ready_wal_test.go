package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestControlReadyPerScanReadsReleaseRealSQLiteHandlesAndLetWALCheckpoint is
// the #6255 regression pinned on a real SQLite ledger rather than a counting
// fake, at the per-scan read frequency ga-vnycm2.9 introduced.
//
// #6255: re-primed snapshots that never closed their backing store held read
// marks on the shared ledger's WAL, so checkpoints could never truncate it (875
// MB WAL vs 483 MB DB) and federated work queries starved. A read per scan
// multiplies the number of opens, so every one of them must be released.
//
// The scan here opens its scoped leg as a fresh SQLiteStore over the same
// database a long-lived writer keeps appending to, exactly as a control
// dispatcher's scope leg does between worker writes. After many scans
// interleaved with writes, every scan-opened store must be closed, and a
// TRUNCATE checkpoint from an outside connection must complete without being
// blocked by any reader and leave the WAL empty.
func TestControlReadyPerScanReadsReleaseRealSQLiteHandlesAndLetWALCheckpoint(t *testing.T) {
	ledgerDir := t.TempDir()
	opened, err := beads.OpenSQLiteStore(ledgerDir)
	if err != nil {
		t.Fatalf("open writer store: %v", err)
	}
	writer := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = writer.CloseStore() })

	var scanStores []*beads.SQLiteStore
	installControlReadyLegSourcesFn(t, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		s, err := beads.OpenSQLiteStore(ledgerDir)
		if err != nil {
			return nil, nil, err
		}
		scoped := s.(*beads.SQLiteStore)
		scanStores = append(scanStores, scoped)
		return []beads.Store{scoped}, []beads.Store{scoped}, nil
	})

	const scans = 60
	for i := 0; i < scans; i++ {
		// A worker closes a step and the dispatcher's own write lands a new
		// control: the WAL grows between every scan.
		b, err := writer.Create(beads.Bead{
			Type:     "task",
			Title:    fmt.Sprintf("control %d", i),
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: freshnessControlTarget, beadmeta.KindMetadataKey: beadmeta.KindScopeCheck},
		})
		if err != nil {
			t.Fatalf("scan %d: writer create: %v", i, err)
		}
		if i%2 == 1 {
			if err := writer.Close(b.ID); err != nil {
				t.Fatalf("scan %d: writer close: %v", i, err)
			}
		}

		ready, legsOpened, err := controlReadyLegsReady(ledgerDir, ledgerDir, nil)
		if err != nil || !legsOpened {
			t.Fatalf("scan %d: controlReadyLegsReady = opened %t, err %v", i, legsOpened, err)
		}
		if want := i/2 + 1; len(ready) != want {
			t.Fatalf("scan %d: ready = %d, want %d (fresh read of the writer's state)", i, len(ready), want)
		}
	}

	if len(scanStores) != scans {
		t.Fatalf("scan-opened stores = %d, want one per scan (%d)", len(scanStores), scans)
	}
	for i, s := range scanStores {
		if _, err := s.Get("gc-1"); !errors.Is(err, beads.ErrStoreClosed) {
			t.Fatalf("scan %d's store still open after the scan returned (Get err = %v, want ErrStoreClosed): a leaked handle pins the WAL (#6255)", i, err)
		}
	}

	walPath := filepath.Join(ledgerDir, "beads.sqlite-wal")
	if info, err := os.Stat(walPath); err != nil || info.Size() == 0 {
		t.Fatalf("premise: the writer's WAL should be non-empty before the checkpoint (stat err=%v)", err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(ledgerDir, "beads.sqlite"))
	if err != nil {
		t.Fatalf("open raw checkpoint connection: %v", err)
	}
	defer raw.Close() //nolint:errcheck // test cleanup
	var busy, logFrames, checkpointed int
	if err := raw.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatalf("wal_checkpoint(TRUNCATE): %v", err)
	}
	if busy != 0 {
		t.Fatalf("wal_checkpoint(TRUNCATE) busy=%d (log=%d checkpointed=%d): a reader still holds a WAL read mark after %d scans", busy, logFrames, checkpointed, scans)
	}
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat WAL after checkpoint: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("WAL size after TRUNCATE checkpoint = %d bytes, want 0", info.Size())
	}
}
