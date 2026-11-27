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
	port   int
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
	return claimAuthorityFixture{store: store, db: db, port: port, tables: capability.Tables}
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

func (f claimAuthorityFixture) reconcile(t *testing.T, pub ClaimAuthorityPublisher, prune bool, identities ...string) ClaimAuthorityReconcileReport {
	t.Helper()
	conn, err := f.db.Conn(t.Context())
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup
	entries := make([]ClaimAuthorityEntry, 0, len(identities))
	for _, identity := range identities {
		entries = append(entries, ClaimAuthorityEntry{Identity: identity, Source: ClaimAuthoritySession})
	}
	report, err := ReconcileClaimAuthority(t.Context(), conn, ClaimAuthorityReconcile{
		Publisher: pub, Entries: entries, Now: time.Now().UTC(), Prune: prune,
	})
	if err != nil {
		t.Fatalf("ReconcileClaimAuthority(%s, prune=%v): %v", pub.Mode, prune, err)
	}
	return report
}

func testClaimAuthorityPublisher(city, mode string) ClaimAuthorityPublisher {
	return ClaimAuthorityPublisher{Tag: ClaimAuthorityCityTag("/cities/"+city, city), City: city, Mode: mode}
}

func assertClaimed(t *testing.T, store *NativeDoltStore, id, assignee string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Assignee != assignee {
		t.Fatalf("Get(%s).Assignee = %q, want %q", id, got.Assignee, assignee)
	}
}

// AC-2: an auto-mode reconcile publishes an open pool session's identities
// before it arms, so that session claims routed work on both tiers; a
// pre-start publish (prune=false) for a session that started mid-tick
// authorizes it at once without dropping the identities already published.
func TestClaimAuthorityPublishedPoolSessionCanClaim(t *testing.T) {
	f := newClaimAuthorityFixture(t)
	pub := testClaimAuthorityPublisher("alpha", ClaimAuthorityModeAuto)
	const running = "rig/pool-1"

	report := f.reconcile(t, pub, true, running, "pool-gc-abc")
	if !report.Armed || !report.ArmedNow {
		t.Fatalf("auto reconcile on a capable store = %+v, want it armed now", report)
	}
	for _, ephemeral := range []bool{false, true} {
		t.Run(f.tableOf(ephemeral), func(t *testing.T) {
			started := "rig/pool-started-" + f.tableOf(ephemeral)
			bead := f.createRouted(t, ephemeral)
			if _, _, err := f.store.Claim(bead.ID, running); err != nil {
				t.Fatalf("published pool session %s claim: %v", running, err)
			}
			assertClaimed(t, f.store, bead.ID, running)

			fresh := f.createRouted(t, ephemeral)
			_, _, err := f.store.Claim(fresh.ID, started)
			assertClaimAuthorityRefusal(t, "claim before the pre-start publish", err)
			f.reconcile(t, pub, false, started)
			if _, _, err := f.store.Claim(fresh.ID, started); err != nil {
				t.Fatalf("claim by %s after its pre-start publish: %v", started, err)
			}
			assertClaimed(t, f.store, fresh.ID, started)

			again := f.createRouted(t, ephemeral)
			if _, _, err := f.store.Claim(again.ID, running); err != nil {
				t.Fatalf("the additive pre-start publish dropped %s: %v", running, err)
			}
		})
	}
}

