package builtinpacks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// corruptionTarget is a file present in every materialized synthetic repo.
const corruptionTarget = "internal/bootstrap/packs/core/pack.toml"

// TestValidateSyntheticRepoDetectsCorruptionAfterSuccessfulValidation pins the
// self-heal contract that pack content validation must keep honoring: a cached
// file corrupted AFTER a successful validation, in the SAME process, is still
// detected.
//
// TestValidateSyntheticRepoRejectsTamperedContent does not cover this. It
// tampers before any validation has run, so a validator that caches its result
// still reports the tamper on what is, for it, a cold first call. This test
// validates first, which is what arms a cache, and only then corrupts.
func TestValidateSyntheticRepoDetectsCorruptionAfterSuccessfulValidation(t *testing.T) {
	dst := materializeTestRepo(t)

	if err := ValidateSyntheticRepo(dst, Repository, testCommit); err != nil {
		t.Fatalf("first ValidateSyntheticRepo: %v", err)
	}

	writeFile(t, filepath.Join(dst, corruptionTarget), `
[pack]
name = "tampered"
schema = 1
`)

	err := ValidateSyntheticRepo(dst, Repository, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted content corrupted after a successful validation")
	}
	if !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("error = %v, want content differs", err)
	}
}

// TestValidateSyntheticRepoDetectsSameSizeCorruptionAfterSuccessfulValidation is
// the same contract for a corruption that does not change the file's size, so
// nothing but the modification time distinguishes the corrupted cache from the
// good one. A validator that skips re-reading unchanged files must not decide
// "unchanged" on size alone.
func TestValidateSyntheticRepoDetectsSameSizeCorruptionAfterSuccessfulValidation(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(dst, corruptionTarget)

	if err := ValidateSyntheticRepo(dst, Repository, testCommit); err != nil {
		t.Fatalf("first ValidateSyntheticRepo: %v", err)
	}

	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", target, err)
	}
	corrupted := []byte(strings.Repeat("#", len(original)))
	writeFile(t, target, string(corrupted))

	if got, err := os.ReadFile(target); err != nil {
		t.Fatalf("ReadFile(%q) after corruption: %v", target, err)
	} else if len(got) != len(original) {
		t.Fatalf("corrupted file is %d bytes, want the original %d", len(got), len(original))
	}

	err = ValidateSyntheticRepo(dst, Repository, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted same-size content corruption after a successful validation")
	}
	if !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("error = %v, want content differs", err)
	}
}

// TestValidateSyntheticRepoDetectsMtimeRestoredCorruptionAfterSuccessfulValidation
// is the same contract for a same-size corruption whose modification time is
// then put back (os.Chtimes, cp -p, touch -r, rsync --times, a restore from
// backup), so size and mtime both match the good cache and only the inode
// change time (ctime) can tell the two apart. Userspace cannot set ctime, and
// the restoring Chtimes call itself bumps it.
//
// It does not cover coarse timestamp granularity (ext3, HFS+, some network
// mounts), where two writes inside one tick share an mtime AND a ctime without
// any deliberate restore: that depends on the filesystem's clock and cannot be
// reproduced portably in a unit test.
func TestValidateSyntheticRepoDetectsMtimeRestoredCorruptionAfterSuccessfulValidation(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(dst, corruptionTarget)

	if err := ValidateSyntheticRepo(dst, Repository, testCommit); err != nil {
		t.Fatalf("first ValidateSyntheticRepo: %v", err)
	}

	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", target, err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(%q): %v", target, err)
	}

	// Let the filesystem clock tick past the materialization so the rewrite's
	// ctime is distinguishable from the original on a coarse-clock kernel.
	time.Sleep(20 * time.Millisecond)

	writeFile(t, target, strings.Repeat("#", len(original)))
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("Chtimes(%q): %v", target, err)
	}

	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(%q) after restore: %v", target, err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("restore left size/mtime %d/%v, want the original %d/%v", after.Size(), after.ModTime(), before.Size(), before.ModTime())
	}

	err = ValidateSyntheticRepo(dst, Repository, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted same-size corruption with a restored mtime after a successful validation")
	}
	if !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("error = %v, want content differs", err)
	}
}

// BenchmarkValidateSyntheticRepoRepeated measures the repeated-validation path,
// which is what a gc invocation actually does: the cache is materialized once
// and then validated many times over the life of the process.
func BenchmarkValidateSyntheticRepoRepeated(b *testing.B) {
	dst := b.TempDir()
	if err := MaterializeSyntheticRepo(dst, Repository, testCommit); err != nil {
		b.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	if err := ValidateSyntheticRepo(dst, Repository, testCommit); err != nil {
		b.Fatalf("ValidateSyntheticRepo: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ValidateSyntheticRepo(dst, Repository, testCommit); err != nil {
			b.Fatalf("ValidateSyntheticRepo: %v", err)
		}
	}
}
