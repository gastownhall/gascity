//go:build !windows

package runtime

// redactionInertEnvKey reports whether key names a variable in
// [envRedactionInert]. Variable names are case-sensitive here, so the lookup is
// exact.
func redactionInertEnvKey(key string) bool {
	return envRedactionInert[key]
}
