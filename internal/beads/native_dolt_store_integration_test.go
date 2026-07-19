//go:build integration

package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreRegularUpdateEventRecording verifies that calling
// SetMetadata on a non-ephemeral bead succeeds. This exercises
// RecordEventInTable on the regular events table, which regresses when the
// INSERT omits the id column and the live schema has no DEFAULT for it.
func TestNativeDoltStoreRegularUpdateEventRecording(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "update-event-regression", "gc")

	bead, err := store.Create(Bead{Title: "regular update event regression bead"})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}
	if bead.Ephemeral {
		t.Fatalf("Ephemeral = true on regular bead, want false")
	}
	if err := store.SetMetadata(bead.ID, "gc.routed_to", "gascity/builder"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get bead after SetMetadata: %v", err)
	}
	if got.Metadata["gc.routed_to"] != "gascity/builder" {
		t.Fatalf("Metadata[gc.routed_to] = %q, want %q", got.Metadata["gc.routed_to"], "gascity/builder")
	}
}

// TestNativeDoltStoreEphemeralMailSend verifies that creating an ephemeral message
// bead (the gc mail send code path) succeeds through the upstream beads library.
//
// Regression tripwire for the 2026-06-11 P0 incident: a beads version-skew broke
// gc mail send with "Field 'id' doesn't have a default value" because a newer
// schema migration dropped DEFAULT (UUID()) from wisp_events.id while the linked
// beads code still omitted id on INSERT. Released beads v1.0.5 is coherent, so
// this test PASSES today. It FAILS if a future go.mod upgrade ships a version
// where code and schema disagree on wisp_events.id.
func TestNativeDoltStoreEphemeralMailSend(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "mail-wisp-regression", "gc")

	// Create an ephemeral message bead — the beadmail.Send() path.
	// Ephemeral=true routes the INSERT to wisps + wisp_events tables.
	// A NOT NULL / missing-DEFAULT failure here reproduces the 2026-06-11 incident.
	sent, err := store.Create(Bead{
		Title:     "hello from mail regression",
		Type:      "message",
		Assignee:  "builder",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create ephemeral message bead (wisp_events INSERT): %v", err)
	}
	if !sent.Ephemeral {
		t.Fatalf("Ephemeral = false on returned bead %s, want true", sent.ID)
	}
	if sent.ID == "" {
		t.Fatal("returned bead has empty ID")
	}

	// List with TierWisps to confirm the bead is retrievable after the INSERT.
	results, err := store.List(ListQuery{
		TierMode: TierWisps,
		Assignee: "builder",
	})
	if err != nil {
		t.Fatalf("List wisp beads: %v", err)
	}
	var found bool
	for _, b := range results {
		if b.ID == sent.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created wisp bead %s not in List(TierWisps); got %d beads total", sent.ID, len(results))
	}
}

// TestNativeDoltStoreEventsIDDefaultRepair reproduces the live-DB regression
// where Dolt stripped DEFAULT (uuid()) from events.id: RecordEventInTable
// (reached via SetMetadata on a non-ephemeral bead) then fails because the
// upstream INSERT omits the id column. It proves repairIDDefault restores the
// default so the write succeeds — the same self-heal gc applies at store open.
func TestNativeDoltStoreEventsIDDefaultRepair(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	accessor, ok := storage.(rawDBGetter)
	if !ok {
		t.Skip("storage does not expose a raw DB")
	}
	db := accessor.DB()
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "events-default-repair", "gc")

	// Create while the default is intact (Create itself records an event).
	bead, err := store.Create(Bead{Title: "events id default repair bead"})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}

	// Reproduce the regression: strip the DEFAULT from events.id.
	if _, err := db.Exec("ALTER TABLE `events` MODIFY COLUMN `id` char(36) NOT NULL"); err != nil {
		t.Fatalf("strip events.id default: %v", err)
	}
	if err := store.SetMetadata(bead.ID, "gc.routed_to", "gascity/builder"); err == nil {
		t.Fatalf("SetMetadata succeeded with events.id default stripped, want failure")
	}

	// Repair restores the default; the same write then succeeds.
	if err := repairIDDefault(db, "events"); err != nil {
		t.Fatalf("repairIDDefault(events): %v", err)
	}
	if err := store.SetMetadata(bead.ID, "gc.routed_to", "gascity/builder"); err != nil {
		t.Fatalf("SetMetadata after repair: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after repair: %v", err)
	}
	if got.Metadata["gc.routed_to"] != "gascity/builder" {
		t.Fatalf("Metadata[gc.routed_to] = %q, want %q", got.Metadata["gc.routed_to"], "gascity/builder")
	}
}

