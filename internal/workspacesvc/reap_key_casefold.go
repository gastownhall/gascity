//go:build windows || darwin

package workspacesvc

import "strings"

// foldServiceRootKey folds case: both Windows (NTFS) and macOS (APFS, HFS+)
// default to case-insensitive, case-preserving file systems, so two
// spellings of a state root that differ only in case name the same
// directory and must collide on one reap-registry key
// (TestProxyProcessReapRegistryMatchesAnEquivalentSpelling). A case-sensitive
// APFS volume is possible but not the default Apple ships, and this fold
// only widens which spellings collide — it never splits one directory's
// pending reaps across two keys.
func foldServiceRootKey(key string) string {
	return strings.ToLower(key)
}
