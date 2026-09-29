package beads

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// sessionHexIDs are caller-pinned session ids whose hex tails end in long digit
// runs. The first parsed as > MaxInt64 (clamped, then wrapped the allocator);
// the second inflated it to 7230288047389818274 under the old loose parser.
var sessionHexIDs = []string{
	"gcg-session-720a2f0e555819670941710447925531",
	"gcg-session-0f0e1d2c3b4a59687230288047389818274",
}

func openSeqStore(t *testing.T, dir string) *SQLiteStore {
	t.Helper()
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

func mustMint(t *testing.T, store *SQLiteStore) string {
	t.Helper()
	created, err := store.Create(Bead{Title: "minted"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created.ID
}

func mustPin(t *testing.T, store *SQLiteStore, id string) {
	t.Helper()
	if _, err := store.CreateWithForeignID(Bead{ID: id, Title: "pinned"}); err != nil {
		t.Fatalf("CreateWithForeignID(%q): %v", id, err)
	}
}

func autoValue(t *testing.T, id string) int64 {
	t.Helper()
	n, ok := parseSQLiteAutoIDSuffix(sqliteGraphPrefix, id)
	if !ok {
		t.Fatalf("minted id %q is not a strict auto id", id)
	}
	return n
}

func TestParseSQLiteAutoIDSuffixIsStrict(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want int64
		ok   bool
	}{
		{"gcg-1", 1, true},
		{"gcg-98636506", 98636506, true},
		{"gcg-9223372036854775807", math.MaxInt64, true},
		{"gcg--9223372036854775808", math.MinInt64, true},
		{"gcg--1", -1, true},
		{"gcg-9223372036854775808", 0, false}, // out of range: never clamps
		{"gcg--9223372036854775809", 0, false},
		{"gcg-0", 0, false},
		{"gcg--0", 0, false},
		{"gcg-007", 0, false},
		{"gcg--07", 0, false},
		{"gcg-+7", 0, false},
		{"gcg- 7", 0, false},
		{"gcg-7 ", 0, false},
		{"gcg-", 0, false},
		{"gcg--", 0, false},
		{"gcg---1", 0, false},
		{"gcg-12a", 0, false},
		{"gcg-session-720a2f0e555819670941710447925531", 0, false},
		{"gcg-session-12345", 0, false},
		{"gcg-mol-12", 0, false},
		{"gcgx-12", 0, false},
		{"GCG-12", 0, false},
		{"gc-12", 0, false},
		{"12", 0, false},
	} {
		got, ok := parseSQLiteAutoIDSuffix(sqliteGraphPrefix, tc.id)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseSQLiteAutoIDSuffix(%q) = (%d, %v), want (%d, %v)", tc.id, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSequenceArithmeticNeverWraps(t *testing.T) {
	if _, err := sequenceNext(math.MaxInt64); !errors.Is(err, ErrSQLiteSequenceExhausted) {
		t.Fatalf("sequenceNext(MaxInt64) err = %v, want exhausted", err)
	}
	if _, err := sequenceNext(-1); !errors.Is(err, ErrSQLiteSequenceExhausted) {
		t.Fatalf("sequenceNext(-1) err = %v, want exhausted (never re-enter 0 or the positive range)", err)
	}
	if got, err := sequenceNext(math.MinInt64); err != nil || got != math.MinInt64+1 {
		t.Fatalf("sequenceNext(MinInt64) = (%d, %v)", got, err)
	}
	for _, tc := range []struct{ base, want int64 }{
		{0, sqliteSequenceBlockSize},
		{math.MaxInt64 - 10, math.MaxInt64},
		{math.MaxInt64 - sqliteSequenceBlockSize, math.MaxInt64},
		{math.MinInt64, math.MinInt64 + sqliteSequenceBlockSize},
		{-10, -1},
		{-1 - sqliteSequenceBlockSize, -1},
	} {
		got, err := sequenceBlockEnd(tc.base, sqliteSequenceBlockSize)
		if err != nil || got != tc.want {
			t.Errorf("sequenceBlockEnd(%d) = (%d, %v), want %d", tc.base, got, err, tc.want)
		}
	}
	for _, base := range []int64{math.MaxInt64, -1} {
		if _, err := sequenceBlockEnd(base, sqliteSequenceBlockSize); !errors.Is(err, ErrSQLiteSequenceExhausted) {
			t.Errorf("sequenceBlockEnd(%d) err = %v, want exhausted", base, err)
		}
	}
	// Allocation order: every negative value ranks above every positive one.
	if !sequenceRankAbove(math.MinInt64, math.MaxInt64) || !sequenceRankAbove(-1, math.MinInt64) || sequenceRankAbove(0, 1) {
		t.Fatal("sequence rank order is wrong")
	}
}

func TestSQLiteSessionHexIDsNeverAffectSequence(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	for _, id := range sessionHexIDs {
		mustPin(t, store, id)
	}
	if got := mustMint(t, store); got != "gcg-1" {
		t.Fatalf("first mint beside session ids = %q, want gcg-1", got)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}

	// Reopen: recovery scans the session ids and must ignore them.
	reopened := openSeqStore(t, dir)
	got := autoValue(t, mustMint(t, reopened))
	if got <= 1 || got > 1+2*sqliteSequenceBlockSize {
		t.Fatalf("mint after reopen = %d, want just past the reserved block", got)
	}

	// Collision reseed path: another handle pins the next candidate, so this
	// store's next mint collides and reseeds from a scan that includes the
	// session ids.
	other := openSeqStore(t, dir)
	mustPin(t, other, "gcg-"+strconv.FormatInt(got+1, 10))
	next := autoValue(t, mustMint(t, reopened))
	if next != got+2 {
		t.Fatalf("mint after collision = %d, want %d (reseed must not be inflated by session ids)", next, got+2)
	}
}

func TestSQLiteSequenceRefusesToWrapAtMaxInt64(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	if err := store.SetSequenceFloor(math.MaxInt64 - 1); err != nil {
		t.Fatalf("SetSequenceFloor: %v", err)
	}
	if got := mustMint(t, store); got != "gcg-9223372036854775807" {
		t.Fatalf("last mint = %q, want gcg-9223372036854775807", got)
	}
	_, err := store.Create(Bead{Title: "one too many"})
	if !errors.Is(err, ErrSQLiteSequenceExhausted) {
		t.Fatalf("Create past MaxInt64 err = %v, want ErrSQLiteSequenceExhausted", err)
	}
	if !strings.Contains(err.Error(), sqliteSequenceRepairCommand) {
		t.Fatalf("exhaustion error %q does not name the repair command", err)
	}
	all, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if strings.HasPrefix(b.ID, "gcg--") {
			t.Fatalf("store wrapped and minted %q", b.ID)
		}
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}

	// A pinned MaxInt64 row (as the old loose parser produced) also exhausts
	// rather than wraps, after a reopen with no floor.
	dir2 := t.TempDir()
	pinned := openSeqStore(t, dir2)
	mustPin(t, pinned, "gcg-9223372036854775807")
	if err := pinned.CloseStore(); err != nil {
		t.Fatal(err)
	}
	reopened := openSeqStore(t, dir2)
	if _, err := reopened.Create(Bead{Title: "wrap?"}); !errors.Is(err, ErrSQLiteSequenceExhausted) {
		t.Fatalf("Create after pinned MaxInt64 err = %v, want ErrSQLiteSequenceExhausted", err)
	}
}

func TestSQLiteReopenAfterDeletingHighestIDsNeverReissues(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	issued := map[string]bool{}
	var ids []string
	for i := 0; i < 5; i++ {
		id := mustMint(t, store)
		issued[id] = true
		ids = append(ids, id)
	}
	for _, id := range ids[2:] {
		if err := store.Delete(id); err != nil {
			t.Fatalf("Delete(%q): %v", id, err)
		}
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		reopened := openSeqStore(t, dir)
		id := mustMint(t, reopened)
		if issued[id] {
			t.Fatalf("round %d: reopen reissued deleted id %q", round, id)
		}
		issued[id] = true
		if err := reopened.Delete(id); err != nil {
			t.Fatal(err)
		}
		if err := reopened.CloseStore(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSQLiteCollisionReseedNeverReissuesDeletedID(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, mustMint(t, store))
	}
	for _, id := range ids[1:] {
		if err := store.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	// A second handle pins the next candidate plus rows the old loose parser
	// would have read as huge suffixes, forcing a collision reseed.
	other := openSeqStore(t, dir)
	next := autoValue(t, ids[2]) + 1
	mustPin(t, other, "gcg-"+strconv.FormatInt(next, 10))
	for _, id := range sessionHexIDs {
		mustPin(t, other, id)
	}
	got := mustMint(t, store)
	for _, deleted := range ids[1:] {
		if got == deleted {
			t.Fatalf("collision reseed reissued deleted id %q", got)
		}
	}
	if want := "gcg-" + strconv.FormatInt(next+1, 10); got != want {
		t.Fatalf("mint after collision = %q, want %q", got, want)
	}
}

func TestSQLiteTwoStoresOnOneDirNeverMintSameID(t *testing.T) {
	dir := t.TempDir()
	stores := []*SQLiteStore{openSeqStore(t, dir), openSeqStore(t, dir)}
	const perStore = 3 * sqliteSequenceBlockSize / 2 // crosses a block boundary
	results := make([][]string, len(stores))
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func(i int, store *SQLiteStore) {
			defer wg.Done()
			for n := int64(0); n < perStore; n++ {
				id, err := store.nextID()
				if err != nil {
					t.Errorf("store %d nextID: %v", i, err)
					return
				}
				results[i] = append(results[i], id)
			}
		}(i, store)
	}
	wg.Wait()
	seen := map[string]int{}
	for i, ids := range results {
		for _, id := range ids {
			if prev, dup := seen[id]; dup {
				t.Fatalf("stores %d and %d both minted %q", prev, i, id)
			}
			seen[id] = i
		}
	}
	// A third store opened afterwards starts past both stores' blocks.
	third := openSeqStore(t, dir)
	if id := mustMint(t, third); func() bool { _, dup := seen[id]; return dup }() {
		t.Fatalf("a later store reissued %q", id)
	}
}

func TestSQLiteCrashBetweenReserveAndUseNeverReissues(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	// Ids handed out whose rows never became durable: one handed out with no
	// row at all, and one whose committed row is lost as a WAL tail would be.
	escapedNoRow, err := store.nextID()
	if err != nil {
		t.Fatal(err)
	}
	escapedLostRow := mustMint(t, store)
	if _, err := store.db.Exec(`DELETE FROM beads WHERE id=?`, escapedLostRow); err != nil {
		t.Fatal(err)
	}
	// "Crash": drop the handle without any further bookkeeping.
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	reopened := openSeqStore(t, dir)
	for i := 0; i < 5; i++ {
		id := mustMint(t, reopened)
		if id == escapedNoRow || id == escapedLostRow {
			t.Fatalf("reopen after crash reissued escaped id %q", id)
		}
		if autoValue(t, id) <= sqliteSequenceBlockSize {
			t.Fatalf("reopen minted %q inside the block reserved before the crash", id)
		}
	}
}

func TestSQLiteSequenceFloorIsDurableBeforeIDIsReturned(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	for i := 0; i < int(sqliteSequenceBlockSize)+2; i++ {
		id := mustMint(t, store)
		floor, err := readSQLiteSequenceFloor(store.sequenceFloorPath)
		if err != nil {
			t.Fatal(err)
		}
		if n := autoValue(t, id); sequenceRankAbove(n, floor) {
			t.Fatalf("minted %q while the persisted floor was only %d", id, floor)
		}
	}
}

func TestSQLiteNonGraphPrefixGetsItsOwnLeadingFloor(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gco"))
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	id := mustMint(t, store)
	if id != "gco-1" {
		t.Fatalf("first gco id = %q", id)
	}
	floor, err := readSQLiteSequenceFloor(filepath.Join(dir, "gco.seqfloor"))
	if err != nil || floor != sqliteSequenceBlockSize {
		t.Fatalf("gco.seqfloor = (%d, %v), want %d", floor, err, sqliteSequenceBlockSize)
	}
	if _, err := os.Stat(filepath.Join(dir, sqliteGraphSequenceFloorFilename)); !os.IsNotExist(err) {
		t.Fatalf("a gco store wrote graph.seqfloor: %v", err)
	}
}

func TestSQLiteReadOnlyStoreNeverWritesFloor(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	mustPin(t, store, "gcg-5")
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, sqliteGraphSequenceFloorFilename)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreReadOnly(), WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatal(err)
	}
	ro := opened.(*SQLiteStore)
	defer ro.CloseStore() //nolint:errcheck
	if _, err := ro.nextID(); err == nil {
		t.Fatal("read-only store minted an id")
	}
	if _, err := os.Stat(filepath.Join(dir, sqliteGraphSequenceFloorFilename)); !os.IsNotExist(err) {
		t.Fatalf("read-only store wrote its floor: %v", err)
	}
}

func TestSQLiteAdvanceSequenceFloorIgnoresNonPositive(t *testing.T) {
	store := openSeqStore(t, t.TempDir())
	store.AdvanceSequenceFloor(-5)
	store.AdvanceSequenceFloor(math.MinInt64)
	if got := mustMint(t, store); got != "gcg-1" {
		t.Fatalf("mint after negative AdvanceSequenceFloor = %q, want gcg-1", got)
	}
	if err := store.SetSequenceFloor(-1); err == nil {
		t.Fatal("SetSequenceFloor accepted a negative floor; only the operator repair may")
	}
}

// wrappedStore builds a store holding what a pre-fix build left behind: normal
// positive ids, inflated positive ids, and wrapped negative ids.
func wrappedStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	for _, id := range []string{
		"gcg-7", "gcg-9223372036854775806",
		"gcg--9223372036854775808", "gcg--9223372036854775807", "gcg--9223372036854761185",
	} {
		mustPin(t, store, id)
	}
	for _, id := range sessionHexIDs {
		mustPin(t, store, id)
	}
	if err := writeSQLiteSequenceFloor(store.sequenceFloorPath, 98636506); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSQLiteWrappedStoreFailsClosedUntilRepaired(t *testing.T) {
	dir := wrappedStore(t)
	store := openSeqStore(t, dir)
	_, err := store.Create(Bead{Title: "would reuse"})
	if !errors.Is(err, ErrSQLiteSequenceWrapped) {
		t.Fatalf("Create on wrapped store err = %v, want ErrSQLiteSequenceWrapped", err)
	}
	// Pinned creates and reads still work.
	mustPin(t, store, "gcg-session-feedface")
	if _, err := store.Get("gcg--9223372036854775808"); err != nil {
		t.Fatalf("Get on wrapped store: %v", err)
	}

	state, err := InspectSQLiteSequence(dir, "gcg")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Wrapped || !state.HasNegative || state.HighestNegative != -9223372036854761185 || state.HighestPositive != 9223372036854775806 || state.Floor != 98636506 {
		t.Fatalf("InspectSQLiteSequence = %+v", state)
	}

	const floor = int64(-9223372036854000000)
	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", floor); err != nil {
		t.Fatalf("RaiseSQLiteSequenceFloor: %v", err)
	}
	// The running store resumes without a restart, above the repaired floor.
	id := mustMint(t, store)
	if want := "gcg-" + strconv.FormatInt(floor+1, 10); id != want {
		t.Fatalf("mint after repair = %q, want %q", id, want)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	reopened := openSeqStore(t, dir)
	next := autoValue(t, mustMint(t, reopened))
	if next >= 0 || next <= floor+1 {
		t.Fatalf("mint after reopen = %d, want in the negative range above %d", next, floor+1)
	}
}

func TestRaiseSQLiteSequenceFloorRefusesToLower(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	mustMint(t, store) // persisted floor is now one block
	mustPin(t, store, "gcg-5000")
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", 10); err == nil {
		t.Fatal("raised the floor below the persisted floor")
	}
	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", 4999); err == nil {
		t.Fatal("raised the floor below the highest auto id present")
	}
	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", 100000); err != nil {
		t.Fatalf("raise to 100000: %v", err)
	}
	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", 100000); err != nil {
		t.Fatalf("idempotent raise: %v", err)
	}
	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", 99999); err == nil {
		t.Fatal("lowered the floor from 100000 to 99999")
	}
	// Into the negative range is a raise in allocation order ...
	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", math.MinInt64+10); err != nil {
		t.Fatalf("raise into the negative range: %v", err)
	}
	// ... and coming back out of it is a lowering.
	for _, lower := range []int64{math.MinInt64, math.MaxInt64, 200000} {
		if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", lower); err == nil {
			t.Fatalf("lowered the floor from %d to %d", int64(math.MinInt64+10), lower)
		}
	}
	floor, err := readSQLiteSequenceFloor(filepath.Join(dir, sqliteGraphSequenceFloorFilename))
	if err != nil || floor != math.MinInt64+10 {
		t.Fatalf("persisted floor = (%d, %v), want %d", floor, err, int64(math.MinInt64+10))
	}
	reopened := openSeqStore(t, dir)
	if got := mustMint(t, reopened); got != fmt.Sprintf("gcg-%d", int64(math.MinInt64+11)) {
		t.Fatalf("mint after negative repair = %q", got)
	}
}

