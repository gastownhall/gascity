package storehealth

import (
	"os"
	"path/filepath"
	"reflect"
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

// TestNextQuarantineSeqMonotonic asserts the seq is max(existing)+1, derived
// from directory entries, and starts at 0 on an empty/missing root.
func TestNextQuarantineSeqMonotonic(t *testing.T) {
	root := t.TempDir()
	const scope = "/city/rigs/hq"

	if got := NextQuarantineSeq(root, scope); got != 0 {
		t.Fatalf("first seq on empty root = %d, want 0", got)
	}

	// Create seq 0, 1, 2 directories; next must be 3.
	for i := 0; i < 3; i++ {
		if err := os.Mkdir(filepath.Join(root, QuarantineDirName(scope, i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := NextQuarantineSeq(root, scope); got != 3 {
		t.Fatalf("seq after 0,1,2 = %d, want 3", got)
	}

	// A different scope under the same root is independent.
	if got := NextQuarantineSeq(root, "/city/rigs/vr"); got != 0 {
		t.Fatalf("independent scope seq = %d, want 0", got)
	}

	// A gap (delete seq 1) must still yield max+1 = 3 (monotonic, not
	// gap-filling), so names never collide with a prior reap's artifacts.
	if err := os.RemoveAll(filepath.Join(root, QuarantineDirName(scope, 1))); err != nil {
		t.Fatal(err)
	}
	if got := NextQuarantineSeq(root, scope); got != 3 {
		t.Fatalf("seq after deleting middle entry = %d, want 3 (monotonic)", got)
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
		path, seq := QuarantineDirPath(root, scope)
		seqs = append(seqs, seq)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	if !reflect.DeepEqual(seqs, []int{0, 1, 2, 3}) {
		t.Fatalf("allocated seqs = %v, want [0 1 2 3]", seqs)
	}
	if got := sortedQuarantineSeqs(root, scope); !reflect.DeepEqual(got, []int{0, 1, 2, 3}) {
		t.Fatalf("on-disk seqs = %v, want [0 1 2 3]", got)
	}
}
