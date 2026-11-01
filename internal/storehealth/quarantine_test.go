package storehealth

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestQuarantineDirNameDeterministic asserts the name is a pure function of
// (scope, seq) — never wall-clock or random.
func TestQuarantineDirNameDeterministic(t *testing.T) {
	const scope = "/Users/Shared/city/rigs/vr"
	got1 := QuarantineDirName(scope, 0)
	got2 := QuarantineDirName(scope, 0)
	if got1 != got2 {
		t.Fatalf("non-deterministic name: %q != %q", got1, got2)
	}
	if got := QuarantineDirName(scope, 7); filepath.Base(got) != got {
		t.Fatalf("name %q contains a path separator; must be a single dir component", got)
	}
	// Distinct seqs yield distinct names.
	if QuarantineDirName(scope, 0) == QuarantineDirName(scope, 1) {
		t.Fatalf("seq 0 and 1 produced the same name")
	}
	// Distinct scopes yield distinct names.
	if QuarantineDirName("/a/b", 0) == QuarantineDirName("/a/c", 0) {
		t.Fatalf("distinct scopes collided")
	}
}

// TestQuarantineDirNameInjective asserts scopes that sanitize to the same
// prefix — they differ only in punctuation — still get distinct names, so
// two stores never share one sequence namespace.
func TestQuarantineDirNameInjective(t *testing.T) {
	scopes := []string{
		"/city/rigs/gc-management",
		"/city/rigs/gc/management",
		"/city/rigs/gc management",
		"/city/rigs/gc_management",
	}
	seen := map[string]string{}
	for _, s := range scopes {
		name := QuarantineDirName(s, 0)
		if prev, ok := seen[name]; ok {
			t.Fatalf("scopes %q and %q collided on %q", prev, s, name)
		}
		seen[name] = s
	}
}

// TestQuarantineDirNameBounded asserts a deep scope root still produces a
// name os.Mkdir accepts, and that an empty scope falls back to a stable
// token rather than a bare "-0".
func TestQuarantineDirNameBounded(t *testing.T) {
	deep := "/" + strings.Repeat("segment/", 64) + "leaf"
	name := QuarantineDirName(deep, 0)
	if len(name) > 255 {
		t.Fatalf("name is %d bytes, exceeds NAME_MAX", len(name))
	}
	if err := os.Mkdir(filepath.Join(t.TempDir(), name), 0o755); err != nil {
		t.Fatalf("mkdir deep-scope name: %v", err)
	}
	if got := QuarantineDirName("   ", 0); !strings.HasPrefix(got, "scope.") {
		t.Fatalf("empty scope fallback = %q, want scope.<digest>-0", got)
	}
}

// TestNextQuarantineSeqMonotonic asserts the seq is max(existing)+1, derived
// from directory entries, and starts at 0 on an empty/missing root.
func TestNextQuarantineSeqMonotonic(t *testing.T) {
	root := t.TempDir()
	const scope = "/city/rigs/hq"

	if got := mustNextSeq(t, filepath.Join(root, "missing"), scope); got != 0 {
		t.Fatalf("first seq on missing root = %d, want 0", got)
	}
	if got := mustNextSeq(t, root, scope); got != 0 {
		t.Fatalf("first seq on empty root = %d, want 0", got)
	}

	// Create seq 0, 1, 2 directories; next must be 3.
	for i := 0; i < 3; i++ {
		if err := os.Mkdir(filepath.Join(root, QuarantineDirName(scope, i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustNextSeq(t, root, scope); got != 3 {
		t.Fatalf("seq after 0,1,2 = %d, want 3", got)
	}

	// A different scope under the same root is independent.
	if got := mustNextSeq(t, root, "/city/rigs/vr"); got != 0 {
		t.Fatalf("independent scope seq = %d, want 0", got)
	}

	// A gap (delete seq 1) must still yield max+1 = 3 (monotonic, not
	// gap-filling), so names never collide with a prior reap's artifacts.
	if err := os.RemoveAll(filepath.Join(root, QuarantineDirName(scope, 1))); err != nil {
		t.Fatal(err)
	}
	if got := mustNextSeq(t, root, scope); got != 3 {
		t.Fatalf("seq after deleting middle entry = %d, want 3 (monotonic)", got)
	}
}

// TestNextQuarantineSeqUnreadableRoot asserts a root that exists but cannot
// be listed is an error, not seq 0: it may already hold prior artifacts.
func TestNextQuarantineSeqUnreadableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(root, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if seq, err := NextQuarantineSeq(root, "/city"); err == nil {
		t.Fatalf("NextQuarantineSeq on a non-directory root = %d, nil; want an error", seq)
	}
	if path, _, err := QuarantineDirPath(root, "/city"); err == nil {
		t.Fatalf("QuarantineDirPath on a non-directory root = %q, nil; want an error", path)
	}
}

// TestQuarantineDirPathSequencing asserts repeated path allocation (with
// the directory actually created between calls) accumulates deterministic,
// non-colliding sequence numbers.
func TestQuarantineDirPathSequencing(t *testing.T) {
	root := t.TempDir()
	const scope = "/city"

	var seqs []int
	for i := 0; i < 4; i++ {
		path, seq, err := QuarantineDirPath(root, scope)
		if err != nil {
			t.Fatalf("QuarantineDirPath: %v", err)
		}
		seqs = append(seqs, seq)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	if !reflect.DeepEqual(seqs, []int{0, 1, 2, 3}) {
		t.Fatalf("allocated seqs = %v, want [0 1 2 3]", seqs)
	}
	if got := sortedQuarantineSeqs(t, root, scope); !reflect.DeepEqual(got, []int{0, 1, 2, 3}) {
		t.Fatalf("on-disk seqs = %v, want [0 1 2 3]", got)
	}
}

func mustNextSeq(t *testing.T, root, scope string) int {
	t.Helper()
	seq, err := NextQuarantineSeq(root, scope)
	if err != nil {
		t.Fatalf("NextQuarantineSeq(%q, %q): %v", root, scope, err)
	}
	return seq
}

// sortedQuarantineSeqs returns the existing sequence numbers for a scope in
// ascending order.
func sortedQuarantineSeqs(t *testing.T, quarantineRoot, scope string) []int {
	t.Helper()
	entries, err := os.ReadDir(quarantineRoot)
	if err != nil {
		t.Fatalf("reading quarantine root: %v", err)
	}
	prefix := sanitizeScopeToken(scope) + "-"
	var seqs []int
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), prefix)); err == nil && n >= 0 {
			seqs = append(seqs, n)
		}
	}
	sort.Ints(seqs)
	return seqs
}
