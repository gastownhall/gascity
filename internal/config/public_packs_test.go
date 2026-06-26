package config

import "testing"

// TestSupersededPublicPackVersionsAreUnique guards the append-only superseded
// pin histories against duplicate entries. The lists are documented as
// "oldest first" and appended singly when a canonical pin is bumped; a
// union/concat (e.g. a botched merge conflict resolution) that reintroduces a
// pin must fail here rather than silently double-pin a version.
func TestSupersededPublicPackVersionsAreUnique(t *testing.T) {
	lists := map[string][]string{
		"SupersededBundledPackImportVersions": SupersededBundledPackImportVersions,
		"SupersededPublicGastownPackVersions": SupersededPublicGastownPackVersions,
		"SupersededPublicGascityPackVersions": SupersededPublicGascityPackVersions,
	}
	for name, versions := range lists {
		seen := make(map[string]int, len(versions))
		for i, v := range versions {
			if prev, ok := seen[v]; ok {
				t.Errorf("%s has duplicate %q at indexes %d and %d", name, v, prev, i)
				continue
			}
			seen[v] = i
		}
	}
}
