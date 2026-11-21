//go:build !windows && !darwin

package workspacesvc

// foldServiceRootKey is the identity on platforms whose default file
// systems (ext4, etc.) are case-sensitive: two spellings that differ only in
// case name two different directories, so the reap registry must not treat
// them as one key.
func foldServiceRootKey(key string) string {
	return key
}
