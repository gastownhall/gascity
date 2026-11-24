//go:build !windows && !darwin

package workspacesvc

func foldServiceRootKey(key string) string {
	return key
}