func TestRaiseSQLiteSequenceFloorRefusesBelowWrappedRows(t *testing.T) {
	dir := wrappedStore(t)
	for _, floor := range []int64{math.MaxInt64, math.MinInt64, -9223372036854761186} {
		if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", floor); err == nil {
			t.Fatalf("accepted floor %d below the highest wrapped id", floor)
		}
	}
	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", -9223372036854761185); err != nil {
		t.Fatalf("floor equal to the highest wrapped id: %v", err)
	}
	store := openSeqStore(t, dir)
	if got := mustMint(t, store); got != "gcg--9223372036854761184" {
		t.Fatalf("mint = %q, want gcg--9223372036854761184", got)
	}
}

func TestSQLiteNegativeRangeStopsBeforeZero(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", -3); err != nil {
		t.Fatal(err)
	}
	reopened := openSeqStore(t, dir)
	for _, want := range []string{"gcg--2", "gcg--1"} {
		if got := mustMint(t, reopened); got != want {
			t.Fatalf("mint = %q, want %q", got, want)
		}
	}
	if _, err := reopened.Create(Bead{Title: "zero?"}); !errors.Is(err, ErrSQLiteSequenceExhausted) {
		t.Fatalf("Create past -1 err = %v, want ErrSQLiteSequenceExhausted", err)
	}
}

