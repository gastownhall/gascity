//go:build !windows

package config

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// The strict default is what the controller, start and `gc config` load:
// the error text is exactly the pre-isolation text.
func TestLoadStrictDefaultStillFailsOnDirtyCacheRig(t *testing.T) {
	f := newRigQuarantineFixture(t)
	f.dirtyCache(t)

	_, _, err := LoadWithIncludesOptions(fsys.OSFS{}, f.cityToml(), LoadOptions{})
	if err == nil {
		t.Fatal("strict load succeeded; want the dirty-cache error")
	}
	want := fmt.Sprintf(`expanding packs: rig "bad" import "broken": cached import %s has local worktree changes in %q: " M pack.toml"; run "gc import install"`, quarantineBrokenSource, f.cacheDir)
	if err.Error() != want {
		t.Fatalf("strict error = %q\nwant exactly %q", err, want)
	}
}
