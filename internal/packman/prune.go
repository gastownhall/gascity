package packman

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// PruneEntry describes a single clone directory in the global pack cache and
// the prune decision made for it.
type PruneEntry struct {
	// Name is the cache directory name (a RepoCacheKey hash).
	Name string
	// Path is the absolute path to the clone directory.
	Path string
	// Bytes is the on-disk size of the clone directory.
	Bytes int64
	// ModTime is the directory's modification time, used by the keep-days guard.
	ModTime time.Time
	// Referenced reports whether some city's packs.lock pins this clone.
	Referenced bool
}

// PruneResult summarizes a prune pass over the global pack cache.
type PruneResult struct {
	// Kept lists clones retained because they are referenced or protected by
	// the keep-days guard.
	Kept []PruneEntry
	// Pruned lists clones classified as unreferenced and eligible for removal.
	// When Applied is false these were only reported, not deleted.
	Pruned []PruneEntry
	// FreedBytes is the total size of the Pruned entries.
	FreedBytes int64
	// Applied reports whether the Pruned entries were actually deleted.
	Applied bool
}

// classifyPruneEntries enumerates clone directories under cacheRoot and labels
// each KEEP or PRUNE. A clone is PRUNE only when it is unreferenced AND its
// mtime is older than keepDays days before now; otherwise it is KEEP. keepDays
// of zero disables the age guard. The lock file and any non-directory entries
// are ignored. cacheRoot need not exist (an absent cache prunes nothing).
func classifyPruneEntries(cacheRoot string, referenced map[string]bool, keepDays int, now time.Time) ([]PruneEntry, []PruneEntry, error) {
	dirents, err := os.ReadDir(cacheRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading repo cache root %q: %w", cacheRoot, err)
	}

	var keep, prune []PruneEntry
	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour)
	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		path := filepath.Join(cacheRoot, name)
		info, err := d.Info()
		if err != nil {
			return nil, nil, fmt.Errorf("stat repo cache entry %q: %w", path, err)
		}
		size, err := dirSize(path)
		if err != nil {
			return nil, nil, err
		}
		entry := PruneEntry{
			Name:       name,
			Path:       path,
			Bytes:      size,
			ModTime:    info.ModTime(),
			Referenced: referenced[name],
		}
		// Referenced clones are always kept; unreferenced clones newer than the
		// keep-days cutoff are kept to avoid racing an in-flight install.
		if entry.Referenced || (keepDays > 0 && entry.ModTime.After(cutoff)) {
			keep = append(keep, entry)
			continue
		}
		prune = append(prune, entry)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].Name < keep[j].Name })
	sort.Slice(prune, func(i, j int) bool { return prune[i].Name < prune[j].Name })
	return keep, prune, nil
}

// Prune removes unreferenced clones from the global pack cache root. referenced
// maps RepoCacheKey directory names that any city still pins to true. A clone is
// removed only when unreferenced AND older than keepDays days (keepDays of zero
// disables the age guard). When apply is false the result reports what would be
// removed without deleting anything. The shared repo-cache write lock is held
// across enumeration and deletion so prune never races a concurrent install.
func Prune(cacheRoot string, referenced map[string]bool, keepDays int, apply bool) (PruneResult, error) {
	var result PruneResult
	run := func() error {
		keep, prune, err := classifyPruneEntries(cacheRoot, referenced, keepDays, time.Now())
		if err != nil {
			return err
		}
		result.Kept = keep
		result.Pruned = prune
		result.Applied = apply
		for _, e := range prune {
			result.FreedBytes += e.Bytes
		}
		if !apply {
			return nil
		}
		for _, e := range prune {
			if err := os.RemoveAll(e.Path); err != nil {
				return fmt.Errorf("removing unreferenced cache clone %q: %w", e.Path, err)
			}
		}
		return nil
	}
	// WithRepoCacheReadLock no-ops when cacheRoot is absent; take the exclusive
	// write lock only when we may delete, otherwise the shared read lock.
	if apply {
		if _, err := config.WithRepoCacheWriteLock(cacheRoot, func() (string, error) {
			return "", run()
		}); err != nil {
			return PruneResult{}, err
		}
		return result, nil
	}
	if err := config.WithRepoCacheReadLock(cacheRoot, run); err != nil {
		return PruneResult{}, err
	}
	return result, nil
}

// dirSize returns the total size in bytes of all regular files under root.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("sizing %q: %w", root, err)
	}
	return total, nil
}
