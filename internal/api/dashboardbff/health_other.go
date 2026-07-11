//go:build !linux && !darwin

package dashboardbff

// readPlatformMetrics has no host-metric source on platforms without a
// dedicated reader (only Linux procfs and Darwin sysctls are implemented). It
// returns the zero value so GET /api/health/system still serves a well-formed
// response; heap stats and CPU count in currentSystemHealth remain live.
func readPlatformMetrics() platformMetrics {
	return platformMetrics{}
}
