package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func seedRepairSequenceStore(t *testing.T, pinned ...string) string {
	t.Helper()
	dir := t.TempDir()
	opened, err := beads.OpenSQLiteStore(dir, beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*beads.SQLiteStore)
	for _, id := range pinned {
		if _, err := store.CreateWithForeignID(beads.Bead{ID: id, Title: "pinned"}); err != nil {
			t.Fatalf("CreateWithForeignID(%q): %v", id, err)
		}
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runRepairSequence(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newStorageCmd(&stdout, &stderr)
	cmd.SetArgs(append([]string{storageRepairSequenceVerb}, args...))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestStorageRepairSequenceReportsWrappedStore(t *testing.T) {
	dir := seedRepairSequenceStore(t, "gcg-9223372036854775807", "gcg--9223372036854775808", "gcg-session-720a2f0e555819670941710447925531")
	stdout, stderr, err := runRepairSequence(t, "--dir", dir)
	if err != nil {
		t.Fatalf("report: %v\nstderr: %s", err, stderr)
	}
	for _, want := range []string{
		"highest positive: 9223372036854775807",
		"highest wrapped:  -9223372036854775808",
		"status:           WRAPPED - minting refused: gcg--9223372036854775808 ranks above the persisted floor",
		"remedy:           if a copy or import pinned gcg--<n> rows into a store that never wrapped and they are stray duplicates, ",
		"delete every gcg--<n> row above the persisted floor, not only gcg--9223372036854775808, and reopen the store; ",
		"otherwise (they are real beads, or an older build wrapped this store) raise --floor to at or above every gcg--<n> id ever issued (irreversible)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report missing %q:\n%s", want, stdout)
		}
	}
}

// TestStorageRepairSequenceHelpNamesBothWrappedRemedies pins the operator
// guidance a refusing store points at: stray negative ids pinned into a store
// that never wrapped are all deleted rather than covered by an irreversible
// floor raise, real beads are never deleted to avoid that raise, and the help
// says why the fleet is stopped for a real repair.
func TestStorageRepairSequenceHelpNamesBothWrappedRemedies(t *testing.T) {
	stdout, stderr, err := runRepairSequence(t, "--help")
	if err != nil {
		t.Fatalf("--help: %v\nstderr: %s", err, stderr)
	}
	help := strings.Join(strings.Fields(stdout), " ")
	for _, want := range []string{
		`pinned "<prefix>--<n>" ids into a store that never wrapped. If those rows are stray duplicates, do not raise the floor: ` +
			`delete every "<prefix>--<n>" row above the floor, not only the one the report names, and reopen the store`,
		"Deleting destroys those beads, so if any of them is a real bead, raise the floor instead.",
		"That cannot be undone.",
		"stop every process serving it that runs a build without this fix, so none keeps minting wrapped ids past the floor you pick",
		"one that refuses to mint resumes at its next mint once the floor covers the store",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(help, "because an older build wrapped the sequence") {
		t.Errorf("help still attributes every refusal to an older build:\n%s", stdout)
	}
}

func TestStorageRepairSequenceRaisesIntoNegativeRangeAndRefusesToLower(t *testing.T) {
	dir := seedRepairSequenceStore(t, "gcg-9223372036854775807", "gcg--9223372036854775808")
	stdout, stderr, err := runRepairSequence(t, "--dir", dir, "--floor=-9223372036853761185")
	if err != nil {
		t.Fatalf("raise: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "floor 0 -> -9223372036853761185") || !strings.Contains(stdout, "status:           ok") {
		t.Fatalf("raise output:\n%s", stdout)
	}
	for _, lower := range []string{"--floor=-9223372036853761186", "--floor=100", "--floor=9223372036854775807"} {
		_, stderr, err := runRepairSequence(t, "--dir", dir, lower)
		if err == nil || (!strings.Contains(stderr, "refusing to lower") && !strings.Contains(stderr, "below the highest")) {
			t.Fatalf("%s: err=%v stderr=%q, want refusal to lower", lower, err, stderr)
		}
	}
	for _, bad := range []string{"--floor=abc", "--floor=+5", "--floor=007", "--floor=9223372036854775808"} {
		if _, stderr, err := runRepairSequence(t, "--dir", dir, bad); err == nil || !strings.Contains(stderr, "canonical int64") {
			t.Fatalf("%s: err=%v stderr=%q, want canonical-int64 refusal", bad, err, stderr)
		}
	}

	opened, err := beads.OpenSQLiteStore(dir, beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	created, err := store.Create(beads.Bead{Title: "after repair"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "gcg--9223372036853761184" {
		t.Fatalf("mint after CLI repair = %q, want gcg--9223372036853761184", created.ID)
	}
}
