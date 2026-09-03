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

// testRawDBGetter matches beadslib's internal storage.RawDBAccessor without
// bringing that implementation detail into production code.
type testRawDBGetter interface {
	DB() *sql.DB
}

// TestNativeDoltStoreOpenPreservesMissingIDDefaults verifies the v59 contract:
// dependencies.id, events.id, and wisp_events.id intentionally have no server
// default. Opening a native store must not mutate that schema, and normal
// writes must provide their IDs explicitly.
func TestNativeDoltStoreOpenPreservesMissingIDDefaults(t *testing.T) {
	ctx := context.Background()
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
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
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
	accessor, ok := storage.(testRawDBGetter)
	if !ok {
		t.Skip("storage does not expose a raw DB")
	}
	db := accessor.DB()
	for _, table := range []string{"dependencies", "events", "wisp_events"} {
		if _, err := db.Exec("ALTER TABLE `" + table + "` MODIFY COLUMN `id` char(36) NOT NULL"); err != nil {
			t.Fatalf("strip %s.id default: %v", table, err)
		}
		assertNativeIDDefaultAbsent(t, db, table)
	}

	store, err := newNativeDoltStoreAt(ctx, scopeRoot, nil)
	if err != nil {
		t.Fatalf("newNativeDoltStoreAt: %v", err)
	}
	for _, table := range []string{"dependencies", "events", "wisp_events"} {
		assertNativeIDDefaultAbsent(t, db, table)
	}

	issue, err := store.Create(Bead{Title: "missing-default issue"})
	if err != nil {
		t.Fatalf("Create issue: %v", err)
	}
	dependsOn, err := store.Create(Bead{Title: "missing-default dependency"})
	if err != nil {
		t.Fatalf("Create dependency target: %v", err)
	}
	if err := store.DepAdd(issue.ID, dependsOn.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if err := store.SetMetadata(issue.ID, "gc.routed_to", "gascity/builder"); err != nil {
		t.Fatalf("SetMetadata (events write): %v", err)
	}
	if _, err := store.Create(Bead{
		Title:     "missing-default wisp",
		Type:      "message",
		Assignee:  "builder",
		Ephemeral: true,
	}); err != nil {
		t.Fatalf("Create ephemeral bead (wisp_events write): %v", err)
	}
}

func assertNativeIDDefaultAbsent(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var field, colType, nullable, key, extra string
	var defaultValue any
	if err := db.QueryRow("SHOW COLUMNS FROM `"+table+"` LIKE 'id'").Scan(&field, &colType, &nullable, &key, &defaultValue, &extra); err != nil {
		t.Fatalf("SHOW COLUMNS FROM %s: %v", table, err)
	}
	if defaultValue != nil {
		t.Fatalf("%s.id default = %v, want absent", table, defaultValue)
	}
}

// startTestDoltServer launches a throwaway server for tests that need the raw
// SQL accessor exposed by upstream's server-mode Dolt store.
func startTestDoltServer(t *testing.T) int {
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
	if err := lis.Close(); err != nil {
		t.Fatalf("release test port: %v", err)
	}

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

	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port))
	if err != nil {
		t.Fatalf("open dolt connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
	if _, err := db.Exec("CREATE DATABASE beads"); err != nil {
		t.Fatalf("create beads database: %v", err)
	}
	return port
}

// The weak-ParentID contract is a claim about what the UPSTREAM library does
// with a dependency target it classifies as external, and the in-memory fixture
// the conformance suite runs cannot tell "never resolved" from "refused": its
// AddDependency validates nothing at all. This is the same claim on real Dolt,
// where isCrossPrefixDep routes the target to depends_on_external and the
// existence check is skipped.
//
// Both halves are here on purpose. A foreign parent is carried on Create and on
// Update and reads back verbatim; a dangling id in the store's OWN namespace is
// refused before anything is written, which is the line validateCreatedDependencies
// says it holds so the library's post-commit refusal never turns into a create
// plus a compensating delete.
func TestNativeDoltStoreRealBackendCrossPrefixParent(t *testing.T) {
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

	// A graph-class molecule in another binding, named by a work-class step
	// here. Nothing reconciles the two ledgers, and this store cannot see it.
	foreign := "gcg-70b1e5f2-a"

	control, err := store.Create(Bead{Title: "control, no parent"})
	if err != nil {
		t.Fatalf("Create control: %v", err)
	}
	child, err := store.Create(Bead{Title: "step whose molecule lives in another ledger", ParentID: foreign})
	if err != nil {
		t.Fatalf("Create with a cross-prefix parent was refused on real Dolt: %v — this breaks every cross-store molecule", err)
	}
	if got := nativeBeadIDPrefix(child.ID); got != nativeBeadIDPrefix(control.ID) {
		t.Errorf("a child naming a %q parent was minted as %q, but this store mints %q-shaped ids; placement followed ParentID instead of class", foreign, child.ID, control.ID)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if got.ParentID != foreign {
		t.Errorf("ParentID round-tripped as %q, want %q verbatim", got.ParentID, foreign)
	}
	children, err := store.Children(foreign)
	if err != nil {
		t.Fatalf("Children on a cross-prefix parent: %v", err)
	}
	if len(children) != 1 || children[0].ID != child.ID {
		t.Errorf("Children(%q) returned %d beads, want the one child stored here; a molecule's steps would read as missing", foreign, len(children))
	}

	if err := store.Update(control.ID, UpdateOpts{ParentID: &foreign}); err != nil {
		t.Fatalf("Update reparenting onto a cross-prefix parent was refused: %v — Create admitted the same value", err)
	}
	reparented, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("Get control after reparent: %v", err)
	}
	if reparented.ParentID != foreign {
		t.Errorf("after reparenting, ParentID is %q, want %q verbatim", reparented.ParentID, foreign)
	}

	// The other half of the boundary: inside its own namespace this store can
	// see the absence, and both arms refuse it. Whether that refusal arrived
	// before the write or after a committed create plus a compensating delete is
	// not observable through this interface on real Dolt — the library refuses
	// the same value either way and this backend does not mint a readable
	// sequence — so the timing is pinned on the in-memory fixture, whose
	// AddDependency validates nothing and therefore only refuses when this store
	// does. What is pinned HERE is the library half the fixture cannot model.
	dangling := "gc-999999"
	if _, err := store.Create(Bead{Title: "step naming a missing parent in this ledger", ParentID: dangling}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Create(parent %q) = %v, want ErrNotFound", dangling, err)
	}
	if err := store.Update(control.ID, UpdateOpts{ParentID: &dangling}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(parent %q) = %v, want ErrNotFound", dangling, err)
	}
	stillForeign, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("Get control after the refused reparent: %v", err)
	}
	if stillForeign.ParentID != foreign {
		t.Errorf("ParentID after the refused reparent is %q, want the previous %q; the refusal has to come before the write", stillForeign.ParentID, foreign)
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
