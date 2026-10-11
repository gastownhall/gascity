package main

import "testing"

func TestCountControlBdCallsSplitsReadsAndWrites(t *testing.T) {
	runner := countControlBdCalls(func(_, _ string, _ ...string) ([]byte, error) { return nil, nil })
	start := snapshotControlBdCalls()
	for _, args := range [][]string{
		{"show", "--json", "gc-1"},
		{"list", "--json", "--all"},
		{"dep", "list", "gc-1", "--json"},
		{"ready", "--json"},
		{"update", "--json", "gc-1", "--status", "closed"},
		{"close", "gc-1", "--force"},
		{"--dolt-auto-commit", "off", "create", "--json", "x"},
		{"dep", "add", "gc-1", "gc-2"},
	} {
		if _, err := runner("/city", "bd", args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runner("/city", "git", "status"); err != nil {
		t.Fatal(err)
	}
	got := snapshotControlBdCalls().since(start)
	if got.reads != 4 || got.writes != 4 {
		t.Fatalf("counted %d reads and %d writes, want 4 and 4 (non-bd commands uncounted)", got.reads, got.writes)
	}
}
