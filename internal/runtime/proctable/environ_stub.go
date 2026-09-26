//go:build !linux && !darwin

package proctable

// ProcessEnvValue returns "" on platforms without process environment
// scanning support, where [ScanBySessionID] reports no roots.
func ProcessEnvValue(int, string) (string, error) {
	return "", nil
}
