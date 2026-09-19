package worker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDerivedActivityMemoDerivesOncePerGeneration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sess.json")
	if err := os.WriteFile(path, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatalf("write mirror: %v", err)
	}
	memo := NewDerivedActivityMemo()
	derivations := 0
	derive := func() (TailActivity, error) {
		derivations++
		return TailActivityInTurn, nil
	}

	for i := 0; i < 3; i++ {
		got, err := memo.resolve(path, derive)
		if err != nil || got != TailActivityInTurn {
			t.Fatalf("resolve #%d = %q, %v; want in-turn", i+1, got, err)
		}
	}
	if derivations != 1 {
		t.Fatalf("derivations on an unchanged mirror = %d, want 1", derivations)
	}

	// A rewritten mirror (new size and mtime) is a new generation.
	if err := os.WriteFile(path, []byte(`{"messages":[{"reply":true}]}`), 0o644); err != nil {
		t.Fatalf("rewrite mirror: %v", err)
	}
	if _, err := memo.resolve(path, derive); err != nil {
		t.Fatalf("resolve after rewrite: %v", err)
	}
	if derivations != 2 {
		t.Fatalf("derivations after a rewrite = %d, want 2", derivations)
	}

	// Same size, later mtime: still a new generation — the mirror is rewritten
	// atomically per turn, and a same-size rewrite is not an unchanged one.
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, err := memo.resolve(path, derive); err != nil {
		t.Fatalf("resolve after touch: %v", err)
	}
	if derivations != 3 {
		t.Fatalf("derivations after a touch = %d, want 3", derivations)
	}
}

func TestDerivedActivityMemoDoesNotCacheFailures(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sess.json")
	if err := os.WriteFile(path, []byte(`{`), 0o644); err != nil {
		t.Fatalf("write mirror: %v", err)
	}
	memo := NewDerivedActivityMemo()
	derivations := 0
	failing := func() (TailActivity, error) {
		derivations++
		return TailActivityUnknown, errors.New("torn mirror")
	}
	for i := 0; i < 2; i++ {
		if _, err := memo.resolve(path, failing); err == nil {
			t.Fatalf("resolve #%d swallowed the derivation error", i+1)
		}
	}
	if derivations != 2 {
		t.Fatalf("a failed derivation was memoized: derivations = %d, want 2", derivations)
	}

	// A missing mirror is the derivation's error to report, not the memo's.
	if _, err := memo.resolve(filepath.Join(t.TempDir(), "absent.json"), failing); err == nil {
		t.Fatal("resolve on an absent mirror returned no error")
	}
}

// A nil memo is the zero SessionLogAdapter's: every call derives.
func TestDerivedActivityMemoNilDerivesEveryCall(t *testing.T) {
	t.Parallel()

	var memo *DerivedActivityMemo
	derivations := 0
	derive := func() (TailActivity, error) {
		derivations++
		return TailActivityIdle, nil
	}
	path := filepath.Join(t.TempDir(), "sess.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write mirror: %v", err)
	}
	for i := 0; i < 2; i++ {
		if got, err := memo.resolve(path, derive); err != nil || got != TailActivityIdle {
			t.Fatalf("nil memo resolve #%d = %q, %v", i+1, got, err)
		}
	}
	if derivations != 2 {
		t.Fatalf("nil memo derivations = %d, want 2", derivations)
	}
}
