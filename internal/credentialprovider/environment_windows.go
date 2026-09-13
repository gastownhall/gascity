//go:build windows

package credentialprovider

import "strings"

func environmentLookupKey(key string) string {
	return strings.ToUpper(key)
}

func allowedPlatformEnvironmentKey(key string) bool {
	switch key {
	case "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "SYSTEMROOT", "COMSPEC", "PATHEXT", "TEMP", "TMP":
		return true
	default:
		return false
	}
}
