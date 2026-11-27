//go:build integration

package beads

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	beadslib "github.com/steveyegge/beads"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// claimAuthorityFixture is one bd database on a real dolt sql-server: the
// native store bd's own library writes through, and a raw SQL handle a client
// sharing the root principal would use.
type claimAuthorityFixture struct {
	store  *NativeDoltStore
	db     *sql.DB
	tables []string
}

const absentClaimant = "mac-Mac-4026"

func newClaimAuthorityFixture(t *testing.T) claimAuthorityFixture {
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
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/beads", port))
	if err != nil {
		t.Fatalf("open raw dolt connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	capability, err := ProbeClaimAuthorityCapability(t.Context(), db)
	if err != nil {
		t.Fatalf("ProbeClaimAuthorityCapability: %v", err)
	}
	if !capability.CanHost() {
		t.Fatalf("a bd database on dolt sql-server as root cannot host the boundary: %s", capability.Reason())
	}
	return claimAuthorityFixture{store: store, db: db, tables: capability.Tables}
}

// arm installs the triggers over one pinned session, as the controller does.
func (f claimAuthorityFixture) arm(t *testing.T) {
	t.Helper()
	conn, err := f.db.Conn(t.Context())
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup
	if _, err := ArmClaimAuthority(t.Context(), conn, f.tables); err != nil {
		t.Fatalf("ArmClaimAuthority: %v", err)
	}
}

func (f claimAuthorityFixture) createRouted(t *testing.T, ephemeral bool) Bead {
	t.Helper()
	created, err := f.store.Create(Bead{
		Title:     "routed work",
		Type:      "task",
		Ephemeral: ephemeral,
		Metadata:  map[string]string{beadmeta.RoutedToMetadataKey: "rig/pool"},
	})
	if err != nil {
		t.Fatalf("Create routed bead: %v", err)
	}
	return created
}

func (f claimAuthorityFixture) tableOf(ephemeral bool) string {
	if ephemeral {
		return "wisps"
	}
	return "issues"
}

// awaitRowVisible waits until the raw session sees id in table. A wisp is
// written through bd's ignored-tables connection, and a raw UPDATE that runs
// before the row is visible matches nothing, which proves nothing.
func (f claimAuthorityFixture) awaitRowVisible(t *testing.T, table, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := f.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM `"+table+"` WHERE id = ?", id).Scan(&n); err != nil {
			t.Fatalf("count %s in %s: %v", id, table, err)
		}
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became visible in %s to the raw session", id, table)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func assertBeadOpenUnassigned(t *testing.T, store *NativeDoltStore, id string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("Get(%s) = status %q assignee %q, want open and unassigned", id, got.Status, got.Assignee)
	}
}

func assertClaimAuthorityRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s succeeded, want a claim-authority refusal", what)
	}
	if !strings.Contains(err.Error(), ClaimAuthorityMarker) {
		t.Fatalf("%s error = %v, want the %q marker", what, err, ClaimAuthorityMarker)
	}
	if !errors.Is(ClassifyClaimAuthorityError(err), ErrClaimNotAuthorized) {
		t.Fatalf("%s error %v does not classify as ErrClaimNotAuthorized", what, err)
	}
}

// AC-1: an armed store refuses a routed claim or assignment by an identity no
// city published, through bd's own library (claim and assign), raw SQL UPDATE
// and raw SQL INSERT, on both bead tiers, and leaves the bead open and
// unassigned.
func TestClaimAuthorityRefusesUnpublishedRoutedClaims(t *testing.T) {
	f := newClaimAuthorityFixture(t)
	f.arm(t)
	for _, ephemeral := range []bool{false, true} {
		t.Run(f.tableOf(ephemeral), func(t *testing.T) {
			bead := f.createRouted(t, ephemeral)

			_, _, err := f.store.Claim(bead.ID, absentClaimant)
			assertClaimAuthorityRefusal(t, "library claim", err)
			assertBeadOpenUnassigned(t, f.store, bead.ID)

			assignee := absentClaimant
			err = f.store.Update(bead.ID, UpdateOpts{Assignee: &assignee})
			assertClaimAuthorityRefusal(t, "library assign", err)
			assertBeadOpenUnassigned(t, f.store, bead.ID)

			f.awaitRowVisible(t, f.tableOf(ephemeral), bead.ID)
			res, err := f.db.ExecContext(t.Context(),
				"UPDATE `"+f.tableOf(ephemeral)+"` SET assignee = ?, status = 'in_progress' WHERE id = ?", absentClaimant, bead.ID)
			if err == nil {
				n, _ := res.RowsAffected()
				t.Fatalf("raw UPDATE of %s succeeded (%d rows), want a claim-authority refusal", bead.ID, n)
			}
			assertClaimAuthorityRefusal(t, "raw UPDATE", err)
			var myErr *mysql.MySQLError
			if !errors.As(err, &myErr) || myErr.Number != mysqlErrSignal || string(myErr.SQLState[:]) != "45000" {
				t.Fatalf("raw UPDATE error = %#v, want errno 1644 SQLSTATE 45000", err)
			}
			assertBeadOpenUnassigned(t, f.store, bead.ID)
		})
	}

	_, err := f.db.ExecContext(t.Context(),
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, assignee, metadata) "+
			"VALUES ('gc-rawins', 'raw', '', '', '', '', 'in_progress', 2, 'task', ?, JSON_OBJECT(?, 'rig/pool'))",
		absentClaimant, beadmeta.RoutedToMetadataKey)
	assertClaimAuthorityRefusal(t, "raw INSERT", err)
	if _, err := f.store.Get("gc-rawins"); err == nil {
		t.Fatal("the refused raw INSERT left a row behind")
	}
}
