package packman

import (
	"testing"
)

// TestRepro_8705_StaleStatCacheClearedByRefreshBeforeDirtyCheck reproduces
// gascity#8705: a pack-cache file whose content is byte-identical to what's
// recorded in the cache's git index, but whose on-disk stat metadata
// (mtime/inode) went stale relative to that index — e.g. after a redeploy or
// a filesystem copy that re-renders the same bytes onto a new inode. `git
// status --porcelain` can keep reporting such an entry as locally modified
// purely from the stale cached stat info until something clears it, even
// though the content genuinely matches (git diff on the path is empty).
//
// This models that lifecycle with the package's existing fakeRunGit
// convention: "status --porcelain" answers dirty until "update-index
// --refresh" has been observed, at which point it answers clean — exactly
// the git-documented effect of running update-index --refresh against a
// stat-dirty-but-content-clean worktree. Without the fix (cachedRepoDirty
// never calling update-index --refresh), the status call never transitions
// to clean and this test fails.
func TestRepro_8705_StaleStatCacheClearedByRefreshBeforeDirtyCheck(t *testing.T) {
	refreshed := false
	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch {
		case len(args) == 2 && args[0] == "update-index" && args[1] == "--refresh":
			// Real git: update-index --refresh clears stale cached stat info
			// for entries whose content still matches the index, without
			// touching any file. Model that effect here.
			refreshed = true
			return "", nil
		case len(args) == 2 && args[0] == "status" && args[1] == "--porcelain":
			if !refreshed {
				// Simulates git status trusting stale stat metadata and
				// reporting the content-identical file as modified.
				return " M pack-cache-formula.toml\n", nil
			}
			return "", nil
		default:
			t.Fatalf("unexpected git call: %v", args)
			return "", nil
		}
	}
	t.Cleanup(func() { runGit = prev })

	dirty, err := cachedRepoDirty(t.TempDir())
	if err != nil {
		t.Fatalf("cachedRepoDirty: %v", err)
	}
	if dirty {
		t.Fatalf("cachedRepoDirty = true, want false: a stale-stat/content-clean "+
			"pack-cache file must not be treated as a local worktree change (calls=%v)", calls)
	}
	if !refreshed {
		t.Fatalf("cachedRepoDirty never ran `git update-index --refresh` before `git status --porcelain` (calls=%v)", calls)
	}
}
