//go:build !windows

package workspacesvc

func foldServiceRootKey(key string) string {
	return key
}
