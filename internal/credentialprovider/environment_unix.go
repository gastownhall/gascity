//go:build !windows

package credentialprovider

func environmentLookupKey(key string) string {
	return key
}

func allowedPlatformEnvironmentKey(key string) bool {
	switch key {
	case "HOME", "XDG_CONFIG_HOME":
		return true
	default:
		return false
	}
}