// AC-4: on an armed store, unassigning or releasing routed work held by an
// identity nobody publishes any more, and claiming or assigning a bead with
// no routing metadata, all succeed unchanged, through the library and raw
// SQL, on both tiers.
func TestClaimAuthorityAllowsReleaseAndNonRoutedWrites(t *testing.T) {
	f := newClaimAuthorityFixture(t)
	pub := testClaimAuthorityPublisher("alpha", ClaimAuthorityModeAuto)
	const gone = "rig/pool-gone"
	for _, ephemeral := range []bool{false, true} {
		t.Run(f.tableOf(ephemeral), func(t *testing.T) {
			f.reconcile(t, pub, true, gone)
			held := f.createRouted(t, ephemeral)
			if _, _, err := f.store.Claim(held.ID, gone); err != nil {
				t.Fatalf("claim by published %s: %v", gone, err)
			}
			released := f.createRouted(t, ephemeral)
			if _, _, err := f.store.Claim(released.ID, gone); err != nil {
				t.Fatalf("claim by published %s: %v", gone, err)
			}
			f.reconcile(t, pub, true) // the session ended: its identities are pruned

			empty, open := "", "open"
			if err := f.store.Update(held.ID, UpdateOpts{Assignee: &empty}); err != nil {
				t.Fatalf("unassign of routed work held by the pruned %s: %v", gone, err)
			}
			if err := f.store.Update(released.ID, UpdateOpts{Assignee: &empty, Status: &open}); err != nil {
				t.Fatalf("release of routed work held by the pruned %s: %v", gone, err)
			}
			assertClaimed(t, f.store, held.ID, "")
			assertBeadOpenUnassigned(t, f.store, released.ID)

			plain, err := f.store.Create(Bead{Title: "unrouted work", Type: "task", Ephemeral: ephemeral})
			if err != nil {
				t.Fatalf("Create unrouted bead: %v", err)
			}
			if _, _, err := f.store.Claim(plain.ID, absentClaimant); err != nil {
				t.Fatalf("claim of an unrouted bead by %s: %v", absentClaimant, err)
			}
			assertClaimed(t, f.store, plain.ID, absentClaimant)
			f.awaitRowVisible(t, f.tableOf(ephemeral), plain.ID)
			if _, err := f.db.ExecContext(t.Context(),
				"UPDATE `"+f.tableOf(ephemeral)+"` SET assignee = ?, status = 'in_progress' WHERE id = ?", absentClaimant+"-raw", plain.ID); err != nil {
				t.Fatalf("raw assign of an unrouted bead: %v", err)
			}
		})
	}
}

func (f claimAuthorityFixture) doltCommits(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM dolt_log").Scan(&n); err != nil {
		t.Fatalf("count dolt_log: %v", err)
	}
	return n
}

func (f claimAuthorityFixture) assertArmedCurrent(t *testing.T, when string) {
	t.Helper()
	state, err := ReadClaimAuthorityTriggerState(t.Context(), f.db, f.tables)
	if err != nil {
		t.Fatalf("ReadClaimAuthorityTriggerState: %v", err)
	}
	if !state.Armed() || !state.Current {
		t.Fatalf("%s: trigger state = %+v, want the current triggers installed", when, state)
	}
}

// AC-5: after local_metadata is wiped under live triggers, an off-mode
// co-tenant's tick emits no DDL and no commit and leaves the triggers in
// place; the arming city's next tick republishes its rows, so its live
// session's routed claim, refused in the window, succeeds; unrouted writes
// succeed throughout.
func TestClaimAuthorityWipeKeepsTriggersUntilArmingCityRepublishes(t *testing.T) {
	f := newClaimAuthorityFixture(t)
	alpha := testClaimAuthorityPublisher("alpha", ClaimAuthorityModeAuto)
	bravo := testClaimAuthorityPublisher("bravo", ClaimAuthorityModeOff)
	const live, cotenant = "rig/live", "rig/bravo-1"
	f.reconcile(t, alpha, true, live)
	f.assertArmedCurrent(t, "after arming")

	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM `local_metadata` WHERE `key` LIKE 'gc.claim_authority.%'"); err != nil {
		t.Fatalf("wipe local_metadata: %v", err)
	}
	unrouted := func(when string) {
		t.Helper()
		plain, err := f.store.Create(Bead{Title: "unrouted", Type: "task"})
		if err != nil {
			t.Fatalf("Create unrouted bead: %v", err)
		}
		if _, _, err := f.store.Claim(plain.ID, absentClaimant); err != nil {
			t.Fatalf("%s: unrouted claim: %v", when, err)
		}
	}
	work := f.createRouted(t, false)
	_, _, err := f.store.Claim(work.ID, live)
	assertClaimAuthorityRefusal(t, "live session claim after the wipe", err)
	unrouted("after the wipe")

	commits := f.doltCommits(t)
	report := f.reconcile(t, bravo, true, cotenant)
	if report.ArmedNow || !report.Armed {
		t.Fatalf("off-mode co-tenant reconcile = %+v, want the store left armed and untouched", report)
	}
	if got := f.doltCommits(t); got != commits {
		t.Fatalf("off-mode co-tenant tick made %d Dolt commit(s), want none", got-commits)
	}
	f.assertArmedCurrent(t, "after the off-mode co-tenant tick")
	_, _, err = f.store.Claim(work.ID, live)
	assertClaimAuthorityRefusal(t, "live session claim before its city republishes", err)
	unrouted("after the co-tenant tick")

	report = f.reconcile(t, alpha, true, live)
	if report.ArmedNow || !report.Published.Changed() {
		t.Fatalf("arming city's tick after the wipe = %+v, want a republish and no DDL", report)
	}
	f.assertArmedCurrent(t, "after the arming city's tick")
	if _, _, err := f.store.Claim(work.ID, live); err != nil {
		t.Fatalf("live session claim after its city republished: %v", err)
	}
	assertClaimed(t, f.store, work.ID, live)
	other := f.createRouted(t, false)
	if _, _, err := f.store.Claim(other.ID, cotenant); err != nil {
		t.Fatalf("off-mode co-tenant's session claim on the armed store: %v", err)
	}
	unrouted("after the republish")
}

