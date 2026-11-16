//go:build integration && !windows

package subprocess

// cmdSleepMedium outlives a conformance run. It lives beside the integration
// tag because only the seam conformance suite starts it.
const cmdSleepMedium = "sleep 300"
