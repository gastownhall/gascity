//go:build darwin

package workspacesvc

import "strings"

// foldServiceRootKey folds case: macOS volumes are case-insensitive unless
// formatted otherwise, so two spellings that differ only in case name one
// state root. On a case-sensitive volume, roots that differ only in case share
// a key, which can only make a construction wait on a reap it did not need to.
func foldServiceRootKey(key string) string {
	return strings.ToLower(key)
}