// AC-6: when city A prunes an ended session's identities from a store it
// shares with city B, only rows carrying A's tag go; B's live sessions,
// including one under an identity A also published, still claim there.
func TestClaimAuthorityPruneIsScopedToTheCityTag(t *testing.T) {
	f := newClaimAuthorityFixture(t)
	cityA := testClaimAuthorityPublisher("alpha", ClaimAuthorityModeAuto)
	cityB := testClaimAuthorityPublisher("bravo", ClaimAuthorityModeAuto)
	const ended, shared, bOnly = "rig/a-ended", "rig/shared", "rig/b-live"
	f.reconcile(t, cityA, true, ended, shared)
	f.reconcile(t, cityB, true, bOnly, shared)

	report := f.reconcile(t, cityA, true)
	if report.Published.Deleted != 2 {
		t.Fatalf("city A prune deleted %d rows, want its own 2", report.Published.Deleted)
	}
	for identity, want := range map[string]bool{ended: false, shared: true, bOnly: true} {
		got, err := ClaimAuthorityIdentityAuthorized(t.Context(), f.db, identity)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("after city A's prune, %s authorized = %v, want %v", identity, got, want)
		}
	}
	for _, identity := range []string{bOnly, shared} {
		work := f.createRouted(t, false)
		if _, _, err := f.store.Claim(work.ID, identity); err != nil {
			t.Fatalf("city B session %s claim after city A's prune: %v", identity, err)
		}
	}
	work := f.createRouted(t, false)
	_, _, err := f.store.Claim(work.ID, ended)
	assertClaimAuthorityRefusal(t, "claim by city A's ended session", err)
}

func (f claimAuthorityFixture) authorityRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM `local_metadata` WHERE `key` LIKE 'gc.claim_authority.%'").Scan(&n); err != nil {
		t.Fatalf("count authority rows: %v", err)
	}
	return n
}

func (f claimAuthorityFixture) triggerState(t *testing.T) ClaimAuthorityTriggerState {
	t.Helper()
	state, err := ReadClaimAuthorityTriggerState(t.Context(), f.db, f.tables)
	if err != nil {
		t.Fatalf("ReadClaimAuthorityTriggerState: %v", err)
	}
	return state
}

func (f claimAuthorityFixture) disarm(t *testing.T, force bool) (bool, error) {
	t.Helper()
	conn, err := f.db.Conn(t.Context())
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup
	return DisarmClaimAuthorityGuarded(t.Context(), conn, force)
}