func TestNativeDoltStoreRealBackendRoundTrip(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "native-integration", "gc")

	parent, err := store.Create(Bead{Title: "real native parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	blocker, err := store.Create(Bead{Title: "real native blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	child, err := store.Create(Bead{
		Title:    "real native child",
		ParentID: parent.ID,
		Needs:    []string{"blocks:" + blocker.ID},
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if got.ParentID != parent.ID {
		t.Fatalf("ParentID = %q, want %q", got.ParentID, parent.ID)
	}
	assertNativeDependency(t, got.Dependencies, child.ID, blocker.ID, "blocks")
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("Close child: %v", err)
	}
	closed, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get closed child: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("Status = %q, want closed", closed.Status)
	}
	if _, err := store.Get("gc-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing error = %v, want ErrNotFound", err)
	}
}

// startTestDoltServer launches a throwaway dolt sql-server in a temp data dir
// and returns a *sql.DB connected to a fresh database on it. Skips the test
// when the dolt binary is unavailable.
func startTestDoltServer(t *testing.T) *sql.DB {
	t.Helper()
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt binary not in PATH")
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()

	dataDir := t.TempDir()
	cmd := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", dataDir)
	cmd.Env = append(os.Environ(), "DOLT_ROOT_PATH="+dataDir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open dolt connection: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := db.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server did not become ready on port %d", port)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := db.Exec("CREATE DATABASE repairtest"); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	_ = db.Close()

	db, err = sql.Open("mysql", dsn+"repairtest")
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestRepairIDDefaultAgainstDoltServer exercises the SHOW COLUMNS-based probe
// end-to-end against a real dolt sql-server (the same wire protocol the live
// fleet uses): a stripped DEFAULT is detected and repaired, an intact DEFAULT
// is left alone, and an absent table is not an error. This covers the probe
// rewrite that replaced the per-open INFORMATION_SCHEMA.COLUMNS catalog scan.
func TestRepairIDDefaultAgainstDoltServer(t *testing.T) {
	db := startTestDoltServer(t)

	showIDDefault := func(table string) any {
		var field, colType, null, key, extra string
		var def any
		row := db.QueryRow(fmt.Sprintf("SHOW COLUMNS FROM `%s` LIKE 'id'", table))
		if err := row.Scan(&field, &colType, &null, &key, &def, &extra); err != nil {
			t.Fatalf("SHOW COLUMNS FROM %s: %v", table, err)
		}
		return def
	}

	// Stripped default: probe detects it and the ALTER restores it.
	if _, err := db.Exec("CREATE TABLE events (id char(36) NOT NULL, note text)"); err != nil {
		t.Fatalf("create events: %v", err)
	}
	if err := repairIDDefault(db, "events"); err != nil {
		t.Fatalf("repairIDDefault(events): %v", err)
	}
	if def := showIDDefault("events"); def == nil {
		t.Fatal("events.id Default still NULL after repair, want (uuid())")
	}

	// Intact default: repair is a no-op and must not error.
	if _, err := db.Exec("CREATE TABLE dependencies (id char(36) NOT NULL DEFAULT (uuid()), note text)"); err != nil {
		t.Fatalf("create dependencies: %v", err)
	}
	if err := repairIDDefault(db, "dependencies"); err != nil {
		t.Fatalf("repairIDDefault(dependencies) with intact default: %v", err)
	}
	if def := showIDDefault("dependencies"); def == nil {
		t.Fatal("dependencies.id Default = NULL after no-op repair, want (uuid())")
	}

	// Absent table (e.g. wisp_events on an older schema): tolerated, not an error.
	if err := repairIDDefault(db, "wisp_events"); err != nil {
		t.Fatalf("repairIDDefault(wisp_events) on absent table: %v", err)
	}

	// Table without an id column: nothing to repair, no error.
	if _, err := db.Exec("CREATE TABLE noid (pk int PRIMARY KEY)"); err != nil {
		t.Fatalf("create noid: %v", err)
	}
	if err := repairIDDefault(db, "noid"); err != nil {
		t.Fatalf("repairIDDefault(noid) without id column: %v", err)
	}
}
