package main

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/builtinpacks"
)

// syntheticCacheVerifier deduplicates builtinpacks.ValidateSyntheticRepo
// within ONE builtin readiness pass.
//
// Every bundled source of a repository resolves to a single synthetic cache
// directory — the cache key strips the subpath — and ValidateSyntheticRepo
// checks that repository's pack layouts in that directory regardless of which
// of its sources asked. requiredBuiltinSourcesUsable and
// lockedBundledImportsUsable therefore ask the identical question about the
// identical directory, and that question is not cheap: it os.ReadFile's every
// cached pack file and compares it against the copy embedded in the running
// binary.
//
// Only POSITIVE verdicts are memoized, and that is what makes the repair paths
// safe: no verdict about a broken cache is ever recorded, so there is nothing
// stale for a caller to observe after it repairs one. A caller that repaired a
// cache necessarily got its negative verdict from a full validation, and the
// next question about that directory runs a full validation too.
//
// A verifier is scoped to one readiness pass and is not safe for concurrent
// use. A verdict therefore never outlives the pass that produced it: every
// pass still runs the full validator at least once, which is what detects and
// repairs a corrupted cache.
type syntheticCacheVerifier struct {
	valid    map[string]struct{}
	validate func(cachePath, repository, commit string) error
}

// newSyntheticCacheVerifier returns a verifier scoped to a single readiness
// pass.
func newSyntheticCacheVerifier() *syntheticCacheVerifier {
	return &syntheticCacheVerifier{
		valid:    make(map[string]struct{}),
		validate: builtinpacks.ValidateSyntheticRepo,
	}
}

// warmSyntheticCacheVerdicts memoizes POSITIVE full-validation verdicts across
// readiness passes in this process, gated by a cheap stat fingerprint of the
// materialized cache tree (per-file size+mtime+mode and every directory path,
// no content reads).
//
// The ready fast path must still notice a cached pack file that was corrupted
// in place after the city was readied. Re-reading and comparing every cached
// file on every config load is what made that guarantee cost O(pack files) per
// gc command. Any ordinary write to a cached file changes its size or mtime and
// therefore the fingerprint, which forces the full validator to run again and
// repair the cache.
//
// packContentHashCache is the closest sibling, but the analogy needs stating
// carefully in both directions, because the two memos gate different verdicts.
//
// What it memoizes is a content hash, which depends on paths and bytes alone,
// so a size+mtime+ctime fingerprint covers its verdict entirely bar a triple
// collision. What this memoizes is a ValidateSyntheticRepo verdict, which also
// depends on permission bits and on the set of directories — so the sibling's
// fingerprint, applied here unchanged, would leave two gaps it does not have.
// Hashing mode and every directory path closes those two.
//
// One gap remains, and here it is wider than the sibling's rather than equal to
// it: this fingerprint does not hash ctime, so a content edit that preserves
// size and mtime (cp -p, rsync --checksum --times) can still ride a stale
// positive verdict, which #5367 closed for packContentHashCache. Adopting it
// here needs internal/config's statCtimeNanos to be shared rather than
// duplicated, so it is deliberately left as follow-up work.
var warmSyntheticCacheVerdicts sync.Map // key(string) -> uint64 fingerprint

// newWarmSyntheticCacheVerifier returns a verifier for the ALREADY-READY fast
// path. It runs the same full validation as the cold path, but reuses a
// positive verdict from an earlier pass while the cache tree's stat fingerprint
// is unchanged.
func newWarmSyntheticCacheVerifier() *syntheticCacheVerifier {
	return &syntheticCacheVerifier{
		valid: make(map[string]struct{}),
		validate: func(cachePath, repository, commit string) error {
			key := cachePath + "\x00" + repository + "\x00" + commit
			fp, err := syntheticCacheStatFingerprint(cachePath)
			if err == nil {
				if prev, ok := warmSyntheticCacheVerdicts.Load(key); ok && prev.(uint64) == fp {
					return nil
				}
			}
			if vErr := builtinpacks.ValidateSyntheticRepo(cachePath, repository, commit); vErr != nil {
				warmSyntheticCacheVerdicts.Delete(key)
				return vErr
			}
			if err == nil {
				warmSyntheticCacheVerdicts.Store(key, fp)
			}
			return nil
		},
	}
}

// syntheticCacheStatFingerprint hashes the shape of the tree under dir --
// every entry's path, plus each file's size, modification time and permission
// bits -- reading no file contents.
//
// The entries mirror what ValidateSyntheticRepo rejects, because anything it
// rejects that this cannot see would ride a stale positive verdict:
//
//   - Permission bits are hashed because validatePackFiles fails a file whose
//     mode differs from the embedded copy, and chmod moves ctime rather than
//     mtime or size. The bundled bd shim (gc-beads-bd.sh) is materialized
//     0o755 and exec'd, so a cleared executable bit is a real breakage.
//   - Directories are hashed by path alone because
//     validateSyntheticRepoFileSet rejects an unexpected directory whatever
//     its timestamps, and an added directory need contain no files at all. Its
//     own mtime is deliberately not hashed: it moves whenever a child is
//     written, which that child's own entry already covers, so hashing it
//     would invalidate the memo spuriously.
//
// Entries are tagged "f"/"d" so a file and a directory sharing a path across
// materializations cannot hash alike. This remains stat-only, so the cost is
// unchanged from hashing size and mtime.
func syntheticCacheStatFingerprint(dir string) (uint64, error) {
	h := fnv.New64a()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if d.IsDir() {
			fmt.Fprintf(h, "d\x00%s\x00", slash) //nolint:errcheck
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		fmt.Fprintf(h, "f\x00%s\x00%d\x00%d\x00%04o\x00", slash, info.Size(), info.ModTime().UnixNano(), info.Mode().Perm()) //nolint:errcheck
		return nil
	})
	if err != nil {
		return 0, err
	}
	return h.Sum64(), nil
}

// Valid reports whether the synthetic pack cache at cachePath validates as
// repository's cache at commit, reusing a positive verdict already reached in
// this pass.
//
// The repository is part of the memo key even though a cache directory can
// only ever belong to one repository: a verdict is recorded for the exact
// question that was asked, so a caller that derived a different repository for
// the same directory gets its own answer from the validator rather than one
// reached for a question it did not ask.
func (v *syntheticCacheVerifier) Valid(cachePath, repository, commit string) bool {
	key := cachePath + "\x00" + repository + "\x00" + commit
	if _, ok := v.valid[key]; ok {
		return true
	}
	if v.validate(cachePath, repository, commit) != nil {
		return false
	}
	v.valid[key] = struct{}{}
	return true
}