// AC-7: an off city on a store nobody armed, and an auto city whose principal
// lacks TRIGGER, leave the store with no trigger and no rows; the read-only
// inspection lists routed work held outside the authority set; once armed, no
// reconcile in any mode drops the triggers, and the guarded disarm refuses
// while an auto heartbeat is present or, without force, while none is.
func TestClaimAuthorityOffAndIncapableStoresStayUnarmedAndOnlyDisarmRemoves(t *testing.T) {
	f := newClaimAuthorityFixture(t)
	alpha := testClaimAuthorityPublisher("alpha", ClaimAuthorityModeAuto)
	const live = "rig/live"

	stray := f.createRouted(t, false)
	if _, _, err := f.store.Claim(stray.ID, absentClaimant); err != nil {
		t.Fatalf("claim on the unarmed store: %v", err)
	}
	report := f.reconcile(t, testClaimAuthorityPublisher("alpha", ClaimAuthorityModeOff), true, live)
	if report.Armed || f.triggerState(t).Armed() || f.authorityRows(t) != 0 {
		t.Fatalf("off reconcile on an unarmed store = %+v, %d rows, want no trigger and no rows", report, f.authorityRows(t))
	}

	if _, err := f.db.ExecContext(t.Context(), "CREATE USER 'notrigger'@'%' IDENTIFIED BY 'pw'"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := f.db.ExecContext(t.Context(), "GRANT SELECT, INSERT, UPDATE, DELETE ON `beads`.* TO 'notrigger'@'%'"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	limited, err := sql.Open("mysql", fmt.Sprintf("notrigger:pw@tcp(127.0.0.1:%d)/beads", f.port))
	if err != nil {
		t.Fatalf("open limited connection: %v", err)
	}
	t.Cleanup(func() { _ = limited.Close() })
	conn, err := limited.Conn(t.Context())
	if err != nil {
		t.Fatalf("pin limited connection: %v", err)
	}
	report, err = ReconcileClaimAuthority(t.Context(), conn, ClaimAuthorityReconcile{
		Publisher: alpha, Entries: []ClaimAuthorityEntry{{Identity: live, Source: ClaimAuthoritySession}}, Now: time.Now().UTC(), Prune: true,
	})
	_ = conn.Close()
	if err != nil {
		t.Fatalf("auto reconcile without TRIGGER: %v", err)
	}
	if report.Armed || !strings.Contains(report.Skipped, "TRIGGER") || f.triggerState(t).Armed() || f.authorityRows(t) != 0 {
		t.Fatalf("auto reconcile without TRIGGER = %+v, %d rows, want unarmed, no rows, skip naming TRIGGER", report, f.authorityRows(t))
	}

	inspection, err := InspectClaimAuthority(t.Context(), f.db, []ClaimAuthorityEntry{{Identity: live, Source: ClaimAuthoritySession}})
	if err != nil {
		t.Fatalf("InspectClaimAuthority: %v", err)
	}
	if len(inspection.Outside) != 1 || inspection.Outside[0].BeadID != stray.ID || inspection.Outside[0].Assignee != absentClaimant {
		t.Fatalf("inspection outside holders = %+v, want only %s held by %s", inspection.Outside, stray.ID, absentClaimant)
	}

	f.reconcile(t, alpha, true, live)
	for _, mode := range []string{ClaimAuthorityModeOff, ClaimAuthorityModeAuto, ClaimAuthorityModeRequire} {
		f.reconcile(t, testClaimAuthorityPublisher("bravo", mode), true)
		f.assertArmedCurrent(t, "after a "+mode+" reconcile")
	}
	f.reconcile(t, testClaimAuthorityPublisher("bravo", ClaimAuthorityModeOff), true)

	if _, err := f.disarm(t, true); !errors.Is(err, ErrClaimAuthorityDisarmRefused) {
		t.Fatalf("disarm while alpha advertises auto = %v, want ErrClaimAuthorityDisarmRefused even with force", err)
	}
	f.assertArmedCurrent(t, "after the refused disarm")
	f.reconcile(t, testClaimAuthorityPublisher("alpha", ClaimAuthorityModeOff), true, live)
	f.assertArmedCurrent(t, "after alpha switched to off")
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM `local_metadata` WHERE `key` LIKE 'gc.claim_authority.%'"); err != nil {
		t.Fatalf("wipe local_metadata: %v", err)
	}
	if _, err := f.disarm(t, false); !errors.Is(err, ErrClaimAuthorityDisarmRefused) {
		t.Fatalf("disarm with no heartbeat and no force = %v, want ErrClaimAuthorityDisarmRefused", err)
	}
	f.assertArmedCurrent(t, "after the unforced disarm")
	dropped, err := f.disarm(t, true)
	if err != nil || !dropped {
		t.Fatalf("forced disarm = %v, %v, want the triggers dropped", dropped, err)
	}
	if state := f.triggerState(t); state.Armed() {
		t.Fatalf("after the forced disarm, triggers = %v, want none", state.Installed)
	}
	work := f.createRouted(t, false)
	if _, _, err := f.store.Claim(work.ID, absentClaimant); err != nil {
		t.Fatalf("claim on the disarmed store: %v", err)
	}
}
