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
		"status:           WRAPPED",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report missing %q:\n%s", want, stdout)
		}
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