// TestSQLiteProductionRepairContinuesInNegativeRange reproduces the audited
// production shape: both MaxInt64 and MinInt64 were issued, the positive range
// is spent, and the operator floor is MinInt64 + 1,014,623. Positive rows up to
// MaxInt64 must not drag the allocator back to the wrap point.
func TestSQLiteProductionRepairContinuesInNegativeRange(t *testing.T) {
	const floor = int64(-9223372036853761185)
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	deleted := []string{"gcg-5", "gcg-103", "gcg--9223372036854761185"}
	for _, id := range []string{
		"gcg-9223372036854775807", "gcg--9223372036854775808", "gcg-98636506",
		"gcg--9223372036854761000",
	} {
		mustPin(t, store, id)
	}
	for _, id := range append(append([]string{}, deleted...), sessionHexIDs...) {
		mustPin(t, store, id)
	}
	for _, id := range deleted {
		if err := store.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeSQLiteSequenceFloor(store.sequenceFloorPath, 98636506); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}

	// Before the repair: minting refuses instead of wrapping to MinInt64.
	before := openSeqStore(t, dir)
	if _, err := before.Create(Bead{Title: "pre-repair"}); !errors.Is(err, ErrSQLiteSequenceWrapped) {
		t.Fatalf("pre-repair Create err = %v, want ErrSQLiteSequenceWrapped", err)
	}
	if err := before.CloseStore(); err != nil {
		t.Fatal(err)
	}

	if _, err := RaiseSQLiteSequenceFloor(dir, "gcg", floor); err != nil {
		t.Fatalf("RaiseSQLiteSequenceFloor(%d): %v", floor, err)
	}
	reopened := openSeqStore(t, dir)
	first := mustMint(t, reopened)
	if first != "gcg--9223372036853761184" {
		t.Fatalf("first mint after repair = %q, want gcg--9223372036853761184", first)
	}
	issued := map[string]bool{}
	for _, id := range deleted {
		issued[id] = true
	}
	for round := 0; round < 3; round++ {
		for i := 0; i < 5; i++ {
			id := mustMint(t, reopened)
			n := autoValue(t, id)
			if n >= 0 || n <= floor {
				t.Fatalf("minted %q outside (floor, -1]", id)
			}
			if issued[id] {
				t.Fatalf("reissued %q", id)
			}
			issued[id] = true
			if err := reopened.Delete(id); err != nil {
				t.Fatal(err)
			}
		}
		if err := reopened.CloseStore(); err != nil {
			t.Fatal(err)
		}
		reopened = openSeqStore(t, dir)
	}
	// A collision reseed after the repair cannot drag it back either: the
	// positive MaxInt64 row and MinInt64 row are both in the scan.
	other := openSeqStore(t, dir)
	reopened.sequenceFloorMu.Lock()
	candidate := reopened.seq + 1
	reopened.sequenceFloorMu.Unlock()
	mustPin(t, other, "gcg-"+strconv.FormatInt(candidate, 10))
	id := mustMint(t, reopened)
	if n := autoValue(t, id); n != candidate+1 || issued[id] {
		t.Fatalf("mint after post-repair collision = %q, want gcg-%d", id, candidate+1)
	}
}

