package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
)

func TestFirstManagedDoltCSVValue(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		want string
	}{
		"header + value":   {"@@datadir\n/var/db/dolt\n", "/var/db/dolt"},
		"quoted value":     {"@@datadir\n\"/var/db/dolt\"\n", "/var/db/dolt"},
		"crlf":             {"@@datadir\r\n/var/db/dolt\r\n", "/var/db/dolt"},
		"trailing blanks":  {"@@datadir\n/var/db/dolt\n\n", "/var/db/dolt"},
		"header only":      {"@@datadir\n", ""},
		"empty":            {"", ""},
		"whitespace value": {"@@datadir\n   /var/db/dolt   \n", "/var/db/dolt"},
	} {
		if got := firstManagedDoltCSVValue(tc.out); got != tc.want {
			t.Errorf("%s: firstManagedDoltCSVValue(%q) = %q, want %q", name, tc.out, got, tc.want)
		}
	}
}

func TestDataDirIsMismatch(t *testing.T) {
	expected := filepath.Join(t.TempDir(), ".beads", "dolt")
	for name, tc := range map[string]struct {
		serving string
		want    bool
	}{
		"same path":                           {expected, false},
		"same with whitespace":                {"  " + expected + "  ", false},
		"different path":                      {"/some/other/dolt", true},
		"serving empty":                       {"", false}, // cannot conclude → fail open
		"expected matches via trailing slash": {expected + "/", false},
	} {
		if got := dataDirIsMismatch(tc.serving, expected); got != tc.want {
			t.Errorf("%s: dataDirIsMismatch(%q, %q) = %v, want %v", name, tc.serving, expected, got, tc.want)
		}
	}
	if dataDirIsMismatch("/a/b", "") {
		t.Errorf("dataDirIsMismatch with empty expected = true, want false (fail open)")
	}
}

// TestManagedDoltDataDirMismatchForConn exercises the read+compare fix logic via
// the managedDoltServingDataDirFn seam (no live Dolt). This is the check whose
// absence let a port squatter drain the fleet.
func TestManagedDoltDataDirMismatchForConn(t *testing.T) {
	expected := filepath.Join(t.TempDir(), ".beads", "dolt")
	restore := managedDoltServingDataDirFn
	t.Cleanup(func() { managedDoltServingDataDirFn = restore })

	for name, tc := range map[string]struct {
		serving string
		err     error
		want    bool
	}{
		"store is ours":       {serving: expected, want: false},
		"squatter mismatch":   {serving: "/private/tmp/other/.beads/dolt", want: true},
		"probe error":         {err: errors.New("connection refused"), want: false}, // fail open
		"empty result":        {serving: "", want: false},                           // fail open
		"trailing-slash same": {serving: expected + "/", want: false},
	} {
		managedDoltServingDataDirFn = func(_ context.Context, _ string) (string, error) {
			return tc.serving, tc.err
		}
		got := managedDoltDataDirMismatchForConn(context.Background(), expected, "3306", nil)
		if got != tc.want {
			t.Errorf("%s: managedDoltDataDirMismatchForConn = %v, want %v", name, got, tc.want)
		}
	}
}

// TestStoreIdentityHold exercises the reconciler gate: it must hold ONLY when
// there is no assigned work, running sessions exist, AND the identity check
// confirms a mismatch. The identity check is stubbed via the seam so the gate
// is testable without a live store (this is the test that would have caught the
// original startup-only placement bug).
func TestStoreIdentityHold(t *testing.T) {
	restore := managedDoltDataDirMismatchFn
	t.Cleanup(func() { managedDoltDataDirMismatchFn = restore })

	for name, tc := range map[string]struct {
		drainPending bool
		mismatch     bool
		wantHold     bool
		wantProbed   bool
	}{
		"drain pending + mismatch":  {drainPending: true, mismatch: true, wantHold: true, wantProbed: true},
		"drain pending, store ours": {drainPending: true, mismatch: false, wantHold: false, wantProbed: true},
		"no drain skips probe":      {drainPending: false, mismatch: true, wantHold: false, wantProbed: false},
	} {
		probed := false
		managedDoltDataDirMismatchFn = func(_ context.Context, _ string, _ io.Writer) bool {
			probed = true
			return tc.mismatch
		}
		gotHold := storeIdentityHold(context.Background(), "/city", tc.drainPending, nil)
		if gotHold != tc.wantHold {
			t.Errorf("%s: storeIdentityHold = %v, want %v", name, gotHold, tc.wantHold)
		}
		if probed != tc.wantProbed {
			t.Errorf("%s: identity probed = %v, want %v (probe must be gated to a pending drain)", name, probed, tc.wantProbed)
		}
	}
}
