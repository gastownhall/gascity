package builtinpacks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corruptionTarget is a file present in every materialized synthetic repo.
const corruptionTarget = "internal/bootstrap/packs/core/pack.toml"

// TestValidateSyntheticRepoDetectsCorruptionAfterSuccessfulValidation pins the
// self-heal contract that pack content validation must keep honouring: a cached
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
