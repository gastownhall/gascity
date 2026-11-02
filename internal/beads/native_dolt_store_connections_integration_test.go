//go:build integration

package beads

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreForeignConnectionsNamesEveryOtherSession runs against a
// throwaway sql-server on a free loopback port: the store's own session is
// left out, and a second client is listed with its user, until it goes.
func TestNativeDoltStoreForeignConnectionsNamesEveryOtherSession(t *testing.T) {
	ctx := context.Background()
	port := startTestDoltServer(t)
	switch port {
	case 3307, 45865, 8123:
		t.Fatalf("the throwaway server landed on reserved port %d", port)
	}
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	accessor, ok := storage.(testRawDBGetter)
	if !ok {
		t.Fatal("server-backed storage exposes no raw DB")
	}
	// The migration's one-shot open holds one connection (cmd/gc
	// nativeDoltCliPoolCap); the library's default pool would hold idle ones
	// the check rightly counts.
	accessor.DB().SetMaxOpenConns(1)
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "connections", "gc")

	// The harness's own admin client (no database: it ran CREATE DATABASE)
	// stays connected for the whole test; it is foreign to the store, and is
	// the baseline. The store's session is on beads, so none of the baseline
	// may be.
	baseline, err := foreignOf(t, store)
	if err != nil {
		t.Fatalf("ForeignConnections: %v", err)
	}
	for _, c := range baseline {
		if c.DB == "beads" {
			t.Fatalf("ForeignConnections = %+v with no other client on beads: the store's own session counted", baseline)
		}
	}

	other, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/beads", port))
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("second client: %v", err)
	}
	var otherID int64
	if err := pinned.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	foreign, err := foreignOf(t, store)
	if err != nil {
		t.Fatalf("ForeignConnections: %v", err)
	}
	var onBeads []DoltConnection
	for _, c := range foreign {
		if c.DB == "beads" {
			onBeads = append(onBeads, c)
		}
	}
	if len(foreign) != len(baseline)+1 || len(onBeads) != 1 || onBeads[0].ID != otherID || onBeads[0].User != "root" {
		t.Fatalf("ForeignConnections = %+v, want the baseline %+v plus session %d (root on beads)", foreign, baseline, otherID)
	}

	_ = pinned.Close()
	_ = other.Close()
	gone, err := foreignOf(t, store)
	if err != nil {
		t.Fatalf("ForeignConnections: %v", err)
	}
	if len(gone) != len(baseline) {
		t.Fatalf("ForeignConnections = %+v after the client left, want the baseline %+v", gone, baseline)
	}
}

// foreignOf asks through the exported entry point, as the migration does.
func foreignOf(t *testing.T, store *NativeDoltStore) ([]DoltConnection, error) {
	t.Helper()
	sessions, ok, err := ForeignDoltConnections(context.Background(), WorkStore{Store: store})
	if !ok {
		t.Fatal("ForeignDoltConnections does not recognize a native Dolt store under a work handle")
	}
	return sessions, err
}