// TestSQLitePinnedIDsMoveSequenceOnlyInStrictFormat covers the audited pinned
// path: a caller-supplied id moves the allocator only when it is exactly
// "<prefix>-<canonical positive int64>", and then only upward.
func TestSQLitePinnedIDsMoveSequenceOnlyInStrictFormat(t *testing.T) {
	store := openSeqStore(t, t.TempDir())
	for _, id := range append([]string{
		"gcg-order-tracking-20260929123456",
		"gcg-0000099",
		"gcg-mol-4242",
		"gcg-9223372036854775808", // out of range: must not clamp to MaxInt64
	}, sessionHexIDs...) {
		if _, err := store.Create(Bead{ID: id, Title: "pinned"}); err != nil {
			t.Fatalf("Create(%q): %v", id, err)
		}
	}
	if got := mustMint(t, store); got != "gcg-1" {
		t.Fatalf("mint after non-auto pinned ids = %q, want gcg-1", got)
	}
	if _, err := store.Create(Bead{ID: "gcg-40", Title: "strict pinned"}); err != nil {
		t.Fatal(err)
	}
	if got := mustMint(t, store); got != "gcg-41" {
		t.Fatalf("mint after strict pinned gcg-40 = %q, want gcg-41", got)
	}
	// A lower strict pin never moves it backwards.
	if _, err := store.Create(Bead{ID: "gcg-30", Title: "lower pinned"}); err != nil {
		t.Fatal(err)
	}
	if got := mustMint(t, store); got != "gcg-42" {
		t.Fatalf("mint after lower pinned id = %q, want gcg-42", got)
	}
	// A pinned wrapped id never drags the allocator into the negative range,
	// and is never read as its positive magnitude.
	if _, err := store.CreateWithForeignID(Bead{ID: "gcg--5000", Title: "wrapped pin"}); err != nil {
		t.Fatal(err)
	}
	if got := mustMint(t, store); got != "gcg-43" {
		t.Fatalf("mint after pinned gcg--5000 = %q, want gcg-43", got)
	}
}
