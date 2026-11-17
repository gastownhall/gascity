//go:build windows

package workspacesvc

import "strings"

// foldServiceRootKey folds case: Windows paths are case-insensitive, so two
// spellings that differ only in case name one state root.
func foldServiceRootKey(key string) string {
	return strings.ToLower(key)
}
